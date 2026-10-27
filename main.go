package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"instafix/handlers"
	scraper "instafix/handlers/scraper"
	"instafix/observability"
	"instafix/utils"
	"instafix/views"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	bolt "go.etcd.io/bbolt"
)

//go:embed BingSiteAuth.xml
var bingSiteAuthXML []byte

const telegramWebPageMaxInlineVideoBytes int64 = 20 << 20

func init() {
	// Create static folder if not exists. The stateless experiment does not use
	// it for generated grids, but legacy routes still remain available for A/B.
	os.Mkdir("static", 0755)
}

func main() {
	defaultListen := "0.0.0.0:3000"
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		defaultListen = "0.0.0.0:" + port
	}
	listenAddr := flag.String("listen", defaultListen, "Address to listen on")
	gridCacheMaxFlag := flag.String("grid-cache-entries", "1024", "Maximum number of grid images to cache")
	remoteScraperAddr := flag.String("remote-scraper", "", "Remote scraper address (https://github.com/Wikidepia/InstaFix-remote-scraper)")
	videoProxyAddr := flag.String("video-proxy-addr", "", "Video proxy address (https://github.com/Wikidepia/InstaFix-proxy)")
	flag.Parse()

	statelessExperiment := strings.EqualFold(strings.TrimSpace(os.Getenv("INSTAFIX_EXPERIMENT_MODE")), "stateless_cloudrun")
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	observability.Configure(observability.Config{TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"), TelegramChat: os.Getenv("TELEGRAM_CHAT_ID"), ReportHourUTC: observability.EnvReportHour()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observability.Default.Run(ctx)

	// The experiment is intentionally self-contained and anonymous. Its image
	// embeds only a harmless build-time dictionary placeholder and therefore must
	// never attempt the legacy remote-scraper protocol.
	if *remoteScraperAddr != "" {
		if statelessExperiment {
			slog.Warn("remote scraper ignored in stateless experiment")
		} else {
			if !strings.HasPrefix(*remoteScraperAddr, "http") {
				panic("Remote scraper address must start with http:// or https://")
			}
			scraper.RemoteScraperAddr = *remoteScraperAddr
		}
	}

	// The stateless experiment is deliberately cookie-free even if a stale
	// AUTH_HELPER_URL happens to be present in deployment secrets.
	if statelessExperiment {
		scraper.AuthHelperURL = ""
		handlers.ConfigureStatelessCacheDurations(
			time.Duration(envInt("INSTAFIX_STATELESS_EDGE_TTL_SECONDS", 300))*time.Second,
			time.Duration(envInt("INSTAFIX_STATELESS_CDN_EXPIRY_MARGIN_SECONDS", 1800))*time.Second,
		)
		slog.Info("stateless experiment enabled", "mode", "stateless_cloudrun", "commit", buildCommit)
	} else if authHelperURL := strings.TrimSpace(os.Getenv("AUTH_HELPER_URL")); authHelperURL != "" {
		// Initialize optional authenticated fallback helper. It should normally be
		// a local-only service because it can use Instagram session cookies internally.
		if err := validateLocalHTTPURL(authHelperURL); err != nil {
			panic(err)
		}
		scraper.AuthHelperURL = strings.TrimRight(authHelperURL, "/")
		slog.Info("auth helper configured", "url", scraper.AuthHelperURL)
	}

	// Legacy video proxy controls remain available for production/A-B routes,
	// but EmbedStateless never advertises these routes as og:video.
	if *videoProxyAddr != "" {
		if !strings.HasPrefix(*videoProxyAddr, "http") {
			panic("Video proxy address must start with http:// or https://")
		}
		handlers.VideoProxyAddr = *videoProxyAddr
		if !strings.HasSuffix(handlers.VideoProxyAddr, "/") {
			handlers.VideoProxyAddr += "/"
		}
	}
	previewVideoProxyEnabled, _ := strconv.ParseBool(os.Getenv("PREVIEW_VIDEO_PROXY_ENABLED"))
	handlers.ConfigurePreviewVideoProxy(previewVideoProxyEnabled, os.Getenv("PREVIEW_VIDEO_PROXY_USER_AGENTS"))
	if seconds, err := strconv.Atoi(os.Getenv("PREVIEW_VIDEO_PROXY_TIMEOUT_SECONDS")); err == nil {
		handlers.ConfigurePreviewVideoProxyTimeout(seconds)
	}
	if handlers.PreviewVideoProxyEnabled {
		slog.Info("preview video proxy configured", "user_agents", strings.Join(handlers.PreviewVideoProxyUserAgents, ","), "timeout", handlers.PreviewVideoProxyTimeout.String())
	}
	previewVideoCDNRedirectEnabled, _ := strconv.ParseBool(os.Getenv("PREVIEW_VIDEO_CDN_REDIRECT_ENABLED"))
	handlers.ConfigurePreviewVideoCDNRedirect(previewVideoCDNRedirectEnabled, os.Getenv("PREVIEW_VIDEO_CDN_REDIRECT_USER_AGENTS"))
	if handlers.PreviewVideoCDNRedirectEnabled {
		slog.Info("preview video CDN redirect configured", "user_agents", strings.Join(handlers.PreviewVideoCDNRedirectUserAgents, ","))
	}
	configuredInlineVideoBytes := telegramWebPageMaxInlineVideoBytes
	if maxBytes, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("MAX_INLINE_VIDEO_BYTES")), 10, 64); err == nil {
		configuredInlineVideoBytes = maxBytes
	} else if strings.TrimSpace(os.Getenv("MAX_INLINE_VIDEO_MB")) != "" {
		configuredInlineVideoBytes = int64(envInt("MAX_INLINE_VIDEO_MB", 20)) << 20
	}
	if configuredInlineVideoBytes > telegramWebPageMaxInlineVideoBytes {
		slog.Warn("clamping inline video limit to measured Telegram WebPage boundary",
			"configured_bytes", configuredInlineVideoBytes,
			"max_bytes", telegramWebPageMaxInlineVideoBytes)
		configuredInlineVideoBytes = telegramWebPageMaxInlineVideoBytes
	}
	// Fresh Telegram WebPage ingestion has a measured hard boundary at exactly
	// 20 MiB: 20,971,520 bytes is accepted and 20,971,521 is rejected.
	handlers.ConfigureMaxInlineVideoBytes(configuredInlineVideoBytes)

	if !statelessExperiment {
		gridCacheMax, err := strconv.Atoi(*gridCacheMaxFlag)
		if err != nil || gridCacheMax <= 0 {
			panic(err)
		}
		scraper.InitLRU(gridCacheMax)
	}

	// Preserve scraper cache semantics during the first experiment, but make the
	// storage disposable. Cloud Run never needs a volume or state restoration.
	var cacheInitErr error
	if statelessExperiment {
		cacheInitErr = scraper.InitEphemeralDB()
	} else {
		cacheInitErr = scraper.InitDB()
	}
	if cacheInitErr != nil {
		observability.Default.RecordDBError("db_init", cacheInitErr)
		panic(cacheInitErr)
	}
	defer scraper.CloseDB()
	observability.Default.AlertStartup()

	go func() {
		for {
			evictCache()
			time.Sleep(5 * time.Minute)
		}
	}()

	go func() {
		http.ListenAndServe("localhost:6060", nil)
	}()

	r := chi.NewRouter()
	r.Use(observability.Default.Middleware)
	r.Use(middleware.Recoverer)
	r.Use(middleware.StripSlashes)
	r.Use(validatePublicationIDMiddleware)
	r.Use(newRequestProtectorFromEnv().Middleware)

	embedHandler := handlers.Embed
	gridHandler := handlers.Grid
	offloadHandler := handlers.Offload
	if statelessExperiment {
		embedHandler = handlers.EmbedStateless
		gridHandler = handlers.GridStateless
		offloadHandler = handlers.OffloadStateless
	}

	r.Get("/tv/{postID}", embedHandler)
	r.Get("/reel/{postID}", embedHandler)
	r.Get("/reels/{postID}", embedHandler)
	// Stories retain the legacy resolver during this experiment because they use
	// media IDs and are outside the public Post/Reel acceptance corpus.
	r.Get("/stories/{username}/{postID}", handlers.Embed)
	r.Get("/p/{postID}", embedHandler)
	r.Get("/p/{postID}/{mediaNum}", embedHandler)

	r.Get("/{username}/p/{postID}", embedHandler)
	r.Get("/{username}/p/{postID}/{mediaNum}", embedHandler)
	r.Get("/{username}/reel/{postID}", embedHandler)

	r.Get("/images/{postID}/{mediaNum}", handlers.Images)
	r.Head("/images/{postID}/{mediaNum}", handlers.Images)
	r.Get("/videos/{postID}/{mediaNum}", handlers.Videos)
	r.Head("/videos/{postID}/{mediaNum}", handlers.Videos)
	r.Get("/offload/{postID}/{mediaNum}", offloadHandler)
	r.Head("/offload/{postID}/{mediaNum}", offloadHandler)
	r.Get("/player/{postID}/{mediaNum}", handlers.Player)
	r.Get("/grid/{postID}", gridHandler)
	if statelessExperiment {
		r.Head("/grid/{postID}", gridHandler)
	}
	r.Get("/fallback/{postID}.png", handlers.FallbackPreview)
	if !statelessExperiment {
		r.Get("/admin/cookies", handlers.CookieAdmin)
		r.Post("/admin/cookies", handlers.CookieAdmin)
	}
	r.Post("/admin/report/test", handlers.ReportAdmin)
	r.Get("/oembed", handlers.OEmbed)
	r.Get("/healthz", healthHandler)
	r.Get("/version", healthHandler)
	r.Get("/favicon.svg", serveFavicon)
	r.Get("/favicon.ico", serveFavicon)
	r.Get("/assets/demo/instagram7-test-reel.mp4", serveDemoReelVideo)
	r.Head("/assets/demo/instagram7-test-reel.mp4", serveDemoReelVideo)
	r.Get("/assets/demo/instagram7-test-reel-poster.webp", serveDemoReelPoster)
	r.Head("/assets/demo/instagram7-test-reel-poster.webp", serveDemoReelPoster)
	r.Get("/api/{postID}", func(w http.ResponseWriter, r *http.Request) {
		postID := chi.URLParam(r, "postID")
		preferVideo := strings.EqualFold(r.URL.Query().Get("kind"), "reel") || strings.EqualFold(r.URL.Query().Get("prefer"), "video")
		var item *scraper.InstaData
		var err error
		if preferVideo {
			if statelessExperiment {
				item, err = scraper.GetDataPreferVideoQuiet(postID)
			} else {
				item, err = scraper.GetDataPreferVideo(postID)
			}
		} else {
			if statelessExperiment {
				item, err = scraper.GetDataQuiet(postID)
			} else {
				item, err = scraper.GetData(postID)
			}
			if err != nil || item == nil || len(item.Medias) == 0 || item.Username == "" {
				var videoItem *scraper.InstaData
				var videoErr error
				if statelessExperiment {
					videoItem, videoErr = scraper.GetDataPreferVideoQuiet(postID)
				} else {
					videoItem, videoErr = scraper.GetDataPreferVideo(postID)
				}
				if videoErr == nil && videoItem != nil && videoItem.HasVideo() {
					item = videoItem
					err = nil
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		type previewMedia struct {
			TypeName string
		}
		payload := struct {
			Username string
			Caption  string
			Medias   []previewMedia
		}{Username: item.Username, Caption: item.Caption}
		if len(item.Medias) > 0 {
			payload.Medias = []previewMedia{{TypeName: item.Medias[0].TypeName}}
		}
		json.NewEncoder(w).Encode(payload)
	})
	homeHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
		views.Home(w)
	}
	r.Get("/", homeHandler)
	r.Head("/", homeHandler)

	howItWorksHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
		if !views.HowInstagram7Works(w) {
			http.Error(w, "unable to render how Instagram7 works", http.StatusInternalServerError)
		}
	}
	r.Get("/how-instagram7-works", howItWorksHandler)
	r.Head("/how-instagram7-works", howItWorksHandler)

	guideIndexHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
		if !views.GuideIndex(w) {
			http.Error(w, "unable to render guide index", http.StatusInternalServerError)
		}
	}
	r.Get("/guides", guideIndexHandler)
	r.Head("/guides", guideIndexHandler)

	guideHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=3600, stale-while-revalidate=86400")
		if !views.Guide(chi.URLParam(r, "guide"), w) {
			http.NotFound(w, r)
		}
	}
	r.Get("/guides/{guide}", guideHandler)
	r.Head("/guides/{guide}", guideHandler)

	robotsHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(strings.ReplaceAll("User-agent: *\nAllow: /\nDisallow: /admin/\nDisallow: /api/\nDisallow: /fallback/\nDisallow: /grid/\nDisallow: /images/\nDisallow: /offload/\nDisallow: /oembed\nDisallow: /player/\nDisallow: /videos/\nSitemap: __INSTAFIX_BASE_URL__/sitemap.xml\n", "__INSTAFIX_BASE_URL__", views.PublicBaseURL())))
	}
	r.Get("/robots.txt", robotsHandler)
	r.Head("/robots.txt", robotsHandler)

	sitemapHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(strings.ReplaceAll(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url>
    <loc>__INSTAFIX_BASE_URL__/</loc>
    <lastmod>2026-08-08</lastmod>
  </url>
  <url>
    <loc>__INSTAFIX_BASE_URL__/how-instagram7-works</loc>
    <lastmod>2026-08-08</lastmod>
  </url>
  <url>
    <loc>__INSTAFIX_BASE_URL__/guides</loc>
    <lastmod>2026-07-29</lastmod>
  </url>
  <url>
    <loc>__INSTAFIX_BASE_URL__/guides/instagram-link-preview-fixer</loc>
    <lastmod>2026-07-27</lastmod>
  </url>
  <url>
    <loc>__INSTAFIX_BASE_URL__/guides/instagram-reels-preview</loc>
    <lastmod>2026-07-27</lastmod>
  </url>
  <url>
    <loc>__INSTAFIX_BASE_URL__/guides/telegram-instagram-preview</loc>
    <lastmod>2026-07-27</lastmod>
  </url>
  <url>
    <loc>__INSTAFIX_BASE_URL__/guides/discord-instagram-embed</loc>
    <lastmod>2026-07-29</lastmod>
  </url>
</urlset>
`, "__INSTAFIX_BASE_URL__", views.PublicBaseURL())))
	}
	r.Get("/sitemap.xml", sitemapHandler)
	r.Head("/sitemap.xml", sitemapHandler)

	bingVerificationHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(bingSiteAuthXML)
	}
	r.Get("/BingSiteAuth.xml", bingVerificationHandler)
	r.Head("/BingSiteAuth.xml", bingVerificationHandler)

	r.Get("/site-preview.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630" role="img" aria-labelledby="title desc">
  <title id="title">InstaFix Revived — Cleaner Instagram Previews</title>
  <desc id="desc">A preview image for InstaFix Revived, an Instagram link preview fixer for chat apps.</desc>
  <defs>
    <linearGradient id="bg" x1="0" x2="1" y1="0" y2="1">
      <stop offset="0" stop-color="#fff7ed"/>
      <stop offset="0.48" stop-color="#fdf2f8"/>
      <stop offset="1" stop-color="#eef2ff"/>
    </linearGradient>
    <linearGradient id="brand" x1="0" x2="1" y1="0" y2="1">
      <stop offset="0" stop-color="#d946ef"/>
      <stop offset="0.5" stop-color="#ec4899"/>
      <stop offset="1" stop-color="#f97316"/>
    </linearGradient>
  </defs>
  <rect width="1200" height="630" fill="url(#bg)"/>
  <circle cx="160" cy="140" r="190" fill="#f97316" opacity="0.12"/>
  <circle cx="1000" cy="500" r="230" fill="#6366f1" opacity="0.12"/>
  <rect x="90" y="95" width="1020" height="440" rx="42" fill="white" opacity="0.82"/>
  <rect x="135" y="140" width="130" height="130" rx="32" fill="url(#brand)"/>
  <circle cx="200" cy="205" r="33" fill="none" stroke="white" stroke-width="14"/>
  <circle cx="238" cy="166" r="11" fill="white"/>
  <text x="305" y="190" font-family="Inter, Segoe UI, Arial, sans-serif" font-size="62" font-weight="800" fill="#2c241e">InstaFix Revived</text>
  <text x="305" y="252" font-family="Inter, Segoe UI, Arial, sans-serif" font-size="32" font-weight="600" fill="#6e645a">Cleaner Instagram previews for chat apps</text>
  <text x="135" y="350" font-family="Inter, Segoe UI, Arial, sans-serif" font-size="42" font-weight="800" fill="#2c241e">Fix Instagram posts and Reels</text>
  <text x="135" y="405" font-family="Inter, Segoe UI, Arial, sans-serif" font-size="28" fill="#6e645a">Convert public Instagram URLs to shareable links for cleaner previews and playable video where supported.</text>
  <rect x="135" y="452" width="360" height="50" rx="25" fill="#4f46e5"/>
  <text x="165" y="487" font-family="Inter, Segoe UI, Arial, sans-serif" font-size="24" font-weight="700" fill="white">Open-source by Bl0ck154</text>
</svg>`))
	})

	slog.Info("service listening", "event", "startup", "listen_addr", *listenAddr, "stateless_experiment", statelessExperiment, "commit", buildCommit)
	server := &http.Server{Addr: *listenAddr, Handler: r, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		observability.Default.Alert("🚨 InstaFix HTTP server stopped: " + err.Error())
		slog.Error("Failed to listen", "err", err)
	}
}

func validateLocalHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" {
		return errors.New("AUTH_HELPER_URL must use http and point to a local service")
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("AUTH_HELPER_URL must point to localhost/loopback")
	}
	return nil
}

// Remove cache entries after their stale TTL expires.
func evictCache() {
	if scraper.DB == nil {
		return
	}
	curTime := time.Now().UnixNano()
	err := scraper.DB.Batch(func(tx *bolt.Tx) error {
		ttlBucket := tx.Bucket([]byte("ttl"))
		if ttlBucket == nil {
			return nil
		}
		dataBucket := tx.Bucket([]byte("data"))
		if dataBucket == nil {
			return nil
		}
		freshBucket := tx.Bucket([]byte("fresh"))
		c := ttlBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if n, err := strconv.ParseInt(utils.B2S(k), 10, 64); err == nil {
				if n < curTime {
					ttlBucket.Delete(k)
					dataBucket.Delete(v)
					if freshBucket != nil {
						freshBucket.Delete(v)
					}
				}
			} else {
				slog.Error("Failed to parse expire timestamp in cache", "err", err)
			}
		}
		if negativeBucket := tx.Bucket([]byte("negative")); negativeBucket != nil {
			c := negativeBucket.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				raw := utils.B2S(v)
				expRaw := strings.SplitN(raw, "\t", 2)[0]
				if n, err := strconv.ParseInt(expRaw, 10, 64); err == nil && n < curTime {
					negativeBucket.Delete(k)
				}
			}
		}
		return nil
	})
	if err != nil {
		observability.Default.RecordDBError("cache_evict", err)
	}
}
