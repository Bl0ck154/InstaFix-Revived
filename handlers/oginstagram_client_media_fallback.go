package handlers

import (
	scraper "instafix/handlers/scraper"
	"instafix/observability"
	"instafix/views"
	"instafix/views/model"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	ogInstagramClientMediaFallbackModeEnv  = "OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE"
	ogInstagramClientMediaFallbackTelegram = "telegram"
	ogInstagramClientMediaDelivery         = "og-client-fallback"
	telegramMinimalStreamDelivery          = "telegram-edge-stream-v1"
	telegramPreviewProbeQuery              = "ig7probe"
	telegramCDNRedirectProbe               = "cdn-redirect-v1"
	telegramCDNRedirectProbeDelivery       = "telegram-cdn-redirect-probe-v1"
	telegramOGDirectProbe                  = "og-direct-v1"
	telegramAuthDimensionsProbe            = "auth-dimensions-v1"
	ogInstagramClientMediaUpstreamEnv      = "OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM"
	ogInstagramClientMediaForcePostsEnv    = "OGINSTAGRAM_CLIENT_MEDIA_FORCE_POSTS"
	defaultOGInstagramClientMediaUpstream  = "https://d.oginstagram.com"
)

func telegramPreviewProbeVideoURL(r *http.Request, publicBaseURL, postID string, mediaNum int, localVideoAvailable bool) (string, bool) {
	if !isTelegramBot(r.UserAgent()) {
		return "", false
	}

	switch r.URL.Query().Get(telegramPreviewProbeQuery) {
	case telegramCDNRedirectProbe:
		if !localVideoAvailable {
			return "", false
		}
		return publicBaseURL + "/offload/" + url.PathEscape(postID) + "/" + strconv.Itoa(max(1, mediaNum)) + ".mp4?delivery=" + telegramCDNRedirectProbeDelivery, true
	case telegramOGDirectProbe:
		return ogInstagramClientMediaURL(postID, mediaNum)
	default:
		return "", false
	}
}

// TryOGInstagramClientMediaFallback keeps the visible preview metadata on
// Instagram7 while advertising OGInstagram's client-side media endpoint
// directly. Telegram does not reliably follow a cross-domain redirect from an
// og:video URL, so the media URL itself must point at the upstream. The
// production server never fetches the upstream.
func TryOGInstagramClientMediaFallback(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, viewsData *model.ViewsData, item *scraper.InstaData, scrapeErr error) bool {
	return tryOGInstagramClientMediaFallback(w, r, postID, mediaNum, viewsData, item, scrapeErr, false)
}

// Permits /p/ links only after the caller has independently classified
// the publication as a video. Unclassified photo posts remain unchanged.
func TryOGInstagramClientMediaFallbackForKnownVideo(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, viewsData *model.ViewsData, item *scraper.InstaData, scrapeErr error) bool {
	return tryOGInstagramClientMediaFallback(w, r, postID, mediaNum, viewsData, item, scrapeErr, true)
}

func tryOGInstagramClientMediaFallback(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, viewsData *model.ViewsData, item *scraper.InstaData, scrapeErr error, allowPostAlias bool) bool {
	allowedPath := isVideoPublicationPath(r.URL.Path) || (allowPostAlias && strings.Contains(r.URL.Path, "/p/"))
	if strings.ToLower(strings.TrimSpace(os.Getenv(ogInstagramClientMediaFallbackModeEnv))) != ogInstagramClientMediaFallbackTelegram ||
		!isTelegramBot(r.UserAgent()) || !allowedPath {
		return false
	}

	mediaNum = max(1, mediaNum)
	publicBaseURL := requestPublicBaseURL(r)
	mediaPath := publicBaseURL + "/offload/" + url.PathEscape(postID) + "/" + strconv.Itoa(mediaNum)
	videoURL := ""
	localVideoAvailable := false
	forceClientMedia := forceTelegramClientMediaFallback(postID)
	if item != nil && len(item.Medias) >= mediaNum {
		media := item.Medias[mediaNum-1]
		if !forceClientMedia && media.IsVideo() && media.URL != "" && telegramLocalVideoPresentationUsable(media) {
			localVideoAvailable = true
			// Keep locally resolved oversized videos same-origin. This delivery
			// mode mirrors OGInstagram's successful Cloudflare media response:
			// a minimal 200 video/mp4 stream rather than a cross-domain redirect.
			videoURL = mediaPath + ".mp4?delivery=" + telegramMinimalStreamDelivery
		}
	}
	if probeURL, ok := telegramPreviewProbeVideoURL(r, publicBaseURL, postID, mediaNum, localVideoAvailable); ok {
		videoURL = probeURL
		w.Header().Set("X-Instagram7-Preview-Probe", r.URL.Query().Get(telegramPreviewProbeQuery))
	}
	if videoURL == "" {
		var ok bool
		videoURL, ok = ogInstagramClientMediaURL(postID, mediaNum)
		if !ok {
			slog.Warn("client-media fallback skipped: invalid upstream", "postID", postID)
			return false
		}
	}
	viewsData.Card = "summary_large_image"
	viewsData.OGType = "article"
	viewsData.Site = "InstaFix Revived"
	viewsData.NoRedirect = true
	viewsData.VideoURL = videoURL
	// Reset fields which Embed initializes before scraping. A fallback must not
	// accidentally inherit the generic "Instagram preview" title or stale
	// geometry from an earlier partial rendering attempt.
	viewsData.Title = ""
	viewsData.Creator = ""
	viewsData.ArticleAuthor = ""
	viewsData.Description = ""
	viewsData.Width = 720
	viewsData.Height = 1280
	viewsData.ImageURL = publicBaseURL + "/fallback/" + url.PathEscape(postID) + ".png?kind=reel"
	viewsData.ImageWidth = 720
	viewsData.ImageHeight = 1280
	viewsData.FaviconURL = publicBaseURL + "/favicon.svg"
	viewsData.AppleIconURL = publicBaseURL + "/favicon.svg"

	hasOriginalIdentity := false
	if item != nil {
		if item.Username != "" {
			hasOriginalIdentity = true
			viewsData.Title = "@" + item.Username
			viewsData.Creator = "@" + item.Username
			viewsData.ArticleAuthor = "https://www.instagram.com/" + item.Username + "/"
		}
		// Caption/stats and media geometry can still be valid when Instagram's
		// partial public response omitted the username. Preserve each field
		// independently instead of discarding the entire partial result.
		if description := embedDescription(item); description != "" {
			viewsData.Description = description
			if viewsData.Title == "" {
				viewsData.Title = previewTitleFromDescription(description)
			}
		}
		if len(item.Medias) >= mediaNum {
			media := item.Medias[mediaNum-1]
			// Public fallbacks often expose the real Reel cover as a GraphImage
			// even when the video URL, author, and caption are unavailable. It is
			// still a better Telegram poster than our generated placeholder. Keep
			// it behind the same-origin offload route so it can be refreshed.
			if media.ThumbnailURL != "" || media.IsImage() && media.URL != "" {
				viewsData.ImageURL = mediaPath + "?thumbnail=1"
			}
			if width, height := videoDisplaySize(media.Width, media.Height); width > 0 && height > 0 {
				viewsData.Width, viewsData.Height = width, height
				viewsData.ImageWidth, viewsData.ImageHeight = width, height
			} else if viewsData.ImageURL == mediaPath+"?thumbnail=1" {
				// Let Telegram inspect the real cover instead of lying that its
				// dimensions match the generated 9:16 placeholder.
				viewsData.ImageWidth, viewsData.ImageHeight = 0, 0
			}
		}
	}

	w.Header().Set("X-Instagram7-Preview-Source", "client-media-fallback")
	observability.Default.RecordOGClientRedirect(r, postID, "telegram_media_fallback")
	previewOutcome := "fallback"
	previewReason := "telegram_media_fallback_missing_metadata"
	if hasOriginalIdentity {
		previewOutcome = "full"
		previewReason = "telegram_media_fallback"
	}
	observability.Default.RecordPreviewWithReason(r, postID, previewOutcome, "video", previewReason)
	slog.Info("serving Telegram client-media fallback",
		"postID", postID,
		"media_index", mediaNum,
		"scrape_err", scrapeErr,
	)
	views.Embed(viewsData, w)
	return true
}

// previewTitleFromDescription uses real caption text instead of inventing a
// generic author or Reel title when the public source is missing identity.
func previewTitleFromDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	if i := strings.IndexByte(description, '\n'); i >= 0 {
		description = strings.TrimSpace(description[:i])
	}
	const maxRunes = 96
	r := []rune(description)
	if len(r) > maxRunes {
		description = strings.TrimSpace(string(r[:maxRunes-1])) + "…"
	}
	return description
}

func telegramLocalVideoPresentationUsable(media scraper.Media) bool {
	width, height := videoDisplaySize(media.Width, media.Height)
	return media.IsVideo() && media.URL != "" && media.ThumbnailURL != "" && width > 0 && height > 0
}

func forceTelegramClientMediaFallback(postID string) bool {
	for _, candidate := range strings.FieldsFunc(os.Getenv(ogInstagramClientMediaForcePostsEnv), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if candidate == postID {
			return true
		}
	}
	return false
}

// TryOGInstagramClientMediaRedirect is intentionally a redirect only. It must
// never fetch the upstream from the production server.
func TryOGInstagramClientMediaRedirect(w http.ResponseWriter, r *http.Request, postID string, mediaNum int) bool {
	if r.URL.Query().Get("delivery") != ogInstagramClientMediaDelivery ||
		strings.ToLower(strings.TrimSpace(os.Getenv(ogInstagramClientMediaFallbackModeEnv))) != ogInstagramClientMediaFallbackTelegram ||
		!isTelegramBot(r.UserAgent()) {
		return false
	}

	upstreamURL, ok := ogInstagramClientMediaURL(postID, mediaNum)
	if !ok {
		slog.Warn("client-media fallback skipped: invalid upstream", "postID", postID)
		return false
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Instagram7-Video-Delivery", "og-client-redirect")
	http.Redirect(w, r, upstreamURL, http.StatusFound)
	slog.Info("Telegram media request redirected to OGInstagram direct client endpoint", "postID", postID, "media_index", mediaNum)
	return true
}

func ogInstagramClientMediaURL(postID string, mediaNum int) (string, bool) {
	upstream := strings.TrimSpace(os.Getenv(ogInstagramClientMediaUpstreamEnv))
	if upstream == "" {
		upstream = defaultOGInstagramClientMediaUpstream
	}
	base, err := url.Parse(upstream)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return "", false
	}
	// OGInstagram /offload URLs are signed capabilities. The dedicated d.
	// host is the public client-side entrypoint that resolves a post directly;
	// never construct an unsigned /offload URL here.
	base.Path = strings.TrimRight(base.Path, "/") + "/p/" + url.PathEscape(postID) + "/"
	base.RawPath = ""
	query := url.Values{}
	if mediaNum > 1 {
		query.Set("img_index", strconv.Itoa(mediaNum))
	}
	base.RawQuery = query.Encode()
	base.Fragment = ""
	return base.String(), true
}
