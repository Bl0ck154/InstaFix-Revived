package handlers

import (
	scraper "instafix/handlers/scraper"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

func Images(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")
	mediaNum, err := strconv.Atoi(chi.URLParam(r, "mediaNum"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	item, err := scraper.GetData(postID)
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
	imageURL := media.URL
	if !media.IsImage() {
		if media.ThumbnailURL == "" {
			http.Error(w, "media is not an image", http.StatusNotFound)
			return
		}
		imageURL = media.ThumbnailURL
	}
	http.Redirect(w, r, imageURL, http.StatusFound)
}
