package handlers

import (
    "errors"
    scraper "instafix/handlers/scraper"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/go-chi/chi/v5"
)

const publicHighlightToken = "aGlnaGxpZ2h0OjE4MTA1MTY4MDQxMDM5MDc1"

func highlightTestRequest(path string, agent string) *httptest.ResponseRecorder {
    router := chi.NewRouter()
    router.Get("/s/{postID}", HighlightEmbed)
    router.Get("/stories/highlights/{postID}", HighlightEmbed)
    rec := httptest.NewRecorder()
    req := httptest.NewRequest(http.MethodGet, "https://preview.test"+path, nil)
    req.Header.Set("User-Agent", agent)
    router.ServeHTTP(rec, req)
    return rec
}

func TestPublicHighlightPreviewIsDisabledByDefault(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "false")
    rec := highlightTestRequest("/s/"+publicHighlightToken, "TelegramBot")
    if rec.Code != http.StatusNotFound { t.Fatalf("status=%d, want 404", rec.Code) }
}

func TestPublicHighlightPreviewRejectsInvalidIDs(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "true")
    for _, path := range []string{
        "/s/bad!",
        "/stories/highlights/0",
        "/stories/highlights/18105168041039075?story_media_id=123_bad",
        "/s/"+publicHighlightToken+"?story_media_id=123_456_789",
    } {
        rec := highlightTestRequest(path, "TelegramBot")
        if rec.Code != http.StatusBadRequest { t.Errorf("%s: status=%d, want 400", path, rec.Code) }
    }
}

func TestPublicHighlightPreviewRedirectDoesNotFetchOrLeakSharingToken(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "true")
    oldCollection, oldStory := highlightPublicCollection, highlightPublicStory
    t.Cleanup(func(){highlightPublicCollection, highlightPublicStory = oldCollection, oldStory})
    highlightPublicCollection = func(string)(*scraper.InstaData,error){t.Fatal("browser fetched Highlight");return nil,nil}
    highlightPublicStory = func(string)(*scraper.InstaData,error){t.Fatal("browser fetched story");return nil,nil}
    rec := highlightTestRequest("/s/"+publicHighlightToken+"?story_media_id=3950493641290700498_49655035864&srtk=secret", "Mozilla/5.0")
    if rec.Code != http.StatusFound {t.Fatalf("status=%d, want 302",rec.Code)}
    want := "https://www.instagram.com/s/"+publicHighlightToken+"?story_media_id=3950493641290700498"
    if got := rec.Header().Get("Location"); got != want {t.Fatalf("location=%q, want %q",got,want)}
}

func TestPublicHighlightCollectionUsesPublicCoverOnly(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "true")
    oldCollection := highlightPublicCollection
    t.Cleanup(func(){highlightPublicCollection = oldCollection})
    highlightPublicCollection = func(id string)(*scraper.InstaData,error){
        if id != "18105168041039075" {t.Errorf("id=%q",id)}
        return &scraper.InstaData{PostID:id,Username:"creator",Caption:"Public collection",Medias:[]scraper.Media{{TypeName:"GraphImage",URL:"https://scontent.cdninstagram.com/cover.jpg"}}},nil
    }
    rec := highlightTestRequest("/stories/highlights/18105168041039075", "TelegramBot")
    if rec.Code != http.StatusOK {t.Fatalf("status=%d body=%s",rec.Code,rec.Body.String())}
    if !strings.Contains(rec.Body.String(), "https://scontent.cdninstagram.com/cover.jpg") {t.Fatal("missing public collection cover")}
}

func TestPublicHighlightSelectedStoryUsesExactMediaAndNeverAuth(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "true")
    oldStory,oldVideo := highlightPublicStory,highlightPublicVideoStory
    t.Cleanup(func(){highlightPublicStory,highlightPublicVideoStory=oldStory,oldVideo})
    highlightPublicStory=func(code string)(*scraper.InstaData,error){
        if code!="DbS-QlAklbS" {t.Errorf("shortcode=%q",code)}
        return &scraper.InstaData{PostID:code,Username:"creator",Medias:[]scraper.Media{{TypeName:"GraphImage",URL:"https://scontent.cdninstagram.com/selected.jpg"}}},nil
    }
    highlightPublicVideoStory=func(string)(*scraper.InstaData,error){t.Fatal("unexpected video fallback");return nil,nil}
    rec := highlightTestRequest("/s/"+publicHighlightToken+"?story_media_id=3950493641290700498_49655035864&srtk=private", "TelegramBot")
    if rec.Code!=http.StatusOK {t.Fatalf("status=%d body=%s",rec.Code,rec.Body.String())}
    html:=rec.Body.String()
    if !strings.Contains(html,"https://scontent.cdninstagram.com/selected.jpg") || strings.Contains(html,"srtk") || strings.Contains(html,"private") {
        t.Fatalf("selected public story leaked a sharing token or lost its image: %s",html)
    }
}

func TestPublicHighlightUnavailableIsNotRecoveredWithCredentials(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "true")
    oldStory,oldVideo:=highlightPublicStory,highlightPublicVideoStory
    t.Cleanup(func(){highlightPublicStory,highlightPublicVideoStory=oldStory,oldVideo})
    highlightPublicStory=func(string)(*scraper.InstaData,error){return nil,scraper.ErrNotFound}
    highlightPublicVideoStory=func(string)(*scraper.InstaData,error){return nil,errors.New("not public")}
    rec:=highlightTestRequest("/stories/highlights/18105168041039075?story_media_id=3950493641290700498","TelegramBot")
    if rec.Code!=http.StatusNotFound {t.Fatalf("status=%d, want 404",rec.Code)}
}

func TestPublicHighlightVideoUsesThumbnailNotPlayableMedia(t *testing.T) {
    t.Setenv("INSTAFIX_PUBLIC_HIGHLIGHTS_ENABLED", "true")
    oldStory,oldVideo:=highlightPublicStory,highlightPublicVideoStory
    t.Cleanup(func(){highlightPublicStory,highlightPublicVideoStory=oldStory,oldVideo})
    highlightPublicStory=func(string)(*scraper.InstaData,error){return nil,scraper.ErrNotFound}
    highlightPublicVideoStory=func(code string)(*scraper.InstaData,error){
        return &scraper.InstaData{PostID:code,Username:"creator",Medias:[]scraper.Media{{TypeName:"GraphVideo",URL:"https://scontent.cdninstagram.com/selected.mp4",ThumbnailURL:"https://scontent.cdninstagram.com/poster.jpg"}}},nil
    }
    rec:=highlightTestRequest("/s/"+publicHighlightToken+"?story_media_id=3950493641290700498","TelegramBot")
    if rec.Code!=http.StatusOK {t.Fatalf("status=%d body=%s",rec.Code,rec.Body.String())}
    if !strings.Contains(rec.Body.String(),"poster.jpg") || strings.Contains(rec.Body.String(),"selected.mp4") {
        t.Fatal("public video Highlight should advertise only its thumbnail")
    }
}
