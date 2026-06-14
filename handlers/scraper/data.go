package handlers

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"instafix/observability"
	"instafix/utils"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kelindar/binary"
	"github.com/klauspost/compress/gzhttp"
	"github.com/klauspost/compress/zstd"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
	"github.com/tidwall/gjson"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

var (
	RemoteScraperAddr string
	AuthHelperURL     string
	ErrNotFound       = errors.New("post not found")
	ErrRestricted     = errors.New("Instagram content restricted")
	timeout           = 5 * time.Second
	transport         http.RoundTripper
	transportNoProxy  *http.Transport
	sflightScraper    singleflight.Group
	remoteZSTDReader  *zstd.Decoder
)

//go:embed dictionary.bin
var zstdDict []byte

type Media struct {
	TypeName     string
	URL          string
	ThumbnailURL string
	Width        int
	Height       int
}

func (m Media) IsVideo() bool { return strings.Contains(strings.ToLower(m.TypeName), "video") }
func (m Media) IsImage() bool { return !m.IsVideo() }

type InstaData struct {
	PostID   string
	Username string
	Caption  string
	Medias   []Media
}

func (i *InstaData) HasVideo() bool {
	if i == nil {
		return false
	}
	for _, media := range i.Medias {
		if media.IsVideo() {
			return true
		}
	}
	return false
}

func init() {
	var err error
	transport = gzhttp.Transport(http.DefaultTransport, gzhttp.TransportAlwaysDecompress(true))
	transportNoProxy = http.DefaultTransport.(*http.Transport).Clone()
	transportNoProxy.Proxy = nil // Skip any proxy

	remoteZSTDReader, err = zstd.NewReader(nil, zstd.WithDecoderLowmem(true), zstd.WithDecoderDicts(zstdDict))
	if err != nil {
		panic(err)
	}
}

func GetData(postID string) (*InstaData, error) {
	if !validPostID(postID) {
		return nil, errors.New("postID is not a valid Instagram post ID")
	}

	i := &InstaData{PostID: postID}
	err := DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(postID))
		if v == nil {
			return nil
		}
		err := binary.Unmarshal(v, i)
		if err != nil {
			slog.Warn("Failed to unmarshal cached data; ignoring stale cache", "postID", postID, "err", err)
			*i = InstaData{PostID: postID}
			return nil
		}
		slog.Debug("Data parsed from cache", "postID", postID)
		return nil
	})
	if err != nil {
		observability.Default.RecordDBError("cache_read", err)
		return nil, err
	}

	// Successfully parsed from cache
	if len(i.Medias) != 0 {
		observability.Default.RecordCacheHit()
		return i, nil
	}

	ret, err, _ := sflightScraper.Do(postID, func() (interface{}, error) {
		item := new(InstaData)
		item.PostID = postID
		if err := item.ScrapeData(); err != nil {
			observability.Default.RecordScrape(false, item.PostID, err)
			return nil, err
		}

		if err := normalizeMediaURLs(item); err != nil {
			slog.Error("Failed to normalize media URLs", "postID", item.PostID, "err", err)
			return false, err
		}

		if err := saveDataToCache(item); err != nil {
			return false, err
		}
		observability.Default.RecordScrape(true, item.PostID, nil)
		return item, nil
	})
	if err != nil {
		return nil, err
	}
	return ret.(*InstaData), nil
}

func GetDataPreferVideo(postID string) (*InstaData, error) {
	item, err := GetData(postID)
	if err == nil && item.HasVideo() {
		return item, nil
	}
	refreshed, refreshErr := RefreshDataFromAuthHelper(postID)
	if refreshErr == nil && len(refreshed.Medias) > 0 {
		return refreshed, nil
	}
	if refreshErr != nil {
		slog.Debug("Failed to refresh video data from auth helper", "postID", postID, "err", refreshErr)
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

func RefreshDataFromAuthHelper(postID string) (*InstaData, error) {
	if !validPostID(postID) {
		return nil, errors.New("postID is not a valid Instagram post ID")
	}
	item := &InstaData{PostID: postID}
	if err := scrapeAuthHelper(item); err != nil {
		return nil, err
	}
	if len(item.Medias) == 0 {
		return nil, ErrNotFound
	}
	if err := normalizeMediaURLs(item); err != nil {
		return nil, err
	}
	if err := saveDataToCache(item); err != nil {
		slog.Warn("Failed to save auth helper refresh to cache", "postID", item.PostID, "err", err)
	}
	return item, nil
}

func normalizeMediaURLs(item *InstaData) error {
	// Replace public image CDN hosts with scontent.cdninstagram.com while preserving
	// original video CDN hosts. Video URLs are signed and host-sensitive.
	for n, media := range item.Medias {
		u, err := url.Parse(media.URL)
		if err != nil {
			return err
		}
		if !media.IsVideo() {
			u.Host = "scontent.cdninstagram.com"
		}
		item.Medias[n].URL = u.String()
		if media.ThumbnailURL != "" {
			thumb, err := url.Parse(media.ThumbnailURL)
			if err == nil && thumb.Host != "" && (thumb.Scheme == "http" || thumb.Scheme == "https") {
				thumb.Host = "scontent.cdninstagram.com"
				item.Medias[n].ThumbnailURL = thumb.String()
			}
		}
		if media.Width < 0 {
			item.Medias[n].Width = 0
		}
		if media.Height < 0 {
			item.Medias[n].Height = 0
		}
	}
	return nil
}

func saveDataToCache(item *InstaData) error {
	bb, err := binary.Marshal(item)
	if err != nil {
		slog.Error("Failed to marshal data", "postID", item.PostID, "err", err)
		return err
	}

	err = DB.Batch(func(tx *bolt.Tx) error {
		dataBucket := tx.Bucket([]byte("data"))
		if dataBucket == nil {
			return nil
		}
		dataBucket.Put(utils.S2B(item.PostID), bb)

		ttlBucket := tx.Bucket([]byte("ttl"))
		if ttlBucket == nil {
			return nil
		}
		expTime := strconv.FormatInt(time.Now().Add(24*time.Hour).UnixNano(), 10)
		ttlBucket.Put(utils.S2B(expTime), utils.S2B(item.PostID))
		return nil
	})
	if err != nil {
		slog.Error("Failed to save data to cache", "postID", item.PostID, "err", err)
		observability.Default.RecordDBError("cache_write", err)
		return err
	}
	return nil
}

func validPostID(postID string) bool {
	if len(postID) < 6 || len(postID) > 32 {
		return false
	}
	for _, r := range postID {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (i *InstaData) ScrapeData() error {
	// Scrape from remote scraper if available
	if len(RemoteScraperAddr) > 0 {
		remoteClient := http.Client{Transport: transportNoProxy, Timeout: timeout}
		req, err := http.NewRequest("GET", RemoteScraperAddr+"/scrape/"+i.PostID, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept-Encoding", "zstd.dict")
		res, err := remoteClient.Do(req)
		if err == nil && res != nil {
			defer res.Body.Close()
			remoteData, err := io.ReadAll(res.Body)
			if err == nil && res.StatusCode == 200 {
				remoteDecomp, err := remoteZSTDReader.DecodeAll(remoteData, nil)
				if err != nil {
					slog.Warn("remote scraper decode failed; using local fallback", "postID", i.PostID, "err", err)
				} else if err := binary.Unmarshal(remoteDecomp, i); err == nil {
					if len(i.Username) > 0 && len(i.Medias) > 0 {
						slog.Info("Data parsed from remote scraper", "postID", i.PostID)
						return nil
					}
				}
			}
			slog.Warn("Failed to scrape data from remote scraper", "postID", i.PostID, "status", res.StatusCode, "err", err)
		}
		if err != nil {
			slog.Warn("Failed when trying to scrape data from remote scraper", "postID", i.PostID, "err", err)
		}
	}

	client := http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequest("GET", "https://www.instagram.com/p/"+i.PostID+"/embed/captioned/", nil)
	if err != nil {
		return err
	}

	var body []byte
	for retries := 0; retries < 3; retries++ {
		err := func() error {
			res, err := client.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				return errors.New("status code is not 200")
			}

			body, err = io.ReadAll(res.Body)
			if err != nil {
				return err
			}
			return nil
		}()
		if err == nil {
			break
		}
	}

	var embedData gjson.Result
	var timeSliceData gjson.Result
	if len(body) > 0 {
		var scriptText []byte

		// TimeSliceImpl (very fragile)
		for _, line := range bytes.Split(body, []byte("\n")) {
			if bytes.Contains(line, []byte("shortcode_media")) {
				scriptText = line
				break
			}
		}

		if len(scriptText) > 0 {
			// Remove <script>
			findFirstMoreThan := bytes.Index(scriptText, []byte(">"))
			scriptText = scriptText[findFirstMoreThan+1:]

			lexer := js.NewLexer(parse.NewInputBytes(scriptText))
			for {
				tt, text := lexer.Next()
				if tt == js.ErrorToken || text == nil {
					break
				}
				if tt == js.StringToken && bytes.Contains(text, []byte("shortcode_media")) {
					// Strip quotes from start and end
					text = text[1 : len(text)-1]
					unescapeData := utils.UnescapeJSONString(utils.B2S(text))
					if !gjson.Valid(unescapeData) {
						slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "invalid JSON")
						continue
					}
					timeSliceData = gjson.Parse(unescapeData).Get("gql_data")
				}
			}
		} else {
			slog.Debug("Failed to parse data from TimeSliceImpl", "postID", i.PostID, "err", "No script found")
		}

		// Scrape from embed HTML
		embedHTML, err := scrapeFromEmbedHTML(body)
		if err != nil {
			slog.Debug("Failed to parse data from scrapeFromEmbedHTML", "postID", i.PostID, "err", err)
		} else {
			embedData = gjson.Parse(embedHTML)
		}
	}

	var gqlData gjson.Result
	videoBlocked := bytes.Contains(body, []byte("WatchOnInstagram"))
	// Scrape from GraphQL API only if video is blocked or embed data is empty
	if videoBlocked || len(body) == 0 || (!timeSliceData.Exists() && !embedData.Exists()) {
		gqlValue, err := scrapeFromGQL(i.PostID)
		if err != nil {
			slog.Debug("Failed to scrape data from scrapeFromGQL", "postID", i.PostID, "err", err)
		}
		if gqlValue != nil && !bytes.Contains(gqlValue, []byte("require_login")) {
			gqlData = gjson.Parse(utils.B2S(gqlValue)).Get("data")
			slog.Info("Data parsed from GraphQL API", "postID", i.PostID)
		}
	}

	// If gqlData is blocked, use timeSliceData or embedData
	if !gqlData.Exists() {
		if timeSliceData.Exists() {
			gqlData = timeSliceData
			slog.Info("Data parsed from TimeSliceImpl", "postID", i.PostID)
		} else {
			gqlData = embedData
			if embedData.Exists() {
				slog.Info("Data parsed from embedHTML", "postID", i.PostID)
			}
		}
	}

	status := gqlData.Get("status").String()
	item := gqlData.Get("shortcode_media")
	if !item.Exists() {
		item = gqlData.Get("xdt_shortcode_media")
		if !item.Exists() {
			if err := scrapeAuthHelper(i); err == nil {
				return nil
			} else if err != ErrNotFound {
				slog.Debug("Failed to scrape data from auth helper", "postID", i.PostID, "err", err)
			}
			if status == "fail" {
				if err := scrapeRestriction(i.PostID); err != nil {
					return err
				}
				return errors.New("scrapeFromGQL is blocked")
			}
			if err := scrapeRestriction(i.PostID); err != nil {
				return err
			}
			return ErrNotFound
		}
	}

	media := []gjson.Result{item}
	if item.Get("edge_sidecar_to_children").Exists() {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	}

	// Get username
	i.Username = item.Get("owner.username").String()

	// Get caption
	i.Caption = strings.TrimSpace(item.Get("edge_media_to_caption.edges.0.node.text").String())

	// Get medias
	i.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		thumbnailURL := ""
		displayURL := strings.TrimSpace(m.Get("display_url").String())
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		} else if displayURL != "" {
			if u, err := url.Parse(displayURL); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
				thumbnailURL = displayURL
			}
		}
		rawURL := mediaURL.String()
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		typeName := m.Get("__typename").String()
		if strings.Contains(strings.ToLower(typeName), "video") && !m.Get("video_url").Exists() {
			continue
		}
		width := int(m.Get("dimensions.width").Int())
		height := int(m.Get("dimensions.height").Int())
		i.Medias = append(i.Medias, Media{
			TypeName:     typeName,
			URL:          rawURL,
			ThumbnailURL: thumbnailURL,
			Width:        width,
			Height:       height,
		})
	}

	// Failed to scrape from Embed
	if len(i.Medias) == 0 {
		if err := scrapeAuthHelper(i); err == nil {
			return nil
		} else if err != ErrNotFound {
			slog.Debug("Failed to scrape data from auth helper", "postID", i.PostID, "err", err)
		}
		if err := scrapeRestriction(i.PostID); err != nil {
			return err
		}
		return ErrNotFound
	}
	return nil
}

// Taken from https://github.com/PuerkitoBio/goquery
// Modified to add new line every <br>
func gqTextNewLine(s *goquery.Selection) string {
	// Slightly optimized vs calling Each: no single selection object created
	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			// Keep newlines and spaces, like jQuery
			sb.WriteString(n.Data)
		} else if n.Type == html.ElementNode && n.Data == "br" {
			sb.WriteString("\n")
		}
		if n.FirstChild != nil {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}
	}
	for _, n := range s.Nodes {
		f(n)
	}
	return sb.String()
}

func scrapeFromEmbedHTML(embedHTML []byte) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(embedHTML))
	if err != nil {
		return "", err
	}

	// Get media URL
	typename := "GraphImage"
	embedMedia := doc.Find(".EmbeddedMediaImage, meta[property='og:image']")
	if embedMedia.Length() == 0 {
		typename = "GraphVideo"
		embedMedia = doc.Find(".EmbeddedMediaVideo, video source, meta[property='og:video'], meta[property='og:video:secure_url']")
	}
	mediaURL, ok := embedMedia.Attr("src")
	if !ok {
		mediaURL, ok = embedMedia.Attr("content")
	}
	if !ok {
		return "", ErrNotFound
	}

	// Get username
	username := doc.Find(".UsernameText").Text()

	// Get caption
	captionComments := doc.Find(".CaptionComments")
	if captionComments.Length() > 0 {
		captionComments.Remove()
	}
	captionUsername := doc.Find(".CaptionUsername")
	if captionUsername.Length() > 0 {
		captionUsername.Remove()
	}
	caption := gqTextNewLine(doc.Find(".Caption"))

	// Check if contains WatchOnInstagram
	videoBlocked := strconv.FormatBool(bytes.Contains(embedHTML, []byte("WatchOnInstagram")))

	// Totally safe 100% valid JSON 👍
	return `{
		"shortcode_media": {
			"owner": {"username": "` + username + `"},
			"node": {"__typename": "` + typename + `", "display_url": "` + mediaURL + `"},
			"edge_media_to_caption": {"edges": [{"node": {"text": ` + utils.EscapeJSONString(caption) + `}}]},
			"dimensions": {"height": null, "width": null},
			"video_blocked": ` + videoBlocked + `
		}
	}`, nil
}

// scrapeRestriction identifies posts that Instagram intentionally excludes from
// public embeds. It is only called after all media parsers fail, so it does not
// add latency to successful scrapes or replace the original not-found result
// when the diagnostic endpoint is unavailable.
func scrapeRestriction(postID string) error {
	postURL := "https://www.instagram.com/reel/" + postID + "/"
	oembedURL := "https://www.instagram.com/api/v1/oembed/?" + url.Values{"url": {postURL}}.Encode()
	req, err := http.NewRequest("GET", oembedURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; InstaFix/1.0)")

	client := http.Client{Transport: transport, Timeout: timeout}
	res, err := client.Do(req)
	if err != nil || res == nil {
		return nil
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil || !gjson.ValidBytes(body) {
		return nil
	}

	data := gjson.ParseBytes(body)
	if data.Get("message").String() != "geoblock_required" {
		return nil
	}
	title := strings.TrimSpace(data.Get("title").String())
	reason := strings.TrimSpace(data.Get("blocks_logging_data").String())
	if title == "" {
		title = "public embed unavailable"
	}
	if reason == "" {
		reason = "geoblock_required"
	}
	return fmt.Errorf("%w: %s (%s)", ErrRestricted, title, reason)
}

func scrapeAuthHelper(i *InstaData) error {
	if AuthHelperURL == "" {
		return ErrNotFound
	}
	base, err := url.Parse(AuthHelperURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("invalid auth helper URL")
	}
	u := base.ResolveReference(&url.URL{Path: strings.TrimRight(base.Path, "/") + "/oembed/" + i.PostID})
	client := http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil || res == nil {
		if err == nil {
			err = errors.New("auth helper returned no response")
		}
		observability.Default.RecordAuthHelperResult(false, i.PostID, "auth_helper_unreachable", err)
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 256*1024))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		message := gjson.GetBytes(body, "error").String()
		code := gjson.GetBytes(body, "error_code").String()
		if message == "" {
			message = res.Status
		}
		err := fmt.Errorf("auth helper HTTP %s: %s", res.Status, message)
		observability.Default.RecordAuthHelperResult(false, i.PostID, code, err)
		return err
	}
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "ok").Bool() {
		return errors.New("auth helper returned invalid response")
	}
	username := strings.TrimSpace(gjson.GetBytes(body, "username").String())
	caption := strings.TrimSpace(gjson.GetBytes(body, "caption").String())
	thumbnail := strings.TrimSpace(gjson.GetBytes(body, "thumbnail_url").String())
	video := strings.TrimSpace(gjson.GetBytes(body, "video_url").String())
	width := int(gjson.GetBytes(body, "width").Int())
	height := int(gjson.GetBytes(body, "height").Int())
	mediaURL, err := url.Parse(thumbnail)
	if err != nil || mediaURL.Host == "" || (mediaURL.Scheme != "http" && mediaURL.Scheme != "https") {
		return errors.New("auth helper returned invalid thumbnail_url")
	}
	if username == "" {
		username = "instagram"
	}
	i.Username = username
	i.Caption = caption
	if video != "" {
		videoURL, err := url.Parse(video)
		if err == nil && videoURL.Host != "" && (videoURL.Scheme == "http" || videoURL.Scheme == "https") {
			i.Medias = []Media{{TypeName: "GraphVideo", URL: video, ThumbnailURL: thumbnail, Width: width, Height: height}}
		} else {
			i.Medias = []Media{{TypeName: "GraphImage", URL: thumbnail}}
		}
	} else {
		i.Medias = []Media{{TypeName: "GraphImage", URL: thumbnail}}
	}
	observability.Default.RecordAuthHelperResult(true, i.PostID, "", nil)
	slog.Info("Data parsed from auth helper", "postID", i.PostID)
	return nil
}

func scrapeFromGQL(postID string) ([]byte, error) {
	gqlParams := url.Values{
		"av":                       {"0"},
		"__d":                      {"www"},
		"__user":                   {"0"},
		"__a":                      {"1"},
		"__req":                    {"k"},
		"__hs":                     {"19888.HYP:instagram_web_pkg.2.1..0.0"},
		"dpr":                      {"2"},
		"__ccg":                    {"UNKNOWN"},
		"__rev":                    {"1014227545"},
		"__s":                      {"trbjos:n8dn55:yev1rm"},
		"__hsi":                    {"7380500578385702299"},
		"__dyn":                    {"7xeUjG1mxu1syUbFp40NonwgU7SbzEdF8aUco2qwJw5ux609vCwjE1xoswaq0yE6ucw5Mx62G5UswoEcE7O2l0Fwqo31w9a9wtUd8-U2zxe2GewGw9a362W2K0zK5o4q3y1Sx-0iS2Sq2-azo7u3C2u2J0bS1LwTwKG1pg2fwxyo6O1FwlEcUed6goK2O4UrAwCAxW6Uf9EObzVU8U"},
		"__csr":                    {"n2Yfg_5hcQAG5mPtfEzil8Wn-DpKGBXhdczlAhrK8uHBAGuKCJeCieLDyExenh68aQAKta8p8ShogKkF5yaUBqCpF9XHmmhoBXyBKbQp0HCwDjqoOepV8Tzk8xeXqAGFTVoCciGaCgvGUtVU-u5Vp801nrEkO0rC58xw41g0VW07ISyie2W1v7F0CwYwwwvEkw8K5cM0VC1dwdi0hCbc094w6MU1xE02lzw"},
		"__comet_req":              {"7"},
		"lsd":                      {"AVoPBTXMX0Y"},
		"jazoest":                  {"2882"},
		"__spin_r":                 {"1014227545"},
		"__spin_b":                 {"trunk"},
		"__spin_t":                 {"1718406700"},
		"fb_api_caller_class":      {"RelayModern"},
		"fb_api_req_friendly_name": {"PolarisPostActionLoadPostQueryQuery"},
		"variables":                {`{"shortcode":"` + postID + `","fetch_comment_count":40,"parent_comment_count":24,"child_comment_count":3,"fetch_like_count":10,"fetch_tagged_user_count":null,"fetch_preview_comment_count":2,"has_threaded_comments":true,"hoisted_comment_id":null,"hoisted_reply_id":null}`},
		"server_timestamps":        {"true"},
		"doc_id":                   {"25531498899829322"},
	}
	req, err := http.NewRequest("POST", "https://www.instagram.com/graphql/query/", strings.NewReader(gqlParams.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"Accept":                      {"*/*"},
		"Accept-Language":             {"en-US,en;q=0.9"},
		"Content-Type":                {"application/x-www-form-urlencoded"},
		"Origin":                      {"https://www.instagram.com"},
		"Priority":                    {"u=1, i"},
		"Sec-Ch-Prefers-Color-Scheme": {"dark"},
		"Sec-Ch-Ua":                   {`"Google Chrome";v="125", "Chromium";v="125", "Not.A/Brand";v="24"`},
		"Sec-Ch-Ua-Full-Version-List": {`"Google Chrome";v="125.0.6422.142", "Chromium";v="125.0.6422.142", "Not.A/Brand";v="24.0.0.0"`},
		"Sec-Ch-Ua-Mobile":            {"?0"},
		"Sec-Ch-Ua-Model":             {`""`},
		"Sec-Ch-Ua-Platform":          {`"macOS"`},
		"Sec-Ch-Ua-Platform-Version":  {`"12.7.4"`},
		"Sec-Fetch-Dest":              {"empty"},
		"Sec-Fetch-Mode":              {"cors"},
		"Sec-Fetch-Site":              {"same-origin"},
		"User-Agent":                  {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"},
		"X-Asbd-Id":                   {"129477"},
		"X-Bloks-Version-Id":          {"e2004666934296f275a5c6b2c9477b63c80977c7cc0fd4b9867cb37e36092b68"},
		"X-Fb-Friendly-Name":          {"PolarisPostActionLoadPostQueryQuery"},
		"X-Ig-App-Id":                 {"936619743392459"},
	}

	client := http.Client{Transport: transport, Timeout: timeout}
	res, err := client.Do(req)
	if err != nil || res == nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GraphQL HTTP status %s", res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(body) {
		return nil, errors.New("GraphQL returned invalid JSON")
	}
	return body, nil
}
