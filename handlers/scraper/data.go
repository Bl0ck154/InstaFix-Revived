package handlers

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"instafix/observability"
	"instafix/utils"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kelindar/binary"
	"github.com/klauspost/compress/gzhttp"
	"github.com/klauspost/compress/zstd"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
	"github.com/tidwall/gjson"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

var (
	RemoteScraperAddr  string
	AuthHelperURL      string
	ErrNotFound        = errors.New("post not found")
	ErrRestricted      = errors.New("Instagram content restricted")
	ErrAuthUnavailable = errors.New("cookie pool unavailable")
	timeout            = 5 * time.Second
	transport          http.RoundTripper
	transportNoProxy   *http.Transport
	sflightScraper     singleflight.Group
	sflightAuthHelper  singleflight.Group
	authHelperSlots    = make(chan struct{}, max(1, envInt("INSTAFIX_AUTH_HELPER_MAX_CONCURRENT", 1)))
	embedAuthFallback  = envBool("INSTAFIX_EMBED_AUTH_FALLBACK", false)
	embedAuthNegTTL    = envDurationSeconds("INSTAFIX_EMBED_AUTH_NEGATIVE_TTL_SECONDS", time.Hour)
	embedAuthNegMu     sync.Mutex
	embedAuthNeg       = make(map[string]embedAuthNegative)
	authHealthMu       sync.Mutex
	authHealthUntil    time.Time
	authHealthStatus   authHealth
	cacheFreshTTL      = envDurationSeconds("INSTAFIX_CACHE_FRESH_TTL_SECONDS", 24*time.Hour)
	cacheStaleTTL      = envDurationSeconds("INSTAFIX_CACHE_STALE_TTL_SECONDS", 30*24*time.Hour)
	negativeCacheTTL   = envDurationSeconds("INSTAFIX_NEGATIVE_CACHE_TTL_SECONDS", 6*time.Hour)
)

type authHealth struct {
	checked   bool
	available int
	total     int
}

type embedAuthNegative struct {
	until  time.Time
	reason string
}

const (
	maxRemoteScraperBodyBytes    int64 = 1 << 20
	maxRemoteScraperDecodedBytes int64 = 2 << 20
	maxInstagramEmbedBodyBytes   int64 = 2 << 20
	maxTimeSliceJSONBytes        int64 = 2 << 20
	maxGraphQLBodyBytes          int64 = 2 << 20
	maxAuthHelperBodyBytes       int64 = 256 << 10
	maxRestrictionBodyBytes      int64 = 64 << 10
	maxCacheEntryBytes                 = 512 << 10
	maxMediaItems                      = 20
	maxMediaURLLength                  = 8192
	maxCaptionLength                   = 4096
	maxUsernameLength                  = 128
)

//go:embed dictionary.bin
var zstdDict []byte

type Media struct {
	TypeName     string
	URL          string
	ThumbnailURL string
	Width        int
	Height       int
}

func (m Media) IsVideo() bool { return strings.Contains(strings.ToLower(m.TypeName), "video") }
func (m Media) IsImage() bool { return !m.IsVideo() }

type InstaData struct {
	PostID   string
	Username string
	Caption  string
	Medias   []Media
}

func (i *InstaData) HasVideo() bool {
	if i == nil {
		return false
	}
	for _, media := range i.Medias {
		if media.IsVideo() {
			return true
		}
	}
	return false
}

func init() {
	transport = gzhttp.Transport(http.DefaultTransport, gzhttp.TransportAlwaysDecompress(true))
	transportNoProxy = http.DefaultTransport.(*http.Transport).Clone()
	transportNoProxy.Proxy = nil // Skip any proxy
}

func GetData(postID string) (*InstaData, error) {
	return getData(postID, true)
}

func GetDataQuiet(postID string) (*InstaData, error) {
	return getData(postID, false)
}

func getData(postID string, recordScrape bool) (*InstaData, error) {
	if !validPostID(postID) {
		return nil, errors.New("postID is not a valid Instagram post ID")
	}

	cached, cacheFound, cacheFresh, err := loadDataFromCache(postID)
	if err != nil {
		observability.Default.RecordDBError("cache_read", err)
		return nil, err
	}

	if cacheFresh {
		observability.Default.RecordCacheHit()
		return cached, nil
	}

	if reason, ok := negativeCacheHit(postID); ok {
		if cacheFound {
			observability.Default.RecordCacheHit()
			slog.Debug("Using stale cached data due to negative cache", "postID", postID, "reason", reason)
			return cached, nil
		}
		return nil, errorForNegativeReason(reason)
	}

	sflightKey := postID
	if !recordScrape {
		sflightKey += ":quiet"
	}
	ret, err, _ := sflightScraper.Do(sflightKey, func() (interface{}, error) {
		item := new(InstaData)
		item.PostID = postID
		var scrapeErr error
		if recordScrape {
			scrapeErr = item.ScrapeData()
		} else {
			scrapeErr = item.ScrapeDataNoAuth()
		}
		if scrapeErr != nil {
			if recordScrape {
				observability.Default.RecordScrape(false, item.PostID, scrapeErr)
			}
			return nil, scrapeErr
		}

		if err := normalizeMediaURLs(item); err != nil {
			slog.Error("Failed to normalize media URLs", "postID", item.PostID, "err", err)
			return false, err
		}

		if err := saveDataToCache(item); err != nil {
			return false, err
		}
		if recordScrape {
			observability.Default.RecordScrape(true, item.PostID, nil)
		}
		return item, nil
	})
	if err != nil {
		saveNegativeCacheIfUseful(postID, err)
		if cacheFound {
			observability.Default.RecordCacheHit()
			slog.Warn("Using stale cached data after scrape failure", "postID", postID, "err", err)
			return cached, nil
		}
		return nil, err
	}
	return ret.(*InstaData), nil
}

func GetDataPreferVideo(postID string) (*InstaData, error) {
	return getDataPreferVideo(postID, true)
}

func GetDataPreferVideoQuiet(postID string) (*InstaData, error) {
	return getDataPreferVideo(postID, false)
}

func getDataPreferVideo(postID string, recordScrape bool) (*InstaData, error) {
	item, err := getData(postID, recordScrape)
	if err == nil && item.HasVideo() {
		return item, nil
	}
	if !recordScrape {
		if err != nil {
			return nil, err
		}
		return item, nil
	}
	refreshed, refreshErr := RefreshDataFromAuthHelper(postID)
	if refreshErr == nil && len(refreshed.Medias) > 0 {
		return refreshed, nil
	}
	if refreshErr != nil {
		slog.Debug("Failed to refresh video data from auth helper", "postID", postID, "err", refreshErr)
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

func RefreshDataFromAuthHelper(postID string) (*InstaData, error) {
	if !validPostID(postID) {
		return nil, errors.New("postID is not a valid Instagram post ID")
	}
	if reason, ok := negativeCacheHit(postID); ok {
		return nil, errorForNegativeReason(reason)
	}
	item := &InstaData{PostID: postID}
	if err := scrapeAuthHelperSingleflight(item); err != nil {
		saveNegativeCacheIfUseful(postID, err)
		return nil, err
	}
	if len(item.Medias) == 0 {
		return nil, ErrNotFound
	}
	if err := normalizeMediaURLs(item); err != nil {
		return nil, err
	}
	if err := saveDataToCache(item); err != nil {
		slog.Warn("Failed to save auth helper refresh to cache", "postID", item.PostID, "err", err)
	}
	return item, nil
}

func GetDataEmbedAuthFallback(postID string) (*InstaData, error) {
	if !embedAuthFallback {
		return nil, ErrNotFound
	}
	if !validPostID(postID) {
		return nil, errors.New("postID is not a valid Instagram post ID")
	}
	if reason, ok := embedAuthNegativeHit(postID); ok {
		return nil, fmt.Errorf("embed auth fallback negative cache: %s", reason)
	}
	item := &InstaData{PostID: postID}
	if err := scrapeAuthHelperSingleflight(item); err != nil {
		saveEmbedAuthNegative(postID, err)
		return nil, err
	}
	if len(item.Medias) == 0 {
		err := ErrNotFound
		saveEmbedAuthNegative(postID, err)
		return nil, err
	}
	if err := normalizeMediaURLs(item); err != nil {
		return nil, err
	}
	if err := saveDataToCache(item); err != nil {
		slog.Warn("Failed to save embed auth fallback to cache", "postID", item.PostID, "err", err)
	}
	return item, nil
}

func normalizeMediaURLs(item *InstaData) error {
	if len(item.Username) > maxUsernameLength {
		item.Username = item.Username[:maxUsernameLength]
	}
	if len(item.Caption) > maxCaptionLength {
		item.Caption = item.Caption[:maxCaptionLength]
	}
	if len(item.Medias) > maxMediaItems {
		item.Medias = item.Medias[:maxMediaItems]
	}
	// Replace public image CDN hosts with scontent.cdninstagram.com while preserving
	// original video CDN hosts. Video URLs are signed and host-sensitive.
	for n, media := range item.Medias {
		if len(media.URL) > maxMediaURLLength {
			return fmt.Errorf("media URL too large")
		}
		if len(media.ThumbnailURL) > maxMediaURLLength {
			return fmt.Errorf("media thumbnail URL too large")
		}
		u, err := url.Parse(media.URL)
		if err != nil {
			return err
		}
		if !media.IsVideo() {
			u.Host = "scontent.cdninstagram.com"
		}
		item.Medias[n].URL = u.String()
		if media.ThumbnailURL != "" {
			thumb, err := url.Parse(media.ThumbnailURL)
			if err == nil && thumb.Host != "" && (thumb.Scheme == "http" || thumb.Scheme == "https") {
				thumb.Host = "scontent.cdninstagram.com"
				item.Medias[n].ThumbnailURL = thumb.String()
			}
		}
		if media.Width < 0 {
			item.Medias[n].Width = 0
		}
		if media.Height < 0 {
			item.Medias[n].Height = 0
		}
	}
	return nil
}

func saveDataToCache(item *InstaData) error {
	bb, err := binary.Marshal(item)
	if err != nil {
		slog.Error("Failed to marshal data", "postID", item.PostID, "err", err)
		return err
	}
	if len(bb) > maxCacheEntryBytes {
		return fmt.Errorf("cache entry too large: %d bytes", len(bb))
	}

	now := time.Now()
	freshExp := now.Add(cacheFreshTTL)
	staleExp := now.Add(cacheStaleTTL)
	if staleExp.Before(freshExp) {
		staleExp = freshExp
	}
	err = DB.Batch(func(tx *bolt.Tx) error {
		dataBucket := tx.Bucket([]byte("data"))
		if dataBucket == nil {
			return nil
		}
		deleteTTLForPost(tx.Bucket([]byte("ttl")), item.PostID)
		dataBucket.Put(utils.S2B(item.PostID), bb)

		ttlBucket := tx.Bucket([]byte("ttl"))
		if ttlBucket == nil {
			return nil
		}
		expTime := strconv.FormatInt(staleExp.UnixNano(), 10)
		ttlBucket.Put(utils.S2B(expTime), utils.S2B(item.PostID))

		if freshBucket := tx.Bucket([]byte("fresh")); freshBucket != nil {
			freshBucket.Put(utils.S2B(item.PostID), utils.S2B(strconv.FormatInt(freshExp.UnixNano(), 10)))
		}
		if negativeBucket := tx.Bucket([]byte("negative")); negativeBucket != nil {
			negativeBucket.Delete(utils.S2B(item.PostID))
		}
		return nil
	})
	if err != nil {
		slog.Error("Failed to save data to cache", "postID", item.PostID, "err", err)
		observability.Default.RecordDBError("cache_write", err)
		return err
	}
	return nil
}

func loadDataFromCache(postID string) (*InstaData, bool, bool, error) {
	item := &InstaData{PostID: postID}
	fresh := false
	found := false
	err := DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(postID))
		if v == nil {
			return nil
		}
		if len(v) > maxCacheEntryBytes {
			slog.Warn("Cached data too large; ignoring stale cache", "postID", postID, "size", len(v))
			return nil
		}
		if err := binary.Unmarshal(v, item); err != nil {
			slog.Warn("Failed to unmarshal cached data; ignoring stale cache", "postID", postID, "err", err)
			return nil
		}
		if len(item.Medias) == 0 {
			return nil
		}
		if err := normalizeMediaURLs(item); err != nil {
			slog.Warn("Cached data failed validation; ignoring stale cache", "postID", postID, "err", err)
			return nil
		}
		found = true
		fresh = isCacheFresh(tx, postID, time.Now())
		return nil
	})
	if err != nil {
		return nil, false, false, err
	}
	if found {
		slog.Debug("Data parsed from cache", "postID", postID, "fresh", fresh)
	}
	return item, found, fresh, nil
}

func isCacheFresh(tx *bolt.Tx, postID string, now time.Time) bool {
	b := tx.Bucket([]byte("fresh"))
	if b != nil {
		raw := b.Get([]byte(postID))
		if raw != nil {
			exp, err := strconv.ParseInt(utils.B2S(raw), 10, 64)
			return err == nil && exp > now.UnixNano()
		}
	}
	// Backward compatibility for entries written before the separate fresh bucket
	// existed: the old ttl bucket meant 24h freshness and hard eviction.
	if ttlBucket := tx.Bucket([]byte("ttl")); ttlBucket != nil {
		c := ttlBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if utils.B2S(v) != postID {
				continue
			}
			exp, err := strconv.ParseInt(utils.B2S(k), 10, 64)
			return err == nil && exp > now.UnixNano()
		}
	}
	return false
}

func deleteTTLForPost(ttlBucket *bolt.Bucket, postID string) {
	if ttlBucket == nil {
		return
	}
	c := ttlBucket.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if utils.B2S(v) == postID {
			_ = c.Delete()
		}
	}
}

func scrapeAuthHelperSingleflight(i *InstaData) error {
	ret, err, _ := sflightAuthHelper.Do(i.PostID, func() (interface{}, error) {
		item := &InstaData{PostID: i.PostID}
		if err := acquireAuthHelperSlot(); err != nil {
			return nil, err
		}
		defer releaseAuthHelperSlot()
		if err := scrapeAuthHelper(item); err != nil {
			return nil, err
		}
		return item, nil
	})
	if err != nil {
		return err
	}
	item, ok := ret.(*InstaData)
	if !ok || item == nil {
		return errors.New("auth helper returned invalid shared result")
	}
	*i = *item
	return nil
}

func acquireAuthHelperSlot() error {
	select {
	case authHelperSlots <- struct{}{}:
		return nil
	case <-time.After(envDurationSeconds("INSTAFIX_AUTH_HELPER_ACQUIRE_TIMEOUT_SECONDS", 2*time.Second)):
		return fmt.Errorf("auth helper busy")
	}
}

func releaseAuthHelperSlot() {
	select {
	case <-authHelperSlots:
	default:
	}
}

func embedAuthNegativeHit(postID string) (string, bool) {
	if embedAuthNegTTL <= 0 || postID == "" {
		return "", false
	}
	now := time.Now()
	embedAuthNegMu.Lock()
	entry, ok := embedAuthNeg[postID]
	if ok && now.After(entry.until) {
		delete(embedAuthNeg, postID)
		ok = false
	}
	embedAuthNegMu.Unlock()
	if !ok {
		return "", false
	}
	return entry.reason, true
}

func saveEmbedAuthNegative(postID string, err error) {
	if embedAuthNegTTL <= 0 || postID == "" || err == nil {
		return
	}
	ttl := embedAuthNegTTL
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "busy") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") {
		ttl = minDuration(ttl, time.Minute)
	} else if errors.Is(err, ErrAuthUnavailable) || strings.Contains(msg, "auth_circuit_open") || strings.Contains(msg, "cooling down") {
		ttl = minDuration(ttl, 5*time.Minute)
	}
	embedAuthNegMu.Lock()
	if len(embedAuthNeg) > 4096 {
		for k, v := range embedAuthNeg {
			if time.Now().After(v.until) {
				delete(embedAuthNeg, k)
			}
		}
	}
	embedAuthNeg[postID] = embedAuthNegative{until: time.Now().Add(ttl), reason: compactErrorReason(err)}
	embedAuthNegMu.Unlock()
}

func compactErrorReason(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return msg
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 || a > b {
		return b
	}
	return a
}

func negativeCacheHit(postID string) (string, bool) {
	if negativeCacheTTL <= 0 || DB == nil || postID == "" {
		return "", false
	}
	now := time.Now().UnixNano()
	reason := ""
	expired := false
	_ = DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("negative"))
		if b == nil {
			return nil
		}
		raw := utils.B2S(b.Get([]byte(postID)))
		if raw == "" {
			return nil
		}
		parts := strings.SplitN(raw, "\t", 2)
		exp, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || exp <= now {
			expired = true
			return nil
		}
		if len(parts) == 2 {
			reason = parts[1]
		} else {
			reason = "unavailable"
		}
		return nil
	})
	if expired {
		_ = DB.Batch(func(tx *bolt.Tx) error {
			if b := tx.Bucket([]byte("negative")); b != nil {
				return b.Delete([]byte(postID))
			}
			return nil
		})
	}
	return reason, reason != ""
}

func saveNegativeCacheIfUseful(postID string, err error) {
	if negativeCacheTTL <= 0 || DB == nil || postID == "" {
		return
	}
	reason, ok := negativeReason(err)
	if !ok {
		return
	}
	exp := strconv.FormatInt(time.Now().Add(negativeCacheTTL).UnixNano(), 10)
	value := exp + "\t" + reason
	if dbErr := DB.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("negative"))
		if b == nil {
			return nil
		}
		return b.Put([]byte(postID), []byte(value))
	}); dbErr != nil {
		observability.Default.RecordDBError("negative_cache_write", dbErr)
	}
}

func negativeReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, ErrRestricted) {
		return "restricted", true
	}
	if errors.Is(err, ErrNotFound) {
		return "not_found", true
	}
	msg := strings.ToLower(err.Error())
	for _, token := range []string{"login_required", "checkpoint", "challenge", "cookie_missing", "auth_helper_unreachable", "context deadline", "timeout", "connection refused", "too many", " 429", "http 429", "http 5"} {
		if strings.Contains(msg, token) {
			return "", false
		}
	}
	for _, token := range []string{"geoblock", "restricted", "private", "deleted", "not found", "unavailable", "media info http 400", "instagram_error", "scrapefromgql is blocked"} {
		if strings.Contains(msg, token) {
			return token, true
		}
	}
	return "", false
}

func errorForNegativeReason(reason string) error {
	switch reason {
	case "restricted", "geoblock":
		return ErrRestricted
	case "not_found", "private", "deleted":
		return ErrNotFound
	default:
		return fmt.Errorf("Instagram content unavailable: %s", reason)
	}
}

func envDurationSeconds(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
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
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func validPostID(postID string) bool {
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

func readLimitedBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("invalid body limit")
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body too large: limit %d bytes", limit)
	}
	return body, nil
}

func readLimitedHTTPBody(res *http.Response, limit int64) ([]byte, error) {
	if res.ContentLength > limit {
		return nil, fmt.Errorf("response body too large: content-length %d", res.ContentLength)
	}
	return readLimitedBody(res.Body, limit)
}

func decodeRemoteScraperBody(body []byte) ([]byte, error) {
	reader, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderLowmem(true), zstd.WithDecoderDicts(zstdDict))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return readLimitedBody(reader, maxRemoteScraperDecodedBytes)
}

func (i *InstaData) ScrapeData() error {
	return i.scrapeData(true)
}

func (i *InstaData) ScrapeDataNoAuth() error {
	return i.scrapeData(false)
}

func (i *InstaData) scrapeData(allowAuth bool) error {
	// Scrape from remote scraper if available
	if len(RemoteScraperAddr) > 0 {
		remoteClient := http.Client{Transport: transportNoProxy, Timeout: timeout}
		req, err := http.NewRequest("GET", RemoteScraperAddr+"/scrape/"+i.PostID, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept-Encoding", "zstd.dict")
		res, err := remoteClient.Do(req)
		if err == nil && res != nil {
			defer res.Body.Close()
			remoteData, err := readLimitedHTTPBody(res, maxRemoteScraperBodyBytes)
			if err == nil && res.StatusCode == 200 {
				remoteDecomp, err := decodeRemoteScraperBody(remoteData)
				if err != nil {
					slog.Warn("remote scraper decode failed; using local fallback", "postID", i.PostID, "err", err)
				} else if err := binary.Unmarshal(remoteDecomp, i); err == nil {
					if len(i.Username) > 0 && len(i.Medias) > 0 {
						slog.Info("Data parsed from remote scraper", "postID", i.PostID)
						return nil
					}
				}
			}
			slog.Warn("Failed to scrape data from remote scraper", "postID", i.PostID, "status", res.StatusCode, "err", err)
		}
		if err != nil {
			slog.Warn("Failed when trying to scrape data from remote scraper", "postID", i.PostID, "err", err)
		}
	}

	client := http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequest("GET", "https://www.instagram.com/p/"+i.PostID+"/embed/captioned/", nil)
	if err != nil {
		return err
	}

	var body []byte
	for retries := 0; retries < 3; retries++ {
		err := func() error {
			res, err := client.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				return errors.New("status code is not 200")
			}

			body, err = readLimitedHTTPBody(res, maxInstagramEmbedBodyBytes)
			if err != nil {
				return err
			}
			return nil
		}()
		if err == nil {
			break
		}
	}

	var embedData gjson.Result
	var timeSliceData gjson.Result
	if len(body) > 0 {
		var scriptText []byte

		// TimeSliceImpl (very fragile)
		for _, line := range bytes.Split(body, []byte("\n")) {
			if bytes.Contains(line, []byte("shortcode_media")) {
				scriptText = line
				break
			}
		}

		if len(scriptText) > 0 {
			// Remove <script>
			findFirstMoreThan := bytes.Index(scriptText, []byte(">"))
			if findFirstMoreThan < 0 {
				scriptText = nil
			} else {
				scriptText = scriptText[findFirstMoreThan+1:]
			}
			if int64(len(scriptText)) > maxTimeSliceJSONBytes {
				slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "script too large")
				scriptText = nil
			}

			lexer := js.NewLexer(parse.NewInputBytes(scriptText))
			for {
				tt, text := lexer.Next()
				if tt == js.ErrorToken || text == nil {
					break
				}
				if tt == js.StringToken && bytes.Contains(text, []byte("shortcode_media")) {
					// Strip quotes from start and end
					text = text[1 : len(text)-1]
					if int64(len(text)) > maxTimeSliceJSONBytes {
						slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "JSON string too large")
						continue
					}
					unescapeData := utils.UnescapeJSONString(utils.B2S(text))
					if int64(len(unescapeData)) > maxTimeSliceJSONBytes {
						slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "unescaped JSON too large")
						continue
					}
					if !gjson.Valid(unescapeData) {
						slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "invalid JSON")
						continue
					}
					timeSliceData = gjson.Parse(unescapeData).Get("gql_data")
				}
			}
		} else {
			slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "No script found")
		}

		// Scrape from embed HTML
		embedHTML, err := scrapeFromEmbedHTML(body)
		if err != nil {
			slog.Debug("Failed to parse data from scrapeFromEmbedHTML", "postID", i.PostID, "err", err)
		} else {
			embedData = gjson.Parse(embedHTML)
		}
	}

	var gqlData gjson.Result
	videoBlocked := bytes.Contains(body, []byte("WatchOnInstagram"))
	// Scrape from GraphQL API only if video is blocked or embed data is empty
	if videoBlocked || len(body) == 0 || (!timeSliceData.Exists() && !embedData.Exists()) {
		gqlValue, err := scrapeFromGQL(i.PostID)
		if err != nil {
			slog.Debug("Failed to scrape data from scrapeFromGQL", "postID", i.PostID, "err", err)
		}
		if gqlValue != nil && !bytes.Contains(gqlValue, []byte("require_login")) {
			gqlData = gjson.Parse(utils.B2S(gqlValue)).Get("data")
			slog.Info("Data parsed from GraphQL API", "postID", i.PostID)
		}
	}

	// If gqlData is blocked, use timeSliceData or embedData
	if !gqlData.Exists() {
		if timeSliceData.Exists() {
			gqlData = timeSliceData
			slog.Info("Data parsed from TimeSliceImpl", "postID", i.PostID)
		} else {
			gqlData = embedData
			if embedData.Exists() {
				slog.Info("Data parsed from embedHTML", "postID", i.PostID)
			}
		}
	}

	status := gqlData.Get("status").String()
	item := gqlData.Get("shortcode_media")
	if !item.Exists() {
		item = gqlData.Get("xdt_shortcode_media")
		if !item.Exists() {
			if err := scrapePublicOEmbed(i); err == nil {
				slog.Info("Data parsed from public oEmbed fallback", "postID", i.PostID)
				return nil
			} else {
				slog.Debug("Failed to scrape data from public oEmbed fallback", "postID", i.PostID, "err", err)
			}
			if allowAuth {
				if err := scrapeAuthHelperSingleflight(i); err == nil {
					return nil
				} else if err != ErrNotFound {
					slog.Debug("Failed to scrape data from auth helper", "postID", i.PostID, "err", err)
				}
			}
			if status == "fail" {
				if err := scrapeRestriction(i.PostID); err != nil {
					return err
				}
				return errors.New("scrapeFromGQL is blocked")
			}
			if err := scrapeRestriction(i.PostID); err != nil {
				return err
			}
			return ErrNotFound
		}
	}

	media := []gjson.Result{item}
	if item.Get("edge_sidecar_to_children").Exists() {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	}
	if len(media) > maxMediaItems {
		media = media[:maxMediaItems]
	}

	// Get username
	i.Username = item.Get("owner.username").String()

	// Get caption
	i.Caption = strings.TrimSpace(item.Get("edge_media_to_caption.edges.0.node.text").String())

	// Get medias
	i.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		thumbnailURL := ""
		displayURL := strings.TrimSpace(m.Get("display_url").String())
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		} else if displayURL != "" {
			if u, err := url.Parse(displayURL); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
				thumbnailURL = displayURL
			}
		}
		rawURL := mediaURL.String()
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		typeName := m.Get("__typename").String()
		if strings.Contains(strings.ToLower(typeName), "video") && !m.Get("video_url").Exists() {
			continue
		}
		width := int(m.Get("dimensions.width").Int())
		height := int(m.Get("dimensions.height").Int())
		i.Medias = append(i.Medias, Media{
			TypeName:     typeName,
			URL:          rawURL,
			ThumbnailURL: thumbnailURL,
			Width:        width,
			Height:       height,
		})
	}

	// Failed to scrape from Embed
	if len(i.Medias) == 0 {
		if err := scrapePublicOEmbed(i); err == nil {
			slog.Info("Data parsed from public oEmbed fallback", "postID", i.PostID)
			return nil
		} else {
			slog.Debug("Failed to scrape data from public oEmbed fallback", "postID", i.PostID, "err", err)
		}
		if allowAuth {
			if err := scrapeAuthHelperSingleflight(i); err == nil {
				return nil
			} else if err != ErrNotFound {
				slog.Debug("Failed to scrape data from auth helper", "postID", i.PostID, "err", err)
			}
		}
		if err := scrapeRestriction(i.PostID); err != nil {
			return err
		}
		return ErrNotFound
	}
	return nil
}

func scrapePublicOEmbed(i *InstaData) error {
	if i == nil || !validPostID(i.PostID) {
		return ErrNotFound
	}
	postURL := "https://www.instagram.com/p/" + i.PostID + "/"
	u := "https://www.instagram.com/api/v1/oembed/?url=" + url.QueryEscape(postURL)
	client := http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json,text/html,*/*")
	req.Header.Set("Referer", postURL)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := readLimitedHTTPBody(res, maxAuthHelperBodyBytes)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		message := strings.TrimSpace(gjson.GetBytes(body, "message").String())
		if message == "" {
			message = res.Status
		}
		return fmt.Errorf("public oEmbed HTTP %s: %s", res.Status, message)
	}
	if !gjson.ValidBytes(body) {
		return errors.New("public oEmbed returned invalid JSON")
	}
	thumbnail := strings.TrimSpace(gjson.GetBytes(body, "thumbnail_url").String())
	mediaURL, err := url.Parse(thumbnail)
	if err != nil || mediaURL.Host == "" || (mediaURL.Scheme != "http" && mediaURL.Scheme != "https") {
		return errors.New("public oEmbed returned invalid thumbnail_url")
	}
	username := strings.TrimSpace(gjson.GetBytes(body, "author_name").String())
	username = strings.TrimPrefix(username, "@")
	if username == "" {
		username = "instagram"
	}
	i.Username = username
	i.Caption = strings.TrimSpace(gjson.GetBytes(body, "title").String())
	i.Medias = []Media{{TypeName: "GraphImage", URL: thumbnail}}
	return nil
}

// Taken from https://github.com/PuerkitoBio/goquery
// Modified to add new line every <br>
func gqTextNewLine(s *goquery.Selection) string {
	// Slightly optimized vs calling Each: no single selection object created
	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			// Keep newlines and spaces, like jQuery
			sb.WriteString(n.Data)
		} else if n.Type == html.ElementNode && n.Data == "br" {
			sb.WriteString("\n")
		}
		if n.FirstChild != nil {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}
	}
	for _, n := range s.Nodes {
		f(n)
	}
	return sb.String()
}

func scrapeFromEmbedHTML(embedHTML []byte) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(embedHTML))
	if err != nil {
		return "", err
	}

	// Get media URL
	typename := "GraphImage"
	embedMedia := doc.Find(".EmbeddedMediaImage, meta[property='og:image']")
	if embedMedia.Length() == 0 {
		typename = "GraphVideo"
		embedMedia = doc.Find(".EmbeddedMediaVideo, video source, meta[property='og:video'], meta[property='og:video:secure_url']")
	}
	mediaURL, ok := embedMedia.Attr("src")
	if !ok {
		mediaURL, ok = embedMedia.Attr("content")
	}
	if !ok {
		return "", ErrNotFound
	}

	// Get username
	username := doc.Find(".UsernameText").Text()

	// Get caption
	captionComments := doc.Find(".CaptionComments")
	if captionComments.Length() > 0 {
		captionComments.Remove()
	}
	captionUsername := doc.Find(".CaptionUsername")
	if captionUsername.Length() > 0 {
		captionUsername.Remove()
	}
	caption := gqTextNewLine(doc.Find(".Caption"))

	// Check if contains WatchOnInstagram
	videoBlocked := strconv.FormatBool(bytes.Contains(embedHTML, []byte("WatchOnInstagram")))

	// Totally safe 100% valid JSON 👍
	return `{
		"shortcode_media": {
			"owner": {"username": "` + username + `"},
			"node": {"__typename": "` + typename + `", "display_url": "` + mediaURL + `"},
			"edge_media_to_caption": {"edges": [{"node": {"text": ` + utils.EscapeJSONString(caption) + `}}]},
			"dimensions": {"height": null, "width": null},
			"video_blocked": ` + videoBlocked + `
		}
	}`, nil
}

// scrapeRestriction identifies posts that Instagram intentionally excludes from
// public embeds. It is only called after all media parsers fail, so it does not
// add latency to successful scrapes or replace the original not-found result
// when the diagnostic endpoint is unavailable.
func scrapeRestriction(postID string) error {
	postURL := "https://www.instagram.com/reel/" + postID + "/"
	oembedURL := "https://www.instagram.com/api/v1/oembed/?" + url.Values{"url": {postURL}}.Encode()
	req, err := http.NewRequest("GET", oembedURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; InstaFix/1.0)")

	client := http.Client{Transport: transport, Timeout: timeout}
	res, err := client.Do(req)
	if err != nil || res == nil {
		return nil
	}
	defer res.Body.Close()
	body, err := readLimitedHTTPBody(res, maxRestrictionBodyBytes)
	if err != nil || !gjson.ValidBytes(body) {
		return nil
	}

	data := gjson.ParseBytes(body)
	if data.Get("message").String() != "geoblock_required" {
		return nil
	}
	title := strings.TrimSpace(data.Get("title").String())
	reason := strings.TrimSpace(data.Get("blocks_logging_data").String())
	if title == "" {
		title = "public embed unavailable"
	}
	if reason == "" {
		reason = "geoblock_required"
	}
	return fmt.Errorf("%w: %s (%s)", ErrRestricted, title, reason)
}

func scrapeAuthHelper(i *InstaData) error {
	if AuthHelperURL == "" {
		return ErrNotFound
	}
	if health := authHelperHealth(); health.checked && health.total > 0 && health.available <= 0 {
		return fmt.Errorf("%w: exhausted (%d/%d available)", ErrAuthUnavailable, health.available, health.total)
	}
	base, err := url.Parse(AuthHelperURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("invalid auth helper URL")
	}
	u := base.ResolveReference(&url.URL{Path: strings.TrimRight(base.Path, "/") + "/oembed/" + i.PostID})
	client := http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil || res == nil {
		if err == nil {
			err = errors.New("auth helper returned no response")
		}
		observability.Default.RecordAuthHelperResult(false, i.PostID, "auth_helper_unreachable", err)
		return err
	}
	defer res.Body.Close()
	body, err := readLimitedHTTPBody(res, maxAuthHelperBodyBytes)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		message := gjson.GetBytes(body, "error").String()
		code := gjson.GetBytes(body, "error_code").String()
		if message == "" {
			message = res.Status
		}
		err := fmt.Errorf("auth helper HTTP %s: %s", res.Status, message)
		observability.Default.RecordAuthHelperResult(false, i.PostID, code, err)
		return err
	}
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "ok").Bool() {
		return errors.New("auth helper returned invalid response")
	}
	username := strings.TrimSpace(gjson.GetBytes(body, "username").String())
	caption := strings.TrimSpace(gjson.GetBytes(body, "caption").String())
	thumbnail := strings.TrimSpace(gjson.GetBytes(body, "thumbnail_url").String())
	video := strings.TrimSpace(gjson.GetBytes(body, "video_url").String())
	width := int(gjson.GetBytes(body, "width").Int())
	height := int(gjson.GetBytes(body, "height").Int())
	mediaURL, err := url.Parse(thumbnail)
	if err != nil || mediaURL.Host == "" || (mediaURL.Scheme != "http" && mediaURL.Scheme != "https") {
		return errors.New("auth helper returned invalid thumbnail_url")
	}
	if username == "" {
		username = "instagram"
	}
	i.Username = username
	i.Caption = caption
	if video != "" {
		videoURL, err := url.Parse(video)
		if err == nil && videoURL.Host != "" && (videoURL.Scheme == "http" || videoURL.Scheme == "https") {
			i.Medias = []Media{{TypeName: "GraphVideo", URL: video, ThumbnailURL: thumbnail, Width: width, Height: height}}
		} else {
			i.Medias = []Media{{TypeName: "GraphImage", URL: thumbnail}}
		}
	} else {
		i.Medias = []Media{{TypeName: "GraphImage", URL: thumbnail}}
	}
	observability.Default.RecordAuthHelperResult(true, i.PostID, "", nil)
	slog.Info("Data parsed from auth helper", "postID", i.PostID)
	return nil
}

func authHelperHealth() authHealth {
	authHealthMu.Lock()
	now := time.Now()
	if now.Before(authHealthUntil) {
		health := authHealthStatus
		authHealthMu.Unlock()
		return health
	}
	authHealthUntil = now.Add(30 * time.Second)
	authHealthMu.Unlock()

	health := authHealth{}
	base, err := url.Parse(AuthHelperURL)
	if err != nil || base.Host == "" {
		return health
	}
	u := base.ResolveReference(&url.URL{Path: strings.TrimRight(base.Path, "/") + "/healthz"})
	client := http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(u.String())
	if err != nil || res == nil {
		return health
	}
	defer res.Body.Close()
	body, err := readLimitedHTTPBody(res, maxAuthHelperBodyBytes)
	if err != nil || !gjson.ValidBytes(body) {
		return health
	}
	health.checked = true
	health.total = int(gjson.GetBytes(body, "cookie_pool.total").Int())
	health.available = int(gjson.GetBytes(body, "cookie_pool.available").Int())
	authHealthMu.Lock()
	authHealthStatus = health
	authHealthMu.Unlock()
	return health
}

func scrapeFromGQL(postID string) ([]byte, error) {
	gqlParams := url.Values{
		"av":                       {"0"},
		"__d":                      {"www"},
		"__user":                   {"0"},
		"__a":                      {"1"},
		"__req":                    {"k"},
		"__hs":                     {"19888.HYP:instagram_web_pkg.2.1..0.0"},
		"dpr":                      {"2"},
		"__ccg":                    {"UNKNOWN"},
		"__rev":                    {"1014227545"},
		"__s":                      {"trbjos:n8dn55:yev1rm"},
		"__hsi":                    {"7380500578385702299"},
		"__dyn":                    {"7xeUjG1mxu1syUbFp40NonwgU7SbzEdF8aUco2qwJw5ux609vCwjE1xoswaq0yE6ucw5Mx62G5UswoEcE7O2l0Fwqo31w9a9wtUd8-U2zxe2GewGw9a362W2K0zK5o4q3y1Sx-0iS2Sq2-azo7u3C2u2J0bS1LwTwKG1pg2fwxyo6O1FwlEcUed6goK2O4UrAwCAxW6Uf9EObzVU8U"},
		"__csr":                    {"n2Yfg_5hcQAG5mPtfEzil8Wn-DpKGBXhdczlAhrK8uHBAGuKCJeCieLDyExenh68aQAKta8p8ShogKkF5yaUBqCpF9XHmmhoBXyBKbQp0HCwDjqoOepV8Tzk8xeXqAGFTVoCciGaCgvGUtVU-u5Vp801nrEkO0rC58xw41g0VW07ISyie2W1v7F0CwYwwwvEkw8K5cM0VC1dwdi0hCbc094w6MU1xE02lzw"},
		"__comet_req":              {"7"},
		"lsd":                      {"AVoPBTXMX0Y"},
		"jazoest":                  {"2882"},
		"__spin_r":                 {"1014227545"},
		"__spin_b":                 {"trunk"},
		"__spin_t":                 {"1718406700"},
		"fb_api_caller_class":      {"RelayModern"},
		"fb_api_req_friendly_name": {"PolarisPostActionLoadPostQueryQuery"},
		"variables":                {`{"shortcode":"` + postID + `","fetch_comment_count":40,"parent_comment_count":24,"child_comment_count":3,"fetch_like_count":10,"fetch_tagged_user_count":null,"fetch_preview_comment_count":2,"has_threaded_comments":true,"hoisted_comment_id":null,"hoisted_reply_id":null}`},
		"server_timestamps":        {"true"},
		"doc_id":                   {"25531498899829322"},
	}
	req, err := http.NewRequest("POST", "https://www.instagram.com/graphql/query/", strings.NewReader(gqlParams.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"Accept":                      {"*/*"},
		"Accept-Language":             {"en-US,en;q=0.9"},
		"Content-Type":                {"application/x-www-form-urlencoded"},
		"Origin":                      {"https://www.instagram.com"},
		"Priority":                    {"u=1, i"},
		"Sec-Ch-Prefers-Color-Scheme": {"dark"},
		"Sec-Ch-Ua":                   {`"Google Chrome";v="125", "Chromium";v="125", "Not.A/Brand";v="24"`},
		"Sec-Ch-Ua-Full-Version-List": {`"Google Chrome";v="125.0.6422.142", "Chromium";v="125.0.6422.142", "Not.A/Brand";v="24.0.0.0"`},
		"Sec-Ch-Ua-Mobile":            {"?0"},
		"Sec-Ch-Ua-Model":             {`""`},
		"Sec-Ch-Ua-Platform":          {`"macOS"`},
		"Sec-Ch-Ua-Platform-Version":  {`"12.7.4"`},
		"Sec-Fetch-Dest":              {"empty"},
		"Sec-Fetch-Mode":              {"cors"},
		"Sec-Fetch-Site":              {"same-origin"},
		"User-Agent":                  {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"},
		"X-Asbd-Id":                   {"129477"},
		"X-Bloks-Version-Id":          {"e2004666934296f275a5c6b2c9477b63c80977c7cc0fd4b9867cb37e36092b68"},
		"X-Fb-Friendly-Name":          {"PolarisPostActionLoadPostQueryQuery"},
		"X-Ig-App-Id":                 {"936619743392459"},
	}

	client := http.Client{Transport: transport, Timeout: timeout}
	res, err := client.Do(req)
	if err != nil || res == nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GraphQL HTTP status %s", res.Status)
	}
	body, err := readLimitedHTTPBody(res, maxGraphQLBodyBytes)
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(body) {
		return nil, errors.New("GraphQL returned invalid JSON")
	}
	return body, nil
}
