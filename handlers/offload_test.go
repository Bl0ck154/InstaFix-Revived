package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scraper "instafix/handlers/scraper"
	"instafix/observability"

	"github.com/go-chi/chi/v5"
)

func offloadRequest(t *testing.T, method, target, postID, mediaNum string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("postID", postID)
	route.URLParams.Add("mediaNum", mediaNum)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
}

func stubOffloadData(t *testing.T, item *scraper.InstaData, refresh func(string) (*scraper.InstaData, error)) {
	t.Helper()
	oldGetData := offloadGetData
	oldGetDataQuiet := offloadGetDataQuiet
	oldGet := offloadGetDataPreferVideo
	oldQuiet := offloadGetDataPreferVideoQuiet
	oldRefresh := offloadRefreshDataPreferVideo
	oldClient := offloadVideoClient
	oldAllowed := offloadMediaURLAllowed
	offloadGetData = func(string) (*scraper.InstaData, error) { return item, nil }
	offloadGetDataQuiet = func(string) (*scraper.InstaData, error) { return item, nil }
	offloadGetDataPreferVideo = func(string) (*scraper.InstaData, error) { return item, nil }
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) { return item, nil }
	offloadRefreshDataPreferVideo = refresh
	offloadVideoClient = &http.Client{}
	offloadMediaURLAllowed = func(string) bool { return true }
	t.Cleanup(func() {
		offloadGetData = oldGetData
		offloadGetDataQuiet = oldGetDataQuiet
		offloadGetDataPreferVideo = oldGet
		offloadGetDataPreferVideoQuiet = oldQuiet
		offloadRefreshDataPreferVideo = oldRefresh
		offloadVideoClient = oldClient
		offloadMediaURLAllowed = oldAllowed
	})
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "")
}

func TestOffloadVideoRangeReturnsDirect206(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-3" {
			t.Fatalf("upstream Range = %q", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123")
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DaRange1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called")
		return nil, nil
	})

	req := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DaRange1/1", "DaRange1", "1")
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	Offload(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Fatalf("video stream must not redirect, Location = %q", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-3/10" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := rec.Body.String(); got != "0123" {
		t.Fatalf("body = %q", got)
	}
}

func TestOffloadVideoCanOutliveServerWriteTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "8")
		_, _ = io.WriteString(w, "0123")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(120 * time.Millisecond)
		_, _ = io.WriteString(w, "4567")
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DaSlow01", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called")
		return nil, nil
	})

	router := chi.NewRouter()
	router.Use(observability.Default.Middleware)
	router.Get("/offload/{postID}/{mediaNum}", Offload)
	server := httptest.NewUnstartedServer(router)
	server.Config.WriteTimeout = 50 * time.Millisecond
	server.Start()
	defer server.Close()

	res, err := server.Client().Get(server.URL + "/offload/DaSlow01/1")
	if err != nil {
		t.Fatalf("GET slow video: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read slow video: %v", err)
	}
	if res.StatusCode != http.StatusOK || string(body) != "01234567" {
		t.Fatalf("status = %d, body = %q", res.StatusCode, body)
	}
}

func TestOffloadVideoHEADFallsBackToRangeProbe(t *testing.T) {
	var headCalls atomic.Int32
	var getCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCalls.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		getCalls.Add(1)
		if got := r.Header.Get("Range"); got != "bytes=0-0" {
			t.Fatalf("probe Range = %q", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 0-0/100")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DaHead01", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called")
		return nil, nil
	})

	rec := httptest.NewRecorder()
	Offload(rec, offloadRequest(t, http.MethodHead, "https://instagram7.test/offload/DaHead01/1", "DaHead01", "1"))

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD returned %d body bytes", rec.Body.Len())
	}
	if headCalls.Load() != 1 || getCalls.Load() != 1 {
		t.Fatalf("calls: HEAD=%d GET=%d", headCalls.Load(), getCalls.Load())
	}
}

func TestOffloadVideoRefreshesRejectedSignedURL(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer stale.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-2" {
			t.Fatalf("fresh Range = %q", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "new")
	}))
	defer fresh.Close()

	staleItem := &scraper.InstaData{PostID: "DaStale1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: stale.URL + "/expired.mp4"}}}
	freshItem := &scraper.InstaData{PostID: "DaStale1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: fresh.URL + "/fresh.mp4"}}}
	var refreshCalls atomic.Int32
	stubOffloadData(t, staleItem, func(string) (*scraper.InstaData, error) {
		refreshCalls.Add(1)
		return freshItem, nil
	})

	req := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DaStale1/1", "DaStale1", "1")
	req.Header.Set("Range", "bytes=0-2")
	req.Header.Set("User-Agent", "TelegramBot")
	rec := httptest.NewRecorder()
	Offload(rec, req)

	if rec.Code != http.StatusPartialContent || rec.Body.String() != "new" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d", refreshCalls.Load())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("refreshed video response must not redirect")
	}
}

func TestOffloadImageStillRedirects(t *testing.T) {
	item := &scraper.InstaData{PostID: "DaImage1", Medias: []scraper.Media{{TypeName: "GraphImage", URL: "https://scontent.cdninstagram.com/image.jpg"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) { return nil, nil })

	rec := httptest.NewRecorder()
	Offload(rec, offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DaImage1/1", "DaImage1", "1"))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.Contains(got, "image.jpg") {
		t.Fatalf("Location = %q", got)
	}
}

func TestTelegramMinimalStreamReturnsSameOrigin200WithMinimalHeaders(t *testing.T) {
	var upstreamRange atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"upstream-etag"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "video")
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DbMinimal1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called")
		return nil, nil
	})

	req := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DbMinimal1/1.mp4?delivery=telegram-edge-stream-v1", "DbMinimal1", "1.mp4")
	req.Header.Set("User-Agent", "TelegramBot")
	req.Header.Set("Range", "bytes=0-1023")
	rec := httptest.NewRecorder()
	Offload(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "video" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := upstreamRange.Load(); got != "" {
		t.Fatalf("upstream Range = %q, want empty", got)
	}
	for _, header := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if got := rec.Header().Get(header); got != "" {
			t.Fatalf("%s = %q, want omitted", header, got)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "cross-origin" {
		t.Fatalf("Cross-Origin-Resource-Policy = %q", got)
	}
	if got := rec.Header().Get("X-Instagram7-Video-Stream"); got != "telegram-minimal" {
		t.Fatalf("stream mode = %q", got)
	}
}

func TestLegacyVideosRouteAlsoStreamsWithoutRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "video")
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DaLegacy", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called")
		return nil, nil
	})

	rec := httptest.NewRecorder()
	Videos(rec, offloadRequest(t, http.MethodGet, "https://instagram7.test/videos/DaLegacy/1", "DaLegacy", "1"))

	if rec.Code != http.StatusOK || rec.Body.String() != "video" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("legacy video route must not redirect")
	}
}

func TestGetOffloadDataUsesAuthOnlyForPreviewClients(t *testing.T) {
	oldGet := offloadGetDataPreferVideo
	oldQuiet := offloadGetDataPreferVideoQuiet
	var authCalls atomic.Int32
	var publicCalls atomic.Int32
	item := &scraper.InstaData{PostID: "DaGate01", Medias: []scraper.Media{{TypeName: "GraphVideo"}}}
	offloadGetDataPreferVideo = func(string) (*scraper.InstaData, error) {
		authCalls.Add(1)
		return item, nil
	}
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		publicCalls.Add(1)
		return item, nil
	}
	t.Cleanup(func() {
		offloadGetDataPreferVideo = oldGet
		offloadGetDataPreferVideoQuiet = oldQuiet
	})

	generic := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DaGate01/1.mp4", "DaGate01", "1.mp4")
	generic.Header.Set("User-Agent", "python-requests/2.32")
	if _, err := getOffloadData(generic, "DaGate01"); err != nil {
		t.Fatal(err)
	}

	preview := offloadRequest(t, http.MethodGet, "https://instagram7.test/offload/DaGate01/1.mp4", "DaGate01", "1.mp4")
	preview.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	if _, err := getOffloadData(preview, "DaGate01"); err != nil {
		t.Fatal(err)
	}

	if authCalls.Load() != 1 || publicCalls.Load() != 1 {
		t.Fatalf("auth calls = %d, public calls = %d", authCalls.Load(), publicCalls.Load())
	}
}

func TestPreviewMediaBotRejectsGenericScanners(t *testing.T) {
	for _, userAgent := range []string{"python-requests/2.32", "curl/8.13.0", "Mozilla/5.0"} {
		if isPreviewMediaBot(userAgent) {
			t.Fatalf("generic client %q must not receive authenticated fallback", userAgent)
		}
	}
	for _, userAgent := range []string{"TelegramBot", "Discordbot/2.0", "Slackbot-LinkExpanding", "Mastodon/4.0", "vkShare"} {
		if !isPreviewMediaBot(userAgent) {
			t.Fatalf("preview client %q should receive authenticated fallback", userAgent)
		}
	}
}

func TestOffloadRedirectsConfiguredPreviewBotToValidatedCDNURL(t *testing.T) {
	var probeMethod atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeMethod.Store(r.Method)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "31434927")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DbLarge1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called for a valid CDN URL")
		return nil, nil
	})
	oldEnabled := PreviewVideoCDNRedirectEnabled
	oldAgents := PreviewVideoCDNRedirectUserAgents
	ConfigurePreviewVideoCDNRedirect(true, "telegrambot")
	t.Cleanup(func() {
		PreviewVideoCDNRedirectEnabled = oldEnabled
		PreviewVideoCDNRedirectUserAgents = oldAgents
	})

	req := offloadRequest(t, http.MethodGet, "https://fix.example/offload/DbLarge1/1.mp4", "DbLarge1", "1.mp4")
	req.Header.Set("User-Agent", "TelegramBot (like TwitterBot)")
	rec := httptest.NewRecorder()
	Offload(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != upstream.URL+"/video.mp4" {
		t.Fatalf("Location = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-InstaFix-Video-Delivery"); got != "cdn-redirect" {
		t.Fatalf("delivery header = %q", got)
	}
	if got := probeMethod.Load(); got != http.MethodHead {
		t.Fatalf("probe method = %v", got)
	}
}

func TestOffloadCDNProbeRedirectsEvenWhenGlobalRedirectIsDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	item := &scraper.InstaData{PostID: "DbProbe1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("refresh should not be called for a valid probe URL")
		return nil, nil
	})
	oldEnabled := PreviewVideoCDNRedirectEnabled
	ConfigurePreviewVideoCDNRedirect(false, "telegrambot")
	t.Cleanup(func() { PreviewVideoCDNRedirectEnabled = oldEnabled })

	req := offloadRequest(t, http.MethodGet, "https://fix.example/offload/DbProbe1/1.mp4?delivery=telegram-cdn-redirect-probe-v1", "DbProbe1", "1.mp4")
	req.Header.Set("User-Agent", "TelegramBot (like TwitterBot)")
	rec := httptest.NewRecorder()
	Offload(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != upstream.URL+"/video.mp4" {
		t.Fatalf("probe redirect status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestOffloadRedirectRefreshesRejectedCDNURL(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer stale.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
	}))
	defer fresh.Close()

	item := &scraper.InstaData{PostID: "DbStale2", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: stale.URL + "/video.mp4"}}}
	refreshed := &scraper.InstaData{PostID: "DbStale2", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: fresh.URL + "/video.mp4"}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		return refreshed, nil
	})
	oldEnabled := PreviewVideoCDNRedirectEnabled
	oldAgents := PreviewVideoCDNRedirectUserAgents
	ConfigurePreviewVideoCDNRedirect(true, "telegrambot")
	t.Cleanup(func() {
		PreviewVideoCDNRedirectEnabled = oldEnabled
		PreviewVideoCDNRedirectUserAgents = oldAgents
	})

	req := offloadRequest(t, http.MethodGet, "https://fix.example/offload/DbStale2/1", "DbStale2", "1")
	req.Header.Set("User-Agent", "TelegramBot (like TwitterBot)")
	rec := httptest.NewRecorder()
	Offload(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != fresh.URL+"/video.mp4" {
		t.Fatalf("Location = %q", got)
	}
}
