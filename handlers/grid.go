package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	scraper "instafix/handlers/scraper"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/RyanCarrier/dijkstra/v2"
	"github.com/go-chi/chi/v5"
	"golang.org/x/image/draw"
	"golang.org/x/sync/singleflight"
)

var timeout = 60 * time.Second

const maxGridImages = 5
const maxGridImageBytes int64 = 8 << 20
const maxGridImagePixels int64 = 4_000_000
const maxGridTotalPixels int64 = 10_000_000
const maxGridCanvasPixels int64 = 12_000_000

var transport = &http.Transport{
	Proxy: nil, // Skip any proxy
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}
var sflightGrid singleflight.Group

// getHeight returns the height of the rows, imagesWH [w,h]
func getHeight(imagesWH [][]float64, canvasWidth int) float64 {
	var height float64
	for _, image := range imagesWH {
		height += image[0] / image[1]
	}
	return float64(canvasWidth) / height
}

// costFn returns the cost of the row graph thingy
func costFn(imagesWH [][]float64, i, j, canvasWidth, maxRowHeight int) float64 {
	slices := imagesWH[i:j]
	rowHeight := getHeight(slices, canvasWidth)
	return math.Pow(float64(maxRowHeight)-rowHeight, 2)
}

func createGraph(imagesWH [][]float64, start, canvasWidth int) map[int]uint64 {
	results := make(map[int]uint64, len(imagesWH))
	results[start] = 0
	for i := start + 1; i < len(imagesWH); i++ {
		// Max 3 images for every row
		if i-start > 3 {
			break
		}
		results[i] = uint64(costFn(imagesWH, start, i, canvasWidth, 1000))
	}
	return results
}

func avg(n []float64) float64 {
	var sum float64
	for _, v := range n {
		sum += v
	}
	return sum / float64(len(n))
}

// GenerateGrid generates a grid of images
// based on https://blog.vjeux.com/2014/image/google-plus-layout-find-best-breaks.html
func GenerateGrid(images []image.Image) (image.Image, error) {
	if len(images) == 0 {
		return nil, errors.New("no images for grid")
	}
	var imagesWH [][]float64
	images = append(images, image.Rect(0, 0, 0, 0)) // Needed as for some reason the last image is not added
	for _, img := range images {
		if img == nil {
			return nil, errors.New("nil image for grid")
		}
		bounds := img.Bounds()
		if bounds.Dx() < 0 || bounds.Dy() < 0 {
			return nil, errors.New("invalid image dimensions for grid")
		}
		imagesWH = append(imagesWH, []float64{float64(bounds.Dx()), float64(bounds.Dy())})
	}

	// Calculate canvas width by taking the average of width of all images
	// There should be a better way to do this
	var allWidth []float64
	for _, img := range imagesWH {
		allWidth = append(allWidth, img[0])
	}
	canvasWidth := int(avg(allWidth) * 1.5)
	if canvasWidth <= 0 {
		return nil, errors.New("invalid grid canvas width")
	}

	graph := dijkstra.NewGraph()
	for i := range images {
		graph.AddVertexAndArcs(i, createGraph(imagesWH, i, canvasWidth))
	}

	// Get the shortest path from 0 to len(images)-1
	best, err := graph.Shortest(0, len(images)-1)
	if err != nil {
		return nil, err
	}
	path := best.Path

	canvasHeight := 0
	var heightRows []int
	// Calculate height of each row and canvas height
	for i := 1; i < len(path); i++ {
		if len(imagesWH) < path[i-1] {
			return nil, errors.New("imagesWH is not long enough")
		}
		rowWH := imagesWH[path[i-1]:path[i]]

		rowHeight := int(getHeight(rowWH, canvasWidth))
		heightRows = append(heightRows, rowHeight)
		canvasHeight += rowHeight
	}
	if canvasHeight <= 0 {
		return nil, errors.New("invalid grid canvas height")
	}
	if int64(canvasWidth)*int64(canvasHeight) > maxGridCanvasPixels {
		return nil, fmt.Errorf("grid canvas too large: %dx%d", canvasWidth, canvasHeight)
	}

	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))

	oldRowHeight := 0
	for i := 1; i < len(path); i++ {
		inRow := images[path[i-1]:path[i]]
		oldImWidth := 0
		if len(heightRows) < i {
			return nil, errors.New("heightRows is not long enough")
		}
		heightRow := heightRows[i-1]
		for _, imageOne := range inRow {
			newWidth := float64(heightRow) * float64(imageOne.Bounds().Dx()) / float64(imageOne.Bounds().Dy())
			draw.ApproxBiLinear.Scale(canvas, image.Rect(oldImWidth, oldRowHeight, oldImWidth+int(newWidth), oldRowHeight+int(heightRow)), imageOne, imageOne.Bounds(), draw.Src, nil)
			oldImWidth += int(newWidth)
		}
		oldRowHeight += heightRow
	}
	return canvas, nil
}

func redirectGridFallback(w http.ResponseWriter, r *http.Request, postID string) {
	http.Redirect(w, r, "/images/"+postID+"/1", http.StatusFound)
}

func decodeGridImage(client *http.Client, mediaURL string) (image.Image, int64, error) {
	req, err := http.NewRequest(http.MethodGet, mediaURL, http.NoBody)
	if err != nil {
		return nil, 0, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("image upstream status %s", res.Status)
	}
	if res.ContentLength > maxGridImageBytes {
		return nil, 0, fmt.Errorf("grid image too large: content-length %d", res.ContentLength)
	}

	data, err := io.ReadAll(io.LimitReader(res.Body, maxGridImageBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > maxGridImageBytes {
		return nil, 0, fmt.Errorf("grid image too large: downloaded more than %d bytes", maxGridImageBytes)
	}

	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	pixels := int64(config.Width) * int64(config.Height)
	if config.Width <= 0 || config.Height <= 0 || pixels <= 0 {
		return nil, 0, fmt.Errorf("invalid grid image dimensions: %dx%d", config.Width, config.Height)
	}
	if pixels > maxGridImagePixels {
		return nil, 0, fmt.Errorf("grid image dimensions too large: %dx%d", config.Width, config.Height)
	}

	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	return img, pixels, nil
}

func Grid(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")
	gridFname := filepath.Join("static", postID+".jpeg")

	// If already exists, return from cache
	if _, ok := scraper.LRU.Get(gridFname); ok {
		f, err := os.Open(gridFname)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "image/jpeg")
			io.Copy(w, f)
			return
		}
	}

	item, err := scraper.GetData(postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter media only include image
	var mediaURLs []string
	for _, media := range item.Medias {
		if !media.IsImage() {
			continue
		}
		mediaURLs = append(mediaURLs, media.URL)
	}

	if len(item.Medias) == 1 || len(mediaURLs) == 1 {
		redirectGridFallback(w, r, postID)
		return
	}
	if len(mediaURLs) == 0 {
		http.Error(w, "no images for grid", http.StatusNotFound)
		return
	}
	if len(mediaURLs) > maxGridImages {
		slog.Warn("grid generation skipped: too many images", "postID", postID, "count", len(mediaURLs), "max", maxGridImages)
		redirectGridFallback(w, r, postID)
		return
	}

	_, err, _ = sflightGrid.Do(postID, func() (interface{}, error) {
		client := http.Client{Transport: transport, Timeout: timeout}
		images := make([]image.Image, 0, len(mediaURLs))
		var totalPixels int64
		for _, mediaURL := range mediaURLs {
			img, pixels, err := decodeGridImage(&client, mediaURL)
			if err != nil {
				return false, err
			}
			if totalPixels+pixels > maxGridTotalPixels {
				return false, fmt.Errorf("grid total image dimensions too large: %d pixels", totalPixels+pixels)
			}
			totalPixels += pixels
			images = append(images, img)
		}

		// Create grid Images
		grid, err := GenerateGrid(images)
		if err != nil {
			return false, err
		}

		// Write grid to static folder
		f, err := os.Create(gridFname)
		if err != nil {
			return false, err
		}
		defer f.Close()

		if err := jpeg.Encode(f, grid, &jpeg.Options{Quality: 80}); err != nil {
			return false, err
		}
		scraper.LRU.Add(gridFname, true)
		return true, nil
	})

	if err != nil {
		slog.Warn("grid generation skipped; falling back to first image", "postID", postID, "err", err)
		redirectGridFallback(w, r, postID)
		return
	}

	f, err := os.Open(gridFname)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	io.Copy(w, f)
}
