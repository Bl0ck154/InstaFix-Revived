package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	scraper "instafix/handlers/scraper"
	"instafix/views"
	"instafix/views/model"
)

func fiveImageCarousel() *scraper.InstaData {
	item := &scraper.InstaData{PostID: "DeJW3z2CGDa", Username: "creator"}
	for i := 1; i <= 5; i++ {
		item.Medias = append(item.Medias, scraper.Media{
			TypeName: "GraphImage",
			URL:      "https://scontent.cdninstagram.com/image" + strconv.Itoa(i) + ".jpg",
		})
	}
	return item
}

func TestDiscordCarouselRendersFourDistinctImagesInOrder(t *testing.T) {
	item := fiveImageCarousel()
	primary, additional := carouselPreviewImages("https://fix.example", item.PostID, item.Medias, "Mozilla/5.0 (compatible; Discordbot/2.0)")
	var out bytes.Buffer
	views.Embed(&model.ViewsData{ImageURL: primary, ImageURLs: additional, NoRedirect: true}, &out)
	html := out.String()
	if strings.Contains(html, "/grid/") {
		t.Fatalf("Discord carousel must not mix a grid with individual images: %s", html)
	}
	if got := strings.Count(html, `property="og:image"`); got != 4 {
		t.Fatalf("got %d OG images, want four: %s", got, html)
	}
	last := -1
	for i := 1; i <= 4; i++ {
		tag := `property="og:image" content="https://fix.example/offload/` + item.PostID + "/" + strconv.Itoa(i) + `"`
		index := strings.Index(html, tag)
		if index <= last || strings.Count(html, tag) != 1 {
			t.Fatalf("image %d missing, duplicated, or out of order: %s", i, html)
		}
		last = index
	}
}

func TestCarouselOffloadPreservesAllFiveImageIndexes(t *testing.T) {
	item := fiveImageCarousel()
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) {
		t.Fatal("image carousel must not refresh video data")
		return nil, nil
	})
	// Reproduce the shorter image-only result returned by video resolution.
	short := &scraper.InstaData{Medias: item.Medias[:1]}
	offloadGetDataPreferVideo = func(string) (*scraper.InstaData, error) {
		t.Error("image carousel used authenticated video resolution")
		return short, nil
	}
	offloadGetDataPreferVideoQuiet = func(string) (*scraper.InstaData, error) {
		t.Error("image carousel used quiet video resolution")
		return short, nil
	}
	for _, ua := range []string{"Discordbot/2.0", "Mozilla/5.0"} {
		for i := 1; i <= 5; i++ {
			number := strconv.Itoa(i)
			req := offloadRequest(t, http.MethodGet, "https://fix.example/offload/"+item.PostID+"/"+number, item.PostID, number)
			req.Header.Set("User-Agent", ua)
			recorder := httptest.NewRecorder()
			Offload(recorder, req)
			if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != item.Medias[i-1].URL {
				t.Fatalf("UA %q image %d: status=%d Location=%q body=%q", ua, i, recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
			}
		}
	}
}

func TestGridDecodesPNGInsteadOfFallingBackToFirstImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data.Bytes())
	}))
	defer upstream.Close()
	decoded, pixels, err := decodeGridImage(upstream.Client(), "DeJW3z2CGDa", upstream.URL)
	if err != nil {
		t.Fatalf("grid rejected a valid non-JPEG image: %v", err)
	}
	if decoded.Bounds() != img.Bounds() || pixels != 6 {
		t.Fatalf("decoded bounds=%v pixels=%d", decoded.Bounds(), pixels)
	}
}

func TestLegacyVideosRetainsVideoPreferredResolution(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video"))
	}))
	defer upstream.Close()
	item := &scraper.InstaData{PostID: "DaLegacy1", Medias: []scraper.Media{{TypeName: "GraphVideo", URL: upstream.URL}}}
	stubOffloadData(t, item, func(string) (*scraper.InstaData, error) { return nil, nil })
	cover := &scraper.InstaData{Medias: []scraper.Media{{TypeName: "GraphImage"}}}
	offloadGetData = func(string) (*scraper.InstaData, error) {
		t.Error("legacy video route used image resolution")
		return cover, nil
	}
	offloadGetDataQuiet = offloadGetData
	for _, ua := range []string{"Discordbot/2.0", "Mozilla/5.0"} {
		req := offloadRequest(t, http.MethodGet, "https://fix.example/videos/DaLegacy1/1", "DaLegacy1", "1")
		req.Header.Set("User-Agent", ua)
		recorder := httptest.NewRecorder()
		Videos(recorder, req)
		if recorder.Code != http.StatusOK || recorder.Body.String() != "video" {
			t.Fatalf("UA %q: status=%d body=%q", ua, recorder.Code, recorder.Body.String())
		}
	}
}

func TestMixedCarouselPreservesOriginalIndexesAndVideoThumbnails(t *testing.T) {
	item := fiveImageCarousel()
	item.Medias[1] = scraper.Media{TypeName: "GraphVideo", URL: "https://scontent.cdninstagram.com/video.mp4", ThumbnailURL: "https://scontent.cdninstagram.com/cover.jpg"}
	item.Medias[2] = scraper.Media{TypeName: "GraphVideo", URL: "https://scontent.cdninstagram.com/no-cover.mp4"}
	primary, additional := carouselPreviewImages("https://fix.example", item.PostID, item.Medias, "Discordbot/2.0")
	urls := append([]string{primary}, additional...)
	want := []string{
		"https://fix.example/offload/DeJW3z2CGDa/1",
		"https://fix.example/offload/DeJW3z2CGDa/2?thumbnail=1",
		"https://fix.example/offload/DeJW3z2CGDa/4",
		"https://fix.example/offload/DeJW3z2CGDa/5",
	}
	if len(urls) != len(want) {
		t.Fatalf("mixed carousel URLs=%v", urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("mixed carousel URL %d=%q, want %q", i, urls[i], want[i])
		}
	}
}
