package handlers

import (
    "encoding/base64"
    "errors"
    "fmt"
    scraper "instafix/handlers/scraper"
    "instafix/utils"
    "instafix/views"
    "instafix/views/model"
    "net/http"
    "net/url"
    "os"
    "strconv"
    "strings"

    "github.com/go-chi/chi/v5"
)

var (
    highlightPublicCollection = scraper.GetHighlightPreviewNoAuth
    highlightPublicStory = scraper.GetDataQuiet
    highlightPublicVideoStory = scraper.GetDataPreferVideoQuiet
)

// HighlightEmbed handles public Instagram Highlight links without cookies.
// It is opt-in and intentionally independent of legacy Reel/Post auth helpers,
// OGInstagram redirect/proxy paths, and the stateless video remux.
func HighlightEmbed(w http.ResponseWriter, r *http.Request) {
    if os.Getenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED") != "true" {
        http.NotFound(w, r)
        return
    }
    rawID := chi.URLParam(r, "postID")
    highlightID, err := decodePublicHighlightID(r.URL.Path, rawID)
    if err != nil {
        http.Error(w, "invalid Highlight ID", http.StatusBadRequest)
        return
    }
    originalStoryParam := r.URL.Query().Get("story_media_id")
    storyID, err := parsePublicStoryMediaID(originalStoryParam)
    if err != nil {
        http.Error(w, "invalid story_media_id", http.StatusBadRequest)
        return
    }
    // Only retain the selected media ID. Sharing/security tokens from the
    // incoming query are never mirrored into public preview metadata.
    canonical := "https://www.instagram.com" + r.URL.Path
    if storyID != "" {
        canonical += "?story_media_id=" + url.QueryEscape(storyID)
    }
    if !utils.IsBot(r.UserAgent()) {
        http.Redirect(w, r, canonical, http.StatusFound)
        return
    }

    var item *scraper.InstaData
    postID := highlightID
    if storyID == "" {
        item, err = highlightPublicCollection(highlightID)
    } else {
        postID, err = shortcodeForStoryID(storyID)
        if err != nil {
            http.Error(w, "invalid story ID", http.StatusBadRequest)
            return
        }
        item, err = highlightPublicStory(postID)
        if err != nil || item == nil || len(item.Medias) == 0 || item.Username == "" {
            if video, videoErr := highlightPublicVideoStory(postID); videoErr == nil && video != nil && len(video.Medias) > 0 && video.Username != "" {
                item, err = video, nil
            }
        }
    }
    // Fail closed on inaccessible/restricted Highlight stories. Do not attempt
    // a session-cookie recovery even when AUTH_HELPER_URL is configured.
    if err != nil || item == nil || len(item.Medias) == 0 || item.Username == "" {
        http.Error(w, "public Highlight preview unavailable", http.StatusNotFound)
        return
    }

    media := item.Medias[0]
    imageURL := media.URL
    if media.IsVideo() {
        // Videos can be rendered as thumbnails without relaying private bytes.
        // Video playback is deliberately deferred until separately verified.
        imageURL = media.ThumbnailURL
    }
    if !scraper.IsAllowedMediaURL(imageURL) {
        http.Error(w, "public Highlight media unavailable", http.StatusNotFound)
        return
    }
    _ = postID // Post shortcode is used only for public resolution and its cache.
    baseURL := requestPublicBaseURL(r)
    v := &model.ViewsData{
        Site: "InstaFix Revived",
        URL: canonical,
        CanonicalURL: canonical,
        Title: "@" + item.Username,
        Creator: "@" + item.Username,
        ArticleAuthor: "https://www.instagram.com/" + item.Username + "/",
        Card: "summary_large_image",
        OGType: "article",
        NoRedirect: true,
        ImageURL: imageURL,
        ImageWidth: media.Width,
        ImageHeight: media.Height,
        FaviconURL: baseURL + "/favicon.svg",
        AppleIconURL: baseURL + "/favicon.svg",
    }
    if item.Caption != "" {
        v.Description = item.Caption
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Header().Set("Cache-Control", "public, max-age=0")
    w.Header().Set("Cloudflare-CDN-Cache-Control", "public, max-age=60")
    views.Embed(v, w)
}

func validHighlightNumericID(id string) bool {
    if len(id) == 0 || len(id) > 19 || id[0] == '0' {
        return false
    }
    for _, r := range id {
        if r < '0' || r > '9' {
            return false
        }
    }
    _, err := strconv.ParseUint(id, 10, 64)
    return err == nil
}

func decodePublicHighlightID(path, raw string) (string, error) {
    if strings.HasPrefix(path, "/stories/highlights/") {
        if validHighlightNumericID(raw) {
            return raw, nil
        }
        return "", errors.New("invalid numeric Highlight ID")
    }
    if !strings.HasPrefix(path, "/s/") || len(raw) == 0 || len(raw) > 64 {
        return "", errors.New("invalid Highlight share URL")
    }
    decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
    if err != nil || !strings.HasPrefix(string(decoded), "highlight:") {
        return "", errors.New("invalid Highlight token")
    }
    id := strings.TrimPrefix(string(decoded), "highlight:")
    if !validHighlightNumericID(id) {
        return "", errors.New("invalid Highlight token ID")
    }
    return id, nil
}

func parsePublicStoryMediaID(raw string) (string, error) {
    if raw == "" {
        return "", nil
    }
    parts := strings.Split(raw, "_")
    if len(parts) > 2 || !validHighlightNumericID(parts[0]) || (len(parts) == 2 && !validHighlightNumericID(parts[1])) {
        return "", errors.New("invalid composite story media ID")
    }
    return parts[0], nil
}

func shortcodeForStoryID(id string) (string, error) {
    n, err := strconv.ParseUint(id, 10, 64)
    if err != nil || n == 0 {
        return "", fmt.Errorf("invalid story media ID")
    }
    const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
    var out string
    for n > 0 {
        out = string(alphabet[n%64]) + out
        n /= 64
    }
    return out, nil
}
