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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type stubReply struct {
	status      int
	contentType string
	body        string
	err         error
	location    string
}

func htmlReply(body string) stubReply {
	return stubReply{
		status:      http.StatusOK,
		contentType: "text/html; charset=utf-8",
		body:        body,
	}
}

func responseFor(req *http.Request, reply stubReply) *http.Response {
	header := make(http.Header)
	if reply.contentType != "" {
		header.Set("Content-Type", reply.contentType)
	}
	if reply.location != "" {
		header.Set("Location", reply.location)
	}

	return &http.Response{
		StatusCode: reply.status,
		Status: fmt.Sprintf(
			"%d %s",
			reply.status,
			http.StatusText(reply.status),
		),
		Header:  header,
		Body:    io.NopCloser(strings.NewReader(reply.body)),
		Request: req,
	}
}

func newStubClient(
	t *testing.T,
	replies map[string]stubReply,
) (*http.Client, map[string]int) {
	t.Helper()

	calls := make(map[string]int)
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := req.Context().Err(); err != nil {
				return nil, err
			}

			requestURL := req.URL.String()
			calls[requestURL]++

			reply, ok := replies[requestURL]
			if !ok {
				t.Errorf("unexpected request: %s %s", req.Method, requestURL)
				return nil, fmt.Errorf("unexpected URL: %s", requestURL)
			}
			if reply.err != nil {
				return nil, reply.err
			}

			return responseFor(req, reply), nil
		}),
	}
	return client, calls
}

func analyzePage(
	t *testing.T,
	ctx context.Context,
	rootURL string,
	client *http.Client,
) PageReport {
	t.Helper()

	payload, err := Analyze(ctx, Options{
		URL:        rootURL,
		HTTPClient: client,
	})
	require.NoError(t, err)

	var report Report
	require.NoError(t, json.Unmarshal(payload, &report))
	require.Equal(t, rootURL, report.RootURL)
	require.Len(t, report.Pages, 1)
	require.Equal(t, rootURL, report.Pages[0].URL)

	var raw struct {
		Pages []map[string]json.RawMessage `json:"pages"`
	}
	require.NoError(t, json.Unmarshal(payload, &raw))
	require.Len(t, raw.Pages, 1)
	require.Contains(t, raw.Pages[0], "error")
	require.Contains(t, raw.Pages[0], "broken_links")
	require.NotEqual(t, "null", string(raw.Pages[0]["broken_links"]))

	var broken []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(
		raw.Pages[0]["broken_links"],
		&broken,
	))
	for _, item := range broken {
		require.Contains(t, item, "url")
		require.Contains(t, item, "status_code")
		require.Contains(t, item, "error")
	}

	return report.Pages[0]
}

func TestAnalyze(t *testing.T) {
	const root = "http://simple.test/"

	t.Run("success", func(t *testing.T) {
		client, calls := newStubClient(t, map[string]stubReply{
			root: htmlReply("<html><body>Hello</body></html>"),
		})

		page := analyzePage(t, context.Background(), root, client)

		assert.Equal(t, http.StatusOK, page.HTTPStatus)
		assert.Equal(t, "ok", page.Status)
		assert.Empty(t, page.Error)
		assert.Empty(t, page.BrokenLinks)
		assert.Equal(t, 1, calls[root])
	})

	t.Run("page not found", func(t *testing.T) {
		client, _ := newStubClient(t, map[string]stubReply{
			root: {status: http.StatusNotFound},
		})

		page := analyzePage(t, context.Background(), root, client)

		assert.Equal(t, http.StatusNotFound, page.HTTPStatus)
		assert.Equal(t, "error", page.Status)
		assert.Contains(t, page.Error, "404 Not Found")
		assert.Empty(t, page.BrokenLinks)
	})

	t.Run("root network error", func(t *testing.T) {
		client, _ := newStubClient(t, map[string]stubReply{
			root: {err: errors.New("test network failure")},
		})

		page := analyzePage(t, context.Background(), root, client)

		assert.Zero(t, page.HTTPStatus)
		assert.Equal(t, "error", page.Status)
		assert.Contains(t, page.Error, "test network failure")
		assert.Empty(t, page.BrokenLinks)
	})

	t.Run("not HTML", func(t *testing.T) {
		client, _ := newStubClient(t, map[string]stubReply{
			root: {
				status:      http.StatusOK,
				contentType: "application/json",
				body:        `{}`,
			},
		})

		page := analyzePage(t, context.Background(), root, client)

		assert.Equal(t, "error", page.Status)
		assert.Equal(t, ErrorNotHTML.Error(), page.Error)
		assert.Empty(t, page.BrokenLinks)
	})

	t.Run("invalid URL", func(t *testing.T) {
		client, calls := newStubClient(t, nil)

		payload, err := Analyze(context.Background(), Options{
			URL:        "invalidurl",
			HTTPClient: client,
		})

		require.ErrorIs(t, err, ErrorInvalidURL)
		assert.Nil(t, payload)
		assert.Empty(t, calls)
	})

	t.Run("nil client", func(t *testing.T) {
		payload, err := Analyze(context.Background(), Options{URL: root})

		require.ErrorIs(t, err, ErrorHTTPClientRequired)
		assert.Nil(t, payload)
	})

	t.Run("one working one broken link", func(t *testing.T) {
		client, calls := newStubClient(t, map[string]stubReply{
			root: htmlReply(`
				<a href="/ok">OK</a>
				<a href="/missing">Missing</a>
			`),
			root + "ok":      {status: http.StatusOK},
			root + "missing": {status: http.StatusNotFound},
		})

		page := analyzePage(t, context.Background(), root, client)

		assert.Equal(t, "ok", page.Status)
		require.Len(t, page.BrokenLinks, 1)
		assert.Equal(t, BrokenLinkReport{
			URL:        root + "missing",
			StatusCode: http.StatusNotFound,
			Error:      "404 Not Found",
		}, page.BrokenLinks[0])
		assert.Equal(t, 1, calls[root+"ok"])
		assert.Equal(t, 1, calls[root+"missing"])
	})

	t.Run("HTTP and network errors including resources", func(t *testing.T) {
		const offline = "https://network.test/offline"
		client, _ := newStubClient(t, map[string]stubReply{
			root: htmlReply(`
				<a href="/ok">OK</a>
				<a href="/missing">Missing</a>
				<a href="/failure">Failure</a>
				<script src="https://network.test/offline"></script>
				<link rel="stylesheet" href="/missing.css">
				<img src="/image.png">
			`),
			root + "ok":          {status: http.StatusOK},
			root + "missing":     {status: http.StatusNotFound},
			root + "failure":     {status: http.StatusServiceUnavailable},
			root + "missing.css": {status: http.StatusNotFound},
			root + "image.png":   {status: http.StatusOK},
			offline:              {err: errors.New("test DNS failure")},
		})

		page := analyzePage(t, context.Background(), root, client)

		require.Len(t, page.BrokenLinks, 4)
		byURL := make(map[string]BrokenLinkReport)
		for _, link := range page.BrokenLinks {
			byURL[link.URL] = link
		}
		require.Len(t, byURL, 4)

		for target, code := range map[string]int{
			root + "missing":     http.StatusNotFound,
			root + "failure":     http.StatusServiceUnavailable,
			root + "missing.css": http.StatusNotFound,
		} {
			require.Contains(t, byURL, target)
			assert.Equal(t, code, byURL[target].StatusCode)
			assert.Equal(t,
				fmt.Sprintf("%d %s", code, http.StatusText(code)),
				byURL[target].Error,
			)
		}

		require.Contains(t, byURL, offline)
		assert.Zero(t, byURL[offline].StatusCode)
		assert.Contains(t, byURL[offline].Error, "test DNS failure")
	})

	t.Run("ignore unsupported empty and malformed links", func(t *testing.T) {
		client, calls := newStubClient(t, map[string]stubReply{
			root: htmlReply(`
				<a>No href</a>
				<a href="">Empty</a>
				<a href="   ">Whitespace</a>
				<a href="mailto:test@example.com">Mail</a>
				<a href="tel:123">Phone</a>
				<a href="javascript:void(0)">JS</a>
				<a href="data:text/plain,hello">Data</a>
				<a href="ftp://files.test/file">FTP</a>
				<a href="/bad%ZZ">Malformed URL</a>
				<a href="#section">Same document</a>
			`),
		})

		page := analyzePage(t, context.Background(), root, client)

		assert.Equal(t, "ok", page.Status)
		assert.Empty(t, page.BrokenLinks)
		assert.Equal(t, map[string]int{root: 1}, calls)
	})

	t.Run("deduplicate links and fragments", func(t *testing.T) {
		client, calls := newStubClient(t, map[string]stubReply{
			root: htmlReply(`
				<a href="/missing">First</a>
				<a href="/missing">Duplicate</a>
				<a href="http://simple.test/missing#one">Fragment</a>
				<a href="/missing#two">Another fragment</a>
			`),
			root + "missing": {status: http.StatusNotFound},
		})

		page := analyzePage(t, context.Background(), root, client)

		require.Len(t, page.BrokenLinks, 1)
		assert.Equal(t, root+"missing", page.BrokenLinks[0].URL)
		assert.Equal(t, 1, calls[root+"missing"])
	})

	t.Run("redirect determines document base URL", func(t *testing.T) {
		const finalURL = root + "blog/index.html"
		client, calls := newStubClient(t, map[string]stubReply{
			root: {
				status:   http.StatusFound,
				location: finalURL,
			},
			finalURL:              htmlReply(`<a href="missing">Missing</a>`),
			root + "blog/missing": {status: http.StatusNotFound},
		})

		page := analyzePage(t, context.Background(), root, client)

		require.Len(t, page.BrokenLinks, 1)
		assert.Equal(t, root+"blog/missing", page.BrokenLinks[0].URL)
		assert.Equal(t, 1, calls[finalURL])
	})

	t.Run("cancellation does not mark links broken", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var requested []string
		client := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requested = append(requested, req.URL.String())

				switch req.URL.String() {
				case root:
					return responseFor(req, htmlReply(`
						<a href="/first">First</a>
						<a href="/second">Second</a>
					`)), nil
				case root + "first":
					cancel()
					return nil, req.Context().Err()
				default:
					t.Errorf("request after cancellation: %s", req.URL)
					return nil, context.Canceled
				}
			}),
		}

		page := analyzePage(t, ctx, root, client)

		assert.Equal(t, "error", page.Status)
		assert.Contains(t, page.Error, context.Canceled.Error())
		assert.Empty(t, page.BrokenLinks)
		assert.Equal(t, []string{root, root + "first"}, requested)
	})
}
