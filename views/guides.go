package views

import (
	"html/template"
	"io"
	"strings"
)

type guidePage struct {
	Slug        string
	Title       string
	Description string
	Heading     string
	Eyebrow     string
	Body        template.HTML
	IsGenerator bool
	IsIndex     bool
}

var guidePages = map[string]guidePage{
	"instagram-link-preview-fixer": {
		Slug:        "instagram-link-preview-fixer",
		Title:       "Instagram Link Preview Fixer: A Practical Guide",
		Description: "Learn why Instagram link previews break and how to create a cleaner preview for public posts and Reels in chat apps.",
		Heading:     "How to fix an Instagram link preview",
		Eyebrow:     "Practical guide",
		Body: template.HTML(`
			<p>Instagram links do not always produce a useful card when they are pasted into a messenger. A chat app may show only a bare URL, an old thumbnail, or no playable video. The reason is usually not the message itself: preview bots request a page separately and depend on Open Graph and Twitter Card metadata that can change, expire, or be unavailable.</p>
			<h2>The quick method</h2>
			<ol>
				<li>Copy a public Instagram post or Reel URL.</li>
				<li>Paste the Instagram URL into the converter on this instance; it will generate a link with your own hostname.</li>
				<li>Send the converted link as a new message so the chat app requests a fresh preview.</li>
			</ol>
			<p>InstaFix Revived reads public metadata and produces a compact preview page for compatible clients. It can provide an author name, caption, thumbnail, and playable media when the source makes those fields available. Private, deleted, age-restricted, or region-restricted posts may still be unavailable.</p>
			<h2>Why a preview can stay stale</h2>
			<p>Telegram, Discord, Slack, and other services cache preview results. Editing an old message may reuse the cached card. Sending the link again, without tracking parameters, is usually the cleanest test. A different post should also be used when you need to distinguish a cache problem from a source-media problem.</p>
			<h2>Safe expectations</h2>
			<p>The service does not unlock private content and is not affiliated with Instagram or Meta. It reformats metadata for public links; the original Instagram post remains the authoritative source.</p>
		`),
	},
	"instagram-reels-preview": {
		Slug:        "instagram-reels-preview",
		Title:       "Instagram Reels Preview Guide for Chat Apps",
		Description: "Create a cleaner Instagram Reels preview with a vertical thumbnail or playable video card when the public media is available.",
		Heading:     "Get a cleaner Instagram Reels preview",
		Eyebrow:     "Reels guide",
		Body: template.HTML(`
			<p>Reels are harder to preview than image posts because a chat client needs more than a title and thumbnail. It may need a direct video stream, a supported content type, dimensions, and a fallback image. If any part of that chain is missing or cached incorrectly, the result can be a blank card or a thumbnail that never plays.</p>
			<h2>Convert a Reel link</h2>
			<ol>
				<li>Open the public Reel and use Instagram's share menu to copy its URL.</li>
				<li>Paste it into the converter on this instance to generate a shareable link.</li>
				<li>Share the new URL in the target chat and wait for its preview bot to finish.</li>
			</ol>
			<p>The converter recognizes <code>/reel/</code>, <code>/reels/</code>, and legacy <code>/tv/</code> publication paths. Profile pages, Explore pages, arbitrary text, and look-alike domains are rejected because they are not publication URLs.</p>
			<h2>Thumbnail versus playable video</h2>
			<p>Different clients support different cards. One app may play the Reel in place, while another deliberately shows only the thumbnail and opens the original link. InstaFix Revived supplies the strongest metadata it has, but the receiving client makes the final rendering decision.</p>
			<h2>When a Reel is unavailable</h2>
			<p>Check that the Reel is public and still opens in a logged-out browser. If it is restricted or Instagram temporarily withholds its media URL, retrying many times will not make it public. Open the original post instead and respect the creator's access settings.</p>
		`),
	},
	"telegram-instagram-preview": {
		Slug:        "telegram-instagram-preview",
		Title:       "Fix Instagram Link Previews in Telegram",
		Description: "A focused guide to cleaner Instagram post and Reel previews in Telegram, including common cache and media limitations.",
		Heading:     "Fix Instagram previews in Telegram",
		Eyebrow:     "Telegram guide",
		Body: template.HTML(`
			<p>Telegram creates link cards with its own crawler. That crawler can see a different response from the one shown in your browser, and it can keep a cached result after the source changes. A converted InstaFix Revived link gives Telegram a small preview-focused page instead of asking it to interpret the full Instagram experience.</p>
			<h2>Three steps</h2>
			<ol>
				<li>Copy the URL of a public Instagram post or Reel.</li>
				<li>Use the InstaFix Revived converter to create a clean URL.</li>
				<li>Paste the new URL into a fresh Telegram message and wait for the card before sending.</li>
			</ol>
			<p>For Reels, Telegram may show a playable vertical video when the media stream is currently available. For carousels, it may select one image even when the metadata includes more than one. These are Telegram rendering choices rather than converter errors.</p>
			<h2>If Telegram shows an old preview</h2>
			<p>First test in a new message instead of repeatedly editing the old one. Remove unrelated tracking parameters and make sure the hostname matches the one used by your instance. If another public post works, the first result is probably cached or source-specific.</p>
			<h2>Privacy and access</h2>
			<p>InstaFix Revived processes public publication URLs. It does not make private accounts public, and people who open a preview are redirected to the original Instagram publication for the full context.</p>
		`),
	},
	"discord-instagram-embed": {
		Slug:        "discord-instagram-embed",
		Title:       "Instagram Discord Embed Generator and Link Fixer",
		Description: "Generate a cleaner Discord share link for a public Instagram post or Reel, then troubleshoot missing or stale embed cards.",
		Heading:     "Instagram Discord embed generator",
		Eyebrow:     "Free browser tool",
		IsGenerator: true,
		Body: template.HTML(`
			<p>The generator above runs in your browser and turns a public Instagram publication URL or shortcode into an InstaFix Revived share link. Discord then requests a compact page with Open Graph and Twitter Card fields while the original Instagram publication remains the destination.</p>
			<h2>Generate and share the link</h2>
			<ol>
				<li>Paste a public Instagram post or Reel URL. If you only have its shortcode, choose Post or Reel.</li>
				<li>Select <strong>Generate Discord link</strong>, then copy the result.</li>
				<li>Paste it into a new Discord message and wait for the card before sending.</li>
			</ol>
			<p>An image post normally produces a thumbnail card. A Reel can produce a video-oriented card when the media is available and Discord supports the supplied stream. The exact card layout is controlled by Discord and can change independently of this service.</p>
			<h2>What the generator accepts</h2>
			<p>Supported publication paths are <code>/p/</code>, <code>/reel/</code>, <code>/reels/</code>, and legacy <code>/tv/</code>. It rejects profiles, Explore pages, arbitrary text, and look-alike domains. The conversion happens locally in the page; generating the link does not submit the input to a separate analytics service.</p>
			<h2>Troubleshooting a missing embed</h2>
			<p>Confirm that embeds are permitted in the channel, the post is public, and the URL contains a supported publication path. Try a fresh message with another known-public post: editing an existing Discord message may reuse an older cached card. If all previews are disabled in the channel, changing the URL cannot override that permission.</p>
			<h2>What the service does not do</h2>
			<p>It does not bypass private profiles or reproduce an Instagram account. It provides preview metadata for public links and sends normal browser visitors to the original publication.</p>
			<p><small>Generator input rules last reviewed July 29, 2026.</small></p>
		`),
	},
}

var guideIndexPage = guidePage{
	Title:       "Instagram Preview Guides and Embed Tools",
	Description: "Practical Instagram link fixer guides and free tools for cleaner post and Reel previews in Discord, Telegram, and other chat apps.",
	Heading:     "Instagram preview guides and tools",
	Eyebrow:     "InstaFix Revived knowledge base",
	IsIndex:     true,
	Body: template.HTML(`
		<p>Start with the converter on the homepage, or choose the guide that matches the app and publication type you are troubleshooting. Every guide is written for public Instagram publications and documents the limits imposed by source availability and each chat client's cache.</p>
		<h2>Choose a guide</h2>
		<div class="guide-card-grid">
			<a class="guide-card" href="/guides/instagram-link-preview-fixer"><span>Link fixer</span><strong>Instagram link previews</strong><small>Why cards fail and how to test a converted link.</small></a>
			<a class="guide-card" href="/guides/instagram-reels-preview"><span>Reels</span><strong>Vertical video previews</strong><small>Troubleshoot video, thumbnails, and playable media.</small></a>
			<a class="guide-card" href="/guides/telegram-instagram-preview"><span>Telegram</span><strong>Telegram preview cache</strong><small>Work with Telegram's crawler and cached cards.</small></a>
			<a class="guide-card" href="/guides/discord-instagram-embed"><span>Discord</span><strong>Discord embed generator</strong><small>Generate a clean share link and diagnose missing embeds.</small></a>
		</div>
		<h2>Before troubleshooting</h2>
		<p>Verify that the original publication is public and still opens. Use a fresh chat message and a second known-public publication to distinguish a cached client result from a source-specific failure. InstaFix Revived cannot make private, deleted, age-restricted, or region-restricted media public.</p>
	`),
}

var guideTemplate = template.Must(template.New("guide").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="description" content="{{.Description}}">
  <meta name="robots" content="index, follow, max-snippet:-1, max-image-preview:large">
  <link rel="canonical" href="__INSTAFIX_BASE_URL__/guides{{if .Slug}}/{{.Slug}}{{end}}">
  <link rel="alternate" hreflang="en" href="__INSTAFIX_BASE_URL__/guides{{if .Slug}}/{{.Slug}}{{end}}">
  <link rel="alternate" hreflang="x-default" href="__INSTAFIX_BASE_URL__/guides{{if .Slug}}/{{.Slug}}{{end}}">
  <meta property="og:type" content="article">
  <meta property="og:site_name" content="InstaFix Revived">
  <meta property="og:title" content="{{.Title}}">
  <meta property="og:description" content="{{.Description}}">
  <meta property="og:url" content="__INSTAFIX_BASE_URL__/guides{{if .Slug}}/{{.Slug}}{{end}}">
  <meta property="og:image" content="__INSTAFIX_BASE_URL__/site-preview.svg">
  <meta name="twitter:card" content="summary_large_image">
  <meta name="twitter:title" content="{{.Title}}">
  <meta name="twitter:description" content="{{.Description}}">
  <meta name="twitter:image" content="__INSTAFIX_BASE_URL__/site-preview.svg">
  <title>{{.Title}} | InstaFix Revived</title>
  <script type="application/ld+json">
  {
    "@context": "https://schema.org",
    "@type": "{{if .IsIndex}}CollectionPage{{else}}TechArticle{{end}}",
    "headline": {{.Title}},
    "description": {{.Description}},
    "datePublished": "2026-07-27",
    "dateModified": "2026-07-29",
    "inLanguage": "en",
    "isAccessibleForFree": true,
    "mainEntityOfPage": "__INSTAFIX_BASE_URL__/guides{{if .Slug}}/{{.Slug}}{{end}}",
    "author": {"@type": "Person", "name": "Bl0ck154", "url": "https://github.com/Bl0ck154"},
    "publisher": {"@type": "Person", "name": "Bl0ck154", "url": "https://github.com/Bl0ck154"}
  }
  </script>
  <style>
    :root {
      color-scheme: light;
      --bg-main: #fbf9f6;
      --bg-card: rgba(255,255,255,.85);
      --border-card: rgba(139,92,26,.12);
      --border-card-hover: rgba(139,92,26,.22);
      --text-primary: #2c241e;
      --text-secondary: #6e645a;
      --text-muted: #9c8e80;
      --color-accent: #6366f1;
      --color-accent-hover: #4f46e5;
      --font-primary: 'Plus Jakarta Sans',-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
      --font-mono: 'JetBrains Mono',ui-monospace,monospace;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background-color: var(--bg-main);
      background-image:
        radial-gradient(circle at 10% 20%,rgba(249,115,22,.05) 0%,transparent 40%),
        radial-gradient(circle at 90% 80%,rgba(236,72,153,.04) 0%,transparent 40%);
      color: var(--text-primary);
      font-family: var(--font-primary);
      line-height: 1.65;
      -webkit-font-smoothing: antialiased;
    }
    a { color: inherit; text-decoration: none; transition: color .2s ease,transform .2s ease,border-color .2s ease; }
    a:hover { color: var(--color-accent-hover); }
    .container { width: min(1080px,calc(100% - 48px)); margin: 0 auto; padding: 40px 0; }
    header { display:flex; justify-content:space-between; align-items:center; gap:24px; padding-bottom:24px; border-bottom:1px solid rgba(139,92,26,.1); margin-bottom:50px; }
    .brand-group { display:flex; align-items:center; gap:12px; }
    .brand { font-size:1.4rem; font-weight:800; letter-spacing:-.03em; background:linear-gradient(135deg,#d946ef,#ec4899,#f97316); -webkit-background-clip:text; -webkit-text-fill-color:transparent; }
    header nav { display:flex; align-items:center; gap:20px; color:var(--text-secondary); font-size:.9rem; font-weight:500; }
    header nav a { display:flex; align-items:center; gap:6px; }
    header nav a:hover { color:var(--text-primary); }
    .guide-hero { text-align:center; max-width:820px; margin:0 auto 30px; }
    .eyebrow { display:inline-flex; align-items:center; width:fit-content; margin-bottom:14px; padding:5px 10px; border:1px solid rgba(99,102,241,.14); border-radius:999px; background:rgba(99,102,241,.07); color:#4f46e5; font-size:.74rem; font-weight:700; letter-spacing:.06em; text-transform:uppercase; }
    h1 { margin-bottom:16px; color:var(--text-primary); font-size:clamp(2.2rem,5vw,3.5rem); font-weight:800; line-height:1.12; letter-spacing:-.04em; }
    .lede { max-width:720px; margin:0 auto; color:var(--text-secondary); font-size:clamp(1rem,2vw,1.15rem); }
    .tool-showcase { margin:30px 0 34px; padding:24px; border:1px solid var(--border-card); border-radius:30px; background:linear-gradient(135deg,rgba(255,255,255,.78),rgba(255,247,237,.58)),radial-gradient(circle at 15% 20%,rgba(236,72,153,.08),transparent 36%),radial-gradient(circle at 86% 78%,rgba(99,102,241,.08),transparent 34%); box-shadow:0 24px 70px rgba(139,92,26,.08); }
    .generator { padding:clamp(22px,5vw,34px); border:1px solid rgba(139,92,26,.1); border-radius:24px; background:rgba(255,255,255,.82); box-shadow:0 16px 36px rgba(139,92,26,.06); }
    .generator h2 { margin:0 0 8px; font-size:clamp(1.5rem,3vw,2.1rem); line-height:1.15; letter-spacing:-.035em; }
    .generator p { max-width:660px; margin:0 0 20px; color:var(--text-secondary); }
    .generator label { display:block; margin-bottom:7px; color:var(--text-primary); font-size:.9rem; font-weight:650; }
    .generator input[type="text"] { width:100%; padding:12px 16px; border:1px solid rgba(139,92,26,.2); border-radius:12px; outline:0; background:#fff; color:var(--text-primary); font:inherit; }
    .generator input[type="text"]:focus { border-color:var(--color-accent); box-shadow:0 0 0 2px rgba(99,102,241,.15); }
    .generate { margin-top:12px; padding:12px 20px; border:0; border-radius:12px; background:linear-gradient(135deg,#6366f1,#4f46e5); color:#fff; cursor:pointer; font:inherit; font-weight:700; }
    .generate:hover { transform:translateY(-1px); }
    .generator-result { display:none; margin-top:18px; padding:13px; border:1px solid rgba(99,102,241,.16); border-radius:14px; background:linear-gradient(135deg,rgba(238,242,255,.88),rgba(253,242,248,.74)); }
    .generator-result.active { display:block; }
    .output-row { display:flex; gap:9px; }
    .output-row input { min-width:0; margin:0!important; color:#4f46e5!important; font-family:var(--font-mono)!important; font-size:.78rem!important; font-weight:650!important; }
    .copy { flex-shrink:0; padding:0 15px; border:1px solid rgba(79,70,229,.13); border-radius:10px; background:rgba(79,70,229,.09); color:#4338ca; cursor:pointer; font:inherit; font-size:.78rem; font-weight:700; }
    .generator-status { min-height:1.4em; margin-top:8px!important; color:var(--text-secondary)!important; font-size:.82rem; }
    .generator-status:empty { display:none; }
    .generator-status.error { display:block; color:#b91c1c!important; }
    .article { margin-top:32px; padding:clamp(26px,6vw,52px); border:1px solid var(--border-card); border-radius:24px; background:var(--bg-card); box-shadow:0 18px 55px rgba(71,55,38,.07); }
    .article h2 { margin:2.2rem 0 .75rem; color:var(--text-primary); font-size:1.55rem; line-height:1.25; letter-spacing:-.02em; }
    .article > h2:first-child { margin-top:0; }
    .article p,.article li { color:#51483f; }
    .article p + p,.article p + ol,.article p + ul,.article ol + p,.article ul + p { margin-top:1rem; }
    .article ol,.article ul { margin:1rem 0 0 1.25rem; }
    .article li + li { margin-top:.55rem; }
    .article > p:first-child { color:var(--text-primary); font-size:1.08rem; }
    code { padding:.12em .36em; border-radius:.35em; background:#f0ece7; font-family:var(--font-mono); font-size:.9em; }
    .guide-card-grid { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:16px; margin-top:18px; }
    .guide-card { display:flex; min-height:170px; flex-direction:column; padding:22px; border:1px solid var(--border-card); border-radius:18px; background:#fff; box-shadow:0 4px 12px rgba(139,92,26,.02); }
    .guide-card:hover { transform:translateY(-2px); border-color:var(--border-card-hover); box-shadow:0 10px 25px rgba(139,92,26,.08); }
    .guide-card span { width:fit-content; margin-bottom:20px; padding:4px 8px; border-radius:999px; background:rgba(99,102,241,.07); color:#4f46e5; font-size:.68rem; font-weight:750; letter-spacing:.05em; text-transform:uppercase; }
    .guide-card strong { color:var(--text-primary); font-size:1.05rem; letter-spacing:-.015em; }
    .guide-card small { margin-top:7px; color:var(--text-secondary); font-size:.82rem; line-height:1.45; }
    .cta { margin-top:40px; padding:24px; border:1px solid rgba(99,102,241,.12); border-radius:18px; background:linear-gradient(135deg,rgba(238,242,255,.88),rgba(253,242,248,.74)); }
    .cta a { display:inline-block; margin-top:10px; padding:10px 16px; border-radius:12px; background:linear-gradient(135deg,#6366f1,#4f46e5); color:#fff; font-weight:700; }
    .related { display:flex; flex-wrap:wrap; gap:10px; margin-top:24px; }
    .related a { padding:8px 13px; border:1px solid var(--border-card); border-radius:999px; background:#fff; color:var(--text-secondary); font-size:.82rem; font-weight:650; }
    .related a:hover { color:var(--color-accent-hover); border-color:rgba(99,102,241,.28); }
    footer { margin-top:50px; padding:28px 0 12px; border-top:1px solid rgba(139,92,26,.1); color:var(--text-secondary); font-size:.88rem; }
    :focus-visible { outline:2px solid var(--color-accent); outline-offset:3px; }
    @media(max-width:720px) {
      .container { width:min(100% - 32px,1080px); padding-top:24px; }
      header { align-items:flex-start; margin-bottom:38px; }
      header nav { flex-wrap:wrap; justify-content:flex-end; gap:10px 14px; font-size:.82rem; }
      .guide-card-grid { grid-template-columns:1fr; }
      .output-row { flex-direction:column; }
      .copy { padding:11px 15px; }
      .tool-showcase { padding:16px; border-radius:24px; }
    }
    @media(prefers-reduced-motion:reduce) {
      *,*::before,*::after { scroll-behavior:auto!important; transition-duration:.01ms!important; animation-duration:.01ms!important; animation-iteration-count:1!important; }
    }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <div class="brand-group"><a class="brand" href="/">InstaFix Revived</a></div>
      <nav aria-label="Primary navigation">
        <a href="/">Converter</a>
        <a href="/guides">Guides</a>
        <a href="https://github.com/Bl0ck154/InstaFix-Revived" target="_blank" rel="noopener noreferrer">
          <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" clip-rule="evenodd" d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0112 6.844c.85.004 1.705.115 2.504.337 1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0022 12.017C22 6.484 17.522 2 12 2z"></path></svg>
          GitHub
        </a>
      </nav>
    </header>
    <main>
      <section class="guide-hero">
        <div class="eyebrow">{{.Eyebrow}}</div>
        <h1>{{.Heading}}</h1>
        <p class="lede">{{.Description}}</p>
      </section>
      {{if .IsGenerator}}
      <div class="tool-showcase">
        <section class="generator" aria-labelledby="generator-title">
          <h2 id="generator-title">Generate a Discord-ready Instagram link</h2>
          <p>Paste a public Instagram URL or shortcode.</p>
          <form id="discord-generator" novalidate>
            <label for="generator-input">Instagram URL or shortcode</label>
            <input id="generator-input" type="text" placeholder="instagram.com/reel/... or DaJlro2MFT6" autocomplete="off" autocapitalize="none" spellcheck="false" aria-describedby="generator-status">
            <button class="generate" type="submit">Generate Discord link</button>
            <div class="generator-result" id="generator-result">
              <label for="generator-output">Share this link in Discord</label>
              <div class="output-row">
                <input id="generator-output" type="text" readonly>
                <button class="copy" id="generator-copy" type="button">Copy link</button>
              </div>
            </div>
            <p class="generator-status" id="generator-status" aria-live="polite"></p>
          </form>
        </section>
      </div>
      {{end}}
      <article class="article">
        {{.Body}}
        {{if not .IsIndex}}
        <div class="cta">
          <strong>Ready to try it?</strong><br>
          Convert a real public Instagram publication URL on the homepage.<br>
          <a href="/">Open the InstaFix Revived converter</a>
        </div>
        {{end}}
      </article>
      <nav class="related" aria-label="Related guides">
        <a href="/guides">All guides</a>
        <a href="/guides/instagram-link-preview-fixer">Link preview basics</a>
        <a href="/guides/instagram-reels-preview">Reels previews</a>
        <a href="/guides/telegram-instagram-preview">Telegram</a>
        <a href="/guides/discord-instagram-embed">Discord</a>
      </nav>
    </main>
    <footer>InstaFix Revived is an independent open-source service and is not affiliated with Instagram or Meta.</footer>
  </div>
  {{if .IsGenerator}}
  <script>
  (function () {
    const form = document.getElementById('discord-generator');
    const input = document.getElementById('generator-input');
    const output = document.getElementById('generator-output');
    const result = document.getElementById('generator-result');
    const status = document.getElementById('generator-status');
    const copy = document.getElementById('generator-copy');
    const pathPattern = /^\/(p|reel|reels|tv)\/([A-Za-z0-9_-]{5,64})\/?$/i;
    const shortcodePattern = /^[A-Za-z0-9_-]{5,64}$/;

    function parsePublication(raw) {
      let value = raw.trim().replace(/^[\u201c\u201d"']+|[\u201c\u201d"']+$/g, '');
      if (shortcodePattern.test(value)) return { kind: 'p', id: value };
      let match;
      try {
        const urlValue = /^https?:\/\//i.test(value) ? value : 'https://' + value;
        const url = new URL(urlValue);
        const host = url.hostname.toLowerCase().replace(/^www\./, '').replace(/^m\./, '');
        if (host !== 'instagram.com' && host !== '__INSTAFIX_HOST__') return null;
        match = url.pathname.match(pathPattern);
      } catch (_) {
        match = null;
      }
      if (match) {
        const kind = match[1].toLowerCase();
        return { kind: kind === 'p' ? 'p' : 'reel', id: match[2] };
      }
      return null;
    }

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      const publication = parsePublication(input.value);
      if (!publication) {
        result.classList.remove('active');
        status.className = 'generator-status error';
        status.textContent = 'Enter a public Instagram publication URL or a valid shortcode/ID.';
        return;
      }
      output.value = '__INSTAFIX_BASE_URL__/' + publication.kind + '/' + publication.id + '/';
      result.classList.add('active');
      status.className = 'generator-status';
      status.textContent = '';
    });

    copy.addEventListener('click', async function () {
      if (!output.value) return;
      try {
        await navigator.clipboard.writeText(output.value);
        status.textContent = 'Copied to clipboard.';
      } catch (_) {
        output.select();
        document.execCommand('copy');
        status.textContent = 'Copied to clipboard.';
      }
    });
  }());
  </script>
  {{end}}
</body>
</html>`))

func Guide(slug string, wr io.Writer) bool {
	page, ok := guidePages[slug]
	if !ok {
		return false
	}
	var rendered strings.Builder
	if err := guideTemplate.Execute(&rendered, page); err != nil { return false }
	_, err := io.WriteString(wr, renderSiteMarkup(rendered.String()))
	return err == nil
}

func GuideIndex(wr io.Writer) bool {
	var rendered strings.Builder
	if err := guideTemplate.Execute(&rendered, guideIndexPage); err != nil { return false }
	_, err := io.WriteString(wr, renderSiteMarkup(rendered.String()))
	return err == nil
}
