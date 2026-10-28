package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/tidwall/gjson"
)

// Crawler-page fallback only reads public post pages. All requests go through
// DoPublicInstagramRequest to respect the fail-closed proxy-only setting.
func RefreshVideoFromCrawlerPage(postID string) (*InstaData, error) {
	if !validPostID(postID) {
		return nil, errors.New("postID is not a valid Instagram post ID")
	}
	ret, err, _ := sflightScraper.Do(postID+":crawler-page-video-refresh", func() (interface{}, error) {
		item, err := scrapeCrawlerPageMedia(postID)
		if err != nil {
			return nil, err
		}
		if item == nil || !item.HasVideo() {
			return nil, ErrNotFound
		}
		if err := normalizeMediaURLs(item); err != nil {
			return nil, err
		}
		if err := saveDataToCache(item); err != nil {
			slog.Debug("Failed to cache crawler-page media", "postID", postID, "err", err)
		}
		return item, nil
	})
	if err != nil {
		return nil, err
	}
	return ret.(*InstaData), nil
}

func scrapeCrawlerPageMedia(postID string) (*InstaData, error) {
	u := "https://www.instagram.com/p/" + url.PathEscape(postID) + "/"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Googlebot/2.1 (+http://www.google.com/bot.html)")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", "text/html,*/*;q=0.8")
	res, err := DoPublicInstagramRequest(req, "crawler page")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crawler page HTTP %s", res.Status)
	}
	body, err := readLimitedHTTPBody(res, maxInstagramEmbedBodyBytes)
	if err != nil {
		return nil, err
	}
	raw, err := extractCrawlerMediaObject(body)
	if err != nil {
		return nil, err
	}
	root := gjson.ParseBytes(raw)
	node := root.Get("if_not_gated_logged_out")
	if !presentResult(node) && root.Get("media_type").Exists() {
		node = root
	}
	if !presentResult(node) {
		return nil, ErrNotFound
	}
	item := &InstaData{PostID: postID}
	if err := parseV1MediaData(item, node); err != nil {
		return nil, err
	}
	slog.Info("Data parsed from crawler page xig_polaris_media", "postID", postID, "has_video", item.HasVideo())
	return item, nil
}

func extractCrawlerMediaObject(body []byte) ([]byte, error) {
	markers := [][]byte{
		[]byte("\"xig_polaris_media\":"),
		[]byte("\\\"xig_polaris_media\\\":"),
	}
	var lastErr error = ErrNotFound
	for _, marker := range markers {
		raw, err := extractJSONObjectAfterMarker(body, marker)
		if err == nil {
			return raw, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func extractJSONObjectAfterMarker(body, marker []byte) ([]byte, error) {
	idx := bytes.Index(body, marker)
	if idx < 0 {
		return nil, ErrNotFound
	}
	rel := bytes.IndexByte(body[idx+len(marker):], '{')
	if rel < 0 {
		return nil, ErrNotFound
	}
	start := idx + len(marker) + rel
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(body); i++ {
		ch := body[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start : i+1], nil
			}
			if depth < 0 {
				return nil, errors.New("crawler media JSON is unbalanced")
			}
		}
	}
	return nil, errors.New("crawler media JSON is truncated")
}

