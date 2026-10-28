package handlers

import (
	"errors"
	scraper "instafix/handlers/scraper"
	"instafix/observability"
	"instafix/utils"
	"instafix/views"
	"instafix/views/model"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var (
	statelessNormalEdgeTTL   = 5 * time.Minute
	statelessSignedURLMargin = 30 * time.Minute
)

// ConfigureStatelessCacheDurations tunes the experiment's Cloudflare edge TTL.
// Pages that advertise signed Instagram CDN URLs are always capped by the URL
// expiry minus the configured safety margin.
func ConfigureStatelessCacheDurations(normalTTL, signedURLMargin time.Duration) {
	if normalTTL > 0 {
		statelessNormalEdgeTTL = normalTTL
	}
	if signedURLMargin >= 0 {
		statelessSignedURLMargin = signedURLMargin
	}
}

// EmbedStateless is the experimental cookie-free preview path. It intentionally
// does not call auth-helper, OGInstagram proxy/fallback code, or the local MP4
// proxy. Video OpenGraph metadata points at the fresh Instagram CDN URL itself.
func EmbedStateless(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Instagram7-Experiment", "stateless-azure")
	w.Header().Add("Vary", "User-Agent")

	viewsData := &model.ViewsData{}
	postID := chi.URLParam(r, "postID")
	userAgent := r.Header.Get("User-Agent")
	preferVideo := strings.Contains(r.URL.Path, "/reel/") || strings.Contains(r.URL.Path, "/reels/") || strings.Contains(r.URL.Path, "/tv/")
	trackTelegramVideo := isTelegramBot(userAgent) && preferVideo && r.URL.Query().Get("original") != "1" && r.URL.Query().Get("tgdiag") == ""

	viewsData.URL = instagramOriginURL(r.URL.Path, postID)
	viewsData.CanonicalURL = viewsData.URL
	viewsData.Site = "InstaFix Revived"
	if !utils.IsBot(userAgent) {
		redirectStatelessNoStore(w, r, viewsData.URL)
		return
	}

	mediaNumRaw := chi.URLParam(r, "mediaNum")
	if mediaNumRaw == "" {
		mediaNumRaw = r.URL.Query().Get("img_index")
	}
	if mediaNumRaw == "" {
		mediaNumRaw = "0"
	}
	mediaNum, err := strconv.Atoi(mediaNumRaw)
	if err != nil || mediaNum < 0 {
		viewsData.Description = "Invalid img_index parameter"
		if trackTelegramVideo {
			observability.Default.RecordTelegramVideoDecision(postID, "blocked", "invalid_media_index", 0)
		}
		observability.Default.RecordPreviewWithReason(r, postID, "failed", "", "invalid_media_index")
		views.Embed(viewsData, w)
		return
	}

	isDirect, _ := strconv.ParseBool(r.URL.Query().Get("direct"))
	isGallery, _ := strconv.ParseBool(r.URL.Query().Get("gallery"))
	if embedType := r.Header.Get("X-Embed-Type"); embedType == "direct" {
		isDirect = true
	} else if embedType == "gallery" {
		isGallery = true
	}

	var item *scraper.InstaData
	if preferVideo {
		item, err = scraper.GetDataPreferVideoQuiet(postID)
	} else {
		item, err = scraper.GetDataQuiet(postID)
		if upgraded, upgradeErr, ok := tryVideoUpgradeForPostAlias(r, postID, item, err); ok {
			item, err, preferVideo = upgraded, upgradeErr, true
		} else if err != nil || item == nil || len(item.Medias) == 0 {
			if videoItem, videoErr := scraper.GetDataPreferVideoQuiet(postID); videoErr == nil && videoItem != nil && videoItem.HasVideo() {
				item = videoItem
				err = nil
				preferVideo = true
			}
		}
	}
	trackTelegramVideo = isTelegramBot(userAgent) && preferVideo && r.URL.Query().Get("original") != "1" && r.URL.Query().Get("tgdiag") == ""
	if preferVideo && item != nil && isPreviewMediaBot(userAgent) && (item.Username == "" || item.Caption == "") {
		_ = scraper.EnrichMetadataFromJina(item)
	}
	// When the media resolver returned only a Reel poster (or no media at
	// all), advertise the configured client-only media endpoint to Telegram.
	// This also covers confirmed /p/ aliases, without affecting photo posts
	// or complete locally resolved videos.
	if tryStatelessTelegramClientMediaFallback(w, r, postID, mediaNum, preferVideo, viewsData, item, err) {
		return
	}
	if err != nil || item == nil || len(item.Medias) == 0 {
		setStatelessFallbackCacheHeaders(w)
		if err == nil {
			err = scraper.ErrNotFound
		}
		if trackTelegramVideo {
			observability.Default.RecordTelegramVideoDecision(postID, "blocked", telegramVideoFailureReason(err), 0)
		}
		logEmbedDecision(r, postID, preferVideo, mediaNum, item, scraper.Media{}, viewsData, err, isTelegramBot(userAgent), false, "stateless_fallback")
		renderFallbackEmbed(w, r, viewsData, postID, err)
		return
	}

	selected := max(1, mediaNum) - 1
	if selected < 0 || selected >= len(item.Medias) {
		setStatelessFallbackCacheHeaders(w)
		viewsData.Description = "Media number out of range"
		if trackTelegramVideo {
			observability.Default.RecordTelegramVideoDecision(postID, "blocked", "media_index_out_of_range", 0)
		}
		observability.Default.RecordPreviewWithReason(r, postID, "failed", "", "media_index_out_of_range")
		views.Embed(viewsData, w)
		return
	}

	publicBaseURL := requestPublicBaseURL(r)
	viewsData.FaviconURL = publicBaseURL + "/favicon.svg"
	viewsData.AppleIconURL = publicBaseURL + "/favicon.svg"
	viewsData.Card = "summary_large_image"
	viewsData.OGType = "article"
	viewsData.NoRedirect = true
	if item.Username != "" {
		viewsData.Title = "@" + item.Username
		viewsData.Creator = "@" + item.Username
		viewsData.ArticleAuthor = "https://www.instagram.com/" + item.Username + "/"
	} else if !preferVideo {
		viewsData.Title = "Instagram post"
	}
	if !isGallery {
		viewsData.Description = embedDescription(item)
		if preferVideo && viewsData.Title == "" {
			viewsData.Title = previewTitleFromDescription(viewsData.Description)
		}
	}

	originalMedia := item.Medias[selected]
	forceOriginalVideo := r.URL.Query().Get("original") == "1"
	now := time.Now()
	selectedItem, media, selectErr := selectStatelessMedia(postID, selected, item, now)
	if selectErr != nil {
		setStatelessFallbackCacheHeaders(w)
		if trackTelegramVideo {
			observability.Default.RecordTelegramVideoDecision(postID, "blocked", telegramVideoFailureReason(selectErr), 0)
		}
		logEmbedDecision(r, postID, preferVideo, mediaNum, item, originalMedia, viewsData, selectErr, isTelegramBot(userAgent), false, "stateless_fallback")
		renderFallbackEmbed(w, r, viewsData, postID, selectErr)
		return
	}
	item = selectedItem

	// The legacy scrape path can carry the cover-image dimensions for a Reel
	// even when its progressive MP4 is a smaller encode. OGInstagram resolves
	// its attachment from the logged-out V1 GraphQL shape, whose video_versions
	// dimensions match the actual MP4. For the explicit original-path test,
	// refresh from that same public source so Telegram sees truthful og:video
	// dimensions as well as the same CDN object.
	if forceOriginalVideo && media.IsVideo() {
		if refreshed, refreshErr := scraper.RefreshVideoFromPublicGraphQLPreserveMetadata(postID, item); refreshErr == nil {
			if candidate, ok := statelessCandidateMedia(refreshed, selected, true, now); ok {
				item, media = refreshed, candidate
				w.Header().Set("X-InstaFix-Original-Source", "public-gql-v1")
			}
		}

		// Refresh presentation metadata after the V1 replacement as well. The
		// original diagnostic used to calculate Description before this refresh,
		// which could leave og:description/twitter:description empty while the
		// refreshed item already carried a valid caption (visible in image:alt).
		// Keep every metadata field internally consistent with the item that owns
		// the advertised video URL.
		if item.Username != "" {
			viewsData.Title = "@" + item.Username
			viewsData.Creator = "@" + item.Username
			viewsData.ArticleAuthor = "https://www.instagram.com/" + item.Username + "/"
		} else {
			viewsData.Title = ""
			viewsData.Creator = ""
			viewsData.ArticleAuthor = ""
		}
		if !isGallery {
			viewsData.Description = embedDescription(item)
			if viewsData.Title == "" {
				viewsData.Title = previewTitleFromDescription(viewsData.Description)
			}
		}
	}

	compactTelegramVideo := false
	telegramOriginalBytes := int64(0)
	telegramOriginalSizeKnown := false
	telegramVideoOversized := false
	telegramVideoOnlyDASH := false
	telegramNoAudioExpected := false
	telegramVideoAudioMuxUnavailable := false
	if selected == 0 && media.IsVideo() && trackTelegramVideo {
		telegramOriginalBytes, telegramOriginalSizeKnown = inlineVideoContentLength(media.URL)
		telegramVideoOversized = telegramOriginalSizeKnown && MaxInlineVideoBytes > 0 && telegramOriginalBytes > MaxInlineVideoBytes
		telegramVideoOnlyDASH = scraper.IsLikelyVideoOnlyDASHURL(media.URL)
		if telegramVideoOversized || telegramVideoOnlyDASH {
			sources, compactErr := scraper.ResolveCompactPreviewAV(postID, MaxInlineVideoBytes)
			if compactErr == nil && statelessMediaURLPlayable(sources.Video.URL, now) {
				// Keep the original presentation dimensions while the compact endpoint
				// remuxes Instagram's separate DASH video+audio tracks without re-encoding.
				compactTelegramVideo = true
				if telegramVideoOnlyDASH {
					w.Header().Set("X-InstaFix-Preview-Video", "compact-av-audio")
				} else {
					w.Header().Set("X-InstaFix-Preview-Video", "compact-av")
				}
			} else if telegramVideoOnlyDASH && errors.Is(compactErr, scraper.ErrNoAudioExpected) {
				// A genuinely silent Reel is valid. In that case the video-only DASH
				// representation is the complete media, so keep cheap direct delivery.
				telegramNoAudioExpected = true
				w.Header().Set("X-InstaFix-Preview-Video", "direct-no-audio-expected")
			} else if telegramVideoOnlyDASH {
				// Do not knowingly advertise a video-only DASH track when Instagram says
				// the Reel has audio. Falling back to the cover is less misleading than
				// a silent animation and the decision is recorded below for diagnostics.
				telegramVideoAudioMuxUnavailable = true
				w.Header().Set("X-InstaFix-Preview-Video", "image-fallback-audio-unavailable")
			}
		}
	}

	if media.IsImage() {
		if mediaNum == 0 && len(item.Medias) > 1 {
			viewsData.ImageURL = publicBaseURL + "/grid/" + postID
			viewsData.ImageURLs = statelessCarouselImageURLs(item.Medias, 3)
			if isDirect {
				redirectStatelessNoStore(w, r, media.URL)
				return
			}
		} else {
			if !statelessMediaURLPlayable(media.URL, time.Now()) {
				setStatelessFallbackCacheHeaders(w)
				if trackTelegramVideo {
					observability.Default.RecordTelegramVideoDecision(postID, "blocked", "expired_media_url", 0)
				}
				renderFallbackEmbed(w, r, viewsData, postID, errors.New("Instagram image CDN URL is expired or not directly playable"))
				return
			}
			viewsData.ImageURL = media.URL
			viewsData.ImageWidth = media.Width
			viewsData.ImageHeight = media.Height
			if isDirect {
				redirectStatelessNoStore(w, r, media.URL)
				return
			}
		}
	} else {
		if !statelessMediaURLPlayable(media.URL, time.Now()) {
			setStatelessFallbackCacheHeaders(w)
			if trackTelegramVideo {
				observability.Default.RecordTelegramVideoDecision(postID, "blocked", "expired_media_url", telegramOriginalBytes)
			}
			mediaErr := errors.New("Instagram video CDN URL is expired or not directly playable")
			logEmbedDecision(r, postID, preferVideo, mediaNum, item, media, viewsData, mediaErr, isTelegramBot(userAgent), false, "stateless_fallback")
			renderFallbackEmbed(w, r, viewsData, postID, mediaErr)
			return
		}
		viewsData.Width, viewsData.Height = videoDisplaySize(media.Width, media.Height)
		if viewsData.Width <= 0 {
			viewsData.Width = 400
		}
		if viewsData.Height <= 0 {
			viewsData.Height = 400
		}
		// Keep preview media on a same-origin offload URL. Normal stateless offload
		// resolves and redirects to Instagram CDN. Give direct media URLs a fresh
		// per-render token so Telegram does not reuse a stale cached decision for
		// the bare /offload/{post}/1 URL. Compact media stays stable for edge cache.
		mediaRoute := publicBaseURL + "/offload/" + url.PathEscape(postID) + "/" + strconv.Itoa(selected+1)
		mediaVersion := strconv.FormatInt(time.Now().Unix(), 10)
		if forceOriginalVideo && isTelegramBot(userAgent) && r.URL.Query().Get("tgdiag") == "cfproxy" {
			viewsData.VideoURL = mediaRoute + "?tgmedia=cfproxy"
			w.Header().Set("X-InstaFix-Telegram-Diagnostic", "cfproxy")
		} else if forceOriginalVideo && isTelegramBot(userAgent) && r.URL.Query().Get("tgdiag") == "stream200" {
			// Diagnostic only: force the already-existing Telegram minimal stream.
			// The .mp4 path makes Offload fetch the original Instagram MP4 itself,
			// strip Range/upstream transport headers, and return a same-origin 200
			// video/mp4 response. This isolates Telegram's ability to follow/fetch
			// the Instagram CDN redirect from all HTML/metadata variables.
			viewsData.VideoURL = mediaRoute + ".mp4"
			w.Header().Set("X-InstaFix-Telegram-Diagnostic", "stream200")
		} else if forceOriginalVideo && isTelegramBot(userAgent) && r.URL.Query().Get("tgdiag") == "ogmedia" {
			// Diagnostic only: keep Instagram7 HTML/thumbnail semantics unchanged,
			// but make Telegram resolve the original media through OGInstagram's
			// public direct host. That host internally rewrites to OG's offload
			// route and returns a 302 to the same Instagram CDN class of URL.
			// No OG media bytes or server-side OG request traverse our VPS.
			viewsData.VideoURL = "https://d.oginstagram.com/reel/" + url.PathEscape(postID) + "/"
			w.Header().Set("X-InstaFix-Telegram-Diagnostic", "ogmedia")
		} else if compactTelegramVideo {
			viewsData.VideoURL = mediaRoute + "?compact=av4"
		} else if trackTelegramVideo && telegramVideoAudioMuxUnavailable {
			viewsData.VideoURL = ""
		} else {
			viewsData.VideoURL = mediaRoute + "?v=" + mediaVersion
		}
		if statelessMediaURLPlayable(media.ThumbnailURL, time.Now()) {
			viewsData.ImageURL = mediaRoute + "?thumbnail=1&v=" + mediaVersion
			viewsData.ImageWidth = media.Width
			viewsData.ImageHeight = media.Height
			viewsData.ImageAlt = strings.ReplaceAll(strings.TrimSpace(item.Caption), "\n", " ")
		}
		if isDirect {
			redirectStatelessNoStore(w, r, media.URL)
			return
		}
	}

	if trackTelegramVideo {
		switch {
		case media.IsVideo() && compactTelegramVideo && telegramVideoOnlyDASH:
			observability.Default.RecordTelegramVideoDecision(postID, "compact", "video_only_dash_source", telegramOriginalBytes)
		case media.IsVideo() && compactTelegramVideo:
			observability.Default.RecordTelegramVideoDecision(postID, "compact", "oversized_over_20_mib", telegramOriginalBytes)
		case media.IsVideo() && telegramNoAudioExpected:
			observability.Default.RecordTelegramVideoDecision(postID, "direct", "no_audio_expected", telegramOriginalBytes)
		case media.IsVideo() && telegramVideoAudioMuxUnavailable:
			observability.Default.RecordTelegramVideoDecision(postID, "expected_image", "video_only_dash_compact_unavailable", telegramOriginalBytes)
		case media.IsVideo() && telegramVideoOversized:
			observability.Default.RecordTelegramVideoDecision(postID, "expected_image", "oversized_compact_unavailable", telegramOriginalBytes)
		case media.IsVideo() && telegramOriginalSizeKnown:
			observability.Default.RecordTelegramVideoDecision(postID, "direct", "within_20_mib", telegramOriginalBytes)
		case media.IsVideo():
			observability.Default.RecordTelegramVideoDecision(postID, "direct", "size_unknown", 0)
		case media.IsImage():
			observability.Default.RecordTelegramVideoDecision(postID, "expected_image", "reel_resolved_as_image", 0)
		default:
			observability.Default.RecordTelegramVideoDecision(postID, "blocked", "no_renderable_media", 0)
		}
	}

	setStatelessSuccessCacheHeaders(w, item)
	logEmbedDecision(r, postID, preferVideo, mediaNum, item, media, viewsData, nil, isTelegramBot(userAgent), telegramVideoOversized, "stateless_render")
	if forceOriginalVideo && isTelegramBot(userAgent) && r.URL.Query().Get("tgdiag") == "oghtml" {
		w.Header().Set("X-InstaFix-Telegram-Diagnostic", "oghtml")
		views.EmbedOGDiagnostic(viewsData, w)
		return
	}
	views.Embed(viewsData, w)
}

func tryStatelessTelegramClientMediaFallback(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, preferVideo bool, viewsData *model.ViewsData, item *scraper.InstaData, scrapeErr error) bool {
	if !preferVideo || (item != nil && len(item.Medias) > 0 && item.HasVideo()) {
		return false
	}
	if scrapeErr == nil {
		scrapeErr = scraper.ErrNotFound
	}
	return TryOGInstagramClientMediaFallbackForKnownVideo(w, r, postID, mediaNum, viewsData, item, scrapeErr)
}

func redirectStatelessNoStore(w http.ResponseWriter, r *http.Request, target string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("CDN-Cache-Control", "no-store")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

func statelessCarouselImageURLs(medias []scraper.Media, limit int) []string {
	if limit <= 0 {
		return nil
	}
	now := time.Now()
	urls := make([]string, 0, min(limit, len(medias)))
	for _, media := range medias {
		if len(urls) >= limit {
			break
		}
		raw := media.URL
		if media.IsVideo() {
			raw = media.ThumbnailURL
		}
		if statelessMediaURLPlayable(raw, now) {
			urls = append(urls, raw)
		}
	}
	return urls
}

func isInstagramCDNURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "cdninstagram.com" || strings.HasSuffix(host, ".cdninstagram.com") ||
		host == "fbcdn.net" || strings.HasSuffix(host, ".fbcdn.net")
}

func setStatelessSuccessCacheHeaders(w http.ResponseWriter, item *scraper.InstaData) {
	ttl := statelessEdgeTTL(item, time.Now())
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	if ttl <= 0 {
		w.Header().Set("CDN-Cache-Control", "no-store")
		w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
		return
	}
	value := "public, max-age=" + strconv.FormatInt(int64(ttl/time.Second), 10)
	w.Header().Set("CDN-Cache-Control", value)
	w.Header().Set("Cloudflare-CDN-Cache-Control", value)
	w.Header().Set("X-Instagram7-Edge-TTL", strconv.FormatInt(int64(ttl/time.Second), 10))
}

func setStatelessFallbackCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	w.Header().Set("CDN-Cache-Control", "public, max-age=30")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "public, max-age=30")
}

func statelessEdgeTTL(item *scraper.InstaData, now time.Time) time.Duration {
	ttl := statelessNormalEdgeTTL
	if ttl <= 0 {
		return 0
	}
	expiry, ok := earliestStatelessSignedExpiry(item)
	if !ok {
		return ttl
	}
	remaining := expiry.Sub(now) - statelessSignedURLMargin
	if remaining <= 0 {
		return 0
	}
	if remaining < ttl {
		return remaining.Truncate(time.Second)
	}
	return ttl
}

func earliestStatelessSignedExpiry(item *scraper.InstaData) (time.Time, bool) {
	if item == nil {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, media := range item.Medias {
		for _, raw := range []string{media.URL, media.ThumbnailURL} {
			u, err := url.Parse(raw)
			if err != nil {
				continue
			}
			rawExpiry := strings.TrimSpace(u.Query().Get("oe"))
			if rawExpiry == "" {
				continue
			}
			seconds, err := strconv.ParseInt(rawExpiry, 16, 64)
			if err != nil || seconds <= 0 {
				continue
			}
			expiry := time.Unix(seconds, 0)
			if earliest.IsZero() || expiry.Before(earliest) {
				earliest = expiry
			}
		}
	}
	return earliest, !earliest.IsZero()
}
