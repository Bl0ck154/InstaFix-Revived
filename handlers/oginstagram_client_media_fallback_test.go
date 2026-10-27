package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	scraper "instafix/handlers/scraper"
	"instafix/views/model"
)

func TestTelegramClientMediaFallbackKeepsInstagram7Metadata(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbClientFallback/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()
	data := &model.ViewsData{
		URL:          "https://www.instagram.com/reel/DbClientFallback/",
		CanonicalURL: "https://www.instagram.com/reel/DbClientFallback/",
	}
	item := &scraper.InstaData{
		Username: "creator_name",
		Caption:  "Creator caption",
		Medias: []scraper.Media{{
			TypeName:     "GraphVideo",
			URL:          "https://scontent.cdninstagram.com/video.mp4",
			ThumbnailURL: "https://scontent.cdninstagram.com/thumbnail.jpg",
			Width:        720,
			Height:       1280,
		}},
	}

	if !TryOGInstagramClientMediaFallback(recorder, req, "DbClientFallback", 1, data, item, scraper.ErrNotFound) {
		t.Fatal("expected Telegram client-media fallback")
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`property="og:site_name" content="InstaFix Revived"`,
		`property="og:title" content="@creator_name"`,
		`property="og:description" content="Creator caption"`,
		`https://instagram7.test/offload/DbClientFallback/1.mp4?delivery=telegram-edge-stream-v1`,
		`https://instagram7.test/offload/DbClientFallback/1?thumbnail=1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in fallback HTML: %s", want, body)
		}
	}
	if strings.Contains(body, `content="OGInstagram"`) || strings.Contains(body, `content="OG Instagram"`) {
		t.Fatalf("upstream branding leaked into visible preview metadata: %s", body)
	}
	if strings.Contains(body, "delivery=og-client-fallback") {
		t.Fatalf("Telegram must receive the direct client media endpoint, not a redirecting local og:video URL: %s", body)
	}
	if got := recorder.Header().Get("X-Instagram7-Preview-Source"); got != "client-media-fallback" {
		t.Fatalf("preview source = %q", got)
	}
}

func TestTelegramClientMediaFallbackUsesDirectEndpointWithoutLocalVideo(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbNoLocalVideo/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()

	data := &model.ViewsData{Title: "Instagram preview", Width: 400, Height: 400}
	if !TryOGInstagramClientMediaFallback(recorder, req, "DbNoLocalVideo", 1, data, nil, scraper.ErrNotFound) {
		t.Fatal("expected Telegram client-media fallback")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `https://d.oginstagram.example/p/DbNoLocalVideo/`) {
		t.Fatalf("missing direct endpoint in fallback HTML: %s", body)
	}
	for _, unwanted := range []string{
		`property="og:title" content="Instagram preview"`,
		`Instagram Reel preview. Open the original post on Instagram for the full caption.`,
		`content="400"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("unknown-metadata fallback must not contain %q: %s", unwanted, body)
		}
	}
	for _, want := range []string{
		`property="og:video:width" content="720"`,
		`property="og:video:height" content="1280"`,
		`/fallback/DbNoLocalVideo.png?kind=reel`,
		`property="og:image:width" content="720"`,
		`property="og:image:height" content="1280"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("fallback image metadata missing %q: %s", want, body)
		}
	}
}

func TestTelegramClientMediaFallbackUsesDirectEndpointForIncompleteLocalVideo(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbIncompleteVideo/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()
	item := &scraper.InstaData{
		Username: "creator",
		Caption:  "real caption",
		Medias: []scraper.Media{{
			TypeName: "GraphVideo",
			URL:      "https://scontent.cdninstagram.com/video.mp4",
		}},
	}

	if !TryOGInstagramClientMediaFallback(recorder, req, "DbIncompleteVideo", 1, &model.ViewsData{}, item, scraper.ErrNotFound) {
		t.Fatal("expected Telegram client-media fallback")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `https://d.oginstagram.example/p/DbIncompleteVideo/`) {
		t.Fatalf("incomplete local video must use direct client endpoint: %s", body)
	}
	if strings.Contains(body, `delivery=telegram-edge-stream-v1`) {
		t.Fatalf("incomplete local video must not use local stream: %s", body)
	}
	for _, want := range []string{`property="og:title" content="@creator"`, `property="og:description" content="real caption"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("original metadata missing %q: %s", want, body)
		}
	}
}

func TestTelegramClientMediaFallbackForceListOverridesCompleteLocalVideo(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FORCE_POSTS", "OtherCode, DbForcedVideo ; ThirdCode")
	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbForcedVideo/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()
	item := &scraper.InstaData{
		Username: "creator",
		Caption:  "real caption",
		Medias: []scraper.Media{{
			TypeName:     "GraphVideo",
			URL:          "https://scontent.cdninstagram.com/video.mp4",
			ThumbnailURL: "https://scontent.cdninstagram.com/poster.jpg",
			Width:        720,
			Height:       1280,
		}},
	}

	if !TryOGInstagramClientMediaFallback(recorder, req, "DbForcedVideo", 1, &model.ViewsData{}, item, scraper.ErrNotFound) {
		t.Fatal("expected forced Telegram client-media fallback")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `https://d.oginstagram.example/p/DbForcedVideo/`) {
		t.Fatalf("force-listed video must use direct client endpoint: %s", body)
	}
	if strings.Contains(body, `delivery=telegram-edge-stream-v1`) {
		t.Fatalf("force-listed video must not use local stream: %s", body)
	}
}

func TestTelegramClientMediaFallbackPreservesPartialMetadataWithoutUsername(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbPartialData/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()
	item := &scraper.InstaData{
		Caption: "Caption survived the partial public scrape",
		Medias: []scraper.Media{{
			TypeName: "GraphImage",
			URL:      "https://scontent.cdninstagram.com/thumbnail.jpg",
			Width:    720,
			Height:   1280,
		}},
	}

	if !TryOGInstagramClientMediaFallback(recorder, req, "DbPartialData", 1, &model.ViewsData{}, item, scraper.ErrNotFound) {
		t.Fatal("expected Telegram client-media fallback")
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`property="og:title" content="Caption survived the partial public scrape"`,
		`property="og:description" content="Caption survived the partial public scrape"`,
		`property="og:video:width" content="720"`,
		`property="og:video:height" content="1280"`,
		`https://instagram7.test/offload/DbPartialData/1?thumbnail=1`,
		`property="og:image:width" content="720"`,
		`property="og:image:height" content="1280"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("partial metadata missing %q: %s", want, body)
		}
	}
}

func TestTelegramClientMediaFallbackUsesPartialGraphImageAsPosterWithoutDimensions(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbPartialPoster/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()
	item := &scraper.InstaData{Medias: []scraper.Media{{
		TypeName: "GraphImage",
		URL:      "https://scontent.cdninstagram.com/thumbnail.jpg",
	}}}

	if !TryOGInstagramClientMediaFallback(recorder, req, "DbPartialPoster", 1, &model.ViewsData{Title: "Instagram preview"}, item, scraper.ErrNotFound) {
		t.Fatal("expected Telegram client-media fallback")
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`property="og:video:width" content="720"`,
		`property="og:video:height" content="1280"`,
		`https://instagram7.test/offload/DbPartialPoster/1?thumbnail=1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("partial poster metadata missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `property="og:image:width"`) || strings.Contains(body, `property="og:image:height"`) {
		t.Fatalf("unknown real poster dimensions must be omitted: %s", body)
	}
}

func TestTelegramPreviewProbeSelectsIsolatedDeliveryModes(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")

	cdnRequest := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/ProbeCode/?ig7probe=cdn-redirect-v1", nil)
	cdnRequest.Header.Set("User-Agent", "TelegramBot")
	cdnURL, ok := telegramPreviewProbeVideoURL(cdnRequest, "https://instagram7.test", "ProbeCode", 1, true)
	if !ok || cdnURL != "https://instagram7.test/offload/ProbeCode/1.mp4?delivery=telegram-cdn-redirect-probe-v1" {
		t.Fatalf("CDN probe URL = %q, ok = %t", cdnURL, ok)
	}

	ogRequest := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/ProbeCode/?ig7probe=og-direct-v1", nil)
	ogRequest.Header.Set("User-Agent", "TelegramBot")
	ogURL, ok := telegramPreviewProbeVideoURL(ogRequest, "https://instagram7.test", "ProbeCode", 1, true)
	if !ok || ogURL != "https://d.oginstagram.example/p/ProbeCode/" {
		t.Fatalf("OG probe URL = %q, ok = %t", ogURL, ok)
	}

	nonTelegram := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/ProbeCode/?ig7probe=og-direct-v1", nil)
	nonTelegram.Header.Set("User-Agent", "Discordbot/2.0")
	if probeURL, ok := telegramPreviewProbeVideoURL(nonTelegram, "https://instagram7.test", "ProbeCode", 1, true); ok || probeURL != "" {
		t.Fatalf("non-Telegram probe URL = %q, ok = %t", probeURL, ok)
	}
}

func TestClientMediaFallbackOnlyAppliesToTelegramReels(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	for _, tc := range []struct {
		path string
		ua   string
	}{
		{path: "https://instagram7.test/reel/Code/", ua: "Discordbot/2.0"},
		{path: "https://instagram7.test/p/Code/", ua: "TelegramBot"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("User-Agent", tc.ua)
		if TryOGInstagramClientMediaFallback(httptest.NewRecorder(), req, "Code", 1, &model.ViewsData{}, nil, scraper.ErrNotFound) {
			t.Fatalf("unexpected client-media fallback for path=%s ua=%s", tc.path, tc.ua)
		}
	}
}

func TestOffloadClientMediaFallbackRedirectsWithoutServerFetch(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example/base")
	req := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DbClientFallback/1.mp4?delivery=og-client-fallback", "DbClientFallback", "1.mp4")
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()

	Offload(recorder, req)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if got := recorder.Header().Get("Location"); got != "https://d.oginstagram.example/base/p/DbClientFallback/" {
		t.Fatalf("Location = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestOffloadClientMediaFallbackSelectsCarouselIndex(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	req := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DbCarousel/3.mp4?delivery=og-client-fallback", "DbCarousel", "3.mp4")
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()

	Offload(recorder, req)

	if got := recorder.Header().Get("Location"); got != "https://d.oginstagram.example/p/DbCarousel/?img_index=3" {
		t.Fatalf("Location = %q", got)
	}
}

func TestInlineVideoOversizedUsesDecimalByteLimit(t *testing.T) {
	oldLimit := MaxInlineVideoBytes
	ConfigureMaxInlineVideoBytes(30_000_000)
	t.Cleanup(func() { ConfigureMaxInlineVideoBytes(oldLimit) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Length", "31434927")
		w.Header().Set("Content-Type", "video/mp4")
	}))
	defer server.Close()

	if !isInlineVideoOversized(server.URL + "/video.mp4") {
		t.Fatal("31,434,927-byte Telegram video must exceed the 30,000,000-byte preview limit")
	}
}
