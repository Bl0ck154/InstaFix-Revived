package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestGuidesHaveUniqueCanonicalAndSearchMetadata(t *testing.T) {
	for slug, page := range guidePages {
		t.Run(slug, func(t *testing.T) {
			var out bytes.Buffer
			if !Guide(slug, &out) {
				t.Fatal("Guide returned false")
			}
			html := out.String()
			checks := []string{
				`<title>` + page.Title + ` | InstaFix Revived</title>`,
				`rel="canonical" href="http://localhost:3000/guides/` + slug + `"`,
				`<meta name="description" content="` + page.Description + `"`,
				`"@type": "TechArticle"`,
				`href="/"`,
			}
			for _, check := range checks {
				if !strings.Contains(html, check) {
					t.Fatalf("guide output missing %q", check)
				}
			}
		})
	}
}

func TestUnknownGuideIsNotRendered(t *testing.T) {
	var out bytes.Buffer
	if Guide("missing", &out) {
		t.Fatal("unknown guide unexpectedly rendered")
	}
	if out.Len() != 0 {
		t.Fatalf("unknown guide wrote %d bytes", out.Len())
	}
}

func TestGuideIndexLinksToAllGuides(t *testing.T) {
	var out bytes.Buffer
	if !GuideIndex(&out) {
		t.Fatal("GuideIndex returned false")
	}
	html := out.String()
	for _, check := range []string{
		`<title>` + guideIndexPage.Title + ` | InstaFix Revived</title>`,
		`rel="canonical" href="http://localhost:3000/guides"`,
		`"@type": "CollectionPage"`,
		`class="guide-card-grid"`,
		`--bg-main: #fbf9f6`,
		`class="brand" href="/">InstaFix Revived</a>`,
		`href="/guides/instagram-link-preview-fixer"`,
		`href="/guides/instagram-reels-preview"`,
		`href="/guides/telegram-instagram-preview"`,
		`href="/guides/discord-instagram-embed"`,
	} {
		if !strings.Contains(html, check) {
			t.Fatalf("guide index output missing %q", check)
		}
	}
}

func TestDiscordGuideIncludesWorkingGenerator(t *testing.T) {
	var out bytes.Buffer
	if !Guide("discord-instagram-embed", &out) {
		t.Fatal("Discord guide returned false")
	}
	html := out.String()
	for _, check := range []string{
		`id="discord-generator"`,
		`Generate Discord link`,
		`http://localhost:3000/' + publication.kind`,
		`host !== 'instagram.com' && host !== 'localhost:3000'`,
		`if (shortcodePattern.test(value)) return { kind: 'p', id: value };`,
		`class="tool-showcase"`,
	} {
		if !strings.Contains(html, check) {
			t.Fatalf("Discord guide output missing generator behavior %q", check)
		}
	}
	for _, forbidden := range []string{
		`Shortcode type`,
		`name="generator-kind"`,
		`Full URLs choose Post or Reel`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("Discord guide unexpectedly contains removed type selector %q", forbidden)
		}
	}
}
