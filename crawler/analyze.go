package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Options struct {
	URL         string
	Depth       int64
	Retries     int64
	Delay       string
	Timeout     string
	UserAgent   *string
	Concurrency int64
	IndentJSON  string
	HTTPClient  *http.Client
}

var (
	ErrorInvalidURL = errors.New("invalid url")
)

func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	if !isValidWebURL(opts.URL) {
		return nil, ErrorInvalidURL
	}
	crawler := NewCrawler(opts)
	report := crawler.GetReport(ctx, 1) // TODO: calculate depth
	return toFormattedJSON(report, opts.IndentJSON)
}

type Crawler struct {
	client *http.Client
	URL    string
}

func NewCrawler(opts Options) *Crawler {
	return &Crawler{
		client: opts.HTTPClient,
		URL:    opts.URL,
	}
}

type Report struct {
	RootURL     string       `json:"root_url"`
	Depth       int          `json:"depth"`
	GeneratedAt time.Time    `json:"generated_at"`
	Pages       []PageReport `json:"pages"`
}

type PageReport struct {
	URL        string `json:"url"`
	Depth      int    `json:"depth"`
	HTTPStatus int    `json:"http_status"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

func (c *Crawler) GetReport(ctx context.Context, depth int) Report {
	pageReport := c.GetPageReport(ctx, c.URL, depth-1)
	report := Report{
		RootURL:     c.URL,
		Depth:       depth,
		GeneratedAt: time.Now().Truncate(time.Second),
		Pages:       []PageReport{pageReport},
	}
	return report
}

func (c *Crawler) GetPageReport(ctx context.Context, url string, depth int) PageReport {
	report := PageReport{
		URL:   url,
		Depth: depth,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		report.Status = "error"
		report.Error = fmt.Errorf("error creating request: %w", err).Error()
		return report
	}
	resp, err := c.client.Do(req)
	if err != nil {
		report.Status = "error"
		report.Error = fmt.Errorf("error sending request: %w", err).Error()
		return report
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	report.HTTPStatus = resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !ok {
		report.Status = "error"
		report.Error = resp.Status
	} else {
		report.Status = "ok"
	}
	return report
}

func isValidWebURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}

func toFormattedJSON(v Report, indentJSON string) ([]byte, error) {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return payload, nil
}
