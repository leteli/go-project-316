package crawler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Options struct {
	URL        string
	Depth      int
	Retries    int
	Delay      string
	RPS        int
	Workers    int
	IndentJSON string
	HTTPClient *http.Client
}

var (
	ErrorInvalidURL          = errors.New("invalid url")
	ErrorInvalidDepth        = errors.New("invalid depth value")
	ErrorInvalidDelay        = errors.New("invalid delay value")
	ErrorInvalidRetriesCout  = errors.New("invalid retries  count")
	ErrorInvalidRPS          = errors.New("invalid rps value")
	ErrorHTTPClientRequired  = errors.New("http client is required")
	ErrorNotHTML             = errors.New("not an HTML page")
	ErrorRateLimiterRequired = errors.New("http rate limiter is required")
	ErrorInvalidWorkersCount = errors.New("invalid number of workers")
)

func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	httpRateLimiter, err := newHTTPRateLimiter(opts.HTTPClient, opts.RPS, opts.Delay, opts.Retries)
	if err != nil {
		return nil, err
	}
	crawler, err := newCrawler(opts.URL, opts.Depth, opts.Workers, httpRateLimiter)
	if err != nil {
		return nil, err
	}
	report := crawler.buildReport(ctx)

	raw, err := toFormattedJSON(report)
	if err != nil {
		return nil, err
	}
	return raw, ctx.Err()
}

type PageData struct {
	URL   string
	Depth int
}

type Crawler struct {
	rootURL               string
	parsedRootURL         *url.URL
	uniqueActivePageLinks map[string]struct{}
	maxDepth              int
	rateLimiter           *HTTPRateLimiter
	workers               int
}

type HTTPRateLimiter struct {
	client      *http.Client
	mu          sync.Mutex
	reqInterval time.Duration
	nextReqTime time.Time
	retries     int
}

func newCrawler(rootURL string, depth, workers int, rl *HTTPRateLimiter) (*Crawler, error) {
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
	if workers < 0 {
		return nil, ErrorInvalidWorkersCount
	}
	if workers == 0 {
		workers = 1
	}

	return &Crawler{
		rootURL:       rootURL,
		parsedRootURL: u,
		maxDepth:      depth,
		uniqueActivePageLinks: map[string]struct{}{
			normalizeAbsURL(u).String(): {},
		},
		rateLimiter: rl,
		workers:     workers,
	}, nil
}

func newHTTPRateLimiter(client *http.Client, rps int, delay string, retries int) (*HTTPRateLimiter, error) {
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
	if retries < 0 {
		return nil, ErrorInvalidRetriesCout
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
		retries:     retries,
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

type ReqParams struct {
	url    string
	method string
}

type LinkParams struct {
	url               string
	dedupURL          string
	checkInternalPage bool
	isRoot            bool
	depth             int
	parentIndex       int
}

type LinkResult struct {
	URL           string
	DedupURL      string
	Kind          string
	StatusCode    int
	Status        string
	Error         string
	Depth         int
	parentIndex   int
	childrenLinks []LinkParams
	SEO           SEO
}

var (
	KindBroken       = "broken"
	KindInternalPage = "internal_page"
)

func (c *Crawler) buildReport(ctx context.Context) Report {
	report := Report{
		RootURL:     c.rootURL,
		Depth:       c.maxDepth,
		GeneratedAt: time.Now().Truncate(time.Second),
		Pages:       make([]PageReport, 0),
	}
	var currentLevel int
	tasks := []LinkParams{
		{
			url:      c.rootURL,
			dedupURL: normalizeAbsURL(c.parsedRootURL).String(), checkInternalPage: true,
			depth:  currentLevel,
			isRoot: true,
		},
	}
	for currentLevel <= c.maxDepth {
		tasks = c.levelHandler(ctx, &report, currentLevel, tasks)
		if len(tasks) == 0 {
			break
		}
		currentLevel++
	}

	if ctx.Err() != nil {
		return report
	}
	if len(tasks) == 0 {
		return report
	}
	for i := range tasks {
		tasks[i].checkInternalPage = false
	}
	c.levelHandler(ctx, &report, currentLevel, tasks)
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
	}
	return report
}

func (c *Crawler) levelHandler(ctx context.Context, report *Report, level int, tasks []LinkParams) []LinkParams {

	in := make(chan LinkParams, len(tasks))
	out := make(chan LinkResult, len(tasks))
	var wg sync.WaitGroup

	var nextLevelTasks = make([]LinkParams, 0)
	var dedupNextLevelPages = make(map[string]struct{})

	wg.Add(c.workers)
	for range c.workers {
		go func() {
			defer wg.Done()
			for lp := range in {
				result := c.analyzeLink(ctx, lp)
				select {
				case <-ctx.Done():
					return
				case out <- result:
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	for _, task := range tasks {
		in <- task
	}
	close(in)

	for res := range out {
		if ctx.Err() != nil {
			return nextLevelTasks
		}
		if res.Kind == KindBroken {
			valueExists := len(report.Pages) >= res.parentIndex+1 && report.Pages[res.parentIndex].BrokenLinks != nil
			if !valueExists {
				continue
			}
			report.Pages[res.parentIndex].BrokenLinks = append(report.Pages[res.parentIndex].BrokenLinks, BrokenLinkReport{
				URL:        res.URL,
				StatusCode: res.StatusCode,
				Error:      res.Error,
			})
			continue
		}
		if res.Kind == KindInternalPage {
			pageReport := PageReport{
				URL:         res.URL,
				Depth:       res.Depth,
				HTTPStatus:  res.StatusCode,
				Status:      res.Status,
				Error:       res.Error,
				BrokenLinks: make([]BrokenLinkReport, 0),
				SEO:         res.SEO,
			}
			report.Pages = append(report.Pages, pageReport)
			c.uniqueActivePageLinks[res.DedupURL] = struct{}{}
			chL := len(res.childrenLinks)
			if chL == 0 {
				continue
			}
			for _, ch := range res.childrenLinks {
				if _, ok := c.uniqueActivePageLinks[ch.dedupURL]; ok {
					continue
				}

				ch.parentIndex = len(report.Pages) - 1
				if ch.checkInternalPage {
					if _, ok := dedupNextLevelPages[ch.dedupURL]; ok {
						continue
					}
					dedupNextLevelPages[ch.dedupURL] = struct{}{}
					ch.depth = level + 1
				} else {
					ch.depth = level
				}
				nextLevelTasks = append(nextLevelTasks, ch)
			}
		}
	}
	return nextLevelTasks
}

func (c *Crawler) analyzeLink(ctx context.Context, params LinkParams) LinkResult {
	linkResult := LinkResult{
		URL:         params.url,
		DedupURL:    params.dedupURL,
		Depth:       params.depth,
		parentIndex: params.parentIndex,
	}
	method := http.MethodHead
	if params.checkInternalPage {
		method = http.MethodGet
	}
	resp, err := c.rateLimiter.runThrottledRequestWithRetries(ctx, ReqParams{
		url:    params.url,
		method: method,
	})
	if err != nil {
		linkResult.Status = "error"
		linkResult.Error = err.Error()
		switch {
		case ctx.Err() != nil:
		case params.isRoot:
			linkResult.Kind = KindInternalPage
		default:
			linkResult.Kind = KindBroken
		}
		return linkResult
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if params.isRoot || (params.checkInternalPage && isHTMLPage(resp)) {
		linkResult.Kind = KindInternalPage
	}
	linkResult.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !params.isRoot {
			linkResult.Kind = KindBroken
		}
		linkResult.Status = "error"
		linkResult.Error = resp.Status
		return linkResult
	}
	if !params.checkInternalPage {
		return linkResult
	}
	if !isHTMLPage(resp) {
		if params.isRoot {
			linkResult.Status = "error"
			linkResult.Error = ErrorNotHTML.Error()
			return linkResult
		}
		linkResult.Kind = ""
		return linkResult
	}
	doc, err := html.Parse(resp.Body)
	if err != nil {
		linkResult.Status = "error"
		linkResult.Error = fmt.Sprintf("parse error: %v", err)
		return linkResult
	}
	linkResult.SEO = AnalyzeSEO(doc)

	resolvedURL := resp.Request.URL
	links, err := ExtractHTTPLinksFromHTML(
		doc,
		resolvedURL.String(),
	)
	if err != nil {
		linkResult.Status = "error"
		linkResult.Error = fmt.Sprintf("parse error: %v", err)
		return linkResult
	}
	linkResult.Status = "ok"
	linkResult.childrenLinks = make([]LinkParams, len(links))
	for i, l := range links {
		linkResult.childrenLinks[i] = LinkParams{
			url:               l.String(),
			dedupURL:          normalizeAbsURL(l).String(),
			checkInternalPage: l.Hostname() == c.parsedRootURL.Hostname(),
		}
	}
	return linkResult
}

func (r *HTTPRateLimiter) runThrottledRequestWithRetries(ctx context.Context, params ReqParams) (*http.Response, error) {
	var attempt int
	for attempt < r.retries {
		res, err := r.runThrottledRequest(ctx, params)
		attempt++
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			var netErr net.Error
			timeoutErr := errors.As(err, &netErr) && netErr.Timeout()
			if !timeoutErr {
				return res, err
			}
		}
		if res != nil {
			if !isRetryableStatus(res.StatusCode) {
				return res, err
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
			_ = res.Body.Close()
		}
		timer := time.NewTimer(jitter(backoff(attempt)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
			continue
		}
	}
	return r.runThrottledRequest(ctx, params)
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

func isRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

const BaseDelay = time.Duration(100 * time.Millisecond)
const MaxDelay = time.Duration(5 * time.Second)

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * BaseDelay
	if d <= 0 || d > MaxDelay {
		return MaxDelay
	}
	return d
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(d)))
}
