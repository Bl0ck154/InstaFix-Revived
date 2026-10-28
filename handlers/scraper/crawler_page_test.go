package handlers

import (
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractCrawlerPageV1Video(t *testing.T) {
	html := []byte(`<html><script>{"xig_polaris_media":{"if_not_gated_logged_out":{"media_type":2,"user":{"username":"alice"},"caption":{"text":"hello {world}"},"image_versions2":{"candidates":[{"url":"https://scontent.cdninstagram.com/cover.jpg","width":720,"height":1280}]},"video_versions":[{"url":"https://scontent.cdninstagram.com/video.mp4","width":720,"height":1280}]}}}</script></html>`)
	raw, err := extractCrawlerMediaObject(html)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	item, err := parseCrawlerMediaFixture("Crawler123", raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !item.HasVideo() || item.Username != "alice" || item.Caption != "hello {world}" {
		t.Fatalf("unexpected item: %#v", item)
	}
	if len(item.Medias) != 1 || item.Medias[0].ThumbnailURL == "" {
		t.Fatalf("video presentation incomplete: %#v", item.Medias)
	}
}

func TestExtractCrawlerPagePlainJSONVideo(t *testing.T) {
	html := []byte(`<html><script>{"xig_polaris_media":{"if_not_gated_logged_out":{"media_type":2,"user":{"username":"bob"},"caption":{"text":"plain relay payload"},"image_versions2":{"candidates":[{"url":"https://scontent.cdninstagram.com/plain-cover.jpg","width":720,"height":1280}]},"video_versions":[{"url":"https://scontent.cdninstagram.com/plain-video.mp4","width":720,"height":1280}]}}}</script></html>`)
	raw, err := extractCrawlerMediaObject(html)
	if err != nil {
		t.Fatalf("extract plain marker failed: %v", err)
	}
	item, err := parseCrawlerMediaFixture("PlainCrawler123", raw)
	if err != nil {
		t.Fatalf("parse plain marker failed: %v", err)
	}
	if !item.HasVideo() || item.Username != "bob" || item.Caption != "plain relay payload" {
		t.Fatalf("unexpected plain item: %#v", item)
	}
}

func TestExtractCrawlerPageMarkerMissing(t *testing.T) {
	_, err := extractJSONObjectAfterMarker([]byte(`<html>login wall</html>`), []byte(`"xig_polaris_media":`))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing marker error = %v, want ErrNotFound", err)
	}
}

func parseCrawlerMediaFixture(postID string, raw []byte) (*InstaData, error) {
	root := gjson.ParseBytes(raw)
	node := root.Get("if_not_gated_logged_out")
	if !presentResult(node) && root.Get("media_type").Exists() {
		node = root
	}
	item := &InstaData{PostID: postID}
	if err := parseV1MediaData(item, node); err != nil {
		return nil, err
	}
	return item, nil
}
