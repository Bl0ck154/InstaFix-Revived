package handlers

import (
	"errors"
	"fmt"
	scraper "instafix/handlers/scraper"
	"instafix/observability"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var (
	offloadGetData                 = scraper.GetData
	offloadGetDataQuiet            = scraper.GetDataQuiet
	offloadGetDataPreferVideo      = scraper.GetDataPreferVideo
	offloadGetDataPreferVideoQuiet = scraper.GetDataPreferVideoQuiet
	offloadRefreshDataPreferVideo  = scraper.RefreshDataPreferVideo
	offloadMediaURLAllowed         = scraper.IsAllowedMediaURL
	offloadVideoClient             = &http.Client{
		Transport: previewVideoProxyClient.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("video redirect limit exceeded")
			}
			if !scraper.IsAllowedMediaURL(req.URL.String()) {
				return errors.New("video redirect left allowed Instagram CDN hosts")
			}
			return nil
		},
	}
)

// Offload resolves a stable local media URL to the current Instagram CDN URL.
// Images retain the inexpensive redirect behavior. Videos are streamed so
// preview clients never receive a redirect as their MP4 target.
func Offload(w http.ResponseWriter, r *http.Request) {
	if rejectStatelessLegacyMedia(w) {
		return
	}
	postID := chi.URLParam(r, "postID")
	mediaNum, err := strconv.Atoi(strings.TrimSuffix(chi.URLParam(r, "mediaNum"), ".mp4"))
	if err != nil || mediaNum < 1 {
		http.Error(w, "invalid media number", http.StatusBadRequest)
		return
	}
	if TryOGInstagramClientMediaRedirect(w, r, postID, mediaNum) {
		return
	}
	if TryOGInstagramOffloadProxy(w, r) {
		return
	}

	item, err := getOffloadData(r, postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if mediaNum > len(item.Medias) {
		http.Error(w, "media number out of range", http.StatusNotFound)
		return
	}

	media := item.Medias[mediaNum-1]
	target := media.URL
	thumbnail := r.URL.Query().Has("thumbnail")
	if thumbnail {
		if media.ThumbnailURL != "" {
			target = media.ThumbnailURL
		}
	}
	if target == "" {
		http.Error(w, "media URL unavailable", http.StatusNotFound)
		return
	}
	if !r.URL.Query().Has("thumbnail") && media.IsVideo() {
		if r.URL.Query().Get("delivery") == telegramCDNRedirectProbeDelivery || shouldRedirectPreviewVideo(r.UserAgent()) {
			redirectOffloadVideo(w, r, postID, mediaNum, target)
			return
		}
		streamOffloadVideo(w, r, postID, mediaNum, target)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
	slog.Info("offload image redirected", "postID", postID, "media_index", mediaNum, "thumbnail", thumbnail, "status", http.StatusFound)
}

func redirectOffloadVideo(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, videoURL string) {
	target, err := validatedRedirectVideoURL(r, postID, mediaNum, videoURL)
	if err != nil {
		slog.Warn("offload video redirect unavailable after refresh", "postID", postID, "err", err)
		observability.Default.RecordMediaStream(r.Method, r.Header.Get("Range") != "", http.StatusBadGateway, 0, err)
		http.Error(w, "video redirect temporarily unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Location", target)
	w.Header().Set("X-InstaFix-Video-Delivery", "cdn-redirect")
	w.WriteHeader(http.StatusFound)
	slog.Info("offload video redirected to CDN", "postID", postID, "media_index", mediaNum, "status", http.StatusFound)
}

func validatedRedirectVideoURL(r *http.Request, postID string, mediaNum int, videoURL string) (string, error) {
	if err := probeVideoURL(r, videoURL); err == nil {
		return videoURL, nil
	} else {
		slog.Info("offload redirect video URL needs refresh", "postID", postID, "err", err)
	}

	refreshed, refreshErr := offloadRefreshDataPreferVideo(postID)
	if refreshErr != nil {
		return "", fmt.Errorf("refresh failed: %w", refreshErr)
	}
	if mediaNum < 1 || mediaNum > len(refreshed.Medias) {
		return "", errors.New("refreshed media index out of range")
	}
	media := refreshed.Medias[mediaNum-1]
	if !media.IsVideo() || media.URL == "" {
		return "", errors.New("refreshed media did not contain a video URL")
	}
	if err := probeVideoURL(r, media.URL); err != nil {
		return "", fmt.Errorf("refreshed video URL failed validation: %w", err)
	}
	return media.URL, nil
}

func probeVideoURL(r *http.Request, videoURL string) error {
	probe := r.Clone(r.Context())
	probe.Method = http.MethodHead
	probe.Header = r.Header.Clone()
	probe.Header.Del("Range")
	res, err := requestVideoResponse(probe, videoURL)
	if res != nil {
		res.Body.Close()
	}
	return err
}

func getOffloadData(r *http.Request, postID string) (*scraper.InstaData, error) {
	// Image and carousel URLs must retain the same media list used by Embed.
	// Asking for video on an image-only post can trigger an unrelated auth
	// refresh with fewer media, invalidating already-published image indexes.
	getData, getDataQuiet := offloadGetData, offloadGetDataQuiet
	if strings.HasSuffix(chi.URLParam(r, "mediaNum"), ".mp4") || strings.HasPrefix(r.URL.Path, "/videos/") {
		getData, getDataQuiet = offloadGetDataPreferVideo, offloadGetDataPreferVideoQuiet
	}
	if isPreviewMediaBot(r.UserAgent()) {
		return getData(postID)
	}
	item, err := getDataQuiet(postID)
	if err != nil && scraper.AuthHelperURL != "" {
		observability.Default.RecordAuthHelperSkipped(postID, "untrusted_media_client")
	}
	return item, err
}

func isPreviewMediaBot(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	for _, allowed := range PreviewVideoProxyUserAgents {
		if allowed == "*" || strings.Contains(ua, allowed) {
			return true
		}
	}
	return false
}

func streamOffloadVideo(w http.ResponseWriter, r *http.Request, postID string, mediaNum int, videoURL string) {
	finishStream := observability.Default.BeginMediaStream()
	defer finishStream()
	// OGInstagram's edge offload path ignores Telegram's Range probe, fetches the
	// upstream object as a normal GET, and exposes a same-origin 200 response with
	// minimal headers. Keep the explicit probe switch, but also use that behavior
	// automatically for the stable .mp4 URLs advertised in our Open Graph tags.
	minimalTelegramStream := isTelegramBot(r.UserAgent()) &&
		(r.URL.Query().Get("delivery") == telegramMinimalStreamDelivery ||
			strings.HasSuffix(strings.ToLower(r.URL.Path), ".mp4"))
	upstreamRequest := r
	if minimalTelegramStream {
		upstreamRequest = r.Clone(r.Context())
		upstreamRequest.Method = http.MethodGet
		upstreamRequest.Header = r.Header.Clone()
		upstreamRequest.Header.Del("Range")
	}

	// Full Reel downloads can legitimately take longer than the server's
	// page-response WriteTimeout. Clear only this response deadline while the
	// upstream body is streamed; request cancellation still stops the copy.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Debug("offload video write deadline unchanged", "postID", postID, "err", err)
	}

	res, err := requestVideoResponse(upstreamRequest, videoURL)
	if err != nil {
		slog.Info("offload video URL needs refresh", "postID", postID, "err", err)
		if isPreviewMediaBot(r.UserAgent()) {
			refreshed, refreshErr := offloadRefreshDataPreferVideo(postID)
			if refreshErr == nil && mediaNum <= len(refreshed.Medias) {
				media := refreshed.Medias[mediaNum-1]
				if media.IsVideo() && media.URL != "" && media.URL != videoURL {
					res, err = requestVideoResponse(upstreamRequest, media.URL)
				} else {
					err = errors.New("refreshed media did not contain a new video URL")
				}
			} else if refreshErr != nil {
				err = fmt.Errorf("refresh failed: %w", refreshErr)
			}
		} else {
			observability.Default.RecordAuthHelperSkipped(postID, "untrusted_media_refresh")
		}
	}
	if err != nil || res == nil {
		slog.Warn("offload video unavailable after refresh", "postID", postID, "err", err)
		observability.Default.RecordMediaStream(r.Method, r.Header.Get("Range") != "", http.StatusBadGateway, 0, err)
		http.Error(w, "video stream temporarily unavailable", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	if minimalTelegramStream {
		contentType := res.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "video/mp4"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.Header().Set("X-Instagram7-Video-Stream", "telegram-minimal")
	} else {
		copyVideoResponseHeaders(w.Header(), res.Header)
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "video/mp4")
		}
		if w.Header().Get("Accept-Ranges") == "" {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("X-Instagram7-Video-Stream", "direct")
	}
	w.WriteHeader(res.StatusCode)
	if r.Method == http.MethodHead {
		observability.Default.RecordMediaStream(r.Method, r.Header.Get("Range") != "", res.StatusCode, 0, nil)
		slog.Info("offload video HEAD served", "postID", postID, "status", res.StatusCode)
		return
	}

	written, copyErr := io.CopyBuffer(w, res.Body, make([]byte, 64*1024))
	if copyErr != nil {
		observability.Default.RecordMediaStream(r.Method, r.Header.Get("Range") != "", res.StatusCode, written, copyErr)
		slog.Warn("offload video client copy failed", "postID", postID, "bytes", written, "err", copyErr)
		return
	}
	observability.Default.RecordMediaStream(r.Method, r.Header.Get("Range") != "", res.StatusCode, written, nil)
	slog.Info("offload video streamed", "postID", postID, "status", res.StatusCode, "bytes", written, "range", r.Header.Get("Range") != "")
}

type videoUpstreamError struct {
	status int
}

func (e *videoUpstreamError) Error() string {
	return "video upstream returned " + strconv.Itoa(e.status)
}

func requestVideoResponse(r *http.Request, videoURL string) (*http.Response, error) {
	res, err := doVideoRequest(r, r.Method, videoURL, r.Header.Get("Range"))
	if r.Method != http.MethodHead || usableVideoResponse(res, err) {
		return res, responseError(res, err)
	}
	if res != nil {
		res.Body.Close()
	}

	probeRange := r.Header.Get("Range")
	if probeRange == "" {
		probeRange = "bytes=0-0"
	}
	returnRes, returnErr := doVideoRequest(r, http.MethodGet, videoURL, probeRange)
	return returnRes, responseError(returnRes, returnErr)
}

func doVideoRequest(r *http.Request, method, videoURL, rangeHeader string) (*http.Response, error) {
	if !offloadMediaURLAllowed(videoURL) {
		return nil, errors.New("video URL host is not allowed")
	}
	req, err := http.NewRequestWithContext(r.Context(), method, videoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)")
	req.Header.Set("Accept", "video/mp4,video/*;q=0.9,*/*;q=0.1")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return offloadVideoClient.Do(req)
}

func responseError(res *http.Response, err error) error {
	if err != nil {
		return err
	}
	if res == nil {
		return http.ErrAbortHandler
	}
	if !usableVideoResponse(res, nil) {
		res.Body.Close()
		return &videoUpstreamError{status: res.StatusCode}
	}
	return nil
}

func usableVideoResponse(res *http.Response, err error) bool {
	if err != nil || res == nil {
		return false
	}
	if res.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return true
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Type")))
	return !strings.HasPrefix(contentType, "text/") &&
		!strings.Contains(contentType, "application/json")
}

func copyVideoResponseHeaders(dst, src http.Header) {
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Cache-Control"} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
}
