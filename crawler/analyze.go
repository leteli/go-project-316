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

	"golang.org/x/net/html"
)

type Options struct {
	URL         string
	Depth       int
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
	u, err := url.Parse(opts.URL)
	if err != nil || !isValidWebURL(u) {
		return nil, ErrorInvalidURL
	}
	crawler := NewCrawler(opts, u)
	report := crawler.GetReport(ctx)
	return toFormattedJSON(report)
}

type PageData struct {
	URL   string
	Depth int
}

type Crawler struct {
	client                *http.Client
	URL                   string
	ParsedRootURL         *url.URL
	PagesQueue            []PageData
	uniqueActivePageLinks map[string]struct{}
	MaxDepth              int
}

func NewCrawler(opts Options, root *url.URL) *Crawler {
	return &Crawler{
		client:        opts.HTTPClient,
		URL:           opts.URL,
		ParsedRootURL: root,
		MaxDepth:      opts.Depth,
		PagesQueue:    []PageData{{URL: opts.URL, Depth: 0}},
		uniqueActivePageLinks: map[string]struct{}{
			normalizeAbsURL(root).String(): {},
		},
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
	SEO         SEO                `json:"seo"`
}

type BrokenLinkReport struct {
	URL        string `json:"url"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error"`
}

type LinkResult struct {
	URL        string
	Kind       string
	StatusCode int
	Error      string
}

func (c *Crawler) GetReport(ctx context.Context) Report {
	pageReports := c.RunPageReportsQueue(ctx)

	report := Report{
		RootURL:     c.URL,
		Depth:       c.MaxDepth,
		GeneratedAt: time.Now().Truncate(time.Second),
		Pages:       pageReports,
	}
	return report
}

func (c *Crawler) GetPageReport(ctx context.Context, link string, depth int) PageReport {
	report := PageReport{
		URL:         link,
		Depth:       depth,
		BrokenLinks: make([]BrokenLinkReport, 0),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
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
	doc, err := html.Parse(resp.Body)
	if err != nil {
		report.Status = "error"
		report.Error = fmt.Sprintf("parse error: %v", err)
		return report
	}
	report.SEO = AnalyzeSEO(doc)

	resolvedURL := resp.Request.URL
	links, err := ExtractHTTPLinksFromHTML(
		doc,
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

		linkRes := c.CheckExtractedLink(ctx, link, depth)

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

func (c *Crawler) RunPageReportsQueue(ctx context.Context) []PageReport {
	reports := make([]PageReport, 0)
	for len(c.PagesQueue) > 0 {
		if err := ctx.Err(); err != nil {
			break
		}
		current := c.PagesQueue[0]
		c.PagesQueue = c.PagesQueue[1:]
		reports = append(reports, c.GetPageReport(ctx, current.URL, current.Depth))
	}
	return reports
}

func (c *Crawler) CheckExtractedLink(ctx context.Context, link *url.URL, depth int) LinkResult {
	if ctx.Err() != nil {
		return LinkResult{}
	}
	if !isValidWebURL(link) {
		return LinkResult{}
	}
	linkStr := link.String()
	// TODO: use goroutines
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, linkStr, nil)
	if err != nil {
		return LinkResult{
			URL:   linkStr,
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
			URL:   linkStr,
			Kind:  "broken",
			Error: err.Error(),
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		return LinkResult{
			URL:        linkStr,
			Kind:       "broken",
			StatusCode: resp.StatusCode,
			Error:      resp.Status,
		}
	}

	if isHTMLPage(resp) && link.Hostname() == c.ParsedRootURL.Hostname() && depth < c.MaxDepth && c.uniqueActivePageLinks != nil {
		dedupLink := normalizeAbsURL(link).String()
		if _, ok := c.uniqueActivePageLinks[dedupLink]; ok {
			return LinkResult{}
		}
		c.PagesQueue = append(
			c.PagesQueue,
			PageData{URL: linkStr, Depth: depth + 1},
		)
		c.uniqueActivePageLinks[dedupLink] = struct{}{}
	}
	return LinkResult{}
}

func isValidWebURL(u *url.URL) bool {
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
