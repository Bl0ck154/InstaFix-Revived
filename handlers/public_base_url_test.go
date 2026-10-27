package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfiguredPublicOriginOverridesRequestHost(t *testing.T) {
	t.Setenv("INSTAFIX_PUBLIC_BASE_URL", "https://previews.example.org")
	req := httptest.NewRequest(http.MethodGet, "http://internal.example:3000/p/ABCDE/", nil)
	req.Host = "untrusted-host.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := requestPublicBaseURL(req); got != "https://previews.example.org" {
		t.Fatalf("public base URL = %q", got)
	}
	if got := previewURLKind("https://previews.example.org/offload/ABCDE/1"); got != "offload" {
		t.Fatalf("configured offload URL incorrectly classified as %q", got)
	}
	if got := previewURLKind("https://external.example.org/offload/ABCDE/1"); got == "offload" {
		t.Fatalf("external offload URL must not be classified as same-site: %q", got)
	}
}
