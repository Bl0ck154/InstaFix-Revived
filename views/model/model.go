package model

type ViewsData struct {
	Card         string
	ExtraCard    string
	Title        string `default:"Instagram fixed preview"`
	ImageURL     string `default:""`
	ImageURLs    []string
	VideoURL     string `default:""`
	URL          string
	CanonicalURL string
	Description  string
	OEmbedURL    string
	Site         string
	TwitterSite  string
	Creator      string
	OGType       string
	ImageWidth   int
	ImageHeight  int
	ImageAlt     string
	Width        int `default:"400"`
	Height       int `default:"400"`
}

type OEmbedData struct {
	Text string
	URL  string
}
