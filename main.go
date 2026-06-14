package main

import (
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

func init() {
	// Create static folder if not exists
	os.Mkdir("static", 0755)
}

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:3000", "Address to listen on")
	gridCacheMaxFlag := flag.String("grid-cache-entries", "1024", "Maximum number of grid images to cache")
	remoteScraperAddr := flag.String("remote-scraper", "", "Remote scraper address (https://github.com/Wikidepia/InstaFix-remote-scraper)")
	videoProxyAddr := flag.String("video-proxy-addr", "", "Video proxy address (https://github.com/Wikidepia/InstaFix-proxy)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	observability.Configure(observability.Config{})

	// Initialize remote scraper
	if *remoteScraperAddr != "" {
		if !strings.HasPrefix(*remoteScraperAddr, "http") {
			panic("Remote scraper address must start with http:// or https://")
		}
		scraper.RemoteScraperAddr = *remoteScraperAddr
	}

	// Initialize optional authenticated fallback helper. It should normally be a
	// local-only service because it can use Instagram session cookies internally.
	if authHelperURL := strings.TrimSpace(os.Getenv("AUTH_HELPER_URL")); authHelperURL != "" {
		if err := validateLocalHTTPURL(authHelperURL); err != nil {
			panic(err)
		}
		scraper.AuthHelperURL = strings.TrimRight(authHelperURL, "/")
		slog.Info("auth helper configured", "url", scraper.AuthHelperURL)
	}

	// Initialize video proxy
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

	// Initialize LRU
	gridCacheMax, err := strconv.Atoi(*gridCacheMaxFlag)
	if err != nil || gridCacheMax <= 0 {
		panic(err)
	}
	scraper.InitLRU(gridCacheMax)

	// Initialize cache / DB
	if err := scraper.InitDB(); err != nil {
		observability.Default.RecordDBError("db_init", err)
		panic(err)
	}
	defer scraper.DB.Close()
	observability.Default.AlertStartup()

	// Evict cache every minute
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

	r.Get("/tv/{postID}", handlers.Embed)
	r.Get("/reel/{postID}", handlers.Embed)
	r.Get("/reels/{postID}", handlers.Embed)
	r.Get("/stories/{username}/{postID}", handlers.Embed)
	r.Get("/p/{postID}", handlers.Embed)
	r.Get("/p/{postID}/{mediaNum}", handlers.Embed)

	r.Get("/{username}/p/{postID}", handlers.Embed)
	r.Get("/{username}/p/{postID}/{mediaNum}", handlers.Embed)
	r.Get("/{username}/reel/{postID}", handlers.Embed)

	r.Get("/images/{postID}/{mediaNum}", handlers.Images)
	r.Head("/images/{postID}/{mediaNum}", handlers.Images)
	r.Get("/videos/{postID}/{mediaNum}", handlers.Videos)
	r.Head("/videos/{postID}/{mediaNum}", handlers.Videos)
	r.Get("/grid/{postID}", handlers.Grid)
	r.Get("/oembed", handlers.OEmbed)
	r.Get("/api/{postID}", func(w http.ResponseWriter, r *http.Request) {
		postID := chi.URLParam(r, "postID")
		preferVideo := strings.EqualFold(r.URL.Query().Get("kind"), "reel") || strings.EqualFold(r.URL.Query().Get("prefer"), "video")
		var item *scraper.InstaData
		var err error
		if preferVideo {
			item, err = scraper.GetDataPreferVideo(postID)
		} else {
			item, err = scraper.GetData(postID)
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
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		views.Home(w)
	})

	slog.Info("service listening", "event", "startup", "listen_addr", *listenAddr)
	server := &http.Server{Addr: *listenAddr, Handler: r, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	if err := server.ListenAndServe(); err != nil {
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

// Remove cache from Pebble if already expired
func evictCache() {
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
		c := ttlBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if n, err := strconv.ParseInt(utils.B2S(k), 10, 64); err == nil {
				if n < curTime {
					ttlBucket.Delete(k)
					dataBucket.Delete(v)
				}
			} else {
				slog.Error("Failed to parse expire timestamp in cache", "err", err)
			}
		}
		return nil
	})
	if err != nil {
		observability.Default.RecordDBError("cache_evict", err)
	}
}
