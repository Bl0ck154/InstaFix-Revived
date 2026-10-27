package views

import (
	"encoding/json"
	"html/template"
	"io"
	"strings"
)

type demoVideo struct {
	Name          string
	Description   string
	PosterURL     string
	MP4URL        string
	WebMURL       string
	CaptionsURL   string
	TranscriptURL string
	Transcript    string
	UploadDate    string
	Duration      string
}

func (v demoVideo) Ready() bool {
	return strings.TrimSpace(v.Name) != "" &&
		strings.TrimSpace(v.Description) != "" &&
		strings.TrimSpace(v.PosterURL) != "" &&
		strings.TrimSpace(v.UploadDate) != "" &&
		(strings.TrimSpace(v.MP4URL) != "" || strings.TrimSpace(v.WebMURL) != "")
}

func (v demoVideo) ContentURL() string {
	if strings.TrimSpace(v.MP4URL) != "" {
		return v.MP4URL
	}
	return v.WebMURL
}

type howInstaFix RevivedWorksPage struct {
	Video          demoVideo
	StructuredData template.JS
}

func howInstaFix RevivedWorksStructuredData(video demoVideo) template.JS {
	graph := []any{
		map[string]any{
			"@type":        "WebPage",
			"@id":          "__INSTAFIX_BASE_URL__/how-instagram7-works#page",
			"url":          "__INSTAFIX_BASE_URL__/how-instagram7-works",
			"name":         "How InstaFix Revived Fixes Instagram Reel Previews",
			"description":  "See a live Instagram Reel example and the three-step InstaFix Revived workflow.",
			"inLanguage":   "en",
			"dateModified": "2026-08-08",
		},
		map[string]any{
			"@type":       "HowTo",
			"@id":         "__INSTAFIX_BASE_URL__/how-instagram7-works#steps",
			"name":        "How to fix an Instagram Reel preview with InstaFix Revived",
			"description": "Copy a public Instagram Reel URL, convert it on your instance, and send the converted link in a chat.",
			"totalTime":   "PT1M",
			"step": []any{
				map[string]any{"@type": "HowToStep", "position": 1, "name": "Copy the public Reel link", "text": "Open the public Reel in Instagram and copy its share URL."},
				map[string]any{"@type": "HowToStep", "position": 2, "name": "Convert the Instagram URL", "text": "Paste the original URL into your instance's converter."},
				map[string]any{"@type": "HowToStep", "position": 3, "name": "Send the converted link", "text": "Paste the InstaFix Revived link into a new Telegram or Discord message and let the client load its preview."},
			},
		},
		map[string]any{
			"@type": "FAQPage",
			"@id":   "__INSTAFIX_BASE_URL__/how-instagram7-works#faq",
			"mainEntity": []any{
				map[string]any{"@type": "Question", "name": "Does InstaFix Revived download or re-upload the Reel?", "acceptedAnswer": map[string]any{"@type": "Answer", "text": "InstaFix Revived prepares preview metadata for a public publication. Media delivery depends on what Instagram currently makes available and what the receiving chat client supports."}},
				map[string]any{"@type": "Question", "name": "Why can Telegram or Discord still show an old preview?", "acceptedAnswer": map[string]any{"@type": "Answer", "text": "Chat apps cache link previews. Send the converted URL as a fresh message and test another public Reel to distinguish a cached result from a source-specific problem."}},
				map[string]any{"@type": "Question", "name": "Does it work with private Instagram accounts?", "acceptedAnswer": map[string]any{"@type": "Answer", "text": "No. InstaFix Revived only works with publication data that is publicly available and does not make private, deleted, age-restricted, or region-restricted content public."}},
			},
		},
	}
	if video.Ready() {
		videoObject := map[string]any{
			"@type":        "VideoObject",
			"@id":          "__INSTAFIX_BASE_URL__/how-instagram7-works#video",
			"name":         video.Name,
			"description":  video.Description,
			"thumbnailUrl": []string{video.PosterURL},
			"uploadDate":   video.UploadDate,
			"contentUrl":   video.ContentURL(),
		}
		if video.Duration != "" {
			videoObject["duration"] = video.Duration
		}
		if video.Transcript != "" {
			videoObject["transcript"] = video.Transcript
		}
		graph = append(graph, videoObject)
	}
	payload, _ := json.Marshal(map[string]any{"@context": "https://schema.org", "@graph": graph})
	return template.JS(payload)
}

var howInstaFix RevivedWorksTemplate = template.Must(template.New("how-instagram7-works").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="description" content="See a live Instagram Reel example and the three-step InstaFix Revived workflow.">
  <meta name="robots" content="index, follow, max-snippet:-1, max-image-preview:large, max-video-preview:-1">
  <link rel="canonical" href="__INSTAFIX_BASE_URL__/how-instagram7-works">
  <meta property="og:type" content="website">
  <meta property="og:site_name" content="InstaFix Revived">
  <meta property="og:title" content="How InstaFix Revived Fixes Instagram Reel Previews">
  <meta property="og:description" content="A live Reel example and the simple add-7 workflow.">
  <meta property="og:url" content="__INSTAFIX_BASE_URL__/how-instagram7-works">
  <meta property="og:image" content="__INSTAFIX_BASE_URL__/site-preview.svg">
  <meta property="og:image:alt" content="InstaFix Revived public Instagram preview service">
  <meta name="twitter:card" content="summary_large_image">
  <meta name="twitter:title" content="How InstaFix Revived Fixes Instagram Reel Previews">
  <meta name="twitter:description" content="A live Reel example and the simple add-7 workflow.">
  <meta name="twitter:image" content="__INSTAFIX_BASE_URL__/site-preview.svg">
  <title>How InstaFix Revived Fixes Instagram Reel Previews</title>
  <script type="application/ld+json">{{.StructuredData}}</script>
  <style>
    :root{color-scheme:light;--bg:#fbf9f6;--card:#fff;--border:rgba(139,92,26,.14);--text:#2c241e;--muted:#6e645a;--accent:#4f46e5;--accent-soft:#eef2ff;--font:'Plus Jakarta Sans',-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;--mono:'JetBrains Mono',ui-monospace,monospace}
    *{box-sizing:border-box;margin:0;padding:0}body{background:var(--bg);background-image:radial-gradient(circle at 10% 18%,rgba(249,115,22,.05),transparent 38%),radial-gradient(circle at 90% 72%,rgba(99,102,241,.05),transparent 38%);color:var(--text);font-family:var(--font);line-height:1.65;-webkit-font-smoothing:antialiased}a{color:inherit;text-decoration:none}.shell{width:min(1080px,calc(100% - 48px));margin:auto;padding:40px 0}header{display:flex;align-items:center;justify-content:space-between;gap:24px;padding-bottom:24px;border-bottom:1px solid var(--border);margin-bottom:56px}.brand{font-size:1.4rem;font-weight:800;letter-spacing:-.04em;background:linear-gradient(135deg,#d946ef,#ec4899,#f97316);-webkit-background-clip:text;-webkit-text-fill-color:transparent}nav{display:flex;flex-wrap:wrap;gap:18px;color:var(--muted);font-size:.9rem;font-weight:600}nav a:hover,.text-link:hover{color:var(--accent)}main{display:grid;gap:64px}.hero{max-width:860px}.eyebrow{display:inline-flex;margin-bottom:14px;padding:5px 10px;border:1px solid rgba(99,102,241,.16);border-radius:999px;background:rgba(99,102,241,.07);color:var(--accent);font-size:.72rem;font-weight:800;letter-spacing:.07em;text-transform:uppercase}.hero h1{max-width:820px;font-size:clamp(2.45rem,6vw,4.65rem);line-height:1.02;letter-spacing:-.055em}.hero p{max-width:700px;margin-top:20px;color:var(--muted);font-size:clamp(1rem,2vw,1.18rem)}.live-example{display:grid;grid-template-columns:minmax(0,1.02fr) minmax(300px,.72fr);gap:28px;align-items:center;padding:clamp(20px,4vw,38px);border:1px solid var(--border);border-radius:30px;background:linear-gradient(135deg,rgba(255,255,255,.9),rgba(255,247,237,.74));box-shadow:0 24px 70px rgba(71,55,38,.08)}.live-copy h2,.section-heading h2{font-size:clamp(1.7rem,4vw,2.6rem);line-height:1.12;letter-spacing:-.04em}.live-copy p,.section-heading p{margin-top:12px;color:var(--muted)}.domain-change{display:flex;align-items:center;gap:10px;margin:22px 0;flex-wrap:wrap;font-family:var(--mono);font-size:.88rem}.domain-change span{padding:8px 11px;border:1px solid var(--border);border-radius:10px;background:#fff}.domain-change strong{color:#db2777;font-size:1.18em}.cta{display:inline-flex;padding:11px 17px;border-radius:12px;background:linear-gradient(135deg,#6366f1,#4f46e5);color:#fff;font-weight:750}.cta:hover{filter:brightness(.97)}.live-media{overflow:hidden;border-radius:20px;background:#111827;box-shadow:0 18px 40px rgba(17,24,39,.16)}.live-media video{display:block;width:100%;aspect-ratio:9/16;object-fit:cover}.live-media figcaption{padding:12px 14px;background:#fff;color:var(--muted);font-size:.78rem}.section-heading{max-width:720px;margin-bottom:24px}.steps{display:grid;grid-template-columns:repeat(3,1fr);gap:18px;list-style:none}.steps li{padding:22px;border:1px solid var(--border);border-radius:18px;background:rgba(255,255,255,.82)}.steps b{display:block;margin-bottom:8px;font-size:1.04rem}.steps span{display:block;margin-bottom:14px;color:#db2777;font-family:var(--mono);font-size:.75rem;font-weight:800}.steps p{color:var(--muted);font-size:.9rem}.future-video{padding:26px;border:1px dashed rgba(99,102,241,.3);border-radius:22px;background:rgba(238,242,255,.5)}.future-video video{display:block;width:100%;max-height:620px;margin-top:20px;border-radius:16px;background:#111827}.future-video .pending{margin-top:12px;color:var(--muted)}.transcript{margin-top:16px;padding:16px;border-radius:14px;background:#fff}.transcript summary{cursor:pointer;font-weight:700}.transcript p{margin-top:10px;color:var(--muted)}.faq{display:grid;gap:12px}.faq article{padding:18px 20px;border:1px solid var(--border);border-radius:16px;background:#fff}.faq h3{font-size:1rem}.faq p{margin-top:7px;color:var(--muted);font-size:.9rem}footer{margin-top:56px;padding-top:28px;border-top:1px solid var(--border);color:var(--muted);font-size:.86rem}:focus-visible{outline:2px solid var(--accent);outline-offset:3px}@media(max-width:760px){.shell{width:min(100% - 32px,1080px);padding-top:24px}header{align-items:flex-start;margin-bottom:40px}.live-example{grid-template-columns:1fr}.steps{grid-template-columns:1fr}.live-media{max-width:420px}}@media(prefers-reduced-motion:reduce){*,*::before,*::after{scroll-behavior:auto!important;animation-duration:.01ms!important;transition-duration:.01ms!important}}
  </style>
</head>
<body>
  <div class="shell">
    <header><a class="brand" href="/">InstaFix Revived</a><nav aria-label="Primary navigation"><a href="/#converter">Converter</a><a href="/guides">Guides</a><a href="https://github.com/Bl0ck154/InstaFix-Revived" target="_blank" rel="noopener noreferrer">GitHub</a></nav></header>
    <main>
      <section class="hero">
        <span class="eyebrow">Real workflow</span>
        <h1>Fix Instagram Reel previews on your own domain.</h1>
        <p>InstaFix Revived turns a public Instagram publication URL into a preview-focused share link. The original publication stays the destination; the receiving chat app gets cleaner metadata and playable media when available.</p>
      </section>

      <section class="live-example" aria-labelledby="live-example-title">
        <div class="live-copy">
          <span class="eyebrow">Live backend example</span>
          <h2 id="live-example-title">This Reel is loaded through this instance.</h2>
          <p>This example uses an included demo asset, so it can be played without requesting a fresh media URL from Instagram.</p>
          <div class="domain-change" aria-label="Convert an Instagram link for this instance"><span>instagram.com</span><b aria-hidden="true">→</b><span>__INSTAFIX_HOST__</span></div>
          <a class="cta" href="__INSTAFIX_BASE_URL__/reels/Dbidjf_C4nf/">Open this instance's Reel example</a>
        </div>
        <figure class="live-media">
          <video controls muted playsinline preload="metadata" poster="/assets/demo/instagram7-test-reel-poster.webp"><source src="/assets/demo/instagram7-test-reel.mp4" type="video/mp4">Your browser cannot play this Reel preview.</video>
          <figcaption>A bundled demonstration video, served locally by InstaFix Revived.</figcaption>
        </figure>
      </section>

      <section aria-labelledby="steps-title">
        <div class="section-heading"><h2 id="steps-title">Three steps, no extra modes</h2><p>The whole workflow stays deliberately small: convert, copy, and send.</p></div>
        <ol class="steps">
          <li><span>STEP 1</span><b>Copy the public Reel link</b><p>Open the public Reel in Instagram and copy its share URL.</p></li>
          <li><span>STEP 2</span><b>Add 7 after instagram</b><p>Use the converter to generate a URL on your own domain.</p></li>
          <li><span>STEP 3</span><b>Send the converted link</b><p>Paste the InstaFix Revived link into a new Telegram or Discord message and let the client load its preview.</p></li>
        </ol>
      </section>

      <section class="future-video" id="full-demo-video" aria-labelledby="future-video-title" data-video-ready="{{.Video.Ready}}">
        <h2 id="future-video-title">Full walkthrough video</h2>
        {{if .Video.Ready}}
        <p>{{.Video.Description}}</p>
        <video controls preload="metadata" poster="{{.Video.PosterURL}}">
          {{if .Video.WebMURL}}<source src="{{.Video.WebMURL}}" type="video/webm">{{end}}
          {{if .Video.MP4URL}}<source src="{{.Video.MP4URL}}" type="video/mp4">{{end}}
          {{if .Video.CaptionsURL}}<track kind="captions" src="{{.Video.CaptionsURL}}" srclang="en" label="English" default>{{end}}
        </video>
        {{if or .Video.Transcript .Video.TranscriptURL}}<details class="transcript"><summary>Read the video transcript</summary>{{if .Video.Transcript}}<p>{{.Video.Transcript}}</p>{{end}}{{if .Video.TranscriptURL}}<p><a class="text-link" href="{{.Video.TranscriptURL}}">Open the full transcript</a></p>{{end}}</details>{{end}}
        {{else}}
        <p class="pending">The page is ready for an owned MP4/WebM, poster image, captions, transcript, and complete VideoObject data. The schema stays disabled until those real assets are published.</p>
        {{end}}
      </section>

      <section aria-labelledby="faq-title">
        <div class="section-heading"><h2 id="faq-title">Frequently asked questions</h2></div>
        <div class="faq">
          <article><h3>Does InstaFix Revived download or re-upload the Reel?</h3><p>InstaFix Revived prepares preview metadata for a public publication. Media delivery depends on what Instagram currently makes available and what the receiving chat client supports.</p></article>
          <article><h3>Why can Telegram or Discord still show an old preview?</h3><p>Chat apps cache link previews. Send the converted URL as a fresh message and test another public Reel to distinguish a cached result from a source-specific problem.</p></article>
          <article><h3>Does it work with private Instagram accounts?</h3><p>No. InstaFix Revived only works with publication data that is publicly available and does not make private, deleted, age-restricted, or region-restricted content public.</p></article>
        </div>
      </section>

      <section class="section-heading"><h2>Try your own public Instagram link</h2><p>The converter accepts posts, Reels, and legacy TV publication URLs.</p><p><a class="cta" href="/#converter">Back to the InstaFix Revived converter</a></p></section>
    </main>
    <footer>InstaFix Revived is powered by InstaFix Revived and is not affiliated with Instagram or Meta.</footer>
  </div>
</body>
</html>`))

func renderHowInstaFix RevivedWorks(wr io.Writer, video demoVideo) error {
	return howInstaFix RevivedWorksTemplate.Execute(wr, howInstaFix RevivedWorksPage{
		Video:          video,
		StructuredData: howInstaFix RevivedWorksStructuredData(video),
	})
}

func HowInstaFix RevivedWorks(wr io.Writer) bool {
	return renderHowInstaFix RevivedWorks(wr, demoVideo{}) == nil
}
