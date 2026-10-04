package crawler

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"
)

type Report struct {
	RootURL     string       `json:"root_url"`
	Depth       int          `json:"depth"`
	GeneratedAt time.Time    `json:"generated_at"`
	Pages       []PageReport `json:"pages"`
}

type PageReport struct {
	URL          string             `json:"url"`
	Depth        int                `json:"depth"`
	HTTPStatus   int                `json:"http_status"`
	Status       string             `json:"status"`
	Error        string             `json:"error"`
	SEO          SEO                `json:"seo"`
	BrokenLinks  []BrokenLinkReport `json:"broken_links"`
	Assets       []AssetsReport     `json:"assets"`
	DiscoveredAt time.Time          `json:"discovered_at"`
}

type BrokenLinkReport struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error"`
}

type AssetsReport struct {
	URL        string `json:"url"`
	Type       string `json:"type"`
	StatusCode int    `json:"status_code"`
	SizeBytes  int    `json:"size_bytes"`
	Error      string `json:"error"`
}

func toFormattedJSON(v Report, withIndent bool) ([]byte, error) {
	var payload []byte
	var err error

	if withIndent {
		payload, err = json.MarshalIndent(v, "", "  ")
	} else {
		payload, err = json.Marshal(v)
	}
	payload = append(payload, '\n')
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func sortReportLinks(report Report) Report {
	slices.SortFunc(report.Pages, func(a, b PageReport) int {
		if a.Depth != b.Depth {
			return cmp.Compare(a.Depth, b.Depth)
		}
		return cmp.Compare(a.URL, b.URL)
	})
	for i := range report.Pages {
		slices.SortFunc(report.Pages[i].BrokenLinks, func(a, b BrokenLinkReport) int {
			return cmp.Compare(a.URL, b.URL)
		})
		slices.SortFunc(report.Pages[i].Assets, func(a, b AssetsReport) int {
			return cmp.Compare(a.URL, b.URL)
		})
	}
	return report
}
