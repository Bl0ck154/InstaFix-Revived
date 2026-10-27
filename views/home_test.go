package views

import (
	"strings"
	"testing"
)

func TestHomeConverterUsesSingleShortcodePathAndChatPreview(t *testing.T) {
	for _, check := range []string{
		`const canonicalKind = rawInput ? 'p' : normalizedKind;`,
		`aspect-ratio: 9 / 16;`,
		`object-fit: cover;`,
		`<strong>InstaFix Revived</strong>`,
		`aria-label="Live chat preview InstaFix Revived"`,
		`class="chat-presence-indicator"`,
		`aria-label="Available"`,
		`position: static;`,
		`id="mockup-time"`,
		`class="message-checks"`,
	} {
		if !strings.Contains(homeHTML, check) {
			t.Fatalf("homepage output missing %q", check)
		}
	}

	for _, forbidden := range []string{
		`publication-kind`,
		`Looks good. The publication type was detected from the URL.`,
		`If you paste only a shortcode or ID, choose its type:`,
		`Switch the type above if needed`,
		`Live chat preview / online`,
		`<div class="chat-day">Today</div>`,
		`overflow: auto;`,
	} {
		if strings.Contains(homeHTML, forbidden) {
			t.Fatalf("homepage unexpectedly contains removed type-selection UI %q", forbidden)
		}
	}
}

func TestHomeLiveDemoUsesRealConverterOncePerSession(t *testing.T) {
	for _, check := range []string{
		`const DEMO_REEL_URL = 'https://www.instagram.com/reels/Dbidjf_C4nf/';`,
		`const DEMO_REEL_ID = 'Dbidjf_C4nf';`,
		`const DEMO_VIDEO_URL = '/assets/demo/instagram7-test-reel.mp4';`,
		`const DEMO_POSTER_URL = '/assets/demo/instagram7-test-reel-poster.webp';`,
		`const DEMO_SESSION_KEY = 'instafix-revived.live-demo.seen.v2';`,
		`Live demo using our own Instagram Reel`,
		`Math.round(1200 + Math.random() * 600)`,
		`window.sessionStorage.getItem(DEMO_SESSION_KEY)`,
		`window.sessionStorage.setItem(DEMO_SESSION_KEY, '1')`,
		`'__INSTAFIX_BASE_URL__' + new URL(DEMO_REEL_URL).pathname`,
		`convertUrl({preserveInput: true})`,
		`renderOwnedDemoVideo()`,
		`fetch(apiUrl, {signal: requestController.signal})`,
		`new AbortController()`,
		`prefers-reduced-motion: reduce`,
		`inputEl.addEventListener(eventName, letUserTakeOver)`,
		`convertBtn.addEventListener(eventName, letUserTakeOver)`,
	} {
		if !strings.Contains(homeHTML, check) {
			t.Fatalf("homepage live demo missing %q", check)
		}
	}
	if strings.Contains(homeHTML, `const isVideo = preferVideo ||`) {
		t.Fatal("homepage must not treat every Reel URL as a playable video when the API reports image-only media")
	}
}

func TestHomeSectionOrderAndPrivacyFirst(t *testing.T) {
	ordered := []string{
		`<section class="hero"`,
		`<section class="converter-showcase" id="converter"`,
		`<section id="how-to-use"`,
		`<section class="features-section"`,
		`<section id="donate"`,
		`<section class="seo-section"`,
		`<footer>`,
	}
	lastIndex := -1
	for _, marker := range ordered {
		index := strings.Index(homeHTML, marker)
		if index < 0 {
			t.Fatalf("homepage section missing %q", marker)
		}
		if index <= lastIndex {
			t.Fatalf("homepage section %q is out of order", marker)
		}
		lastIndex = index
	}

	if !strings.Contains(homeHTML, `href="/how-instagram7-works"`) {
		t.Fatal("homepage is missing the demonstration page link")
	}
	if !strings.Contains(homeHTML, `Privacy-first`) || strings.Contains(homeHTML, `Absolute Privacy`) {
		t.Fatal("homepage privacy wording was not updated")
	}
}
