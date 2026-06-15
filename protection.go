package main

import (
	scraper "instafix/handlers/scraper"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	protectWindow       = time.Minute
	protectUniqueWindow = 10 * time.Minute
	protectRetryAfter   = 30
	unknownProxyClient  = "proxy-unknown"
)

type routeCost string

const (
	costOther routeCost = "other"
	costHome  routeCost = "home"
	costEmbed routeCost = "embed"
	costAPI   routeCost = "api"
	costImage routeCost = "image"
	costVideo routeCost = "video"
	costGrid  routeCost = "grid"
	costOEmb  routeCost = "oembed"
)

type protectionConfig struct {
	Enabled bool
	PerIP   map[routeCost]int
	Global  map[routeCost]int

	MissPerIP        int
	MissGlobal       int
	InvalidPerIP     int
	UniquePerIP      int
	TrackedCacheMiss bool
}

type requestProtector struct {
	mu      sync.Mutex
	clients map[string]*clientProtectionState
	global  map[string]*windowCounter
	config  protectionConfig
	lastLog map[string]time.Time
}

type clientProtectionState struct {
	lastSeen time.Time
	windows  map[string]*windowCounter
	unique   map[string]time.Time
}

type windowCounter struct {
	start time.Time
	count int
}

type routeInfo struct {
	cost       routeCost
	postID     string
	hasPostID  bool
	checkCache bool
}

func newRequestProtectorFromEnv() *requestProtector {
	cfg := protectionConfig{
		Enabled: envBool("REQUEST_PROTECTION_ENABLED", true),
		PerIP: map[routeCost]int{
			costHome:  180,
			costEmbed: 240,
			costAPI:   80,
			costImage: 180,
			costVideo: 100,
			costGrid:  60,
			costOEmb:  120,
			costOther: 180,
		},
		Global: map[routeCost]int{
			costHome:  2400,
			costEmbed: 3000,
			costAPI:   900,
			costImage: 1800,
			costVideo: 900,
			costGrid:  500,
			costOEmb:  900,
			costOther: 1800,
		},
		MissPerIP:        envInt("REQUEST_PROTECTION_CACHE_MISS_PER_IP_PER_MINUTE", 45),
		MissGlobal:       envInt("REQUEST_PROTECTION_CACHE_MISS_GLOBAL_PER_MINUTE", 500),
		InvalidPerIP:     envInt("REQUEST_PROTECTION_INVALID_PER_IP_PER_MINUTE", 40),
		UniquePerIP:      envInt("REQUEST_PROTECTION_UNIQUE_POSTS_PER_IP_10_MINUTES", 180),
		TrackedCacheMiss: envBool("REQUEST_PROTECTION_TRACK_CACHE_MISSES", true),
	}
	cfg.PerIP[costHome] = envInt("REQUEST_PROTECTION_HOME_PER_IP_PER_MINUTE", cfg.PerIP[costHome])
	cfg.PerIP[costEmbed] = envInt("REQUEST_PROTECTION_EMBED_PER_IP_PER_MINUTE", cfg.PerIP[costEmbed])
	cfg.PerIP[costAPI] = envInt("REQUEST_PROTECTION_API_PER_IP_PER_MINUTE", cfg.PerIP[costAPI])
	cfg.PerIP[costImage] = envInt("REQUEST_PROTECTION_IMAGE_PER_IP_PER_MINUTE", cfg.PerIP[costImage])
	cfg.PerIP[costVideo] = envInt("REQUEST_PROTECTION_VIDEO_PER_IP_PER_MINUTE", cfg.PerIP[costVideo])
	cfg.PerIP[costGrid] = envInt("REQUEST_PROTECTION_GRID_PER_IP_PER_MINUTE", cfg.PerIP[costGrid])
	cfg.PerIP[costOEmb] = envInt("REQUEST_PROTECTION_OEMBED_PER_IP_PER_MINUTE", cfg.PerIP[costOEmb])
	cfg.PerIP[costOther] = envInt("REQUEST_PROTECTION_OTHER_PER_IP_PER_MINUTE", cfg.PerIP[costOther])

	cfg.Global[costHome] = envInt("REQUEST_PROTECTION_HOME_GLOBAL_PER_MINUTE", cfg.Global[costHome])
	cfg.Global[costEmbed] = envInt("REQUEST_PROTECTION_EMBED_GLOBAL_PER_MINUTE", cfg.Global[costEmbed])
	cfg.Global[costAPI] = envInt("REQUEST_PROTECTION_API_GLOBAL_PER_MINUTE", cfg.Global[costAPI])
	cfg.Global[costImage] = envInt("REQUEST_PROTECTION_IMAGE_GLOBAL_PER_MINUTE", cfg.Global[costImage])
	cfg.Global[costVideo] = envInt("REQUEST_PROTECTION_VIDEO_GLOBAL_PER_MINUTE", cfg.Global[costVideo])
	cfg.Global[costGrid] = envInt("REQUEST_PROTECTION_GRID_GLOBAL_PER_MINUTE", cfg.Global[costGrid])
	cfg.Global[costOEmb] = envInt("REQUEST_PROTECTION_OEMBED_GLOBAL_PER_MINUTE", cfg.Global[costOEmb])
	cfg.Global[costOther] = envInt("REQUEST_PROTECTION_OTHER_GLOBAL_PER_MINUTE", cfg.Global[costOther])

	p := &requestProtector{clients: make(map[string]*clientProtectionState), global: make(map[string]*windowCounter), config: cfg, lastLog: make(map[string]time.Time)}
	go p.cleanupLoop()
	return p
}

func (p *requestProtector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.config.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if ok, reason := p.allow(r); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(protectRetryAfter))
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			p.logThrottle(reason, clientIP(r), r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *requestProtector) allow(r *http.Request) (bool, string) {
	now := time.Now()
	ip := clientIP(r)
	info := classifyRequest(r.URL.Path)

	p.mu.Lock()
	defer p.mu.Unlock()

	client := p.clients[ip]
	if client == nil {
		client = &clientProtectionState{windows: make(map[string]*windowCounter), unique: make(map[string]time.Time)}
		p.clients[ip] = client
	}
	client.lastSeen = now
	knownClient := ip != unknownProxyClient

	if knownClient && !p.take(client.windows, "ip:"+string(info.cost), p.config.PerIP[info.cost], protectWindow, now) {
		return false, "per_ip_" + string(info.cost)
	}
	if !p.take(p.global, "global:"+string(info.cost), p.config.Global[info.cost], protectWindow, now) {
		return false, "global_" + string(info.cost)
	}

	if info.hasPostID {
		if !validPublicPostID(info.postID) {
			if knownClient && !p.take(client.windows, "invalid", p.config.InvalidPerIP, protectWindow, now) {
				return false, "invalid_post_id"
			}
			return true, ""
		}
		if knownClient {
			client.unique[info.postID] = now
			for postID, seen := range client.unique {
				if now.Sub(seen) > protectUniqueWindow {
					delete(client.unique, postID)
				}
			}
			if p.config.UniquePerIP > 0 && len(client.unique) > p.config.UniquePerIP {
				return false, "unique_posts"
			}
		}

		if info.checkCache && p.config.TrackedCacheMiss && !scraper.HasCachedData(info.postID) {
			if knownClient && !p.take(client.windows, "cache_miss", p.config.MissPerIP, protectWindow, now) {
				return false, "cache_miss_ip"
			}
			if !p.take(p.global, "cache_miss", p.config.MissGlobal, protectWindow, now) {
				return false, "cache_miss_global"
			}
		}
	}

	return true, ""
}

func (p *requestProtector) take(windows map[string]*windowCounter, key string, limit int, window time.Duration, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	w := windows[key]
	if w == nil || now.Sub(w.start) >= window {
		windows[key] = &windowCounter{start: now, count: 1}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

func (p *requestProtector) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		p.mu.Lock()
		for ip, client := range p.clients {
			if now.Sub(client.lastSeen) > 15*time.Minute {
				delete(p.clients, ip)
				continue
			}
			for postID, seen := range client.unique {
				if now.Sub(seen) > protectUniqueWindow {
					delete(client.unique, postID)
				}
			}
		}
		for key, ts := range p.lastLog {
			if now.Sub(ts) > 10*time.Minute {
				delete(p.lastLog, key)
			}
		}
		p.mu.Unlock()
	}
}

func (p *requestProtector) logThrottle(reason, ip string, r *http.Request) {
	now := time.Now()
	key := reason + ":" + ip
	p.mu.Lock()
	last := p.lastLog[key]
	if now.Sub(last) < time.Minute {
		p.mu.Unlock()
		return
	}
	p.lastLog[key] = now
	p.mu.Unlock()
	slog.Warn("request throttled", "reason", reason, "ip", ip, "path", r.URL.Path, "ua", trimForLog(r.UserAgent(), 160))
}

func classifyRequest(path string) routeInfo {
	path = strings.Trim(path, "/")
	if path == "" || path == "robots.txt" || path == "sitemap.xml" || path == "site-preview.svg" {
		return routeInfo{cost: costHome}
	}
	parts := strings.Split(path, "/")
	switch parts[0] {
	case "api":
		return routeInfo{cost: costAPI, postID: partAt(parts, 1), hasPostID: len(parts) >= 2, checkCache: true}
	case "images":
		return routeInfo{cost: costImage, postID: partAt(parts, 1), hasPostID: len(parts) >= 2, checkCache: true}
	case "videos":
		return routeInfo{cost: costVideo, postID: partAt(parts, 1), hasPostID: len(parts) >= 2, checkCache: true}
	case "grid":
		return routeInfo{cost: costGrid, postID: partAt(parts, 1), hasPostID: len(parts) >= 2, checkCache: true}
	case "oembed":
		return routeInfo{cost: costOEmb}
	case "p", "reel", "reels", "tv":
		return routeInfo{cost: costEmbed, postID: partAt(parts, 1), hasPostID: len(parts) >= 2, checkCache: true}
	case "stories":
		return routeInfo{cost: costEmbed, postID: partAt(parts, 2), hasPostID: len(parts) >= 3, checkCache: true}
	default:
		if len(parts) >= 3 && (parts[1] == "p" || parts[1] == "reel") {
			return routeInfo{cost: costEmbed, postID: parts[2], hasPostID: true, checkCache: true}
		}
		return routeInfo{cost: costOther}
	}
}

func partAt(parts []string, idx int) string {
	if idx >= 0 && idx < len(parts) {
		return parts[idx]
	}
	return ""
}

func validPublicPostID(postID string) bool {
	if len(postID) < 6 || len(postID) > 32 {
		return false
	}
	for _, r := range postID {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func clientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
	remoteIP := net.ParseIP(remoteHost)
	if remoteIP != nil && (remoteIP.IsLoopback() || remoteIP.IsPrivate()) {
		for _, header := range []string{"CF-Connecting-IP", "X-Real-IP", "X-Forwarded-For"} {
			value := r.Header.Get(header)
			if value == "" {
				continue
			}
			if header == "X-Forwarded-For" {
				value = strings.Split(value, ",")[0]
			}
			value = strings.TrimSpace(value)
			if ip := net.ParseIP(value); ip != nil {
				return ip.String()
			}
		}
		return unknownProxyClient
	}
	if remoteIP != nil {
		return remoteIP.String()
	}
	return remoteHost
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func trimForLog(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
