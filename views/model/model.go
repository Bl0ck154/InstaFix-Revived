package model

type ViewsData struct {
	Card         string
	ExtraCard    string
	Title        string `default:"InstaFix"`
	ImageURL     string `default:""`
	VideoURL     string `default:""`
	URL          string
	CanonicalURL string
	Description  string
	OEmbedURL    string
	Site         string
	TwitterSite  string
	Creator      string
	OGType       string
	Width        int `default:"400"`
	Height       int `default:"400"`
}

type OEmbedData struct {
	Text string
	URL  string
}
