package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxJinaMetadataBodyBytes int64 = 1 << 20

type jinaPublication struct {
	item    *InstaData
	isVideo bool
}

// EnrichMetadataFromJina is a last-resort metadata-only fallback for public
// Instagram posts whose normal logged-out/crawler responses are gated.
// Disabled by default: enabling sends public post URLs to a third-party reader. It never
// uses Jina for media delivery: only missing username/caption fields are merged
// into an already resolved InstaData item.
func EnrichMetadataFromJina(item *InstaData) error {
	if item == nil || !validPostID(item.PostID) {
		return ErrNotFound
	}
	if item.Username != "" && item.Caption != "" {
		return nil
	}
	if !envBool("INSTAFIX_JINA_METADATA_FALLBACK", false) {
		return ErrNotFound
	}
	publication, err := refreshPublicationFromJina(item.PostID)
	if err != nil {
		return err
	}
	mergeAvailableMetadata(item, publication.item)
	if item.Username == "" && item.Caption == "" {
		return ErrNotFound
	}
	if err := saveDataToCache(item); err != nil {
		slog.Debug("Failed to cache Jina-enriched metadata", "postID", item.PostID, "err", err)
	}
	return nil
}

// EnrichMetadataAndDetectVideoFromJina is used only after the normal public
// Instagram resolvers fail to classify a /p/{shortcode} publication. Jina is
// still metadata-only: it may confirm that the publication is a video, but it
// never supplies a media URL.
func EnrichMetadataAndDetectVideoFromJina(item *InstaData) (bool, error) {
	if item == nil || !validPostID(item.PostID) {
		return false, ErrNotFound
	}
	if !envBool("INSTAFIX_JINA_METADATA_FALLBACK", false) {
		return false, ErrNotFound
	}
	publication, err := refreshPublicationFromJina(item.PostID)
	if err != nil {
		return false, err
	}
	mergeAvailableMetadata(item, publication.item)
	// Do not create a fresh cache entry that contains metadata but no media.
	// If the normal resolver produced a poster/image shell, enrich that cached
	// item; otherwise keep this Jina-only classification request-scoped.
	if len(item.Medias) > 0 && (item.Username != "" || item.Caption != "") {
		if err := saveDataToCache(item); err != nil {
			slog.Debug("Failed to cache Jina-enriched publication metadata", "postID", item.PostID, "err", err)
		}
	}
	return publication.isVideo, nil
}

func refreshPublicationFromJina(postID string) (*jinaPublication, error) {
	ret, err, _ := sflightScraper.Do(postID+":jina-metadata", func() (interface{}, error) {
		u := "https://r.jina.ai/http://www.instagram.com/reel/" + url.PathEscape(postID) + "/"
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/plain,text/markdown;q=0.9,*/*;q=0.1")
		req.Header.Set("User-Agent", "InstaFix-Metadata/1.0")
		client := http.Client{
			Transport: transportNoProxy,
			Timeout:   envDurationMilliseconds("INSTAFIX_JINA_METADATA_TIMEOUT_MS", 8*time.Second),
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Jina metadata HTTP %s", res.Status)
		}
		body, err := readLimitedHTTPBody(res, maxJinaMetadataBodyBytes)
		if err != nil {
			return nil, err
		}
		item, err := parseJinaInstagramMetadata(postID, string(body))
		if err != nil {
			return nil, err
		}
		publication := &jinaPublication{
			item:    item,
			isVideo: jinaInstagramBodyHasVideo(postID, string(body)),
		}
		slog.Info("Data enriched from Jina Instagram reader", "postID", postID, "username", item.Username != "", "caption", item.Caption != "", "is_video", publication.isVideo)
		return publication, nil
	})
	if err != nil {
		return nil, err
	}
	return ret.(*jinaPublication), nil
}

func jinaInstagramBodyHasVideo(postID, body string) bool {
	body = strings.ToLower(strings.ReplaceAll(body, "\r\n", "\n"))
	postID = strings.ToLower(strings.TrimSpace(postID))
	return strings.Contains(body, ": video by ") ||
		(strings.Contains(body, "[video ") && strings.Contains(body, "](blob:http://www.instagram.com/")) ||
		(postID != "" && strings.Contains(body, "/reel/"+postID+"/"))
}

func parseJinaInstagramMetadata(postID, body string) (*InstaData, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	item := &InstaData{PostID: postID}

	// Jina emits a compact title block before "URL Source:". Instagram's title
	// contains the post caption, and collecting until the blank separator keeps
	// captions that contain line breaks.
	if start := strings.Index(body, "Title: "); start >= 0 {
		title := body[start+len("Title: "):]
		if end := strings.Index(title, "\n\nURL Source:"); end >= 0 {
			title = title[:end]
		} else if end := strings.Index(title, "\n\n"); end >= 0 {
			title = title[:end]
		}
		title = strings.TrimSpace(title)
		const marker = " on Instagram: \""
		if idx := strings.Index(title, marker); idx >= 0 {
			caption := strings.TrimSpace(title[idx+len(marker):])
			caption = strings.TrimSpace(strings.TrimSuffix(caption, "\""))
			if len(caption) > maxCaptionLength {
				caption = caption[:maxCaptionLength]
			}
			item.Caption = caption
		}
	}

	const neverMiss = "Never miss a post from "
	if idx := strings.Index(body, neverMiss); idx >= 0 {
		rest := body[idx+len(neverMiss):]
		end := len(rest)
		for i, r := range rest {
			if !(r == '.' || r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
				end = i
				break
			}
		}
		username := strings.TrimSpace(rest[:end])
		if username != "" && len(username) <= maxUsernameLength {
			item.Username = username
		}
	}

	if item.Username == "" {
		// Some posts omit the "Never miss" banner. Fall back to the first
		// profile-style Markdown link while excluding Instagram application paths.
		const profilePrefix = "](http://www.instagram.com/"
		search := body
		for {
			idx := strings.Index(search, profilePrefix)
			if idx < 0 {
				break
			}
			rest := search[idx+len(profilePrefix):]
			end := strings.Index(rest, "/)")
			if end < 0 {
				break
			}
			candidate := strings.TrimSpace(rest[:end])
			switch candidate {
			case "", "reel", "reels", "p", "accounts", "explore":
			default:
				valid := len(candidate) <= maxUsernameLength
				for _, r := range candidate {
					if !(r == '.' || r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
						valid = false
						break
					}
				}
				if valid {
					item.Username = candidate
				}
			}
			if item.Username != "" {
				break
			}
			search = rest[end+2:]
		}
	}

	if item.Username == "" && item.Caption == "" {
		return nil, errors.New("Jina metadata did not contain Instagram identity or caption")
	}
	return item, nil
}
