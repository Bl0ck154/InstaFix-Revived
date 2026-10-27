package views

import (
	"strings"
	"testing"
)

func TestPublicBaseURLAndHost(t *testing.T) {
	t.Setenv("INSTAFIX_PUBLIC_BASE_URL", "https://preview.example.org:8443/")
	if got := PublicBaseURL(); got != "https://preview.example.org:8443" {
		t.Fatalf("public base URL = %q", got)
	}
	if got := PublicHost(); got != "preview.example.org:8443" {
		t.Fatalf("public host = %q", got)
	}
	if got := renderSiteMarkup(siteBaseToken + "/p/ABCDE/ " + siteHostToken); got != "https://preview.example.org:8443/p/ABCDE/ preview.example.org:8443" {
		t.Fatalf("render site markup = %q", got)
	}
}

func TestPublicBaseURLInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{"https://example.org/path", "https://user:pass@example.org", "https://example.org/?x=1", "javascript:alert(1)"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("INSTAFIX_PUBLIC_BASE_URL", raw)
			if got := PublicBaseURL(); got != defaultPublicBaseURL {
				t.Fatalf("unsafe base URL %q accepted as %q", raw, got)
			}
		})
	}
}

func TestHomeUsesConfiguredSiteURL(t *testing.T) {
	t.Setenv("INSTAFIX_PUBLIC_BASE_URL", "https://preview.example.org")
	var out strings.Builder
	Home(&out)
	if !strings.Contains(out.String(), "https://preview.example.org/p/") {
		t.Fatal("missing configured conversion base URL")
	}
	if strings.Contains(out.String(), siteBaseToken) || strings.Contains(out.String(), siteHostToken) {
		t.Fatal("unresolved site URL placeholder")
	}
	if strings.Contains(out.String(), "instagram7.com") {
		t.Fatal("old deployment hostname leaked into rendered homepage")
	}
}
