package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	scraper "instafix/handlers/scraper"

	"github.com/go-chi/chi/v5"
)

func embedRequest(t *testing.T, target, postID, userAgent string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("User-Agent", userAgent)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("postID", postID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
}

func TestEmbedHonorsOGInstagramClientRedirectBeforeLocalScraping(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_REDIRECT_MODE", "bots_all")
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "off")
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", "https://oginstagram.example")

	recorder := httptest.NewRecorder()
	Embed(recorder, embedRequest(t, "https://instagram7.test/reel/DbRedirectFirst/", "DbRedirectFirst", "TelegramBot"))

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusFound)
	}
	if location := recorder.Header().Get("Location"); location != "https://oginstagram.example/reels/DbRedirectFirst/" {
		t.Fatalf("Location = %q", location)
	}
}

func TestEmbedUsesOGInstagramProxyBeforeLocalScraping(t *testing.T) {
	resetOGInstagramProxyBreakerForTest(t)
	t.Setenv("OGINSTAGRAM_CLIENT_REDIRECT_MODE", "off")
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "bots")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "")

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/reel/DbProxyFirst/" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><meta property="og:site_name" content="OGInstagram"><meta property="og:title" content="OGInstagram fixed preview"><meta property="og:video" content="https://oginstagram.com/offload/DbProxyFirst/1"><meta property="og:image" content="https://oginstagram.com/offload/DbProxyFirst/1?thumbnail=1"></head></html>`))
	}))
	defer upstream.Close()
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", upstream.URL)

	recorder := httptest.NewRecorder()
	Embed(recorder, embedRequest(t, "https://instagram7.test/reel/DbProxyFirst/", "DbProxyFirst", "TelegramBot"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if source := recorder.Header().Get("X-Instagram7-Preview-Source"); source != "fallback" {
		t.Fatalf("preview source = %q, want fallback", source)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `property="og:video"`) {
		t.Fatalf("proxied video metadata missing: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "oginstagram") {
		t.Fatalf("upstream branding was not sanitized: %s", body)
	}
}

func TestVideoDisplaySize(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
		wantW  int
		wantH  int
	}{
		{name: "oversized halved", width: 2160, height: 3840, wantW: 1080, wantH: 1920},
		{name: "instagram reel capped", width: 1216, height: 2160, wantW: 608, wantH: 1080},
		{name: "wide halved", width: 2560, height: 1440, wantW: 1280, wantH: 720},
		{name: "tiny doubled", width: 320, height: 320, wantW: 640, wantH: 640},
		{name: "telegram vertical reel unchanged", width: 720, height: 1280, wantW: 720, wantH: 1280},
		{name: "embed dimensions unchanged", width: 684, height: 1214, wantW: 684, wantH: 1214},
		{name: "tall not doubled when one axis large", width: 320, height: 640, wantW: 320, wantH: 640},
		{name: "unknown unchanged", width: 0, height: 0, wantW: 0, wantH: 0},
	}

	for _, tc := range cases {
		gotW, gotH := videoDisplaySize(tc.width, tc.height)
		if gotW != tc.wantW || gotH != tc.wantH {
			t.Fatalf("%s: videoDisplaySize(%d, %d) = %dx%d, want %dx%d", tc.name, tc.width, tc.height, gotW, gotH, tc.wantW, tc.wantH)
		}
	}
}

func TestTelegramAuthDimensionsProbeUsesAuthenticatedVideoDimensions(t *testing.T) {
	oldRefresh := previewProbeAuthRefresh
	previewProbeAuthRefresh = func(string) (*scraper.InstaData, error) {
		return &scraper.InstaData{Medias: []scraper.Media{{TypeName: "GraphVideo", Width: 720, Height: 1280}}}, nil
	}
	t.Cleanup(func() { previewProbeAuthRefresh = oldRefresh })

	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/ProbeCode/?ig7probe=auth-dimensions-v1", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	recorder := httptest.NewRecorder()
	fallback := scraper.Media{TypeName: "GraphVideo", Width: 684, Height: 1214}

	got := telegramAuthDimensionsProbeMedia(recorder, req, "ProbeCode", 1, fallback)
	if got.Width != 720 || got.Height != 1280 {
		t.Fatalf("probe dimensions = %dx%d, want 720x1280", got.Width, got.Height)
	}
	if recorder.Header().Get("X-Instagram7-Preview-Probe") != telegramAuthDimensionsProbe {
		t.Fatalf("probe response header missing: %q", recorder.Header().Get("X-Instagram7-Preview-Probe"))
	}
}

func TestInstagramOriginURLNormalizesReels(t *testing.T) {
	got := instagramOriginURL("/reels/DaJlro2MFT6/", "DaJlro2MFT6")
	want := "https://www.instagram.com/reel/DaJlro2MFT6/"
	if got != want {
		t.Fatalf("instagramOriginURL() = %q, want %q", got, want)
	}
}

func TestInstagramOriginURLPreservesPublicationTypes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/p/DaJlro2MFT6/", want: "https://www.instagram.com/p/DaJlro2MFT6/"},
		{path: "/creator/p/DaJlro2MFT6/4", want: "https://www.instagram.com/p/DaJlro2MFT6/"},
		{path: "/reel/DaJlro2MFT6/", want: "https://www.instagram.com/reel/DaJlro2MFT6/"},
		{path: "/tv/DaJlro2MFT6/", want: "https://www.instagram.com/tv/DaJlro2MFT6/"},
		{path: "/stories/creator/1234567890123456789/", want: "https://www.instagram.com/stories/creator/1234567890123456789/"},
	} {
		if got := instagramOriginURL(tc.path, "DaJlro2MFT6"); got != tc.want {
			t.Errorf("instagramOriginURL(%q)=%q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestEmbedDescriptionIncludesOnlyAvailableStats(t *testing.T) {
	item := &scraper.InstaData{
		Caption:         "caption",
		ViewCount:       1234,
		LikeCount:       0,
		HasViewCount:    true,
		HasLikeCount:    true,
		HasCommentCount: false,
	}
	got := embedDescription(item)
	want := "▶️ 1,234  ❤️ 0\n\ncaption"
	if got != want {
		t.Fatalf("embedDescription() = %q, want %q", got, want)
	}
}

func TestAuthFallbackOnlyReplacesUsableReelWhenItAddsVideo(t *testing.T) {
	public := &scraper.InstaData{
		Username:     "public",
		Medias:       []scraper.Media{{TypeName: "GraphImage"}, {TypeName: "GraphImage"}},
		LikeCount:    93,
		HasLikeCount: true,
	}
	authImage := &scraper.InstaData{
		Username: "auth",
		Medias:   []scraper.Media{{TypeName: "GraphImage"}},
	}
	authVideo := &scraper.InstaData{
		Username: "auth",
		Medias:   []scraper.Media{{TypeName: "GraphVideo"}},
	}

	if shouldUseAuthFallbackItem(public, authImage, true) {
		t.Fatal("image-only auth result replaced richer public Reel metadata")
	}
	if !shouldUseAuthFallbackItem(public, authVideo, true) {
		t.Fatal("auth video did not replace public image fallback")
	}
	if !shouldUseAuthFallbackItem(nil, authImage, true) {
		t.Fatal("auth image should fill an otherwise missing preview")
	}
}

func TestPreviewVideoRouteVersionsConfiguredCDNRedirect(t *testing.T) {
	oldEnabled := PreviewVideoCDNRedirectEnabled
	oldAgents := PreviewVideoCDNRedirectUserAgents
	ConfigurePreviewVideoCDNRedirect(true, "telegrambot")
	t.Cleanup(func() {
		PreviewVideoCDNRedirectEnabled = oldEnabled
		PreviewVideoCDNRedirectUserAgents = oldAgents
	})

	got := previewVideoRoute("https://fix.example", "DbLarge1", 1, "TelegramBot (like TwitterBot)")
	want := "https://fix.example/offload/DbLarge1/1.mp4?delivery=cloudflare-cdn-v10"
	if got != want {
		t.Fatalf("redirect route = %q, want %q", got, want)
	}
	if got := previewVideoRoute("https://fix.example", "DbLarge1", 1, "Discordbot/2.0"); got != "https://fix.example/offload/DbLarge1/1.mp4" {
		t.Fatalf("non-configured route = %q", got)
	}
}

func TestPreviewVideoRouteVersionsTelegramDirectStream(t *testing.T) {
	oldEnabled := PreviewVideoCDNRedirectEnabled
	oldAgents := PreviewVideoCDNRedirectUserAgents
	ConfigurePreviewVideoCDNRedirect(false, "telegrambot")
	t.Cleanup(func() {
		PreviewVideoCDNRedirectEnabled = oldEnabled
		PreviewVideoCDNRedirectUserAgents = oldAgents
	})

	got := previewVideoRoute("https://fix.example", "DbLarge1", 1, "TelegramBot (like TwitterBot)")
	want := "https://fix.example/offload/DbLarge1/1.mp4"
	if got != want {
		t.Fatalf("direct route = %q, want %q", got, want)
	}
}
