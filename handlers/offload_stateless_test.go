package handlers

import (
	"errors"
	scraper "instafix/handlers/scraper"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOffloadStatelessRedirectsWithoutRelayingVideoBytes(t *testing.T) {
	oldQuiet := offloadGetDataPreferVideoQuiet
	oldRefresh := offloadRefreshDataPreferVideo
	defer func() {
		offloadGetDataPreferVideoQuiet = oldQuiet
		offloadRefreshDataPreferVideo = oldRefresh
	}()

	videoURL := "https://scontent.cdninstagram.com/video.mp4?oe=FFFFFFFF"
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		return &scraper.InstaData{Medias: []scraper.Media{{TypeName: "GraphVideo", URL: videoURL}}}, nil
	}
	offloadRefreshDataPreferVideo = func(string) (*scraper.InstaData, error) {
		t.Fatal("fresh CDN URL should not require refresh")
		return nil, nil
	}

	req := offloadRequest(t, http.MethodGet, "https://stateless.example.test/offload/DbRedirect01/1", "DbRedirect01", "1")
	rec := httptest.NewRecorder()
	OffloadStateless(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != videoURL {
		t.Fatalf("Location = %q, want %q", got, videoURL)
	}
	if got := rec.Header().Get("X-InstaFix-Video-Delivery"); got != "stateless-cdn-redirect" {
		t.Fatalf("delivery = %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("redirect unexpectedly relayed %d body bytes", rec.Body.Len())
	}
}

func TestOffloadStatelessFallsThroughForRequestedCarouselIndex(t *testing.T) {
	oldQuiet := offloadGetDataPreferVideoQuiet
	oldNoAuth := statelessOffloadRefreshNoAuth
	oldPublic := statelessOffloadRefreshPublicVideo
	oldAuthURL := scraper.AuthHelperURL
	defer func() {
		offloadGetDataPreferVideoQuiet = oldQuiet
		statelessOffloadRefreshNoAuth = oldNoAuth
		statelessOffloadRefreshPublicVideo = oldPublic
		scraper.AuthHelperURL = oldAuthURL
		clearStatelessOffloadMiss("CarouselExact01")
	}()

	scraper.AuthHelperURL = ""
	clearStatelessOffloadMiss("CarouselExact01")
	freshOne := "https://scontent.cdninstagram.com/one.mp4?oe=FFFFFFFF"
	freshTwo := "https://scontent.cdninstagram.com/two.mp4?oe=FFFFFFFF"
	noAuthCalls := 0
	publicCalls := 0
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		return &scraper.InstaData{Medias: []scraper.Media{{TypeName: "GraphVideo", URL: freshOne}}}, nil
	}
	statelessOffloadRefreshNoAuth = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
		noAuthCalls++
		return &scraper.InstaData{Medias: []scraper.Media{{TypeName: "GraphVideo", URL: freshOne}}}, nil
	}
	statelessOffloadRefreshPublicVideo = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
		publicCalls++
		return &scraper.InstaData{Medias: []scraper.Media{
			{TypeName: "GraphVideo", URL: freshOne},
			{TypeName: "GraphVideo", URL: freshTwo},
		}}, nil
	}

	req := offloadRequest(t, http.MethodGet, "https://stateless.example.test/offload/CarouselExact01/2", "CarouselExact01", "2")
	rec := httptest.NewRecorder()
	OffloadStateless(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != freshTwo {
		t.Fatalf("Location = %q, want %q", got, freshTwo)
	}
	if noAuthCalls != 1 || publicCalls != 1 {
		t.Fatalf("refresh calls = anonymous:%d public:%d, want 1/1", noAuthCalls, publicCalls)
	}
}

func TestOffloadStatelessNegativeCacheCollapsesCarouselMisses(t *testing.T) {
	oldQuiet := offloadGetDataPreferVideoQuiet
	oldNoAuth := statelessOffloadRefreshNoAuth
	oldPublic := statelessOffloadRefreshPublicVideo
	oldAuthURL := scraper.AuthHelperURL
	defer func() {
		offloadGetDataPreferVideoQuiet = oldQuiet
		statelessOffloadRefreshNoAuth = oldNoAuth
		statelessOffloadRefreshPublicVideo = oldPublic
		scraper.AuthHelperURL = oldAuthURL
		clearStatelessOffloadMiss("CarouselMiss01")
	}()

	scraper.AuthHelperURL = ""
	clearStatelessOffloadMiss("CarouselMiss01")
	noAuthCalls := 0
	publicCalls := 0
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		return nil, scraper.ErrNotFound
	}
	statelessOffloadRefreshNoAuth = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
		noAuthCalls++
		return nil, scraper.ErrNotFound
	}
	statelessOffloadRefreshPublicVideo = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
		publicCalls++
		return nil, scraper.ErrNotFound
	}

	for _, mediaNum := range []string{"2", "3"} {
		req := offloadRequest(t, http.MethodGet, "https://stateless.example.test/offload/CarouselMiss01/"+mediaNum, "CarouselMiss01", mediaNum)
		rec := httptest.NewRecorder()
		OffloadStateless(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("media %s status = %d, want %d", mediaNum, rec.Code, http.StatusBadGateway)
		}
	}

	if noAuthCalls != 1 || publicCalls != 1 {
		t.Fatalf("negative cache did not collapse carousel misses: anonymous:%d public:%d", noAuthCalls, publicCalls)
	}
	entry, ok := statelessOffloadMissHit("CarouselMiss01", time.Now())
	if !ok || entry.reason != "post_not_found" {
		t.Fatalf("negative cache = %#v, %v; want post_not_found", entry, ok)
	}
}

func TestOffloadStatelessConcurrentCarouselMissesShareRecovery(t *testing.T) {
	oldQuiet := offloadGetDataPreferVideoQuiet
	oldNoAuth := statelessOffloadRefreshNoAuth
	oldPublic := statelessOffloadRefreshPublicVideo
	oldAuthURL := scraper.AuthHelperURL
	defer func() {
		offloadGetDataPreferVideoQuiet = oldQuiet
		statelessOffloadRefreshNoAuth = oldNoAuth
		statelessOffloadRefreshPublicVideo = oldPublic
		scraper.AuthHelperURL = oldAuthURL
		clearStatelessOffloadMiss("CarouselConcurrent01")
	}()

	scraper.AuthHelperURL = ""
	clearStatelessOffloadMiss("CarouselConcurrent01")
	var quietReady atomic.Int32
	var noAuthCalls atomic.Int32
	var publicCalls atomic.Int32
	allQuiet := make(chan struct{})
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		if quietReady.Add(1) == 2 {
			close(allQuiet)
		}
		<-allQuiet
		return nil, scraper.ErrNotFound
	}
	statelessOffloadRefreshNoAuth = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
		noAuthCalls.Add(1)
		return nil, scraper.ErrNotFound
	}
	statelessOffloadRefreshPublicVideo = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
		publicCalls.Add(1)
		return nil, scraper.ErrNotFound
	}

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, mediaNum := range []string{"2", "3"} {
		mediaNum := mediaNum
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := offloadRequest(t, http.MethodGet, "https://stateless.example.test/offload/CarouselConcurrent01/"+mediaNum, "CarouselConcurrent01", mediaNum)
			rec := httptest.NewRecorder()
			OffloadStateless(rec, req)
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", code, http.StatusBadGateway)
		}
	}
	if got := noAuthCalls.Load(); got != 1 {
		t.Fatalf("anonymous refresh calls = %d, want 1", got)
	}
	if got := publicCalls.Load(); got != 1 {
		t.Fatalf("public refresh calls = %d, want 1", got)
	}
}

func TestStatelessOffloadMissReasonSeparatesFailureClasses(t *testing.T) {
	tests := []struct {
		name string
		errs []error
		want string
	}{
		{name: "post not found", errs: []error{scraper.ErrNotFound, errors.New("post not found")}, want: "post_not_found"},
		{name: "geoblock", errs: []error{errors.Join(scraper.ErrRestricted, errors.New("geoblock_required"))}, want: "geoblock"},
		{name: "age restricted", errs: []error{errors.Join(scraper.ErrRestricted, errors.New("MIN_AGE_ACCOUNT: People under 15 can't see this content"))}, want: "age_restricted"},
		{name: "no video", errs: []error{errors.New("anonymous refresh returned no playable media for requested index")}, want: "no_video"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statelessOffloadMissReason(tt.errs); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOffloadStatelessNegativeCacheStillServesKnownSlot(t *testing.T) {
	oldQuiet := offloadGetDataPreferVideoQuiet
	defer func() {
		offloadGetDataPreferVideoQuiet = oldQuiet
		clearStatelessOffloadMiss("CarouselKnown01")
	}()

	knownURL := "https://scontent.cdninstagram.com/known.mp4?oe=FFFFFFFF"
	saveStatelessOffloadMiss("CarouselKnown01", &scraper.InstaData{Medias: []scraper.Media{{TypeName: "GraphVideo", URL: knownURL}}}, "no_video", time.Now())
	quietCalls := 0
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		quietCalls++
		return nil, errors.New("quiet resolver should not run for a cached playable slot")
	}

	req := offloadRequest(t, http.MethodGet, "https://stateless.example.test/offload/CarouselKnown01/1", "CarouselKnown01", "1")
	rec := httptest.NewRecorder()
	OffloadStateless(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != knownURL {
		t.Fatalf("Location = %q, want %q", got, knownURL)
	}
	if quietCalls != 0 {
		t.Fatalf("quiet resolver calls = %d, want 0", quietCalls)
	}
}
