package scraper

import (
	"net/http"
	"strings"
	"testing"
)

func TestPublicInstagramProxyOnlyFailsClosed(t *testing.T) {
	t.Setenv("INSTAFIX_PUBLIC_PROXY_ONLY", "true")
	t.Setenv("INSTAFIX_PUBLIC_VIDEO_REFRESH_DIRECT", "true")
	publicProxyMu.Lock()
	original := publicProxyURLs
	publicProxyURLs = nil
	publicProxyMu.Unlock()
	t.Cleanup(func() {
		publicProxyMu.Lock()
		publicProxyURLs = original
		publicProxyMu.Unlock()
	})

	if got := graphQLClientAttempts(); len(got) != 0 {
		t.Fatalf("proxy-only without configured proxies created %d HTTP clients", len(got))
	}
	if publicVideoRefreshEnabled() {
		t.Fatal("direct public-video refresh must not be enabled without a proxy in proxy-only mode")
	}
	req, err := http.NewRequest(http.MethodGet, "https://www.instagram.com/p/ABCDE/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DoPublicInstagramRequest(req, "test"); err == nil || !strings.Contains(err.Error(), "proxy-only") {
		t.Fatalf("expected fail-closed policy, got %v", err)
	}
	if _, err := scrapeFromGQLMobileDirect("ABCDE"); err == nil || !strings.Contains(err.Error(), "proxy-only") {
		t.Fatalf("expected direct mobile GraphQL to be blocked, got %v", err)
	}
}

func TestPublicInstagramRequestRejectsUnexpectedHost(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://notinstagram.example.invalid/p/ABCDE/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DoPublicInstagramRequest(req, "test"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected unexpected host rejection, got %v", err)
	}
}
