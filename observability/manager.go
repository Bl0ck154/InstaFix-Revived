package observability

import (
	"hash/fnv"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct{}

type scrapeBucket struct {
	minute           int64
	success, failure uint64
}

type Manager struct {
	started                                                     time.Time
	requests, cacheHits, scrapeSuccess, scrapeFailure, dbErrors atomic.Uint64
	status                                                      [6]atomic.Uint64
	mu                                                          sync.Mutex
	day                                                         string
	users, posts                                                []uint64
	buckets                                                     [15]scrapeBucket
}

var Default = New(Config{})

func New(cfg Config) *Manager {
	m := &Manager{started: time.Now().UTC(), users: make([]uint64, 1024), posts: make([]uint64, 1024)}
	m.day = m.started.Format("2006-01-02")
	return m
}

func Configure(cfg Config) { Default = New(cfg) }

func hash(s string) uint64 { h := fnv.New64a(); _, _ = h.Write([]byte(s)); return h.Sum64() }

func (m *Manager) rotateDayLocked(now time.Time) {
	d := now.UTC().Format("2006-01-02")
	if d != m.day {
		m.day = d
		clear(m.users)
		clear(m.posts)
	}
}

func addEstimate(bits []uint64, value string) {
	n := hash(value) % uint64(len(bits)*64)
	bits[n/64] |= 1 << (n % 64)
}

func estimate(bits []uint64) int {
	zero := 0
	for _, b := range bits {
		zero += 64 - bitsOnesCount(b)
	}
	if zero == 0 {
		return len(bits) * 64
	}
	m := float64(len(bits) * 64)
	return int(-m * math.Log(float64(zero)/m))
}

func bitsOnesCount(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}

func (m *Manager) RecordCacheHit() { m.cacheHits.Add(1) }

func (m *Manager) RecordScrape(success bool, postID string, err error) {
	if success {
		m.scrapeSuccess.Add(1)
	} else {
		m.scrapeFailure.Add(1)
	}
	now := time.Now().UTC()
	minute := now.Unix() / 60
	m.mu.Lock()
	m.rotateDayLocked(now)
	if postID != "" {
		addEstimate(m.posts, postID)
	}
	b := &m.buckets[minute%15]
	if b.minute != minute {
		*b = scrapeBucket{minute: minute}
	}
	if success {
		b.success++
	} else {
		b.failure++
	}
	m.mu.Unlock()
	if !success {
		slog.Error("final Instagram scrape failed", "event", "scrape_final", "post_id", postID, "err", err)
	} else {
		slog.Info("Instagram scrape succeeded", "event", "scrape_final", "post_id", postID)
	}
}

func (m *Manager) RecordDBError(operation string, err error) {
	m.dbErrors.Add(1)
	slog.Error("database operation failed", "event", "db_error", "operation", operation, "err", err)
}

func (m *Manager) RecordAuthHelperResult(success bool, postID, code string, err error) {
	if success {
		slog.Info("auth helper fallback succeeded", "event", "auth_helper", "post_id", postID)
		return
	}
	slog.Warn("auth helper fallback failed", "event", "auth_helper", "post_id", postID, "code", code, "err", err)
}

func (m *Manager) Alert(text string) { slog.Warn("operator alert", "message", text) }

func (m *Manager) AlertStartup() {
	slog.Info("InstaFix Revived started", "event", "startup", "time", time.Now().UTC().Format(time.RFC3339))
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		code := rw.status
		if code == 0 {
			code = 200
		}
		m.requests.Add(1)
		if code/100 >= 0 && code/100 < len(m.status) {
			m.status[code/100].Add(1)
		}
		now := time.Now().UTC()
		m.mu.Lock()
		m.rotateDayLocked(now)
		if ip := clientIP(r); ip != "" {
			addEstimate(m.users, m.day+":"+ip)
		}
		if p := postID(r.URL.Path); p != "" {
			addEstimate(m.posts, p)
		}
		m.mu.Unlock()
		slog.Info("request completed", "event", "request", "method", r.Method, "path", r.URL.Path, "status", code, "duration_ms", time.Since(start).Milliseconds(), "client", clientClass(r.UserAgent()))
	})
}

func clientIP(r *http.Request) string {
	v := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
	if net.ParseIP(v) != nil {
		return v
	}
	v = strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if net.ParseIP(v) != nil {
		return v
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return h
	}
	return r.RemoteAddr
}

func postID(path string) string {
	for _, v := range strings.Split(strings.Trim(path, "/"), "/") {
		if len(v) >= 6 && (v[0] == 'B' || v[0] == 'C' || v[0] == 'D') {
			return v
		}
	}
	return ""
}

func clientClass(ua string) string {
	u := strings.ToLower(ua)
	switch {
	case strings.Contains(u, "telegrambot"):
		return "telegram"
	case strings.Contains(u, "discordbot"):
		return "discord"
	case strings.Contains(u, "whatsapp"):
		return "whatsapp"
	case strings.Contains(u, "bot") || strings.Contains(u, "crawler") || strings.Contains(u, "spider"):
		return "bot"
	default:
		return "browser"
	}
}

func Percent(n, d uint64) float64 {
	if d == 0 {
		return 0
	}
	return math.Round(1000*float64(n)/float64(d)) / 10
}
