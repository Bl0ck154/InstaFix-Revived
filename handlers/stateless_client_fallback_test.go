package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	scraper "instafix/handlers/scraper"
	"instafix/views/model"
)

func TestStatelessTelegramReelUsesClientOnlyMediaWhenImageOnly(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_UPSTREAM", "https://d.oginstagram.example")
	item := &scraper.InstaData{PostID: "PublicReel123", Username: "creator", Medias: []scraper.Media{{
		TypeName: "GraphImage", URL: "https://scontent.cdninstagram.com/cover.jpg", Width: 720, Height: 1280,
	}}}
	req := httptest.NewRequest(http.MethodGet, "https://preview.example.org/reel/PublicReel123/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	rec := httptest.NewRecorder()
	if !tryStatelessTelegramClientMediaFallback(rec, req, "PublicReel123", 0, true, &model.ViewsData{}, item, scraper.ErrNotFound) {
		t.Fatal("expected Telegram client-only media fallback")
	}
	if !strings.Contains(rec.Body.String(), "https://d.oginstagram.example/p/PublicReel123/") {
		t.Fatalf("no direct client media endpoint in %s", rec.Body.String())
	}
}

func TestStatelessTelegramClientMediaFallbackKeepsLocalVideo(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	item := &scraper.InstaData{Medias: []scraper.Media{{
		TypeName: "GraphVideo", URL: "https://scontent.cdninstagram.com/video.mp4", ThumbnailURL: "https://scontent.cdninstagram.com/cover.jpg",
		Width: 720, Height: 1280,
	}}}
	req := httptest.NewRequest(http.MethodGet, "https://preview.example.org/reel/LocalVideo123/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	if tryStatelessTelegramClientMediaFallback(httptest.NewRecorder(), req, "LocalVideo123", 0, true, &model.ViewsData{}, item, nil) {
		t.Fatal("a complete local video must stay on the local delivery path")
	}
}

func TestStatelessPhotoDoesNotBecomeFakeReel(t *testing.T) {
	t.Setenv("OGINSTAGRAM_CLIENT_MEDIA_FALLBACK_MODE", "telegram")
	req := httptest.NewRequest(http.MethodGet, "https://preview.example.org/p/Photo123/", nil)
	req.Header.Set("User-Agent", "TelegramBot")
	if tryStatelessTelegramClientMediaFallback(httptest.NewRecorder(), req, "Photo123", 0, false, &model.ViewsData{}, nil, scraper.ErrNotFound) {
		t.Fatal("unclassified photo must not enter Reel media fallback")
	}
}
