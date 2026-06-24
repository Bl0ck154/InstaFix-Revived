package handlers

import (
	scraper "instafix/handlers/scraper"
	"net/http"
	"strconv"

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
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}
