package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"instafix/utils"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// GetHighlightPreviewNoAuth reads the public collection poster when no specific
// story was selected. It must not be used as the image for a selected story:
// Instagram's collection Open Graph image is not necessarily that story's image.
func GetHighlightPreviewNoAuth(highlightID string) (*InstaData, error) {
	if !utils.ValidInstagramNumericID(highlightID) {
		return nil, errors.New("invalid highlight ID")
	}
	req, err := http.NewRequest(http.MethodGet, "https://www.instagram.com/stories/highlights/"+highlightID+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	res, err := DoPublicInstagramRequest(req, "public highlight preview")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("public highlight HTTP %s", res.Status)
	}
	body, err := readLimitedHTTPBody(res, maxInstagramEmbedBodyBytes)
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	image, _ := doc.Find(`meta[property="og:image"]`).Attr("content")
	if !IsAllowedMediaURL(image) {
		return nil, ErrNotFound
	}
	title, _ := doc.Find(`meta[property="og:title"]`).Attr("content")
	caption, _ := doc.Find(`meta[name="description"]`).Attr("content")
	username := ""
	if index := strings.LastIndex(title, "@"); index >= 0 {
		username = strings.TrimSpace(title[index+1:])
	}
	if username == "" {
		return nil, ErrNotFound
	}
	item := &InstaData{PostID: highlightID, Username: username, Caption: caption, Medias: []Media{{TypeName: "GraphImage", URL: image}}}
	if err := normalizeMediaURLs(item); err != nil {
		return nil, err
	}
	return item, nil
}
