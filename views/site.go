package views

import (
	"net/url"
	"os"
	"strings"
)

const (
	defaultPublicBaseURL = "http://localhost:3000"
	siteBaseToken = "__INSTAFIX_BASE_URL__"
	siteHostToken = "__INSTAFIX_HOST__"
)

// ConfiguredPublicBaseURL returns a validated, explicit public origin.
// It must not accept a path, credentials, query, or fragment: values appear in
// HTML, JavaScript and public metadata. The request Host header is never used.
func ConfiguredPublicBaseURL() string {
	raw := strings.TrimSpace(os.Getenv("INSTAFIX_PUBLIC_BASE_URL"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" ||
		u.Fragment != "" || u.Opaque != "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// PublicBaseURL is used by the self-hosted web UI and SEO endpoints.
func PublicBaseURL() string {
	if configured := ConfiguredPublicBaseURL(); configured != "" {
		return configured
	}
	return defaultPublicBaseURL
}

func PublicHost() string {
	u, _ := url.Parse(PublicBaseURL())
	return u.Host
}

// renderSiteMarkup expands only trusted, app-owned markers. All domains are
// configurable from one setting, avoiding request Host header injection.
func renderSiteMarkup(s string) string {
	return strings.NewReplacer(
		siteBaseToken, PublicBaseURL(),
		siteHostToken, PublicHost(),
	).Replace(s)
}
