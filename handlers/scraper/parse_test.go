package handlers

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const sampleXDTMedia = `{"shortcode_media":{
  "__typename":"XDTGraphSidecar",
  "owner":{"username":"designcompass"},
  "edge_media_to_caption":{"edges":[{"node":{"text":"hello\nworld"}}]},
  "edge_sidecar_to_children":{"edges":[
    {"node":{"__typename":"XDTGraphImage","is_video":false,"display_url":"https://cdn/img1.jpg","display_resources":[{"src":"https://cdn/img1_small.jpg","config_width":640,"config_height":800},{"src":"https://cdn/img1_big.jpg","config_width":1080,"config_height":1350}],"dimensions":{"width":1080,"height":1350}}},
    {"node":{"__typename":"XDTGraphVideo","is_video":true,"display_url":"https://cdn/vcover.jpg","video_url":"https://cdn/vid.mp4","dimensions":{"width":720,"height":1280}}}
  ]}
}}`

func TestParseXDTMediaData(t *testing.T) {
	item := &InstaData{PostID: "DZWI_exgXz7"}
	if err := parseGraphQLMediaData(item, gjson.Parse(sampleXDTMedia)); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if item.Username != "designcompass" {
		t.Fatalf("username mismatch: %q", item.Username)
	}
	if item.Caption != "hello\nworld" {
		t.Fatalf("caption mismatch: %q", item.Caption)
	}
	if len(item.Medias) != 2 {
		t.Fatalf("want 2 medias, got %d", len(item.Medias))
	}
	if got := item.Medias[0]; !got.IsImage() || got.URL != "https://cdn/img1.jpg" || got.Width != 1080 || got.Height != 1350 {
		t.Fatalf("image media mismatch: %+v", got)
	}
	if got := item.Medias[1]; !got.IsVideo() || got.URL != "https://cdn/vid.mp4" || got.ThumbnailURL != "https://cdn/vcover.jpg" {
		t.Fatalf("video media mismatch: %+v", got)
	}
}

const sampleV1Media = `{"xdt_api__v1__media__shortcode__web_info":{"items":[{
  "code":"DZWI_exgXz7",
  "user":{"username":"designcompass"},
  "caption":{"text":"v1 caption"},
  "carousel_media":[
    {"media_type":1,"image_versions2":{"candidates":[{"url":"https://cdn/small.jpg","width":320,"height":400},{"url":"https://cdn/big.jpg","width":1080,"height":1350}]}},
    {"media_type":2,"image_versions2":{"candidates":[{"url":"https://cdn/cover.jpg","width":720,"height":1280}]},"video_versions":[{"url":"https://cdn/vid_small.mp4","width":360,"height":640},{"url":"https://cdn/vid_big.mp4","width":720,"height":1280}]}
  ]
}]}}`

func TestParseV1MediaData(t *testing.T) {
	item := &InstaData{PostID: "DZWI_exgXz7"}
	if err := parseGraphQLMediaData(item, gjson.Parse(sampleV1Media)); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if item.Username != "designcompass" || item.Caption != "v1 caption" {
		t.Fatalf("metadata mismatch: %+v", item)
	}
	if len(item.Medias) != 2 {
		t.Fatalf("want 2 medias, got %d", len(item.Medias))
	}
	if got := item.Medias[0]; !got.IsImage() || got.URL != "https://cdn/big.jpg" {
		t.Fatalf("v1 image mismatch: %+v", got)
	}
	if got := item.Medias[1]; !got.IsVideo() || got.URL != "https://cdn/vid_big.mp4" || got.ThumbnailURL != "https://cdn/cover.jpg" {
		t.Fatalf("v1 video mismatch: %+v", got)
	}
}

func TestGraphQLBodyReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "require login flag", body: `{"require_login":true}`, want: "require_login"},
		{name: "rate limited message", body: `{"message":"Please wait a few minutes before you try again."}`, want: "rate_limited"},
		{name: "media body", body: `{"data":{"shortcode_media":{"owner":{"username":"u"},"display_url":"https://cdn/img.jpg"}}}`, want: ""},
		{name: "empty success", body: `{"data":{}}`, want: "no_media"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := graphQLBodyReason([]byte(tt.body)); got != tt.want {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
		})
	}
}

func TestGraphQLStatusReason(t *testing.T) {
	if got := graphQLStatusReason(429); got != "rate_limited" {
		t.Fatalf("429 reason mismatch: %q", got)
	}
	if got := graphQLStatusReason(503); got != "server_error" {
		t.Fatalf("503 reason mismatch: %q", got)
	}
}

func TestPublicVideoRefreshEnabled(t *testing.T) {
	origProxies := publicProxyURLs
	t.Cleanup(func() {
		publicProxyURLs = origProxies
		os.Unsetenv("INSTAFIX_PUBLIC_VIDEO_REFRESH_DIRECT")
	})

	publicProxyURLs = nil
	os.Unsetenv("INSTAFIX_PUBLIC_VIDEO_REFRESH_DIRECT")
	if publicVideoRefreshEnabled() {
		t.Fatal("public video refresh should be disabled without proxies or direct override")
	}

	publicProxyURLs = []string{"http://127.0.0.1:8080"}
	if !publicVideoRefreshEnabled() {
		t.Fatal("public video refresh should be enabled with proxies")
	}

	publicProxyURLs = nil
	os.Setenv("INSTAFIX_PUBLIC_VIDEO_REFRESH_DIRECT", "true")
	if !publicVideoRefreshEnabled() {
		t.Fatal("public video refresh should be enabled with direct override")
	}
}

func TestPublicVideoRefreshNegativeCache(t *testing.T) {
	origProxies := publicProxyURLs
	origTTL := publicVideoRefreshNegTTL
	origMap := publicVideoRefreshNeg
	t.Cleanup(func() {
		publicProxyURLs = origProxies
		publicVideoRefreshNegTTL = origTTL
		publicVideoRefreshNeg = origMap
	})

	publicProxyURLs = []string{"http://127.0.0.1:8080"}
	publicVideoRefreshNegTTL = time.Minute
	publicVideoRefreshNeg = map[string]embedAuthNegative{}
	savePublicVideoRefreshNegative("DaD4t0NN2zJ", errors.New("rate_limited"))
	if reason, ok := publicVideoRefreshNegativeHit("DaD4t0NN2zJ"); !ok || reason != "rate_limited" {
		t.Fatalf("expected negative cache hit, got ok=%v reason=%q", ok, reason)
	}
	if shouldTryPublicVideoRefresh("DaD4t0NN2zJ") {
		t.Fatal("should not retry public video refresh during cooldown")
	}
}
