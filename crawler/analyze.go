package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Options struct {
	URL         string
	Depth       int
	Retries     int
	Delay       string
	Timeout     string
	RPS         int
	Concurrency int
	IndentJSON  string
	HTTPClient  *http.Client
}

var (
	ErrorInvalidURL          = errors.New("invalid url")
	ErrorInvalidDepth        = errors.New("invalid depth value")
	ErrorInvalidDelay        = errors.New("invalid delay value")
	ErrorInvalidRPS          = errors.New("invalid rps value")
	ErrorHTTPClientRequired  = errors.New("http client is required")
	ErrorNotHTML             = errors.New("not an HTML page")
	ErrorRateLimiterRequired = errors.New("http rate limiter is required")
)

func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	httpRateLimiter, err := newHTTPRateLimiter(opts.HTTPClient, opts.RPS, opts.Delay)
	if err != nil {
		return nil, err
	}
	crawler, err := newCrawler(opts.URL, opts.Depth, httpRateLimiter)
	if err != nil {
		return nil, err
	}
	report := crawler.getReport(ctx)
	return toFormattedJSON(report)
}

type PageData struct {
	URL   string
	Depth int
}

type ReqParams struct {
	url    string
	method string
}

type Crawler struct {
	rootURL               string
	parsedRootURL         *url.URL
	pagesQueue            []PageData
	uniqueActivePageLinks map[string]struct{}
	maxDepth              int
	rateLimiter           *HTTPRateLimiter
}

type HTTPRateLimiter struct {
	client      *http.Client
	mu          sync.Mutex
	reqInterval time.Duration
	nextReqTime time.Time
}

func newCrawler(rootURL string, depth int, rl *HTTPRateLimiter) (*Crawler, error) {
	if rl == nil {
		return nil, ErrorRateLimiterRequired
	}
	u, err := url.Parse(rootURL)
	if err != nil || !isValidWebURL(u) {
		return nil, ErrorInvalidURL
	}
	if depth < 0 {
		return nil, ErrorInvalidDepth
	}

	return &Crawler{
		rootURL:       rootURL,
		parsedRootURL: u,
		maxDepth:      depth,
		pagesQueue:    []PageData{{URL: rootURL, Depth: 0}},
		uniqueActivePageLinks: map[string]struct{}{
			normalizeAbsURL(u).String(): {},
		},
		rateLimiter: rl,
	}, nil
}

func newHTTPRateLimiter(client *http.Client, rps int, delay string) (*HTTPRateLimiter, error) {
	if client == nil {
		return nil, ErrorHTTPClientRequired
	}
	if rps < 0 {
		return nil, ErrorInvalidRPS
	}
	if delay == "" {
		delay = "0s"
	}
	d, err := time.ParseDuration(delay)
	if err != nil || d < 0 {
		return nil, ErrorInvalidDelay
	}
	var reqInterval time.Duration
	if rps != 0 {
		reqInterval = time.Second / time.Duration(rps)
	} else {
		reqInterval = d
	}
	return &HTTPRateLimiter{
		client:      client,
		mu:          sync.Mutex{},
		reqInterval: reqInterval,
		nextReqTime: time.Now(),
	}, nil
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

func (c *Crawler) getReport(ctx context.Context) Report {
	pageReports := c.runPageReportsQueue(ctx)

	report := Report{
		RootURL:     c.rootURL,
		Depth:       c.maxDepth,
		GeneratedAt: time.Now().Truncate(time.Second),
		Pages:       pageReports,
	}
	return report
}

func (c *Crawler) getPageReport(ctx context.Context, link string, depth int) PageReport {
	report := PageReport{
		URL:         link,
		Depth:       depth,
		BrokenLinks: make([]BrokenLinkReport, 0),
	}
	resp, err := c.rateLimiter.runThrottledRequest(ctx, ReqParams{
		url:    link,
		method: http.MethodGet,
	})
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
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

		linkRes := c.checkExtractedLink(ctx, link, depth)

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

func (c *Crawler) runPageReportsQueue(ctx context.Context) []PageReport {
	reports := make([]PageReport, 0)
	for len(c.pagesQueue) > 0 {
		if err := ctx.Err(); err != nil {
			break
		}
		current := c.pagesQueue[0]
		c.pagesQueue = c.pagesQueue[1:]
		reports = append(reports, c.getPageReport(ctx, current.URL, current.Depth))
	}
	return reports
}

func (c *Crawler) checkExtractedLink(ctx context.Context, link *url.URL, depth int) LinkResult {
	if ctx.Err() != nil {
		return LinkResult{}
	}
	if !isValidWebURL(link) {
		return LinkResult{}
	}
	linkStr := link.String()
	// TODO: use goroutines
	resp, err := c.rateLimiter.runThrottledRequest(ctx, ReqParams{
		url:    linkStr,
		method: http.MethodHead,
	})
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

	if isHTMLPage(resp) && link.Hostname() == c.parsedRootURL.Hostname() && depth < c.maxDepth && c.uniqueActivePageLinks != nil {
		dedupLink := normalizeAbsURL(link).String()
		if _, ok := c.uniqueActivePageLinks[dedupLink]; ok {
			return LinkResult{}
		}
		c.pagesQueue = append(
			c.pagesQueue,
			PageData{URL: linkStr, Depth: depth + 1},
		)
		c.uniqueActivePageLinks[dedupLink] = struct{}{}
	}
	return LinkResult{}
}

func (r *HTTPRateLimiter) runThrottledRequest(ctx context.Context, params ReqParams) (*http.Response, error) {
	if r.reqInterval == 0 {
		return r.makeHTTPRequest(ctx, params)
	}
	left := r.reserveSlot()
	if left <= 0 {
		return r.makeHTTPRequest(ctx, params)
	}
	timer := time.NewTimer(left)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return r.makeHTTPRequest(ctx, params)
	}
}

func (r *HTTPRateLimiter) reserveSlot() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	left := time.Until(r.nextReqTime)
	if left <= 0 {
		r.nextReqTime = time.Now().Add(r.reqInterval)
	} else {
		r.nextReqTime = r.nextReqTime.Add(r.reqInterval)
	}
	return left
}

func (r *HTTPRateLimiter) makeHTTPRequest(ctx context.Context, params ReqParams) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, params.method, params.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
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
