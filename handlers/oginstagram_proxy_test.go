package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func resetOGInstagramProxyBreakerForTest(t *testing.T) {
	t.Helper()
	t.Setenv("OGINSTAGRAM_SERVER_FETCH_ALLOWED", "true")
	ogInstagramProxyBreaker.mu.Lock()
	ogInstagramProxyBreaker.consecutiveFailures = 0
	ogInstagramProxyBreaker.openedAt = time.Time{}
	ogInstagramProxyBreaker.probeInFlight = false
	ogInstagramProxyBreaker.mu.Unlock()
	t.Cleanup(func() {
		ogInstagramProxyBreaker.mu.Lock()
		ogInstagramProxyBreaker.consecutiveFailures = 0
		ogInstagramProxyBreaker.openedAt = time.Time{}
		ogInstagramProxyBreaker.probeInFlight = false
		ogInstagramProxyBreaker.mu.Unlock()
	})
}

func TestOGInstagramAutomaticProxyRequiresServerFetchAcknowledgement(t *testing.T) {
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "all")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "")
	t.Setenv("OGINSTAGRAM_SERVER_FETCH_ALLOWED", "")

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<meta property="og:video" content="https://example.test/video.mp4">`))
	}))
	defer upstream.Close()
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/NoServerFetch/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	if TryOGInstagramEmbedProxy(httptest.NewRecorder(), req, "NoServerFetch") {
		t.Fatal("automatic proxy must be disabled without the explicit server-fetch acknowledgement")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("production server contacted upstream without acknowledgement: calls = %d", got)
	}
}

func TestRewriteOGInstagramHTML(t *testing.T) {
	body := `<head><title>Totally Different Proxy</title><meta property="og:site_name" content="Totally Different Proxy"><meta content="Different Embed Service" property="og:title"><meta name='twitter:title' content='Unrelated Proxy Title'><meta property="og:description" content="Powered by a foreign proxy"><meta property="og:image:alt" content="Real Reel caption &amp; details"><meta name="author" content="og-instagram"><meta property="og:video" content="https://oginstagram.com/offload/CODE/1"><meta property="og:image" content="https://oginstagram.com/offload/CODE/1?thumbnail=1"></head>`

	got := rewriteOGInstagramHTML(body, "https://oginstagram.com", "https://www.instagram7.com")

	lower := strings.ToLower(got)
	if strings.Contains(lower, "oginstagram") || strings.Contains(lower, "og instagram") || strings.Contains(lower, "og_instagram") || strings.Contains(lower, "og-instagram") ||
		strings.Contains(lower, "totally different proxy") || strings.Contains(lower, "different embed service") || strings.Contains(lower, "unrelated proxy title") {
		t.Fatalf("expected upstream branding to be rewritten: %s", got)
	}
	for _, want := range []string{
		`<title>Instagram preview</title>`,
		`content="InstaFix Revived"`,
		`content="Instagram preview"`,
		`content='Instagram preview'`,
		`content="Real Reel caption &amp; details"`,
		`name="twitter:description" content="Real Reel caption &amp; details"`,
		`https://www.instagram7.com/offload/CODE/1`,
		`https://www.instagram7.com/offload/CODE/1?thumbnail=1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %s", want, got)
		}
	}
}

func TestRewriteOGInstagramHTMLPrefersTrustedInstagramText(t *testing.T) {
	body := `<head><meta property="og:site_name" content="Any Proxy"><meta property="og:title" content="Proxy headline"><meta name="twitter:creator" content="@fallback_user"><meta property="og:description" content="Visit proxy.example for better embeds"><meta property="og:image:alt" content="Fallback caption"></head>`
	trusted := fallbackPreviewText{
		title:       "real_creator",
		description: "Real caption costs $1 & stays creator-owned.",
	}

	got := rewriteOGInstagramHTML(body, "https://proxy.example", "https://www.instagram7.com", trusted)

	for _, want := range []string{
		`content="@real_creator"`,
		`content="Real caption costs $1 &amp; stays creator-owned."`,
		`<title>@real_creator</title>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %s", want, got)
		}
	}
	for _, forbidden := range []string{"Any Proxy", "Proxy headline", "Visit proxy.example", "Fallback caption"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unexpected fallback text %q in %s", forbidden, got)
		}
	}
}

func TestRewriteOGInstagramHTMLRejectsPromotionalFallbackCaption(t *testing.T) {
	body := `<head><meta property="og:title" content="Other service"><meta name="twitter:creator" content="@real_creator"><meta property="og:image:alt" content="Powered by AmazingProxy — visit www.example.com"></head>`

	got := rewriteOGInstagramHTML(body, "https://proxy.example", "https://www.instagram7.com")

	if strings.Contains(got, "AmazingProxy") || strings.Contains(got, "www.example.com") {
		t.Fatalf("promotional fallback caption leaked: %s", got)
	}
	if !strings.Contains(got, `content="Public Instagram publication preview. Open the original post on Instagram for the full caption."`) {
		t.Fatalf("neutral fallback description missing: %s", got)
	}
}

func TestAddOGProxyTokenToOffloadURLs(t *testing.T) {
	body := `<meta property="og:video" content="https://www.instagram7.com/offload/CODE/1"><meta property="og:image" content="https://www.instagram7.com/offload/CODE/1?thumbnail=1">`

	got := addOGProxyTokenToOffloadURLs(body, "https://www.instagram7.com", "secret token")

	for _, want := range []string{
		`https://www.instagram7.com/offload/CODE/1?ogproxy=secret+token`,
		`https://www.instagram7.com/offload/CODE/1?thumbnail=1&amp;ogproxy=secret+token`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %s", want, got)
		}
	}
}

func TestRestoreOGInstagramOffloadURLs(t *testing.T) {
	body := `<meta property="og:video" content="https://www.instagram7.com/offload/CODE/1"><meta property="og:image" content="https://www.instagram7.com/offload/CODE/1?thumbnail=1"><link href="https://www.instagram7.com/favicon-64.png" rel="icon">`

	got := restoreOGInstagramOffloadURLs(body, "https://oginstagram.com", "https://www.instagram7.com")

	for _, want := range []string{
		`https://oginstagram.com/offload/CODE/1`,
		`https://oginstagram.com/offload/CODE/1?thumbnail=1`,
		`https://oginstagram.com/favicon-64.png`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %s", want, got)
		}
	}
}

func TestUsableOGInstagramEmbedResponseRejectsUnavailable(t *testing.T) {
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/html"}}}
	body := []byte(`<html><meta property="og:title" content="Temporarily unavailable"></html>`)

	if usableOGInstagramEmbedResponse(res, body) {
		t.Fatal("expected temporary fallback HTML to be rejected")
	}
}

func TestUsableOGInstagramEmbedResponseAcceptsVideoHTML(t *testing.T) {
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/html"}}}
	body := []byte(`<html><meta property="og:title" content="Post"><meta property="og:video" content="https://oginstagram.com/offload/CODE/1"></html>`)

	if !usableOGInstagramEmbedResponse(res, body) {
		t.Fatal("expected video HTML to be accepted")
	}
}

func TestOGInstagramMediaRewriteDefaultsOn(t *testing.T) {
	t.Setenv("OGINSTAGRAM_PROXY_REWRITE_MEDIA", "")
	if cfg := loadOGInstagramProxyConfig(); !cfg.rewriteMedia {
		t.Fatal("OGInstagram media URLs must default to the local streaming endpoint")
	}
}

func TestOGInstagramProxyConfigBoundsCircuitSettings(t *testing.T) {
	t.Setenv("OGINSTAGRAM_PROXY_FAILURE_THRESHOLD", "9999")
	t.Setenv("OGINSTAGRAM_PROXY_COOLDOWN_SECONDS", "999999999")
	cfg := loadOGInstagramProxyConfig()
	if cfg.failureThreshold != maxOGInstagramFailureThreshold {
		t.Fatalf("failure threshold = %d, want %d", cfg.failureThreshold, maxOGInstagramFailureThreshold)
	}
	if cfg.cooldown != maxOGInstagramCooldown {
		t.Fatalf("cooldown = %s, want %s", cfg.cooldown, maxOGInstagramCooldown)
	}

	t.Setenv("OGINSTAGRAM_PROXY_FAILURE_THRESHOLD", "0")
	t.Setenv("OGINSTAGRAM_PROXY_COOLDOWN_SECONDS", "0")
	cfg = loadOGInstagramProxyConfig()
	if cfg.failureThreshold != minOGInstagramFailureThreshold {
		t.Fatalf("minimum failure threshold = %d, want %d", cfg.failureThreshold, minOGInstagramFailureThreshold)
	}
	if cfg.cooldown != minOGInstagramCooldown {
		t.Fatalf("minimum cooldown = %s, want %s", cfg.cooldown, minOGInstagramCooldown)
	}
}

func TestOGInstagramProxyOpensAfterRepeatedCloudflareFailures(t *testing.T) {
	resetOGInstagramProxyBreakerForTest(t)
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "all")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "")
	t.Setenv("OGINSTAGRAM_PROXY_FAILURE_THRESHOLD", "2")
	t.Setenv("OGINSTAGRAM_PROXY_COOLDOWN_SECONDS", "300")

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("cf-mitigated challenge"))
	}))
	defer upstream.Close()
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", upstream.URL)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		if TryOGInstagramEmbedProxy(rec, httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbBreaker/", nil), "DbBreaker") {
			t.Fatal("Cloudflare response must fall back to the local renderer")
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls after threshold = %d, want 2", got)
	}

	if TryOGInstagramEmbedProxy(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbBreaker/", nil), "DbBreaker") {
		t.Fatal("open circuit must fall back without serving upstream HTML")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("open circuit retried upstream: calls = %d, want 2", got)
	}
}

func TestOGInstagramProxyHalfOpenAllowsOnlyOneProbe(t *testing.T) {
	resetOGInstagramProxyBreakerForTest(t)
	cfg := ogInstagramProxyConfig{failureThreshold: 2, cooldown: time.Second}
	ogInstagramProxyBreaker.mu.Lock()
	ogInstagramProxyBreaker.openedAt = time.Now().Add(-2 * cfg.cooldown)
	ogInstagramProxyBreaker.mu.Unlock()

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := allowOGInstagramProxy("mode:all", cfg); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 1 {
		t.Fatalf("half-open probes = %d, want 1", got)
	}
	recordOGInstagramProxySuccess("mode:all", cfg)
}

func TestOGInstagramProxyTokenOverrideBypassesOpenCircuit(t *testing.T) {
	resetOGInstagramProxyBreakerForTest(t)
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "all")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "manual-secret")
	t.Setenv("OGINSTAGRAM_PROXY_FAILURE_THRESHOLD", "1")
	t.Setenv("OGINSTAGRAM_PROXY_COOLDOWN_SECONDS", "300")

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", upstream.URL)

	ogInstagramProxyBreaker.mu.Lock()
	ogInstagramProxyBreaker.consecutiveFailures = 1
	ogInstagramProxyBreaker.openedAt = time.Now()
	ogInstagramProxyBreaker.mu.Unlock()

	manual := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbManual/?ogproxy=manual-secret", nil)
	if TryOGInstagramEmbedProxy(httptest.NewRecorder(), manual, "DbManual") {
		t.Fatal("manual Cloudflare response must still fall back")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("manual override calls = %d, want 1", got)
	}

	automatic := httptest.NewRequest(http.MethodGet, "https://instagram7.test/reel/DbManual/", nil)
	TryOGInstagramEmbedProxy(httptest.NewRecorder(), automatic, "DbManual")
	if got := calls.Load(); got != 1 {
		t.Fatalf("automatic request bypassed open circuit after manual override: calls = %d", got)
	}
}

func TestOGInstagramOffloadDoesNotRetryCloudflareHEAD(t *testing.T) {
	resetOGInstagramProxyBreakerForTest(t)
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "all")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "")
	t.Setenv("OGINSTAGRAM_PROXY_FAILURE_THRESHOLD", "1")
	t.Setenv("OGINSTAGRAM_PROXY_COOLDOWN_SECONDS", "300")

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodHead {
			t.Errorf("unexpected retry method = %s", r.Method)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", upstream.URL)

	req := offloadRequest(t, http.MethodHead, "https://instagram7.test/offload/DbHead403/1?v=2&kid=2026-07&exp=1786533186&sig=test-signature", "DbHead403", "1")
	if TryOGInstagramOffloadProxy(httptest.NewRecorder(), req) {
		t.Fatal("Cloudflare HEAD response must fall back")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Cloudflare HEAD caused a retry: calls = %d, want 1", got)
	}
}

func TestOGInstagramOffloadSkipsUnsignedLocalMedia(t *testing.T) {
	resetOGInstagramProxyBreakerForTest(t)
	t.Setenv("OGINSTAGRAM_PROXY_MODE", "all")
	t.Setenv("OGINSTAGRAM_PROXY_TOKEN", "")

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	t.Setenv("OGINSTAGRAM_PROXY_UPSTREAM", upstream.URL)

	req := offloadRequest(t, http.MethodHead, "https://instagram7.test/offload/DbLocal1/1.mp4", "DbLocal1", "1")
	if TryOGInstagramOffloadProxy(httptest.NewRecorder(), req) {
		t.Fatal("unsigned local media must not be served by the fallback proxy")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("unsigned local media reached fallback upstream: calls = %d", got)
	}
}
