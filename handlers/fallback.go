package handlers

import (
	"image"
	"image/color"
	"image/png"
	"net/http"
)

func FallbackPreview(w http.ResponseWriter, r *http.Request) {
	const width = 1200
	const height = 630
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r := uint8(32 + x*80/width)
			g := uint8(12 + y*28/height)
			b := uint8(54 + (x+y)*70/(width+height))
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	// Simple centered card shape. No text/font dependency, so this stays tiny and
	// deterministic while still producing a real image for preview crawlers.
	drawRect(img, 210, 145, 990, 485, color.RGBA{R: 255, G: 255, B: 255, A: 38})
	drawRect(img, 250, 185, 950, 445, color.RGBA{R: 255, G: 255, B: 255, A: 28})
	drawRect(img, 315, 245, 885, 305, color.RGBA{R: 255, G: 255, B: 255, A: 58})
	drawRect(img, 390, 345, 810, 385, color.RGBA{R: 255, G: 255, B: 255, A: 48})
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = png.Encode(w, img)
}

func drawRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			base := img.RGBAAt(x, y)
			a := uint32(c.A)
			inv := 255 - a
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((uint32(c.R)*a + uint32(base.R)*inv) / 255),
				G: uint8((uint32(c.G)*a + uint32(base.G)*inv) / 255),
				B: uint8((uint32(c.B)*a + uint32(base.B)*inv) / 255),
				A: 255,
			})
		}
	}
}
