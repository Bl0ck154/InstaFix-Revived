package handlers

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const compactMuxOverheadReserve int64 = 512 * 1024

var ErrNoAudioExpected = errors.New("Instagram Reel explicitly has no audio")

// IsLikelyVideoOnlyDASHURL identifies Instagram CDN MP4 renditions that are
// actually a DASH video track without audio. Instagram often keeps the .mp4
// suffix, so relying on MIME type or extension alone makes Telegram render the
// clip like a silent GIF. The efg/_nc_vs URL metadata is the useful signal.
func IsLikelyVideoOnlyDASHURL(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if strings.Contains(lower, "dash_baseline") || strings.Contains(lower, "dashinit") || strings.Contains(lower, "video_dash") {
		return true
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	query := parsed.Query()
	for _, key := range []string{"efg", "_nc_vs"} {
		value := strings.TrimSpace(query.Get(key))
		if value == "" {
			continue
		}
		decoded, ok := decodeInstagramURLMetadata(value)
		if !ok {
			continue
		}
		hint := strings.ToLower(string(decoded))
		if strings.Contains(hint, "dash_baseline") || strings.Contains(hint, "dashinit") || strings.Contains(hint, "video_dash") {
			return true
		}
	}
	return false
}

func decodeInstagramURLMetadata(value string) ([]byte, bool) {
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, true
		}
	}
	return nil, false
}

// CompactPreviewSources describes separate Instagram DASH audio/video tracks
// that can be remuxed into a Telegram-friendly MP4 without re-encoding.
type CompactPreviewSources struct {
	Video          Media
	AudioURL       string
	VideoBytes     int64
	AudioBytes     int64
	EstimatedBytes int64
}

// ResolveCompactPreviewAV returns the best public Instagram DASH video+audio
// pair whose combined encoded size leaves enough room below maxBytes for MP4
// container overhead. The source URLs stay on Instagram CDN.
func ResolveCompactPreviewAV(postID string, maxBytes int64) (CompactPreviewSources, error) {
	if !validPostID(postID) {
		return CompactPreviewSources{}, errors.New("postID is not a valid Instagram post ID")
	}
	if maxBytes <= 0 {
		return CompactPreviewSources{}, errors.New("compact preview byte limit is disabled")
	}
	key := postID + ":compact-preview-av:" + strconv.FormatInt(maxBytes, 10)
	ret, err, _ := sflightScraper.Do(key, func() (interface{}, error) {
		attempts := []struct {
			name  string
			fetch func(string) ([]byte, error)
		}{
			{name: "mobile", fetch: scrapeFromGQLMobile},
			// Mobile recovery already includes configured retry/proxy attempts.
			// Use web-only fallback to avoid repeating mobile attempts and costs.
			{name: "web", fetch: scrapeFromGQLWeb},
		}
		// A proxy may return HTTP 200 + valid GraphQL while omitting the audio
		// AdaptationSet. When direct refresh is explicitly allowed, bypass the
		// proxy pool once so an incomplete success cannot mask the full manifest.
		if envBool("INSTAFIX_PUBLIC_VIDEO_REFRESH_DIRECT", false) {
			attempts = append(attempts,
				struct {
					name  string
					fetch func(string) ([]byte, error)
				}{name: "mobile_direct_ipv4", fetch: scrapeFromGQLMobileDirect},
				struct {
					name  string
					fetch func(string) ([]byte, error)
				}{name: "mobile_direct_ipv4_retry", fetch: scrapeFromGQLMobileDirect},
			)
		}
		attemptErrs := make([]error, 0, len(attempts))
		sawExpectedNoAudio := false
		sawAudioExpectedFailure := false
		for _, attempt := range attempts {
			body, fetchErr := attempt.fetch(postID)
			if fetchErr != nil {
				attemptErrs = append(attemptErrs, fmt.Errorf("%s GraphQL fetch: %w", attempt.name, fetchErr))
				continue
			}
			sources, sourceErr := compactAVSourcesFromGraphQLBody(postID, body, maxBytes)
			if sourceErr == nil {
				slog.Info("compact AV sources resolved", "postID", postID, "source", attempt.name, "video_bytes", sources.VideoBytes, "audio_bytes", sources.AudioBytes)
				return sources, nil
			}
			if errors.Is(sourceErr, ErrNoAudioExpected) {
				sawExpectedNoAudio = true
			} else {
				sawAudioExpectedFailure = true
			}
			attemptErrs = append(attemptErrs, fmt.Errorf("%s GraphQL DASH: %w", attempt.name, sourceErr))
		}
		if sawExpectedNoAudio && !sawAudioExpectedFailure {
			return nil, ErrNoAudioExpected
		}
		return nil, errors.Join(attemptErrs...)
	})
	if err != nil {
		return CompactPreviewSources{}, err
	}
	sources, ok := ret.(CompactPreviewSources)
	if !ok {
		return CompactPreviewSources{}, errors.New("compact preview resolver returned invalid sources")
	}
	return sources, nil
}

func compactAVSourcesFromGraphQLBody(postID string, body []byte, maxBytes int64) (CompactPreviewSources, error) {
	if !graphQLBodyHasMedia(body) {
		return CompactPreviewSources{}, ErrNotFound
	}
	item, _ := graphQLMediaRoot(gjson.ParseBytes(body).Get("data"))
	if !presentResult(item) {
		return CompactPreviewSources{}, ErrNotFound
	}
	if hasAudio := item.Get("has_audio"); hasAudio.Exists() && !hasAudio.Bool() {
		return CompactPreviewSources{}, ErrNoAudioExpected
	}
	manifest := strings.TrimSpace(item.Get("video_dash_manifest").String())
	if manifest == "" {
		return CompactPreviewSources{}, errors.New("Instagram response has no DASH manifest")
	}
	sources, err := compactAVFromDASHManifest(manifest, maxBytes)
	if err != nil {
		return CompactPreviewSources{}, err
	}
	sources.Video.TypeName = "GraphVideo"
	sources.Video.ThumbnailURL = bestV1ImageURL(item)
	if sources.Video.ThumbnailURL == "" {
		sources.Video.ThumbnailURL = bestDisplayURL(item)
	}
	wrapped := &InstaData{PostID: postID, Medias: []Media{sources.Video}}
	if err := normalizeMediaURLs(wrapped); err != nil {
		return CompactPreviewSources{}, err
	}
	sources.Video = wrapped.Medias[0]

	audio, err := url.Parse(strings.TrimSpace(sources.AudioURL))
	if err != nil || audio.Host == "" || audio.Scheme != "https" {
		return CompactPreviewSources{}, errors.New("compact audio URL is invalid")
	}
	normalizeInstagramCDNHost(audio)
	sources.AudioURL = audio.String()
	return sources, nil
}

// ResolveCompactPreviewVideo is retained for callers that only need the chosen
// video dimensions/URL. Selection still reserves room for the matching audio.
func ResolveCompactPreviewVideo(postID string, maxBytes int64) (Media, error) {
	sources, err := ResolveCompactPreviewAV(postID, maxBytes)
	if err != nil {
		return Media{}, err
	}
	return sources.Video, nil
}

type dashMPD struct {
	Periods []dashPeriod `xml:"Period"`
}

type dashPeriod struct {
	AdaptationSets []dashAdaptationSet `xml:"AdaptationSet"`
}

type dashAdaptationSet struct {
	ContentType     string               `xml:"contentType,attr"`
	MimeType        string               `xml:"mimeType,attr"`
	Representations []dashRepresentation `xml:"Representation"`
}

type dashRepresentation struct {
	Bandwidth     int64  `xml:"bandwidth,attr"`
	MimeType      string `xml:"mimeType,attr"`
	Width         int    `xml:"width,attr"`
	Height        int    `xml:"height,attr"`
	ContentLength int64  `xml:"FBContentLength,attr"`
	BaseURL       string `xml:"BaseURL"`
}

func compactAVFromDASHManifest(raw string, maxBytes int64) (CompactPreviewSources, error) {
	if maxBytes <= compactMuxOverheadReserve {
		return CompactPreviewSources{}, errors.New("compact preview byte limit is too small")
	}
	var manifest dashMPD
	if err := xml.Unmarshal([]byte(raw), &manifest); err != nil {
		return CompactPreviewSources{}, fmt.Errorf("parse DASH manifest: %w", err)
	}

	videos := make([]dashRepresentation, 0, 4)
	audios := make([]dashRepresentation, 0, 2)
	totalSets, totalReps := 0, 0
	positiveLengths, validURLs, allowedURLs := 0, 0, 0
	videoKindReps, audioKindReps := 0, 0
	for _, period := range manifest.Periods {
		for _, set := range period.AdaptationSets {
			totalSets++
			kind := dashTrackKind(set.ContentType, set.MimeType)
			for _, rep := range set.Representations {
				totalReps++
				repKind := kind
				if repKind == "" {
					repKind = dashTrackKind("", rep.MimeType)
				}
				if repKind == "video" {
					videoKindReps++
				} else if repKind == "audio" {
					audioKindReps++
				}
				if rep.ContentLength > 0 {
					positiveLengths++
				}
				if validMediaURL(rep.BaseURL) {
					validURLs++
				}
				if IsAllowedMediaURL(rep.BaseURL) {
					allowedURLs++
				}
				if rep.ContentLength <= 0 || strings.TrimSpace(rep.BaseURL) == "" || !validMediaURL(rep.BaseURL) || !IsAllowedMediaURL(rep.BaseURL) {
					continue
				}
				switch repKind {
				case "video":
					if rep.MimeType == "" || strings.EqualFold(rep.MimeType, "video/mp4") {
						videos = append(videos, rep)
					}
				case "audio":
					if rep.MimeType == "" || strings.EqualFold(rep.MimeType, "audio/mp4") {
						audios = append(audios, rep)
					}
				}
			}
		}
	}
	if len(videos) == 0 || len(audios) == 0 {
		return CompactPreviewSources{}, fmt.Errorf("DASH manifest does not contain usable video and audio tracks (sets=%d reps=%d video_kinds=%d audio_kinds=%d positive_lengths=%d valid_urls=%d allowed_urls=%d usable_videos=%d usable_audios=%d)", totalSets, totalReps, videoKindReps, audioKindReps, positiveLengths, validURLs, allowedURLs, len(videos), len(audios))
	}

	var bestVideo, bestAudio dashRepresentation
	found := false
	for _, video := range videos {
		for _, audio := range audios {
			estimated := video.ContentLength + audio.ContentLength + compactMuxOverheadReserve
			if estimated > maxBytes {
				continue
			}
			area := video.Width * video.Height
			bestArea := bestVideo.Width * bestVideo.Height
			if !found || area > bestArea ||
				(area == bestArea && video.Bandwidth > bestVideo.Bandwidth) ||
				(area == bestArea && video.Bandwidth == bestVideo.Bandwidth && audio.Bandwidth > bestAudio.Bandwidth) {
				bestVideo, bestAudio = video, audio
				found = true
			}
		}
	}
	if !found {
		return CompactPreviewSources{}, fmt.Errorf("DASH manifest has no audio/video pair within %d bytes", maxBytes)
	}

	return CompactPreviewSources{
		Video: Media{
			TypeName: "GraphVideo",
			URL:      strings.TrimSpace(bestVideo.BaseURL),
			Width:    bestVideo.Width,
			Height:   bestVideo.Height,
		},
		AudioURL:       strings.TrimSpace(bestAudio.BaseURL),
		VideoBytes:     bestVideo.ContentLength,
		AudioBytes:     bestAudio.ContentLength,
		EstimatedBytes: bestVideo.ContentLength + bestAudio.ContentLength + compactMuxOverheadReserve,
	}, nil
}

func dashTrackKind(contentType, mimeType string) string {
	if kind := strings.ToLower(strings.TrimSpace(contentType)); kind == "video" || kind == "audio" {
		return kind
	}
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return ""
	}
}

func compactVideoFromDASHManifest(raw string, maxBytes int64) (Media, error) {
	sources, err := compactAVFromDASHManifest(raw, maxBytes)
	if err != nil {
		return Media{}, err
	}
	return sources.Video, nil
}
