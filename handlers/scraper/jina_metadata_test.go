package handlers

import "testing"

func TestParseJinaInstagramMetadata(t *testing.T) {
	body := `Title: Example Creator on Instagram: "A public video"

URL Source: http://www.instagram.com/reel/DdEu-OCMiuQ/

Never miss a post from example_creator

[example_creator](http://www.instagram.com/example_creator/)3h

A public video
`
	item, err := parseJinaInstagramMetadata("DdEu-OCMiuQ", body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if item.Username != "example_creator" {
		t.Fatalf("username = %q, want example_creator", item.Username)
	}
	if item.Caption != "A public video" {
		t.Fatalf("caption = %q", item.Caption)
	}
}

func TestParseJinaInstagramMetadataUsesProfileLinkFallback(t *testing.T) {
	body := `Title: Another Example on Instagram: "A second caption"

URL Source: http://www.instagram.com/reel/Dc3aT0RoHGo/

[another_creator](http://www.instagram.com/another_creator/)3h
`
	item, err := parseJinaInstagramMetadata("Dc3aT0RoHGo", body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if item.Username != "another_creator" {
		t.Fatalf("username = %q, want another_creator", item.Username)
	}
	if item.Caption == "" {
		t.Fatal("caption should be preserved from the title block")
	}
}

func TestJinaInstagramBodyHasVideo(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "blob video", body: "[Video 2](blob:http://www.instagram.com/2303b325-acf4-4f64-9b2b-50de339ed540)"},
		{name: "video alt", body: "![Image 1: Video by Creator on September 04, 2026.](https://cdn.example/cover.jpg)"},
		{name: "exact reel permalink", body: "[August 2](http://www.instagram.com/creator/reel/Dbidjf_C4nf/)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !jinaInstagramBodyHasVideo("Dbidjf_C4nf", tc.body) {
				t.Fatalf("expected video marker in %q", tc.body)
			}
		})
	}
}

func TestJinaInstagramBodyDoesNotTreatPhotoAsVideo(t *testing.T) {
	body := `Title: Creator on Instagram: "A photo caption"

URL Source: http://www.instagram.com/p/PhotoCode123/

![Image 1: Photo by Creator](https://cdn.example/photo.jpg)
[Creator](http://www.instagram.com/creator/)
`
	if jinaInstagramBodyHasVideo("PhotoCode123", body) {
		t.Fatal("photo-only Jina body must not be classified as video")
	}
}
