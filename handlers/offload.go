package handlers

import (
	scraper "instafix/handlers/scraper"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Offload resolves a stable local media URL to the current Instagram CDN URL.
// This keeps embed HTML stable and gives us one place to refresh cached scrape
// data before redirecting bots to image/video bytes.
func Offload(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")
	mediaNum, err := strconv.Atoi(chi.URLParam(r, "mediaNum"))
	if err != nil || mediaNum < 1 {
		http.Error(w, "invalid media number", http.StatusBadRequest)
		return
	}

	item, err := scraper.GetDataPreferVideo(postID)
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
	if r.URL.Query().Has("thumbnail") {
		if media.ThumbnailURL != "" {
			target = media.ThumbnailURL
		}
	}
	if target == "" {
		http.Error(w, "media URL unavailable", http.StatusNotFound)
		return
	}
	if !r.URL.Query().Has("thumbnail") && media.IsVideo() && isPreviewMediaBot(r.Header.Get("User-Agent")) {
		if proxyOffloadVideo(w, r, postID, target) {
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
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

func proxyOffloadVideo(w http.ResponseWriter, r *http.Request, postID, videoURL string) bool {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, videoURL, nil)
	if err != nil {
		slog.Warn("offload video proxy request creation failed", "postID", postID, "err", err)
		return false
	}
	req.Header.Set("User-Agent", "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)")
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	res, err := previewVideoProxyClient.Do(req)
	if err != nil || res == nil {
		if err == nil {
			err = http.ErrAbortHandler
		}
		slog.Warn("offload video proxy upstream failed", "postID", postID, "err", err)
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		slog.Warn("offload video proxy upstream rejected request", "postID", postID, "status", res.Status)
		return false
	}
	if res.ContentLength > maxPreviewVideoProxyBytes {
		slog.Warn("offload video proxy response too large", "postID", postID, "contentLength", res.ContentLength)
		return false
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Cache-Control"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "video/mp4")
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(res.StatusCode)
	if r.Method != http.MethodHead {
		limited := &io.LimitedReader{R: res.Body, N: maxPreviewVideoProxyBytes}
		if _, err := io.Copy(w, limited); err != nil {
			slog.Warn("offload video proxy client copy failed", "postID", postID, "err", err)
		} else if limited.N == 0 {
			slog.Warn("offload video proxy byte limit reached", "postID", postID, "maxBytes", maxPreviewVideoProxyBytes)
		}
	}
	slog.Info("offload video proxied", "postID", postID, "status", res.StatusCode)
	return true
}
