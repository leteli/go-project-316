package crawler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
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
	uniqueAssets          map[string]AssetsReport
	uniqueBrokenLinks     map[string]BrokenLinkReport
	processingLinks       map[string]struct{}
	dedupLinks            map[string][]int
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
		uniqueAssets:      make(map[string]AssetsReport, 0),
		uniqueBrokenLinks: make(map[string]BrokenLinkReport, 0),
		processingLinks:   make(map[string]struct{}, 0),
		dedupLinks:        make(map[string][]int, 0),
		rateLimiter:       rl,
		workers:           workers,
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
	Assets      []AssetsReport     `json:"assets"`
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
	url         string
	dedupURL    string
	assetType   string
	isInternal  bool
	isRoot      bool
	depth       int
	parentIndex int
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
	AssetType     string
	Size          int
}

type AssetsReport struct {
	URL        string `json:"url"`
	Type       string `json:"type"`
	StatusCode int    `json:"status_code"`
	SizeBytes  int    `json:"size_bytes"`
	Error      string `json:"error"`
}

var (
	KindBroken       = "broken"
	KindInternalPage = "internal_page"
	KindAsset        = "asset"
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
			url:        c.rootURL,
			dedupURL:   normalizeAbsURL(c.parsedRootURL).String(),
			isInternal: true,
			depth:      currentLevel,
			isRoot:     true,
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
		return sortReportLinks(report)
	}
	if len(tasks) == 0 {
		return sortReportLinks(report)
	}
	for i := range tasks {
		tasks[i].isInternal = false
	}
	c.levelHandler(ctx, &report, currentLevel, tasks)
	return sortReportLinks(report)
}

func (c *Crawler) levelHandler(ctx context.Context, report *Report, level int, tasks []LinkParams) []LinkParams {

	in := make(chan LinkParams, len(tasks))
	out := make(chan LinkResult, len(tasks))
	var wg sync.WaitGroup

	var nextLevelTasks = make([]LinkParams, 0)

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
		switch res.Kind {
		case KindBroken:
			valueExists := len(report.Pages) >= res.parentIndex+1 && report.Pages[res.parentIndex].BrokenLinks != nil
			if !valueExists {
				continue
			}
			brokenLinkReport := BrokenLinkReport{
				URL:        res.URL,
				StatusCode: res.StatusCode,
				Error:      res.Error,
			}
			report.Pages[res.parentIndex].BrokenLinks = append(report.Pages[res.parentIndex].BrokenLinks, brokenLinkReport)
			c.uniqueBrokenLinks[res.DedupURL] = brokenLinkReport
			continue

		case KindAsset:
			valueExists := len(report.Pages) >= res.parentIndex+1 && report.Pages[res.parentIndex].Assets != nil

			if !valueExists {
				continue
			}
			assetReport := AssetsReport{
				URL:        res.URL,
				StatusCode: res.StatusCode,
				Type:       res.AssetType,
				SizeBytes:  res.Size,
				Error:      res.Error,
			}
			report.Pages[res.parentIndex].Assets = append(report.Pages[res.parentIndex].Assets, assetReport)
			c.uniqueAssets[res.DedupURL] = assetReport

		case KindInternalPage:
			pageReport := PageReport{
				URL:         res.URL,
				Depth:       res.Depth,
				HTTPStatus:  res.StatusCode,
				Status:      res.Status,
				Error:       res.Error,
				BrokenLinks: make([]BrokenLinkReport, 0),
				SEO:         res.SEO,
				Assets:      make([]AssetsReport, 0),
			}
			for _, ch := range res.childrenLinks {
				if _, ok := c.uniqueActivePageLinks[ch.dedupURL]; ok {
					continue
				}
				if asset, ok := c.uniqueAssets[ch.dedupURL]; ok {
					pageReport.Assets = append(pageReport.Assets, asset)
					continue
				}
				if brLink, ok := c.uniqueBrokenLinks[ch.dedupURL]; ok {
					pageReport.BrokenLinks = append(pageReport.BrokenLinks, brLink)
					continue
				}

				ch.parentIndex = len(report.Pages)
				ch.depth = level + 1

				if _, ok := c.processingLinks[ch.dedupURL]; !ok {
					nextLevelTasks = append(nextLevelTasks, ch)
					c.processingLinks[ch.dedupURL] = struct{}{}
					continue
				}

				c.dedupLinks[ch.dedupURL] = append(c.dedupLinks[ch.dedupURL], ch.parentIndex)
			}
			report.Pages = append(report.Pages, pageReport)
			c.uniqueActivePageLinks[res.DedupURL] = struct{}{}
		}
	}
	c.attachResolvedLinks(report)
	return nextLevelTasks
}

func (c *Crawler) attachResolvedLinks(report *Report) {
	for k, v := range c.dedupLinks {
		br, ok := c.uniqueBrokenLinks[k]
		if ok {
			for _, parentIdx := range v {
				report.Pages[parentIdx].BrokenLinks = append(report.Pages[parentIdx].BrokenLinks, br)
			}
			delete(c.dedupLinks, k)
			continue
		}
		asset, ok := c.uniqueAssets[k]
		if ok {
			for _, parentIdx := range v {
				report.Pages[parentIdx].Assets = append(report.Pages[parentIdx].Assets, asset)
			}
			delete(c.dedupLinks, k)
		}
	}
}

func (c *Crawler) analyzeLink(ctx context.Context, params LinkParams) LinkResult {
	linkResult := LinkResult{
		URL:         params.url,
		DedupURL:    params.dedupURL,
		Depth:       params.depth,
		parentIndex: params.parentIndex,
	}
	method := http.MethodHead
	if params.isInternal || params.assetType != "" {
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
		case params.assetType != "":
			linkResult.Kind = KindAsset
			linkResult.AssetType = params.assetType
		default:
			linkResult.Kind = KindBroken
		}
		return linkResult
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if params.isRoot || (params.isInternal && isHTMLPage(resp)) {
		linkResult.Kind = KindInternalPage
	}

	linkResult.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !params.isRoot {
			linkResult.Kind = KindBroken
		}
		if params.assetType != "" {
			linkResult.Kind = KindAsset
			linkResult.AssetType = params.assetType
		}
		linkResult.Status = "error"
		linkResult.Error = resp.Status
		return linkResult
	}

	if params.assetType != "" {
		linkResult.Kind = KindAsset
		linkResult.AssetType = params.assetType
		size, err := getAssetSize(resp)
		if err != nil {
			linkResult.Error = err.Error()
		}
		linkResult.Size = size
		return linkResult
	}
	if !params.isInternal {
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
	linksData, err := ExtractHTTPLinksFromHTML(
		doc,
		resolvedURL.String(),
	)
	if err != nil {
		linkResult.Status = "error"
		linkResult.Error = fmt.Sprintf("parse error: %v", err)
		return linkResult
	}
	linkResult.Status = "ok"
	linkResult.childrenLinks = make([]LinkParams, len(linksData))
	for i, l := range linksData {
		lp := LinkParams{
			url:        l.link.String(),
			dedupURL:   normalizeAbsURL(l.link).String(),
			isInternal: l.link.Hostname() == c.parsedRootURL.Hostname(),
			assetType:  l.assetType,
		}
		linkResult.childrenLinks[i] = lp
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
	mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
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

func getAssetSize(res *http.Response) (int, error) {
	cL := res.ContentLength
	if cL > -1 {
		return int(cL), nil
	}
	size, err := io.Copy(io.Discard, res.Body)
	if err != nil {
		return 0, err
	}
	return int(size), nil
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
