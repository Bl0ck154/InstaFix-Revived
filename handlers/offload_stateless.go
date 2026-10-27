package handlers

import (
	"errors"
	scraper "instafix/handlers/scraper"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

var (
	statelessOffloadRefreshNoAuth      = scraper.RefreshDataNoAuthPreserveMetadata
	statelessOffloadRefreshPublicVideo = scraper.RefreshVideoFromPublicGraphQLPreserveMetadata
	statelessOffloadRefreshAuth        = func(postID string, _ *scraper.InstaData) (*scraper.InstaData, error) {
		return scraper.RefreshDataFromAuthHelper(postID)
	}
	statelessOffloadMissTTL        = 45 * time.Second
	statelessOffloadMissMu         sync.Mutex
	statelessOffloadMisses         = make(map[string]statelessOffloadMiss)
	statelessOffloadRefreshStripes [64]sync.Mutex
)

type statelessOffloadMiss struct {
	until  time.Time
	item   *scraper.InstaData
	reason string
}

// OffloadStateless keeps preview metadata on a stable same-origin URL. Normal
// media requests stay redirect-only; the compact=av1/av2 Telegram fallback is the
// one deliberate exception and serves a small remuxed MP4 with audio.
func OffloadStateless(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")
	mediaNum, err := strconv.Atoi(strings.TrimSuffix(chi.URLParam(r, "mediaNum"), ".mp4"))
	if err != nil || mediaNum < 1 {
		http.Error(w, "invalid media number", http.StatusBadRequest)
		return
	}

	thumbnail := r.URL.Query().Has("thumbnail")
	if compact := r.URL.Query().Get("compact"); !thumbnail && mediaNum == 1 && (compact == "av1" || compact == "av2" || compact == "av3" || compact == "av4") {
		ServeCompactAV(w, r, postID)
		return
	}
	now := time.Now()
	var item *scraper.InstaData
	var fetchErr error
	if cached, hit := statelessOffloadMissHit(postID, now); hit {
		if _, _, playable := statelessOffloadTarget(cached.item, mediaNum, thumbnail, now); playable {
			item = cached.item
		} else if cached.reason != "" {
			slog.Info("stateless offload negative cache hit", "postID", postID, "media_index", mediaNum, "reason", cached.reason)
			http.Error(w, "media temporarily unavailable", http.StatusBadGateway)
			return
		}
	}
	if item == nil {
		item, fetchErr = offloadGetDataPreferVideoQuiet(postID)
	}
	target, media, ok := statelessOffloadTarget(item, mediaNum, thumbnail, now)
	if !ok {
		var refreshErr error
		target, media, refreshErr = refreshStatelessOffloadTarget(postID, mediaNum, thumbnail, item, fetchErr, now)
		if refreshErr != nil {
			slog.Warn("stateless offload refresh failed", "postID", postID, "media_index", mediaNum, "err", refreshErr)
			http.Error(w, "media temporarily unavailable", http.StatusBadGateway)
			return
		}
	}

	// Diagnostic/compatibility path: the normal stateless offload remains a
	// cheap 302 to Instagram CDN. Only an explicitly advertised .mp4 URL fetched
	// by Telegram uses the existing minimal stream, returning same-origin 200
	// video/mp4 with Range/upstream transport headers stripped. Current default
	// embeds never advertise .mp4, so compact=av4 and normal direct delivery are
	// unaffected.
	if media.IsVideo() && !thumbnail && isTelegramBot(r.UserAgent()) && strings.HasSuffix(strings.ToLower(r.URL.Path), ".mp4") {
		streamOffloadVideo(w, r, postID, mediaNum, target)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Location", target)
	w.Header().Set("X-Instagram7-Experiment", "stateless-azure")
	if media.IsVideo() && !thumbnail {
		w.Header().Set("X-InstaFix-Video-Delivery", "stateless-cdn-redirect")
	}
	w.WriteHeader(http.StatusFound)
	slog.Info("stateless offload redirected to CDN",
		"postID", postID,
		"media_index", mediaNum,
		"thumbnail", thumbnail,
		"video", media.IsVideo(),
		"status", http.StatusFound,
	)
}

func statelessOffloadTarget(item *scraper.InstaData, mediaNum int, thumbnail bool, now time.Time) (string, scraper.Media, bool) {
	if item == nil || mediaNum < 1 || mediaNum > len(item.Medias) {
		return "", scraper.Media{}, false
	}
	media := item.Medias[mediaNum-1]
	target := media.URL
	if thumbnail && media.ThumbnailURL != "" {
		target = media.ThumbnailURL
	}
	if !statelessMediaURLPlayable(target, now) {
		return "", media, false
	}
	return target, media, true
}

// refreshStatelessOffloadTarget resolves the exact requested carousel slot.
// A generic "this post has some video" result is not enough: each fallback
// must contain a playable URL for mediaNum. Final misses are cached briefly per
// post so a carousel fan-out cannot repeat the same upstream recovery chain for
// every media index.
func refreshStatelessOffloadTarget(postID string, mediaNum int, thumbnail bool, previous *scraper.InstaData, fetchErr error, now time.Time) (string, scraper.Media, error) {
	if cached, ok := statelessOffloadMissHit(postID, now); ok {
		if target, media, playable := statelessOffloadTarget(cached.item, mediaNum, thumbnail, now); playable {
			return target, media, nil
		}
		if cached.reason != "" {
			return "", scraper.Media{}, errors.New("stateless offload negative cache: " + cached.reason)
		}
		previous = richerStatelessOffloadItem(previous, cached.item)
	}

	refreshLock := statelessOffloadRefreshLock(postID)
	refreshLock.Lock()
	defer refreshLock.Unlock()
	now = time.Now()
	if cached, ok := statelessOffloadMissHit(postID, now); ok {
		if target, media, playable := statelessOffloadTarget(cached.item, mediaNum, thumbnail, now); playable {
			return target, media, nil
		}
		if cached.reason != "" {
			return "", scraper.Media{}, errors.New("stateless offload negative cache: " + cached.reason)
		}
		previous = richerStatelessOffloadItem(previous, cached.item)
	}

	best := previous
	errs := make([]error, 0, 5)
	if fetchErr != nil {
		errs = append(errs, fetchErr)
	}

	type refreshSource struct {
		name string
		fn   func(string, *scraper.InstaData) (*scraper.InstaData, error)
	}
	sources := []refreshSource{
		{name: "anonymous", fn: statelessOffloadRefreshNoAuth},
		{name: "public_graphql", fn: statelessOffloadRefreshPublicVideo},
	}
	if scraper.AuthHelperURL != "" {
		sources = append(sources, refreshSource{name: "auth_helper", fn: statelessOffloadRefreshAuth})
	}

	for _, source := range sources {
		refreshed, err := source.fn(postID, best)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		best = richerStatelessOffloadItem(best, refreshed)
		if target, media, playable := statelessOffloadTarget(refreshed, mediaNum, thumbnail, now); playable {
			saveStatelessOffloadMiss(postID, best, "", now)
			slog.Info("stateless offload recovered requested media index", "postID", postID, "media_index", mediaNum, "source", source.name)
			return target, media, nil
		}
		errs = append(errs, errors.New(source.name+" refresh returned no playable media for requested index"))
	}

	reason := statelessOffloadMissReason(errs)
	saveStatelessOffloadMiss(postID, best, reason, now)
	slog.Warn("stateless offload resolution exhausted", "postID", postID, "media_index", mediaNum, "reason", reason)
	if len(errs) == 0 {
		return "", scraper.Media{}, errors.New(reason)
	}
	return "", scraper.Media{}, errors.Join(errs...)
}

func richerStatelessOffloadItem(current, candidate *scraper.InstaData) *scraper.InstaData {
	if candidate == nil {
		return current
	}
	if current == nil || len(candidate.Medias) >= len(current.Medias) {
		return candidate
	}
	return current
}

func statelessOffloadMissReason(errs []error) string {
	for _, err := range errs {
		if statelessOffloadAgeRestricted(err) {
			return "age_restricted"
		}
		if statelessOffloadGeoblock(err) {
			return "geoblock"
		}
	}

	onlyNotFound := len(errs) > 0
	for _, err := range errs {
		if !errors.Is(err, scraper.ErrNotFound) && !strings.Contains(strings.ToLower(err.Error()), "post not found") {
			onlyNotFound = false
		}
	}
	if onlyNotFound {
		return "post_not_found"
	}
	return "no_video"
}

func statelessOffloadAgeRestricted(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "min_age_account") || strings.Contains(text, "age restricted") || strings.Contains(text, "under 15") || strings.Contains(text, "under 18")
}

func statelessOffloadGeoblock(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "geoblock_required") || strings.Contains(text, "geoblock")
}

func statelessOffloadMissHit(postID string, now time.Time) (statelessOffloadMiss, bool) {
	statelessOffloadMissMu.Lock()
	defer statelessOffloadMissMu.Unlock()
	entry, ok := statelessOffloadMisses[postID]
	if !ok {
		return statelessOffloadMiss{}, false
	}
	if now.After(entry.until) {
		delete(statelessOffloadMisses, postID)
		return statelessOffloadMiss{}, false
	}
	return entry, true
}

func saveStatelessOffloadMiss(postID string, item *scraper.InstaData, reason string, now time.Time) {
	if postID == "" || statelessOffloadMissTTL <= 0 {
		return
	}
	statelessOffloadMissMu.Lock()
	defer statelessOffloadMissMu.Unlock()
	if len(statelessOffloadMisses) > 4096 {
		for key, entry := range statelessOffloadMisses {
			if now.After(entry.until) {
				delete(statelessOffloadMisses, key)
			}
		}
	}
	statelessOffloadMisses[postID] = statelessOffloadMiss{until: now.Add(statelessOffloadMissTTL), item: item, reason: reason}
}

func clearStatelessOffloadMiss(postID string) {
	statelessOffloadMissMu.Lock()
	delete(statelessOffloadMisses, postID)
	statelessOffloadMissMu.Unlock()
}

func statelessOffloadRefreshLock(postID string) *sync.Mutex {
	var hash uint32 = 2166136261
	for i := 0; i < len(postID); i++ {
		hash ^= uint32(postID[i])
		hash *= 16777619
	}
	return &statelessOffloadRefreshStripes[int(hash%uint32(len(statelessOffloadRefreshStripes)))]
}
