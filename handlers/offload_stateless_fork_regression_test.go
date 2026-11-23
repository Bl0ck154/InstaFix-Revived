package handlers

import (
    "fmt"
    scraper "instafix/handlers/scraper"
    "net/http"
    "net/http/httptest"
    "testing"
)

// Regression based on BMJ812/InstaFix-Revived: the cached carousel can have
// only six image slots while the actual publication contains thirteen.
// A missing image index must not depend on a video-only recovery path.
func TestOffloadStatelessRecoversMissingImageOnlyCarouselIndex(t *testing.T) {
    previousGet := offloadGetDataPreferVideoQuiet
    previousAnonymous := statelessOffloadRefreshNoAuth
    previousPublic := statelessOffloadRefreshPublicVideo
    previousAuthURL := scraper.AuthHelperURL
    defer func() {
        offloadGetDataPreferVideoQuiet = previousGet
        statelessOffloadRefreshNoAuth = previousAnonymous
        statelessOffloadRefreshPublicVideo = previousPublic
        scraper.AuthHelperURL = previousAuthURL
        clearStatelessOffloadMiss("ImageOnlyCarousel01")
    }()

    imageSlots := func(count int) []scraper.Media {
        slots := make([]scraper.Media, count)
        for i := range slots {
            slots[i] = scraper.Media{
                TypeName: "GraphImage",
                URL: fmt.Sprintf("https://scontent.cdninstagram.com/carousel-%02d.jpg?oe=FFFFFFFF", i + 1),
            }
        }
        return slots
    }

    scraper.AuthHelperURL = ""
    clearStatelessOffloadMiss("ImageOnlyCarousel01")
    cached := &scraper.InstaData{Medias: imageSlots(6)}
    complete := &scraper.InstaData{Medias: imageSlots(13)}
    offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) { return cached, nil }
    anonymousCalls := 0
    statelessOffloadRefreshNoAuth = func(postID string, old *scraper.InstaData) (*scraper.InstaData, error) {
        anonymousCalls++
        if postID != "ImageOnlyCarousel01" || old != cached {
            t.Fatalf("unexpected anonymous refresh: post=%q old=%p", postID, old)
        }
        return complete, nil
    }
    statelessOffloadRefreshPublicVideo = func(string, *scraper.InstaData) (*scraper.InstaData, error) {
        t.Fatal("image-only carousel should resolve before invoking video-specific GraphQL recovery")
        return nil, scraper.ErrNotFound
    }

    req := offloadRequest(t, http.MethodHead, "https://stateless.example.test/offload/ImageOnlyCarousel01/7", "ImageOnlyCarousel01", "7")
    rec := httptest.NewRecorder()
    OffloadStateless(rec, req)
    if rec.Code != http.StatusFound {
        t.Fatalf("status=%d, want 302; body=%s", rec.Code, rec.Body.String())
    }
    if got, want := rec.Header().Get("Location"), complete.Medias[6].URL; got != want {
        t.Fatalf("location=%q, want %q", got, want)
    }
    if anonymousCalls != 1 {
        t.Fatalf("anonymous refresh called %d times, want 1", anonymousCalls)
    }
}
