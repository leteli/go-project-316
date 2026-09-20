package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const depthRootURL = "https://crawl.test/"

type depthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f depthRoundTripFunc) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	return f(req)
}

type depthFixturePage struct {
	html string
	seo  SEO
}

func depthPage(title string, links ...string) depthFixturePage {
	var head, body strings.Builder
	var seo SEO

	if title != "" {
		fmt.Fprintf(&head,
			`<title>%s</title><meta name="description" content="%s description">`,
			title, title,
		)
		body.WriteString("<h1>Heading</h1>")

		seo = SEO{
			HasTitle:       true,
			Title:          title,
			HasDescription: true,
			Description:    title + " description",
			HasH1:          true,
		}
	}

	for _, link := range links {
		fmt.Fprintf(&body, `<a href="%s">Link</a>`, link)
	}

	return depthFixturePage{
		html: "<!doctype html><html><head>" + head.String() +
			"</head><body>" + body.String() + "</body></html>",
		seo: seo,
	}
}

func depthClient(
	t *testing.T,
	pages map[string]depthFixturePage,
) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: depthRoundTripFunc(
			func(req *http.Request) (*http.Response, error) {
				if err := req.Context().Err(); err != nil {
					return nil, err
				}
				u := req.URL
				if u.Path == "" {
					u.Path = "/"
				}
				page, ok := pages[req.URL.String()]
				if !ok {
					t.Errorf("unexpected URL: %s %s",
						req.Method, req.URL)
					return nil, fmt.Errorf(
						"unexpected URL: %s", req.URL,
					)
				}

				if req.Method != http.MethodGet &&
					req.Method != http.MethodHead {
					t.Errorf("unexpected method: %s", req.Method)
					return nil, fmt.Errorf(
						"unexpected method: %s", req.Method,
					)
				}

				body := page.html
				if req.Method == http.MethodHead {
					body = ""
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header: http.Header{
						"Content-Type": {"text/html; charset=utf-8"},
					},
					Body:    io.NopCloser(strings.NewReader(body)),
					Request: req,
				}, nil
			},
		),
	}
}

type depthReportPage struct {
	URL        string `json:"url"`
	Depth      *int   `json:"depth"`
	HTTPStatus int    `json:"http_status"`
	Status     string `json:"status"`
	SEO        *SEO   `json:"seo"`
}

type depthJSONReport struct {
	Pages []depthReportPage `json:"pages"`
}

func decodeDepthReport(t *testing.T, payload []byte) depthJSONReport {
	t.Helper()

	require.True(t, json.Valid(payload), "report must be valid JSON")

	var report depthJSONReport
	require.NoError(t, json.Unmarshal(payload, &report))
	return report
}

func assertDepthPages(
	t *testing.T,
	report depthJSONReport,
	fixtures map[string]depthFixturePage,
	want map[string]int,
) {
	t.Helper()

	require.Len(t, report.Pages, len(want))

	seen := make(map[string]bool)
	for _, page := range report.Pages {
		require.False(t, seen[page.URL],
			"duplicate page: %s", page.URL)
		seen[page.URL] = true

		wantDepth, ok := want[page.URL]
		require.True(t, ok, "unexpected page: %s", page.URL)

		require.NotNil(t, page.Depth,
			"missing depth: %s", page.URL)
		assert.Equal(t, wantDepth, *page.Depth, page.URL)

		assert.Equal(t, http.StatusOK, page.HTTPStatus, page.URL)
		assert.Equal(t, "ok", page.Status, page.URL)

		require.NotNil(t, page.SEO,
			"missing seo: %s", page.URL)
		assert.Equal(t, fixtures[page.URL].seo, *page.SEO, page.URL)
	}

	for url := range want {
		assert.True(t, seen[url], "missing page: %s", url)
	}
}

func TestAnalyzeDepthLimitAndSEO(t *testing.T) {
	const (
		a        = depthRootURL + "a"
		b        = depthRootURL + "b"
		c        = depthRootURL + "c"
		d        = depthRootURL + "d"
		e        = depthRootURL + "c" + "/e"
		f        = depthRootURL + "c" + "/f"
		external = "https://outside.test/page"
	)

	fixtures := map[string]depthFixturePage{
		depthRootURL: depthPage("Root", "/a", b, external),
		a:            depthPage("A", "/c"),
		b:            depthPage(""),
		c:            depthPage("C", "/d", "/c/e"),
		d:            depthPage("D"),
		e:            depthPage("E"),
		external:     depthPage("External"),
	}

	tests := []struct {
		name  string
		depth int
		want  map[string]int
	}{
		{
			name:  "zero - root only",
			depth: 0,
			want:  map[string]int{depthRootURL: 0},
		},
		{
			name:  "one - root and direct children",
			depth: 1,
			want:  map[string]int{depthRootURL: 0, a: 1, b: 1},
		},
		{
			name:  "two - includes grandchildren",
			depth: 2,
			want: map[string]int{
				depthRootURL: 0, a: 1, b: 1, c: 2,
			},
		},
		{
			name:  "three - includes last level",
			depth: 3,
			want: map[string]int{
				depthRootURL: 0, a: 1, b: 1, c: 2, d: 3, e: 3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(
				context.Background(), 3*time.Second,
			)
			defer cancel()

			payload, err := Analyze(ctx, Options{
				URL:        depthRootURL,
				Depth:      tt.depth,
				HTTPClient: depthClient(t, fixtures),
			})
			require.NoError(t, err)
			require.NoError(t, ctx.Err(), "crawl did not finish in time")

			assertDepthPages(
				t, decodeDepthReport(t, payload), fixtures, tt.want,
			)
		})
	}
}

func TestAnalyzeDepthDuplicatesAndCycles(t *testing.T) {
	const (
		a      = depthRootURL + "a"
		b      = depthRootURL + "b"
		shared = depthRootURL + "shared"
	)

	fixtures := map[string]depthFixturePage{
		depthRootURL: depthPage(
			"Root",
			"/a",
			"/a",
			a,
			"/a#section",
			"/b",
			"/",
		),
		a:      depthPage("A", "/", "/shared"),
		b:      depthPage("B", "/shared", "/a"),
		shared: depthPage("Shared", "/", "/a"),
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 3*time.Second,
	)
	defer cancel()

	payload, err := Analyze(ctx, Options{
		URL:        depthRootURL,
		Depth:      10,
		HTTPClient: depthClient(t, fixtures),
	})
	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "cycles must not keep crawl running")

	assertDepthPages(t, decodeDepthReport(t, payload), fixtures,
		map[string]int{
			depthRootURL: 0,
			a:            1,
			b:            1,
			shared:       2,
		},
	)
}

func TestAnalyzeDepthUsesShortestDistance(t *testing.T) {
	const (
		long   = depthRootURL + "long"
		middle = depthRootURL + "middle"
		target = depthRootURL + "target"
		leaf   = depthRootURL + "leaf"
	)

	fixtures := map[string]depthFixturePage{
		depthRootURL: depthPage("Root", "/long", "/target"),
		long:         depthPage("Long", "/middle"),
		middle:       depthPage("Middle", "/target"),
		target:       depthPage("Target", "/leaf"),
		leaf:         depthPage("Leaf"),
	}

	for _, limit := range []int{2, 3} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(
				context.Background(), 3*time.Second,
			)
			defer cancel()

			payload, err := Analyze(ctx, Options{
				URL:        depthRootURL,
				Depth:      limit,
				HTTPClient: depthClient(t, fixtures),
			})
			require.NoError(t, err)
			require.NoError(t, ctx.Err())

			assertDepthPages(t, decodeDepthReport(t, payload), fixtures,
				map[string]int{
					depthRootURL: 0,
					long:         1,
					middle:       2,
					target:       1,
					leaf:         2,
				},
			)
		})
	}
}

func TestAnalyzeDepthCancellationKeepsCollectedPages(t *testing.T) {
	const child1 = depthRootURL + "child1"
	const child2 = depthRootURL + "child2"

	fixtures := map[string]depthFixturePage{
		depthRootURL: depthPage("Root", "/child1", "/child2"),
		child1:       depthPage("Child1"),
		child2:       depthPage("Child2"),
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 3*time.Second,
	)
	defer cancel()

	client := depthClient(t, fixtures)
	originalTransport := client.Transport

	childrenCalls := 0
	client.Transport = depthRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			u := req.URL.String()
			if req.Method == http.MethodGet &&
				(u == child1 || u == child2) {
				childrenCalls++
				cancel()
				return nil, context.Canceled
			}
			return originalTransport.RoundTrip(req)
		},
	)

	payload, err := Analyze(ctx, Options{
		URL:        depthRootURL,
		Depth:      2,
		HTTPClient: client,
	})

	if childrenCalls == 0 {
		t.Fatal("crawler did not start downloading the child page")
	}

	if err != nil {
		require.True(t, errors.Is(err, context.Canceled),
			"unexpected error: %v", err)
	}

	report := decodeDepthReport(t, payload)
	require.NotEmpty(t, report.Pages)
	require.Equal(t, 1, childrenCalls, "http calls after canceled context")

	seen := make(map[string]bool)
	var root *depthReportPage

	for i := range report.Pages {
		page := &report.Pages[i]

		require.False(t, seen[page.URL],
			"duplicate page: %s", page.URL)
		seen[page.URL] = true

		require.Contains(t, fixtures, page.URL)
		if page.URL == depthRootURL {
			root = page
		}
	}

	require.NotNil(t, root)
	require.NotNil(t, root.Depth)
	assert.Equal(t, 0, *root.Depth)
	assert.Equal(t, "ok", root.Status)
	assert.Equal(t, http.StatusOK, root.HTTPStatus)
	require.NotNil(t, root.SEO)
	assert.Equal(t, fixtures[depthRootURL].seo, *root.SEO)
}
