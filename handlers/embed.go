package handlers

import (
	"errors"
	scraper "instafix/handlers/scraper"
	"instafix/observability"
	"instafix/utils"
	"instafix/views"
	"instafix/views/model"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var MaxInlineVideoBytes int64
var previewProbeAuthRefresh = scraper.FetchDataFromAuthHelperUncached

func mediaidToCode(mediaID int) string {
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var shortCode string

	for mediaID > 0 {
		remainder := mediaID % 64
		mediaID /= 64
		shortCode = string(alphabet[remainder]) + shortCode
	}

	return shortCode
}

func getSharePostID(postID string) (string, error) {
	req, err := http.NewRequest("HEAD", "https://www.instagram.com/share/reel/"+postID+"/", nil)
	if err != nil {
		return postID, err
	}
	resp, err := scraper.DoPublicInstagramRequestNoRedirect(req, "share reel redirect")
	if err != nil {
		return postID, err
	}
	defer resp.Body.Close()
	redirURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return postID, err
	}
	postID = path.Base(redirURL.Path)
	if postID == "login" {
		return postID, errors.New("not logged in")
	}
	return postID, nil
}

func Embed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	viewsData := &model.ViewsData{}
	userAgent := r.Header.Get("User-Agent")
	isTelegram := isTelegramBot(userAgent)
	allowAuth := isPreviewMediaBot(userAgent)

	var err error
	postID := chi.URLParam(r, "postID")
	mediaNumParams := chi.URLParam(r, "mediaNum")
	urlQuery := r.URL.Query()
	if urlQuery == nil {
		return
	}
	if mediaNumParams == "" {
		imgIndex := urlQuery.Get("img_index")
		if imgIndex != "" {
			mediaNumParams = imgIndex
		} else {
			mediaNumParams = "0"
		}
	}
	mediaNum, err := strconv.Atoi(mediaNumParams)
	if err != nil {
		viewsData.Description = "Invalid img_index parameter"
		observability.Default.RecordPreviewWithReason(r, postID, "failed", "", "invalid_media_index")
		views.Embed(viewsData, w)
		return
	}

	isDirect, _ := strconv.ParseBool(urlQuery.Get("direct"))
	isGallery, _ := strconv.ParseBool(urlQuery.Get("gallery"))

	// Get direct/gallery from header too, nginx query params is pain in the ass
	embedType := r.Header.Get("X-Embed-Type")
	if embedType == "direct" {
		isDirect = true
	} else if embedType == "gallery" {
		isGallery = true
	}

	// Stories use mediaID (int) instead of postID
	if strings.Contains(r.URL.Path, "/stories/") {
		mediaID, err := strconv.Atoi(postID)
		if err != nil {
			viewsData.Description = "Invalid postID"
			observability.Default.RecordPreviewWithReason(r, postID, "failed", "", "invalid_story_post_id")
			views.Embed(viewsData, w)
			return
		}
		postID = mediaidToCode(mediaID)
	} else if strings.Contains(r.URL.Path, "/share/") {
		postID, err = getSharePostID(postID)
		if err != nil && len(scraper.RemoteScraperAddr) == 0 {
			slog.Error("Failed to get new postID from share URL", "postID", postID, "err", err)
			viewsData.Description = "Failed to get new postID from share URL"
			observability.Default.RecordPreviewWithReason(r, postID, "failed", "", "share_resolution_failed")
			views.Embed(viewsData, w)
			return
		}
	}

	// If User-Agent is not bot, redirect to Instagram
	viewsData.Title = "Instagram preview"
	viewsData.URL = instagramOriginURL(r.URL.Path, postID)
	viewsData.CanonicalURL = viewsData.URL
	viewsData.Site = "InstaFix Revived"
	if !utils.IsBot(userAgent) {
		http.Redirect(w, r, viewsData.URL, http.StatusFound)
		return
	}
	publicBaseURL := requestPublicBaseURL(r)
	viewsData.FaviconURL = publicBaseURL + "/favicon.svg"
	viewsData.AppleIconURL = publicBaseURL + "/favicon.svg"
	w.Header().Set("Cache-Control", "public, max-age=0")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "public, max-age=300, stale-while-revalidate=86400, stale-if-error=604800")
	// Keep the production OGInstagram switches ahead of local scraping. The
	// client redirect and server-side proxy are separate mechanisms: the first
	// is an emergency cross-domain handoff, while the second can fall back to
	// the local renderer when the upstream is unavailable.
	if !isDirect && !isGallery && TryOGInstagramClientRedirect(w, r, postID, nil) {
		return
	}
	if !isDirect && !isGallery && TryOGInstagramEmbedProxy(w, r, postID) {
		return
	}

	preferVideo := strings.Contains(r.URL.Path, "/reel/") || strings.Contains(r.URL.Path, "/reels/") || strings.Contains(r.URL.Path, "/tv/")
	var item *scraper.InstaData
	if preferVideo {
		if allowAuth {
			item, err = scraper.GetDataPreferVideo(postID)
		} else {
			item, err = scraper.GetDataPreferVideoQuiet(postID)
		}
	} else {
		item, err = scraper.GetDataQuiet(postID)
		if err != nil || item == nil || len(item.Medias) == 0 || item.Username == "" {
			if videoItem, videoErr := scraper.GetDataPreferVideoQuiet(postID); videoErr == nil && videoItem != nil && videoItem.HasVideo() {
				item = videoItem
				err = nil
				preferVideo = true
			}
		}
	}
	needsAuthFallback := err != nil || item == nil || len(item.Medias) == 0 || len(item.Username) == 0 ||
		preferVideo && item != nil && !item.HasVideo()
	var authFallbackErr error
	if needsAuthFallback {
		if allowAuth {
			if authItem, authErr := scraper.GetDataEmbedAuthFallback(postID); authErr == nil && authItem != nil && len(authItem.Medias) > 0 {
				if shouldUseAuthFallbackItem(item, authItem, preferVideo) {
					item = authItem
				}
				err = nil
			} else if authErr != nil {
				authFallbackErr = authErr
				slog.Info("Embed auth fallback unavailable", "postID", postID, "err", authErr)
			}
		} else if scraper.AuthHelperURL != "" {
			observability.Default.RecordAuthHelperSkipped(postID, "untrusted_client")
		}
	}
	proxyAttempted := false
	if !isDirect && !isGallery && preferVideo && (item == nil || len(item.Medias) == 0 || !item.HasVideo()) {
		fallbackErr := err
		if fallbackErr == nil {
			fallbackErr = authFallbackErr
		}
		if fallbackErr == nil {
			fallbackErr = scraper.ErrNotFound
		}
		if TryOGInstagramClientRedirect(w, r, postID, fallbackErr) {
			return
		}
		if TryOGInstagramClientMediaFallback(w, r, postID, mediaNum, viewsData, item, fallbackErr) {
			return
		}
		proxyAttempted = true
		if TryOGInstagramEmbedProxy(w, r, postID, trustedFallbackPreviewText(item)) {
			return
		}
	}
	if err != nil || item == nil || len(item.Medias) == 0 {
		if err == nil {
			err = authFallbackErr
			if err == nil {
				err = scraper.ErrNotFound
			}
		}
		if !isDirect && !isGallery && TryOGInstagramClientRedirect(w, r, postID, err) {
			return
		}
		if !isDirect && !isGallery && !proxyAttempted && TryOGInstagramEmbedProxy(w, r, postID, trustedFallbackPreviewText(item)) {
			return
		}
		logEmbedDecision(r, postID, preferVideo, mediaNum, item, scraper.Media{}, viewsData, err, isTelegram, false, "fallback")
		renderFallbackEmbed(w, r, viewsData, postID, err)
		return
	}

	if mediaNum > len(item.Medias) {
		viewsData.Description = "Media number out of range"
		observability.Default.RecordPreviewWithReason(r, postID, "failed", "", "media_index_out_of_range")
		views.Embed(viewsData, w)
		return
	} else if len(item.Username) == 0 {
		if !isDirect && !isGallery && TryOGInstagramClientRedirect(w, r, postID, scraper.ErrNotFound) {
			return
		}
		if !isDirect && !isGallery && preferVideo && TryOGInstagramClientMediaFallback(w, r, postID, mediaNum, viewsData, item, scraper.ErrNotFound) {
			return
		}
		if !isDirect && !isGallery && TryOGInstagramEmbedProxy(w, r, postID, trustedFallbackPreviewText(item)) {
			return
		}
		logEmbedDecision(r, postID, preferVideo, mediaNum, item, scraper.Media{}, viewsData, scraper.ErrNotFound, isTelegram, false, "fallback")
		renderFallbackEmbed(w, r, viewsData, postID, scraper.ErrNotFound)
		return
	}

	var sb strings.Builder
	sb.Grow(32) // 32 bytes should be enough for most cases
	viewsData.Title = "@" + item.Username
	viewsData.Creator = "@" + item.Username
	viewsData.ArticleAuthor = "https://www.instagram.com/" + item.Username + "/"
	// Gallery do not have any caption
	if !isGallery {
		viewsData.Description = embedDescription(item)
	}

	media := item.Medias[max(1, mediaNum)-1]
	videoMetadata := telegramAuthDimensionsProbeMedia(w, r, postID, mediaNum, media)
	isImage := media.IsImage()
	videoOversized := false
	switch {
	case mediaNum == 0 && isImage && len(item.Medias) > 1:
		viewsData.Card = "summary_large_image"
		viewsData.OGType = "article"
		sb.WriteString(publicBaseURL + "/grid/")
		sb.WriteString(postID)
		viewsData.ImageURL = sb.String()
		viewsData.ImageURLs = carouselImageURLs(publicBaseURL, postID, item.Medias, 3)
		if isDirect {
			sb.Reset()
			sb.WriteString(publicBaseURL + "/offload/")
			sb.WriteString(postID)
			sb.WriteString("/1")
		}
	case isImage:
		viewsData.Card = "summary_large_image"
		viewsData.OGType = "article"
		sb.WriteString(publicBaseURL + "/offload/")
		sb.WriteString(postID)
		sb.WriteString("/")
		sb.WriteString(strconv.Itoa(max(1, mediaNum)))
		viewsData.ImageURL = sb.String()
	default:
		if isTelegram && (forceTelegramClientMediaFallback(postID) || !telegramLocalVideoPresentationUsable(media)) {
			if TryOGInstagramClientMediaFallback(w, r, postID, mediaNum, viewsData, item, errors.New("Telegram local video metadata is incomplete")) {
				return
			}
		}
		if isTelegram && isInlineVideoOversized(media.URL) {
			videoOversized = true
			if TryOGInstagramClientMediaFallback(w, r, postID, mediaNum, viewsData, item, errors.New("Telegram inline video exceeds configured byte limit")) {
				return
			}
		}
		videoRoute := previewVideoRoute(publicBaseURL, postID, max(1, mediaNum), r.UserAgent())
		if probeURL, ok := telegramPreviewProbeVideoURL(r, publicBaseURL, postID, mediaNum, true); ok {
			videoRoute = probeURL
			w.Header().Set("X-Instagram7-Preview-Probe", r.URL.Query().Get(telegramPreviewProbeQuery))
		}
		viewsData.Card = "summary_large_image"
		viewsData.OGType = "article"
		viewsData.Width, viewsData.Height = videoDisplaySize(videoMetadata.Width, videoMetadata.Height)
		sb.WriteString(videoRoute)
		if viewsData.Width <= 0 {
			viewsData.Width = 400
		}
		if viewsData.Height <= 0 {
			viewsData.Height = 400
		}
		if media.ThumbnailURL != "" {
			viewsData.ImageURL = publicBaseURL + "/offload/" + postID + "/" + strconv.Itoa(max(1, mediaNum)) + "?thumbnail=1"
			viewsData.ImageWidth = media.Width
			viewsData.ImageHeight = media.Height
			viewsData.ImageAlt = strings.ReplaceAll(strings.TrimSpace(item.Caption), "\n", " ")
		}
		viewsData.VideoURL = videoRoute

	}
	viewsData.NoRedirect = true
	if isDirect {
		logEmbedDecision(r, postID, preferVideo, mediaNum, item, media, viewsData, nil, isTelegram, videoOversized, "direct_redirect")
		http.Redirect(w, r, sb.String(), http.StatusFound)
		return
	}

	logEmbedDecision(r, postID, preferVideo, mediaNum, item, media, viewsData, nil, isTelegram, videoOversized, "render")
	views.Embed(viewsData, w)
}

func telegramAuthDimensionsProbeMedia(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, fallback scraper.Media) scraper.Media {
	if !isTelegramBot(r.UserAgent()) || r.URL.Query().Get(telegramPreviewProbeQuery) != telegramAuthDimensionsProbe || !fallback.IsVideo() {
		return fallback
	}

	item, err := previewProbeAuthRefresh(postID)
	mediaNum = max(1, mediaNum)
	if err != nil || item == nil || len(item.Medias) < mediaNum || !item.Medias[mediaNum-1].IsVideo() {
		slog.Warn("authenticated dimensions probe unavailable", "postID", postID, "err", err)
		return fallback
	}

	authMedia := item.Medias[mediaNum-1]
	if authMedia.Width <= 0 || authMedia.Height <= 0 {
		return fallback
	}
	w.Header().Set("X-Instagram7-Preview-Probe", telegramAuthDimensionsProbe)
	return authMedia
}

func trustedFallbackPreviewText(item *scraper.InstaData) fallbackPreviewText {
	if item == nil {
		return fallbackPreviewText{}
	}
	return fallbackPreviewText{
		title:       item.Username,
		description: item.Caption,
	}
}

func previewVideoRoute(publicBaseURL, postID string, mediaNum int, userAgent string) string {
	route := publicBaseURL + "/offload/" + postID + "/" + strconv.Itoa(mediaNum) + ".mp4"
	if shouldRedirectPreviewVideo(userAgent) {
		route += "?delivery=cloudflare-cdn-v10"
	}
	return route
}

func shouldUseAuthFallbackItem(current, auth *scraper.InstaData, preferVideo bool) bool {
	if auth == nil || len(auth.Medias) == 0 {
		return false
	}
	if current == nil || len(current.Medias) == 0 || current.Username == "" {
		return true
	}
	if preferVideo {
		return auth.HasVideo()
	}
	return true
}

func embedDescription(item *scraper.InstaData) string {
	if item == nil {
		return ""
	}
	stats := mediaStatsLine(item)
	caption := strings.TrimSpace(item.Caption)
	description := caption
	if stats != "" && caption != "" {
		description = stats + "\n\n" + caption
	} else if stats != "" {
		description = stats
	}
	if len(description) > 255 {
		description = utils.Substr(description, 0, 250) + "..."
	}
	return description
}

func mediaStatsLine(item *scraper.InstaData) string {
	if item == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if item.HasViewCount {
		parts = append(parts, "▶️ "+formatStatCount(item.ViewCount))
	}
	if item.HasLikeCount {
		parts = append(parts, "❤️ "+formatStatCount(item.LikeCount))
	}
	if item.HasCommentCount {
		parts = append(parts, "💬 "+formatStatCount(item.CommentCount))
	}
	return strings.Join(parts, "  ")
}

func formatStatCount(value int64) string {
	if value < 0 {
		value = 0
	}
	raw := strconv.FormatInt(value, 10)
	if len(raw) <= 3 {
		return raw
	}
	var formatted strings.Builder
	prefix := len(raw) % 3
	if prefix > 0 {
		formatted.WriteString(raw[:prefix])
	}
	for i := prefix; i < len(raw); i += 3 {
		if formatted.Len() > 0 {
			formatted.WriteByte(',')
		}
		formatted.WriteString(raw[i : i+3])
	}
	return formatted.String()
}

func isTelegramBot(userAgent string) bool {
	return strings.Contains(strings.ToLower(userAgent), "telegrambot")
}

func logEmbedDecision(r *http.Request, postID string, preferVideo bool, mediaNum int, item *scraper.InstaData, media scraper.Media, viewsData *model.ViewsData, err error, isTelegram bool, videoOversized bool, outcome string) {
	previewOutcome := "failed"
	mediaKind := ""
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	switch {
	case outcome == "fallback":
		previewOutcome = "fallback"
		mediaKind = "generic"
	case err != nil:
		previewOutcome = "failed"
	case viewsData.VideoURL != "":
		previewOutcome = "full"
		mediaKind = "video"
	case viewsData.ImageURL != "":
		previewOutcome = "full"
		mediaKind = "image"
	}
	observability.Default.RecordPreviewWithReason(r, postID, previewOutcome, mediaKind, errText)
	if !isTelegram {
		return
	}
	mediaCount := 0
	hasVideo := false
	username := false
	if item != nil {
		mediaCount = len(item.Medias)
		hasVideo = item.HasVideo()
		username = item.Username != ""
	}
	slog.Info("embed preview decision",
		"postID", postID,
		"path", r.URL.Path,
		"outcome", outcome,
		"prefer_video", preferVideo,
		"media_index", max(1, mediaNum),
		"media_count", mediaCount,
		"username", username,
		"item_has_video", hasVideo,
		"selected_type", media.TypeName,
		"selected_is_video", media.IsVideo(),
		"selected_url_kind", previewURLKind(media.URL),
		"selected_thumb_kind", previewURLKind(media.ThumbnailURL),
		"video_oversized", videoOversized,
		"og_video", viewsData.VideoURL != "",
		"og_video_kind", previewURLKind(viewsData.VideoURL),
		"og_image", viewsData.ImageURL != "",
		"og_image_kind", previewURLKind(viewsData.ImageURL),
		"no_redirect", viewsData.NoRedirect,
		"err", errText,
	)
}

func previewURLKind(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "none"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	host := strings.ToLower(u.Host)
	switch {
	case strings.Contains(host, "cdninstagram.com"), strings.Contains(host, "fbcdn.net"):
		if strings.HasSuffix(strings.ToLower(u.Path), ".mp4") || strings.Contains(strings.ToLower(u.Path), ".mp4") {
			return "direct_cdn_mp4:" + host
		}
		return "direct_cdn:" + host
	case strings.EqualFold(host, views.PublicHost()) && strings.HasPrefix(u.Path, "/offload/"):
		return "offload"
	default:
		return "other:" + host
	}
}

func instagramOriginURL(requestPath, postID string) string {
	postType := "p"
	switch {
	case strings.Contains(requestPath, "/reel/"), strings.Contains(requestPath, "/reels/"):
		postType = "reel"
	case strings.Contains(requestPath, "/tv/"):
		postType = "tv"
	}
	return "https://www.instagram.com/" + postType + "/" + postID + "/"
}

func videoDisplaySize(width, height int) (int, int) {
	if width <= 0 || height <= 0 {
		return width, height
	}
	multiplier := 1.0
	if width > 1920 || height > 1920 {
		multiplier = 0.5
	}
	if width < 400 && height < 400 {
		multiplier = 2
	}
	return int(float64(width)*multiplier + 0.5), int(float64(height)*multiplier + 0.5)
}

func ConfigureMaxInlineVideoBytes(maxBytes int64) {
	if maxBytes >= 0 {
		MaxInlineVideoBytes = maxBytes
	}
}

func inlineVideoContentLength(videoURL string) (int64, bool) {
	if strings.TrimSpace(videoURL) == "" {
		return 0, false
	}
	req, err := http.NewRequest(http.MethodHead, videoURL, http.NoBody)
	if err != nil {
		return 0, false
	}
	req.Header.Set("User-Agent", "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)")
	client := http.Client{Timeout: 4 * time.Second}
	res, err := client.Do(req)
	if err != nil || res == nil {
		return 0, false
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 || res.ContentLength <= 0 {
		return 0, false
	}
	return res.ContentLength, true
}

func isInlineVideoOversized(videoURL string) bool {
	if MaxInlineVideoBytes <= 0 {
		return false
	}
	contentLength, ok := inlineVideoContentLength(videoURL)
	if !ok || contentLength <= MaxInlineVideoBytes {
		return false
	}
	slog.Info("inline video disabled: oversized", "contentLength", contentLength, "maxBytes", MaxInlineVideoBytes, "host", safeURLHost(videoURL))
	return true
}

func safeURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	return u.Host
}

func carouselImageURLs(publicBaseURL, postID string, medias []scraper.Media, limit int) []string {
	if limit <= 0 {
		return nil
	}
	urls := make([]string, 0, limit)
	for idx, media := range medias {
		if len(urls) >= limit {
			break
		}
		if !media.IsImage() && media.ThumbnailURL == "" {
			continue
		}
		mediaURL := publicBaseURL + "/offload/" + postID + "/" + strconv.Itoa(idx+1)
		if !media.IsImage() {
			mediaURL += "?thumbnail=1"
		}
		urls = append(urls, mediaURL)
	}
	return urls
}

func renderFallbackEmbed(w http.ResponseWriter, r *http.Request, viewsData *model.ViewsData, postID string, scrapeErr error) {
	publicBaseURL := requestPublicBaseURL(r)
	viewsData.FaviconURL = publicBaseURL + "/favicon.svg"
	viewsData.AppleIconURL = publicBaseURL + "/favicon.svg"
	viewsData.Creator = "@instagram"
	viewsData.Card = "summary"
	viewsData.OGType = "article"
	viewsData.ImageURL = ""
	viewsData.ImageURLs = nil
	viewsData.ImageWidth = 0
	viewsData.ImageHeight = 0
	viewsData.ImageAlt = ""
	viewsData.VideoURL = ""
	viewsData.PlayerURL = ""
	viewsData.Title, viewsData.Description = fallbackPreviewCopy(scrapeErr)
	if scrapeErr != nil {
		slog.Info("Serving detailed fallback preview", "postID", postID, "err", scrapeErr, "title", viewsData.Title)
	}
	views.Embed(viewsData, w)
}

func telegramVideoFailureReason(scrapeErr error) string {
	if scrapeErr == nil {
		return "unavailable"
	}
	msg := strings.ToLower(scrapeErr.Error())
	switch {
	case errors.Is(scrapeErr, scraper.ErrRestricted) && (strings.Contains(msg, "min_age_account") || strings.Contains(msg, "under 21") || strings.Contains(msg, "people under")):
		return "age_restricted_21"
	case errors.Is(scrapeErr, scraper.ErrRestricted) && (strings.Contains(msg, "geoblock") || strings.Contains(msg, "region")):
		return "region_restricted"
	case errors.Is(scrapeErr, scraper.ErrRestricted):
		return "restricted"
	case errors.Is(scrapeErr, scraper.ErrNotFound) || strings.Contains(msg, "not found") || strings.Contains(msg, "deleted") || strings.Contains(msg, "private"):
		return "not_found_private_or_deleted"
	case strings.Contains(msg, "expired") || strings.Contains(msg, "not directly playable"):
		return "expired_media_url"
	case strings.Contains(msg, "429") || strings.Contains(msg, "too many") || strings.Contains(msg, "rate limit"):
		return "rate_limited"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") || strings.Contains(msg, "connection refused"):
		return "upstream_timeout"
	case strings.Contains(msg, "http 5"):
		return "upstream_5xx"
	default:
		return "unavailable"
	}
}

func fallbackPreviewCopy(scrapeErr error) (string, string) {
	if scrapeErr == nil {
		return "Instagram preview unavailable", "Instagram did not provide public media for this post. Open it on Instagram to check the original."
	}
	msg := strings.ToLower(scrapeErr.Error())
	switch {
	case errors.Is(scrapeErr, scraper.ErrRestricted) && (strings.Contains(msg, "min_age_account") || strings.Contains(msg, "under 21") || strings.Contains(msg, "people under")):
		return "Instagram post is age-restricted", "Instagram limits this post to viewers aged 21 or older, so its media cannot be shown in the preview."
	case errors.Is(scrapeErr, scraper.ErrRestricted) && (strings.Contains(msg, "geoblock") || strings.Contains(msg, "region")):
		return "Instagram post is region-restricted", "Instagram does not make this post available from the region used to build the preview."
	case errors.Is(scrapeErr, scraper.ErrRestricted):
		return "Instagram post is restricted", "Instagram restricts public access to this post, so its media cannot be shown in the preview."
	case errors.Is(scrapeErr, scraper.ErrNotFound) || strings.Contains(msg, "not found") || strings.Contains(msg, "deleted") || strings.Contains(msg, "private"):
		return "Instagram post not available", "The post may have been deleted, made private, or the link may no longer be valid."
	case strings.Contains(msg, "expired") || strings.Contains(msg, "not directly playable"):
		return "Instagram media link expired", "Instagram returned an expired media link. Try sending the post again in a moment to refresh the preview."
	case strings.Contains(msg, "429") || strings.Contains(msg, "too many") || strings.Contains(msg, "rate limit"):
		return "Instagram is temporarily limiting requests", "Instagram temporarily rate-limited the preview request. Try the link again shortly."
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "http 5"):
		return "Instagram preview temporarily unavailable", "Instagram did not respond reliably enough to build the preview. Try the link again shortly."
	default:
		return "Instagram preview unavailable", "Instagram did not provide public media for this post. It may be unavailable or restricted."
	}
}
