package crawler

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Options struct {
	URL         string
	Depth       int
	Retries     int
	Delay       time.Duration
	RPS         int
	Concurrency int
	Timeout     time.Duration
	IndentJSON  bool
	HTTPClient  *http.Client
}

func Analyze(ctx context.Context, opts Options) ([]byte, error) {
	httpRateLimiter, err := newHTTPRateLimiter(opts.HTTPClient, opts.RPS, opts.Delay, opts.Retries)
	if err != nil {
		return nil, err
	}
	crawler, err := newCrawler(opts.URL, opts.Depth, opts.Concurrency, httpRateLimiter)
	if err != nil {
		return nil, err
	}
	report := crawler.buildReport(ctx)
	report.GeneratedAt = time.Now().UTC().Truncate(time.Second)

	raw, err := toFormattedJSON(report, opts.IndentJSON)
	if err != nil {
		return nil, err
	}
	return raw, ctx.Err()
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
	url           string
	dedupURL      string
	kind          string
	statusCode    int
	status        string
	error         string
	depth         int
	parentIndex   int
	childrenLinks []LinkParams
	seo           SEO
	assetType     string
	size          int
}

var (
	KindBroken       = "broken"
	KindInternalPage = "internal_page"
	KindAsset        = "asset"
)

func (c *Crawler) buildReport(ctx context.Context) Report {
	report := Report{
		RootURL: c.rootURL,
		Depth:   c.maxDepth,
		Pages:   make([]PageReport, 0),
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
		switch res.kind {
		case KindBroken:
			valueExists := len(report.Pages) >= res.parentIndex+1 && report.Pages[res.parentIndex].BrokenLinks != nil
			if !valueExists {
				continue
			}
			brokenLinkReport := BrokenLinkReport{
				URL:        res.url,
				StatusCode: res.statusCode,
				Error:      res.error,
			}
			report.Pages[res.parentIndex].BrokenLinks = append(report.Pages[res.parentIndex].BrokenLinks, brokenLinkReport)
			c.uniqueBrokenLinks[res.dedupURL] = brokenLinkReport
			continue

		case KindAsset:
			valueExists := len(report.Pages) >= res.parentIndex+1 && report.Pages[res.parentIndex].Assets != nil

			if !valueExists {
				continue
			}
			assetReport := AssetsReport{
				URL:        res.url,
				StatusCode: res.statusCode,
				Type:       res.assetType,
				SizeBytes:  res.size,
				Error:      res.error,
			}
			report.Pages[res.parentIndex].Assets = append(report.Pages[res.parentIndex].Assets, assetReport)
			c.uniqueAssets[res.dedupURL] = assetReport

		case KindInternalPage:
			pageReport := PageReport{
				URL:        res.url,
				Depth:      res.depth,
				HTTPStatus: res.statusCode,
				Status:     res.status,
				Error:      res.error,
				SEO:        res.seo,
				// NB: commented to match fixtures
				// BrokenLinks:  make([]BrokenLinkReport, 0),
				// Assets:       make([]AssetsReport, 0),
				DiscoveredAt: time.Now().UTC().Truncate(time.Second),
			}
			if res.statusCode != 0 {
				pageReport.BrokenLinks = make([]BrokenLinkReport, 0)
				pageReport.Assets = make([]AssetsReport, 0)
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
			c.uniqueActivePageLinks[res.dedupURL] = struct{}{}
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
		url:         params.url,
		dedupURL:    params.dedupURL,
		depth:       params.depth,
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
		linkResult.status = "error"
		linkResult.error = err.Error()
		switch {
		case ctx.Err() != nil:
		case params.isRoot:
			linkResult.kind = KindInternalPage
		case params.assetType != "":
			linkResult.kind = KindAsset
			linkResult.assetType = params.assetType
		default:
			linkResult.kind = KindBroken
		}
		return linkResult
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if params.isRoot || (params.isInternal && isHTMLPage(resp)) {
		linkResult.kind = KindInternalPage
	}

	linkResult.statusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !params.isRoot {
			linkResult.kind = KindBroken
		}
		if params.assetType != "" {
			linkResult.kind = KindAsset
			linkResult.assetType = params.assetType
		}
		linkResult.status = "error"
		linkResult.error = http.StatusText(resp.StatusCode)
		return linkResult
	}

	if params.assetType != "" {
		linkResult.kind = KindAsset
		linkResult.assetType = params.assetType
		size, err := getAssetSize(resp)
		if err != nil {
			linkResult.error = err.Error()
		}
		linkResult.size = size
		return linkResult
	}
	if !params.isInternal {
		return linkResult
	}
	if !isHTMLPage(resp) {
		if params.isRoot {
			linkResult.status = "error"
			linkResult.error = ErrorNotHTML.Error()
			return linkResult
		}
		linkResult.kind = ""
		return linkResult
	}
	doc, err := html.Parse(resp.Body)
	if err != nil {
		linkResult.status = "error"
		linkResult.error = fmt.Sprintf("parse error: %v", err)
		return linkResult
	}
	linkResult.seo = AnalyzeSEO(doc)

	resolvedURL := resp.Request.URL
	linksData, err := ExtractHTTPLinksFromHTML(
		doc,
		resolvedURL.String(),
	)
	if err != nil {
		linkResult.status = "error"
		linkResult.error = fmt.Sprintf("parse error: %v", err)
		return linkResult
	}
	linkResult.status = "ok"
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
	switch mediaType {
	case "text/html", "application/xhtml+xml", "application/xml", "text/xml", "application/rss+xml", "application/atom+xml":
		return true
	}
	return false
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
