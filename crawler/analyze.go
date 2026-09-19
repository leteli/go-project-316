package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
	ErrorInvalidURL         = errors.New("invalid url")
	ErrorHTTPClientRequired = errors.New("http client is required")
	ErrorNotHTML            = errors.New("not an HTML page")
)

func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	if opts.HTTPClient == nil {
		return nil, ErrorHTTPClientRequired
	}
	if !isValidWebURLStr(opts.URL) {
		return nil, ErrorInvalidURL
	}
	crawler := NewCrawler(opts)
	report := crawler.GetReport(ctx, 1) // TODO: calculate depth
	return toFormattedJSON(report)
}

type Crawler struct {
	client     *http.Client
	URL        string
	PagesQueue []string
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
	URL         string             `json:"url"`
	Depth       int                `json:"depth"`
	HTTPStatus  int                `json:"http_status"`
	Status      string             `json:"status"`
	Error       string             `json:"error"`
	BrokenLinks []BrokenLinkReport `json:"broken_links"`
}

type LinkResult struct {
	URL        string
	Kind       string
	StatusCode int
	Error      string
}

type BrokenLinkReport struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error"`
}

func (c *Crawler) GetReport(ctx context.Context, depth int) Report {
	pageReport := c.GetPageReport(ctx, c.URL, 0)
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
		URL:         url,
		Depth:       depth,
		BrokenLinks: make([]BrokenLinkReport, 0),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		report.Status = "error"
		report.Error = fmt.Sprintf("error creating request: %v", err)
		return report
	}
	resp, err := c.client.Do(req)
	if err != nil {
		report.Status = "error"
		report.Error = fmt.Sprintf("error sending request: %v", err)
		return report
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	report.HTTPStatus = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		report.Status = "error"
		report.Error = resp.Status
		return report
	}
	if !isHTMLPage(resp) {
		report.Status = "error"
		report.Error = ErrorNotHTML.Error()
		return report
	}
	resolvedURL := resp.Request.URL
	links, err := ExtractHTTPLinksFromHTML(
		resp.Body,
		resolvedURL.String(),
	)
	if err != nil {
		report.Status = "error"
		report.Error = fmt.Sprintf("parse error: %v", err)
		return report
	}
	report.Status = "ok"

	for _, link := range links {
		if err := ctx.Err(); err != nil {
			report.Status = "error"
			report.Error = err.Error()
			return report
		}

		linkRes := c.CheckExtractedLink(ctx, link, resolvedURL)

		if err := ctx.Err(); err != nil {
			report.Status = "error"
			report.Error = err.Error()
			return report
		}

		if linkRes.Kind == "broken" {
			report.BrokenLinks = append(report.BrokenLinks, BrokenLinkReport{
				URL:        linkRes.URL,
				StatusCode: linkRes.StatusCode,
				Error:      linkRes.Error,
			})
		}
	}
	return report
}

func (c *Crawler) CheckExtractedLink(ctx context.Context, link string, base *url.URL) LinkResult {
	if ctx.Err() != nil {
		return LinkResult{}
	}
	if !isValidWebURLStr(link) {
		return LinkResult{}
	}
	// TODO: use goroutines
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err != nil {
		return LinkResult{
			URL:   link,
			Kind:  "broken",
			Error: err.Error(),
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return LinkResult{}
		}
		return LinkResult{
			URL:   link,
			Kind:  "broken",
			Error: err.Error(),
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		return LinkResult{
			URL:        link,
			Kind:       "broken",
			StatusCode: resp.StatusCode,
			Error:      resp.Status,
		}
	}
	u, _ := url.Parse(link)

	if isHTMLPage(resp) && u.Host == base.Host {
		c.PagesQueue = append(c.PagesQueue, link)
	}
	return LinkResult{}
}

func isValidWebURLStr(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return isValidWebURL(u)
}

func isValidWebURL(u *url.URL) bool {
	// TODO: security checks
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}

func isHTMLPage(res *http.Response) bool {
	contentType := res.Header.Get("Content-Type")
	return strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml")
}

func toFormattedJSON(v Report) ([]byte, error) {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return payload, nil
}
