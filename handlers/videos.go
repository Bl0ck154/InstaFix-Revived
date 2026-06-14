package handlers

import (
	scraper "instafix/handlers/scraper"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var VideoProxyAddr string
var PreviewVideoProxyEnabled bool
var PreviewVideoProxyUserAgents = []string{"telegrambot", "discordbot", "facebookexternalhit", "whatsapp", "slackbot", "twitterbot", "xbot", "skypeuripreview", "linkedinbot"}
var PreviewVideoProxyTimeout = 25 * time.Second

func ConfigurePreviewVideoProxy(enabled bool, allowlist string) {
	PreviewVideoProxyEnabled = enabled
	if strings.TrimSpace(allowlist) == "" {
		return
	}
	parts := strings.Split(allowlist, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			items = append(items, part)
		}
	}
	if len(items) > 0 {
		PreviewVideoProxyUserAgents = items
	}
}

func ConfigurePreviewVideoProxyTimeout(seconds int) {
	if seconds > 0 {
		PreviewVideoProxyTimeout = time.Duration(seconds) * time.Second
	}
}

func shouldProxyPreviewVideo(userAgent string) bool {
	if !PreviewVideoProxyEnabled || scraper.AuthHelperURL == "" {
		return false
	}
	ua := strings.ToLower(userAgent)
	for _, allowed := range PreviewVideoProxyUserAgents {
		if allowed == "*" || strings.Contains(ua, allowed) {
			return true
		}
	}
	return false
}

func Videos(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")
	mediaNum, err := strconv.Atoi(chi.URLParam(r, "mediaNum"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	item, err := scraper.GetDataPreferVideo(postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Redirect to image URL
	if mediaNum < 1 || mediaNum > len(item.Medias) {
		http.Error(w, "media number out of range", http.StatusNotFound)
		return
	}
	media := item.Medias[mediaNum-1]
	if !media.IsVideo() {
		http.Error(w, "media is not a video", http.StatusNotFound)
		return
	}
	videoURL := media.URL
	previewProxy := r.Method != http.MethodHead && shouldProxyPreviewVideo(r.Header.Get("User-Agent"))
	if previewProxy {
		if proxyVideoThroughAuthHelper(w, r, postID, videoURL) {
			return
		}
		http.Error(w, "preview video proxy unavailable", http.StatusBadGateway)
		return
	}

	// Redirect directly unless a generic legacy proxy was explicitly configured.
	if strings.Contains(r.Header.Get("User-Agent"), "TelegramBot") {
		http.Redirect(w, r, videoURL, http.StatusFound)
		return
	}
	target := videoURL
	if VideoProxyAddr != "" {
		target = VideoProxyAddr + videoURL
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func proxyVideoThroughAuthHelper(w http.ResponseWriter, r *http.Request, postID, videoURL string) bool {
	base, err := url.Parse(scraper.AuthHelperURL)
	if err != nil {
		slog.Warn("preview video proxy skipped: invalid helper URL", "postID", postID, "err", err)
		return false
	}
	path := strings.TrimRight(base.Path, "/") + "/video/" + postID
	proxyURL := base.ResolveReference(&url.URL{Path: path, RawQuery: url.Values{"url": {videoURL}}.Encode()})
	req, err := http.NewRequestWithContext(r.Context(), r.Method, proxyURL.String(), nil)
	if err != nil {
		slog.Warn("preview video proxy request creation failed", "postID", postID, "err", err)
		return false
	}
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	client := http.Client{Transport: &http.Transport{ResponseHeaderTimeout: PreviewVideoProxyTimeout}}
	res, err := client.Do(req)
	if err != nil || res == nil {
		if err == nil {
			err = http.ErrAbortHandler
		}
		slog.Warn("preview video proxy helper request failed", "postID", postID, "err", err)
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		slog.Warn("preview video proxy helper rejected request", "postID", postID, "status", res.Status)
		return false
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Cache-Control"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, res.Body); err != nil {
			slog.Warn("preview video proxy client copy failed", "postID", postID, "err", err)
		}
	}
	slog.Info("preview video proxied through auth helper", "postID", postID, "status", res.StatusCode)
	return true
}
