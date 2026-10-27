package handlers

import (
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"instafix/observability"
	"instafix/views"

	"github.com/go-chi/chi/v5"
)

const (
	defaultOGInstagramUpstream         = "https://oginstagram.com"
	maxOGInstagramHTMLBytes            = 2 << 20
	defaultOGInstagramFailureThreshold = 3
	defaultOGInstagramCooldown         = 5 * time.Minute
	minOGInstagramFailureThreshold     = 1
	maxOGInstagramFailureThreshold     = 100
	minOGInstagramCooldown             = time.Second
	maxOGInstagramCooldown             = 24 * time.Hour
)

type ogInstagramProxyConfig struct {
	mode               string
	token              string
	upstream           string
	rewriteMedia       bool
	serverFetchAllowed bool
	failureThreshold   int
	cooldown           time.Duration
}

type ogInstagramProxyCircuitBreaker struct {
	mu                  sync.Mutex
	consecutiveFailures int
	openedAt            time.Time
	probeInFlight       bool
}

var (
	ogInstagramProxyClient = http.Client{Timeout: 12 * time.Second}
	ogInstagramMediaClient = http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConns:          20,
			MaxIdleConnsPerHost:   4,
		},
	}
	ogInstagramProxyBreaker            ogInstagramProxyCircuitBreaker
	ogInstagramBrandPattern            = regexp.MustCompile(`(?i)\bOG[\s_-]*Instagram\b`)
	instagram7FixedPreviewBrandPattern = regexp.MustCompile(`(?i)\bInstagram7\s+fixed\s+preview\b`)
	ogSiteNameMetaPattern              = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']og:site_name["']`)
	ogTitleMetaPattern                 = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']og:title["']`)
	twitterTitleMetaPattern            = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']twitter:title["']`)
	ogDescriptionMetaPattern           = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']og:description["']`)
	twitterDescriptionMetaPattern      = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']twitter:description["']`)
	ogImageAltMetaPattern              = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']og:image:alt["']`)
	twitterImageAltMetaPattern         = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']twitter:image:alt["']`)
	twitterCreatorMetaPattern          = regexp.MustCompile(`(?i)\b(?:property|name)\s*=\s*["']twitter:creator["']`)
	metaTagPattern                     = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	metaContentDoubleValuePattern      = regexp.MustCompile(`(?i)\bcontent\s*=\s*"([^"]*)"`)
	metaContentSingleValuePattern      = regexp.MustCompile(`(?i)\bcontent\s*=\s*'([^']*)'`)
	htmlTitlePattern                   = regexp.MustCompile(`(?is)<title\b[^>]*>.*?</title>`)
	headClosePattern                   = regexp.MustCompile(`(?i)</head\s*>`)
	instagramCreatorPattern            = regexp.MustCompile(`^@[A-Za-z0-9._]{1,30}$`)
	fallbackPromotionPattern           = regexp.MustCompile(`(?i)(og[\s_-]*instagram|\bproxy\b|powered\s+by|fixed?\s+(?:instagram\s+)?preview|better\s+(?:instagram\s+)?embeds?|download\s+(?:this|with)|https?://|www\.)`)
)

type fallbackPreviewText struct {
	title       string
	description string
}

func TryOGInstagramEmbedProxy(w http.ResponseWriter, r *http.Request, postID string, trusted ...fallbackPreviewText) bool {
	cfg := loadOGInstagramProxyConfig()
	ok, reason := cfg.enabledFor(r)
	if !ok {
		return false
	}
	if allowed, remaining := allowOGInstagramProxy(reason, cfg); !allowed {
		observability.Default.RecordOGProxyFallback(r, postID, "circuit_open")
		slog.Warn("oginstagram emergency proxy circuit open",
			"path", r.URL.Path,
			"postID", postID,
			"reason", reason,
			"remaining", remaining.String(),
			"threshold", cfg.failureThreshold,
			"cooldown", cfg.cooldown.String(),
		)
		return false
	}

	upstreamURL, err := cfg.upstreamURL(r)
	if err != nil {
		releaseOGInstagramProxyProbe(reason)
		observability.Default.RecordOGProxyFallback(r, postID, "bad_upstream_url")
		slog.Warn("oginstagram emergency proxy skipped: bad upstream URL", "err", err, "reason", reason)
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		releaseOGInstagramProxyProbe(reason)
		observability.Default.RecordOGProxyFallback(r, postID, "request_create_failed")
		slog.Warn("oginstagram emergency proxy skipped: request creation failed", "err", err)
		return false
	}
	copyOGInstagramRequestHeaders(req, r)
	res, err := ogInstagramProxyClient.Do(req)
	if err != nil || res == nil {
		if err == nil {
			err = http.ErrAbortHandler
		}
		recordOGInstagramProxyFailure(reason, cfg)
		observability.Default.RecordOGProxyUpstreamFailure(r, postID)
		slog.Warn("oginstagram emergency proxy upstream failed", "path", r.URL.Path, "err", err)
		return false
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxOGInstagramHTMLBytes+1))
	if err != nil {
		recordOGInstagramProxyFailure(reason, cfg)
		observability.Default.RecordOGProxyFallback(r, postID, "read_failed")
		slog.Warn("oginstagram emergency proxy read failed", "path", r.URL.Path, "err", err)
		return false
	}
	if len(body) > maxOGInstagramHTMLBytes {
		recordOGInstagramProxyFailure(reason, cfg)
		observability.Default.RecordOGProxyFallback(r, postID, "response_too_large")
		slog.Warn("oginstagram emergency proxy response too large", "path", r.URL.Path)
		return false
	}
	if !usableOGInstagramEmbedResponse(res, body) {
		recordOGInstagramProxyFailure(reason, cfg)
		if res.StatusCode != http.StatusOK {
			observability.Default.RecordOGProxyHTTPFailure(r, postID, res.StatusCode, res.Header)
		} else {
			observability.Default.RecordOGProxyFallback(r, postID, "unusable_response")
		}
		slog.Warn("oginstagram emergency proxy fallback to local renderer",
			"path", r.URL.Path,
			"status", res.StatusCode,
			"x_og_status", res.Header.Get("X-Og-Status"),
			"x_og_reason", res.Header.Get("X-Og-Reason"),
		)
		return false
	}
	recordOGInstagramProxySuccess(reason, cfg)

	publicBase := requestPublicBaseURL(r)
	upstreamBase := strings.TrimRight(cfg.upstream, "/")
	out := rewriteOGInstagramHTML(string(body), upstreamBase, publicBase, trusted...)
	if !cfg.rewriteMedia {
		out = restoreOGInstagramOffloadURLs(out, upstreamBase, publicBase)
	}
	if cfg.rewriteMedia && cfg.tokenMatches(r) && !cfg.modeEnabledWithoutToken(r) {
		out = addOGProxyTokenToOffloadURLs(out, publicBase, cfg.token)
	}

	for _, key := range []string{"Cache-Control", "X-Og-Cache"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Instagram7-Preview-Source", "fallback")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))

	mediaKind := "image"
	if hasMetaProperty(body, "og:video") {
		mediaKind = "video"
	} else if !hasMetaProperty(body, "og:image") {
		mediaKind = "generic"
	}
	observability.Default.RecordOGProxyServed(r, postID, mediaKind)
	slog.Info("oginstagram emergency proxy served", "path", r.URL.Path, "postID", postID, "reason", reason, "media", mediaKind)
	return true
}

func TryOGInstagramOffloadProxy(w http.ResponseWriter, r *http.Request) bool {
	cfg := loadOGInstagramProxyConfig()
	ok, reason := cfg.enabledFor(r)
	if !ok || !cfg.rewriteMedia || !cfg.tokenMatches(r) && !isSignedOGInstagramOffloadRequest(r) {
		return false
	}
	if allowed, remaining := allowOGInstagramProxy(reason, cfg); !allowed {
		observability.Default.RecordOGProxyFallback(r, chi.URLParam(r, "postID"), "circuit_open")
		slog.Warn("oginstagram offload proxy circuit open",
			"path", r.URL.Path,
			"postID", chi.URLParam(r, "postID"),
			"reason", reason,
			"remaining", remaining.String(),
			"threshold", cfg.failureThreshold,
			"cooldown", cfg.cooldown.String(),
		)
		return false
	}

	postID := chi.URLParam(r, "postID")
	upstreamURL, err := cfg.upstreamURL(r)
	if err != nil {
		releaseOGInstagramProxyProbe(reason)
		observability.Default.RecordOGProxyFallback(r, postID, "offload_bad_upstream_url")
		slog.Warn("oginstagram offload proxy skipped: bad upstream URL", "err", err, "reason", reason)
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		releaseOGInstagramProxyProbe(reason)
		observability.Default.RecordOGProxyFallback(r, postID, "offload_request_create_failed")
		slog.Warn("oginstagram offload proxy skipped: request creation failed", "err", err)
		return false
	}
	copyOGInstagramRequestHeaders(req, r)
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	res, err := ogInstagramMediaClient.Do(req)
	if err != nil || res == nil {
		if err == nil {
			err = http.ErrAbortHandler
		}
		recordOGInstagramProxyFailure(reason, cfg)
		observability.Default.RecordOGProxyUpstreamFailure(r, postID)
		slog.Warn("oginstagram offload proxy upstream failed", "path", r.URL.Path, "err", err)
		return false
	}
	if r.Method == http.MethodHead && res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		if res.StatusCode != http.StatusMethodNotAllowed && res.StatusCode != http.StatusNotImplemented {
			status := res.Status
			statusCode := res.StatusCode
			headers := res.Header.Clone()
			res.Body.Close()
			recordOGInstagramProxyFailure(reason, cfg)
			observability.Default.RecordOGProxyHTTPFailure(r, postID, statusCode, headers)
			slog.Warn("oginstagram offload HEAD rejected upstream", "path", r.URL.Path, "postID", postID, "status", status)
			return false
		}
		res.Body.Close()
		probe, probeErr := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
		if probeErr == nil {
			copyOGInstagramRequestHeaders(probe, r)
			rangeHeader := r.Header.Get("Range")
			if rangeHeader == "" {
				rangeHeader = "bytes=0-0"
			}
			probe.Header.Set("Range", rangeHeader)
			res, err = ogInstagramMediaClient.Do(probe)
		} else {
			err = probeErr
		}
		if err != nil || res == nil {
			recordOGInstagramProxyFailure(reason, cfg)
			observability.Default.RecordOGProxyUpstreamFailure(r, postID)
			slog.Warn("oginstagram offload HEAD probe failed", "path", r.URL.Path, "err", err)
			return false
		}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		recordOGInstagramProxyFailure(reason, cfg)
		observability.Default.RecordOGProxyHTTPFailure(r, postID, res.StatusCode, res.Header)
		slog.Warn("oginstagram offload proxy rejected upstream", "path", r.URL.Path, "status", res.Status)
		return false
	}
	recordOGInstagramProxySuccess(reason, cfg)
	finishStream := observability.Default.BeginMediaStream()
	defer finishStream()
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Cache-Control"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	var written int64
	var copyErr error
	if r.Method != http.MethodHead && res.StatusCode/100 == 2 {
		written, copyErr = io.CopyBuffer(w, res.Body, make([]byte, 64*1024))
	}
	observability.Default.RecordMediaStream(r.Method, r.Header.Get("Range") != "", res.StatusCode, written, copyErr)
	if copyErr != nil {
		slog.Warn("oginstagram offload proxy copy failed", "path", r.URL.Path, "postID", postID, "bytes", written, "err", copyErr)
		return true
	}
	slog.Info("oginstagram offload proxy served", "path", r.URL.Path, "postID", postID, "reason", reason, "status", res.StatusCode)
	return true
}

func isSignedOGInstagramOffloadRequest(r *http.Request) bool {
	query := r.URL.Query()
	return query.Get("v") != "" &&
		query.Get("kid") != "" &&
		query.Get("exp") != "" &&
		query.Get("sig") != ""
}

func loadOGInstagramProxyConfig() ogInstagramProxyConfig {
	upstream := strings.TrimSpace(os.Getenv("OGINSTAGRAM_PROXY_UPSTREAM"))
	if upstream == "" {
		upstream = defaultOGInstagramUpstream
	}
	return ogInstagramProxyConfig{
		mode:               strings.ToLower(strings.TrimSpace(os.Getenv("OGINSTAGRAM_PROXY_MODE"))),
		token:              strings.TrimSpace(os.Getenv("OGINSTAGRAM_PROXY_TOKEN")),
		upstream:           strings.TrimRight(upstream, "/"),
		rewriteMedia:       envBool("OGINSTAGRAM_PROXY_REWRITE_MEDIA", true),
		serverFetchAllowed: envBool("OGINSTAGRAM_SERVER_FETCH_ALLOWED", false),
		failureThreshold:   boundedEnvInt("OGINSTAGRAM_PROXY_FAILURE_THRESHOLD", defaultOGInstagramFailureThreshold, minOGInstagramFailureThreshold, maxOGInstagramFailureThreshold),
		cooldown:           boundedEnvDurationSeconds("OGINSTAGRAM_PROXY_COOLDOWN_SECONDS", defaultOGInstagramCooldown, minOGInstagramCooldown, maxOGInstagramCooldown),
	}
}

func allowOGInstagramProxy(reason string, cfg ogInstagramProxyConfig) (bool, time.Duration) {
	if reason == "token" {
		return true, 0
	}

	now := time.Now()
	ogInstagramProxyBreaker.mu.Lock()
	defer ogInstagramProxyBreaker.mu.Unlock()

	if ogInstagramProxyBreaker.openedAt.IsZero() {
		return true, 0
	}
	if ogInstagramProxyBreaker.probeInFlight {
		return false, cfg.cooldown
	}
	remaining := cfg.cooldown - now.Sub(ogInstagramProxyBreaker.openedAt)
	if remaining > 0 {
		return false, remaining
	}
	ogInstagramProxyBreaker.probeInFlight = true
	slog.Info("oginstagram emergency proxy circuit half-open", "cooldown", cfg.cooldown.String())
	return true, 0
}

func recordOGInstagramProxyFailure(reason string, cfg ogInstagramProxyConfig) {
	if reason == "token" {
		return
	}

	ogInstagramProxyBreaker.mu.Lock()
	defer ogInstagramProxyBreaker.mu.Unlock()

	ogInstagramProxyBreaker.probeInFlight = false
	ogInstagramProxyBreaker.consecutiveFailures++
	if ogInstagramProxyBreaker.consecutiveFailures < cfg.failureThreshold {
		slog.Warn("oginstagram emergency proxy upstream failure",
			"failure_count", ogInstagramProxyBreaker.consecutiveFailures,
			"threshold", cfg.failureThreshold,
		)
		return
	}
	ogInstagramProxyBreaker.openedAt = time.Now()
	slog.Warn("oginstagram emergency proxy circuit opened",
		"failure_count", ogInstagramProxyBreaker.consecutiveFailures,
		"threshold", cfg.failureThreshold,
		"cooldown", cfg.cooldown.String(),
	)
}

func recordOGInstagramProxySuccess(reason string, cfg ogInstagramProxyConfig) {
	if reason == "token" {
		return
	}

	ogInstagramProxyBreaker.mu.Lock()
	defer ogInstagramProxyBreaker.mu.Unlock()
	wasOpen := !ogInstagramProxyBreaker.openedAt.IsZero() || ogInstagramProxyBreaker.probeInFlight
	if ogInstagramProxyBreaker.consecutiveFailures == 0 && !wasOpen {
		return
	}
	ogInstagramProxyBreaker.consecutiveFailures = 0
	ogInstagramProxyBreaker.openedAt = time.Time{}
	ogInstagramProxyBreaker.probeInFlight = false
	if wasOpen {
		slog.Info("oginstagram emergency proxy circuit closed", "cooldown", cfg.cooldown.String())
	}
}

func releaseOGInstagramProxyProbe(reason string) {
	if reason == "token" {
		return
	}
	ogInstagramProxyBreaker.mu.Lock()
	ogInstagramProxyBreaker.probeInFlight = false
	ogInstagramProxyBreaker.mu.Unlock()
}

func (c ogInstagramProxyConfig) enabledFor(r *http.Request) (bool, string) {
	if !c.serverFetchAllowed {
		return false, ""
	}
	if c.tokenMatches(r) {
		return true, "token"
	}
	if c.modeEnabledWithoutToken(r) {
		return true, "mode:" + c.mode
	}
	return false, ""
}

func (c ogInstagramProxyConfig) modeEnabledWithoutToken(r *http.Request) bool {
	switch c.mode {
	case "1", "true", "yes", "on", "all":
		return true
	case "telegram":
		return isTelegramBot(r.Header.Get("User-Agent"))
	case "bots":
		ua := strings.ToLower(r.Header.Get("User-Agent"))
		return strings.Contains(ua, "bot") || strings.Contains(ua, "preview") || strings.Contains(ua, "telegram")
	default:
		return false
	}
}

func (c ogInstagramProxyConfig) tokenMatches(r *http.Request) bool {
	if c.token == "" {
		return false
	}
	return r.URL.Query().Get("ogproxy") == c.token || r.Header.Get("X-OGInstagram-Proxy") == c.token
}

func (c ogInstagramProxyConfig) upstreamURL(r *http.Request) (string, error) {
	base, err := url.Parse(c.upstream)
	if err != nil {
		return "", err
	}
	next := *r.URL
	q := next.Query()
	q.Del("ogproxy")
	next.RawQuery = q.Encode()
	base.Path = next.Path
	base.RawQuery = next.RawQuery
	return base.String(), nil
}

func copyOGInstagramRequestHeaders(dst *http.Request, src *http.Request) {
	if ua := src.Header.Get("User-Agent"); ua != "" {
		dst.Header.Set("User-Agent", ua)
	} else {
		dst.Header.Set("User-Agent", "TelegramBot")
	}
	for _, key := range []string{"Accept", "Accept-Language"} {
		if value := src.Header.Get(key); value != "" {
			dst.Header.Set(key, value)
		}
	}
	dst.Header.Set("X-Forwarded-Host", src.Host)
	dst.Header.Set("X-Forwarded-Proto", "https")
}

func usableOGInstagramEmbedResponse(res *http.Response, body []byte) bool {
	if res.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(res.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "text/html") {
		return false
	}
	lower := strings.ToLower(string(body))
	if strings.Contains(lower, "temporarily unavailable") || strings.Contains(lower, "proxy_pool_empty") {
		return false
	}
	return hasMetaProperty(body, "og:image") || hasMetaProperty(body, "og:video")
}

func hasMetaProperty(body []byte, property string) bool {
	lower := strings.ToLower(string(body))
	prop := strings.ToLower(property)
	return strings.Contains(lower, `property="`+prop+`"`) ||
		strings.Contains(lower, `property='`+prop+`'`) ||
		strings.Contains(lower, `name="`+prop+`"`) ||
		strings.Contains(lower, `name='`+prop+`'`)
}

func rewriteOGInstagramHTML(body, upstreamBase, publicBase string, trusted ...fallbackPreviewText) string {
	previewText := deriveFallbackPreviewText(body, trusted...)
	body = rewriteOGInstagramURL(body, upstreamBase, publicBase)
	body = ogInstagramBrandPattern.ReplaceAllString(body, "InstaFix Revived")
	body = strings.ReplaceAll(body, "oginstagram.com", strings.TrimPrefix(strings.TrimPrefix(publicBase, "https://"), "http://"))
	body = instagram7FixedPreviewBrandPattern.ReplaceAllString(body, "Instagram preview")
	body = normalizeFallbackPreviewMetadata(body, previewText)
	return body
}

func deriveFallbackPreviewText(body string, trusted ...fallbackPreviewText) fallbackPreviewText {
	text := fallbackPreviewText{}
	if len(trusted) > 0 {
		text.title = cleanTrustedTitle(trusted[0].title)
		text.description = cleanTrustedDescription(trusted[0].description)
	}
	if text.title == "" {
		text.title = cleanFallbackCreator(extractMetaContent(body, twitterCreatorMetaPattern))
	}
	if text.title == "" {
		text.title = "Instagram preview"
	}
	if text.description == "" {
		text.description = cleanFallbackCaption(extractMetaContent(body, ogImageAltMetaPattern))
	}
	if text.description == "" {
		text.description = cleanFallbackCaption(extractMetaContent(body, twitterImageAltMetaPattern))
	}
	if text.description == "" {
		text.description = "Public Instagram publication preview. Open the original post on Instagram for the full caption."
	}
	return text
}

func cleanTrustedTitle(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "@") {
		value = "@" + value
	}
	if !instagramCreatorPattern.MatchString(value) {
		return ""
	}
	return value
}

func cleanFallbackCreator(value string) string {
	value = strings.TrimSpace(html.UnescapeString(value))
	if !instagramCreatorPattern.MatchString(value) {
		return ""
	}
	return value
}

func cleanTrustedDescription(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	if value == "" {
		return ""
	}
	if len(value) > 2200 {
		value = value[:2200]
	}
	return value
}

func cleanFallbackCaption(value string) string {
	value = strings.TrimSpace(html.UnescapeString(value))
	value = strings.ReplaceAll(value, "\x00", "")
	if value == "" || len(value) > 2200 || fallbackPromotionPattern.MatchString(value) {
		return ""
	}
	return value
}

func extractMetaContent(body string, keyPattern *regexp.Regexp) string {
	for _, tag := range metaTagPattern.FindAllString(body, -1) {
		if !keyPattern.MatchString(tag) {
			continue
		}
		if match := metaContentDoubleValuePattern.FindStringSubmatch(tag); len(match) == 2 {
			return match[1]
		}
		if match := metaContentSingleValuePattern.FindStringSubmatch(tag); len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

func normalizeFallbackPreviewMetadata(body string, text fallbackPreviewText) string {
	escapedTitle := html.EscapeString(text.title)
	escapedDescription := html.EscapeString(text.description)
	if htmlTitlePattern.MatchString(body) {
		body = htmlTitlePattern.ReplaceAllStringFunc(body, func(string) string {
			return "<title>" + escapedTitle + "</title>"
		})
	} else if headClosePattern.MatchString(body) {
		body = headClosePattern.ReplaceAllStringFunc(body, func(string) string {
			return "<title>" + escapedTitle + "</title></head>"
		})
	} else {
		body = "<title>" + escapedTitle + "</title>" + body
	}
	hasOGDescription := false
	hasTwitterDescription := false
	body = metaTagPattern.ReplaceAllStringFunc(body, func(tag string) string {
		switch {
		case ogSiteNameMetaPattern.MatchString(tag):
			return replaceMetaContent(tag, "InstaFix Revived")
		case ogTitleMetaPattern.MatchString(tag), twitterTitleMetaPattern.MatchString(tag):
			return replaceMetaContent(tag, escapedTitle)
		case ogDescriptionMetaPattern.MatchString(tag):
			hasOGDescription = true
			return replaceMetaContent(tag, escapedDescription)
		case twitterDescriptionMetaPattern.MatchString(tag):
			hasTwitterDescription = true
			return replaceMetaContent(tag, escapedDescription)
		case ogImageAltMetaPattern.MatchString(tag), twitterImageAltMetaPattern.MatchString(tag):
			return replaceMetaContent(tag, escapedDescription)
		default:
			return tag
		}
	})
	var missing strings.Builder
	if !hasOGDescription {
		missing.WriteString(`<meta property="og:description" content="`)
		missing.WriteString(escapedDescription)
		missing.WriteString(`">`)
	}
	if !hasTwitterDescription {
		missing.WriteString(`<meta name="twitter:description" content="`)
		missing.WriteString(escapedDescription)
		missing.WriteString(`">`)
	}
	if missing.Len() > 0 {
		if headClosePattern.MatchString(body) {
			body = headClosePattern.ReplaceAllStringFunc(body, func(string) string {
				return missing.String() + "</head>"
			})
		} else {
			body = missing.String() + body
		}
	}
	return body
}

func replaceMetaContent(tag, value string) string {
	if match := metaContentDoubleValuePattern.FindStringSubmatchIndex(tag); len(match) == 4 {
		return tag[:match[2]] + value + tag[match[3]:]
	}
	if match := metaContentSingleValuePattern.FindStringSubmatchIndex(tag); len(match) == 4 {
		return tag[:match[2]] + value + tag[match[3]:]
	}
	return tag
}

func rewriteOGInstagramURL(value, upstreamBase, publicBase string) string {
	value = strings.ReplaceAll(value, upstreamBase, publicBase)
	value = strings.ReplaceAll(value, "https://www.oginstagram.com", publicBase)
	value = strings.ReplaceAll(value, "https://oginstagram.com", publicBase)
	value = strings.ReplaceAll(value, "http://www.oginstagram.com", publicBase)
	value = strings.ReplaceAll(value, "http://oginstagram.com", publicBase)
	return value
}

func restoreOGInstagramOffloadURLs(body, upstreamBase, publicBase string) string {
	body = strings.ReplaceAll(body, publicBase+"/offload/", upstreamBase+"/offload/")
	body = strings.ReplaceAll(body, publicBase+"/favicon-", upstreamBase+"/favicon-")
	body = strings.ReplaceAll(body, publicBase+"/cdn-cgi/", upstreamBase+"/cdn-cgi/")
	return body
}

func addOGProxyTokenToOffloadURLs(body, publicBase, token string) string {
	if token == "" {
		return body
	}
	escaped := url.QueryEscape(token)
	re := regexp.MustCompile(regexp.QuoteMeta(strings.TrimRight(publicBase, "/")) + `/offload/[^"'<\s]+`)
	return re.ReplaceAllStringFunc(body, func(raw string) string {
		if strings.Contains(raw, "ogproxy=") {
			return raw
		}
		if strings.Contains(raw, "?") {
			return raw + "&amp;ogproxy=" + escaped
		}
		return raw + "?ogproxy=" + escaped
	})
}

func requestPublicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	if configured := views.ConfiguredPublicBaseURL(); configured != "" { return configured }
	return scheme + "://" + r.Host
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func boundedEnvInt(name string, fallback, min, max int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < min {
		return min
	}
	if parsed > max {
		return max
	}
	return parsed
}

func boundedEnvDurationSeconds(name string, fallback, min, max time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	minSeconds := int64(min / time.Second)
	maxSeconds := int64(max / time.Second)
	if seconds < minSeconds {
		return min
	}
	if seconds > maxSeconds {
		return max
	}
	return time.Duration(seconds) * time.Second
}
