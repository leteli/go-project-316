package crawler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	OKPath       = "/ok"
	NotFoundPath = "/404"
	InvalidURL   = "invalidurl"
)

func setupTestServer(t *testing.T) *httptest.Server {
	// Start a local HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case OKPath:
			rw.WriteHeader(http.StatusOK)
		case NotFoundPath:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAnalyze(t *testing.T) {
	t.Run("success - can get from url", func(t *testing.T) {
		server := setupTestServer(t)
		ctx := context.TODO()
		rootURL := server.URL + OKPath
		res, err := Analyze(ctx, Options{
			URL:        rootURL,
			HTTPClient: server.Client(),
			Depth:      1,
			IndentJSON: " ",
		})
		require.NoError(t, err)
		var report Report
		require.NoError(t, json.Unmarshal(res, &report))
		require.Equal(t, rootURL, report.RootURL)
		require.Len(t, report.Pages, 1)
		require.Equal(t, 1, report.Depth)
		pageReport := report.Pages[0]
		require.Equal(t, http.StatusOK, pageReport.HTTPStatus)
		require.Equal(t, "ok", pageReport.Status)
		require.Empty(t, pageReport.Error)
		require.Equal(t, 0, pageReport.Depth)
	})

	t.Run("page not found", func(t *testing.T) {
		server := setupTestServer(t)
		ctx := context.TODO()
		rootURL := server.URL + NotFoundPath
		res, err := Analyze(ctx, Options{
			URL:        rootURL,
			HTTPClient: server.Client(),
			Depth:      1,
			IndentJSON: " ",
		})
		require.NoError(t, err)
		var report Report
		require.NoError(t, json.Unmarshal(res, &report))
		require.Equal(t, rootURL, report.RootURL)
		require.Len(t, report.Pages, 1)
		require.Equal(t, 1, report.Depth)
		pageReport := report.Pages[0]
		require.Equal(t, http.StatusNotFound, pageReport.HTTPStatus)
		require.Equal(t, "error", pageReport.Status)
		require.Contains(t, pageReport.Error, "Not Found")
		require.Equal(t, 0, pageReport.Depth)
	})

	t.Run("network error", func(t *testing.T) {
		server := setupTestServer(t)
		ctx := context.TODO()
		server.Close()
		res, err := Analyze(ctx, Options{
			URL:        server.URL,
			HTTPClient: server.Client(),
			Depth:      1,
			IndentJSON: " ",
		})
		require.NoError(t, err)
		var report Report
		require.NoError(t, json.Unmarshal(res, &report))
		require.Equal(t, server.URL, report.RootURL)
		require.Len(t, report.Pages, 1)
		require.Equal(t, 1, report.Depth)
		pageReport := report.Pages[0]
		require.Equal(t, 0, pageReport.HTTPStatus)
		require.Equal(t, "error", pageReport.Status)
		require.Contains(t, pageReport.Error, "connection refused")
		require.Equal(t, 0, pageReport.Depth)
	})

	t.Run("invalid url", func(t *testing.T) {
		server := setupTestServer(t)
		ctx := context.TODO()

		res, err := Analyze(ctx, Options{
			URL:        InvalidURL,
			HTTPClient: server.Client(),
			Depth:      1,
			IndentJSON: " ",
		})
		require.ErrorIs(t, err, ErrorInvalidURL)
		require.Nil(t, res)
	})
}

func TestIsValidWebURL(t *testing.T) {
	assert.Equal(t, true, isValidWebURL("https://mmm.io"))
	assert.Equal(t, true, isValidWebURL("http://gOOgle.c"))
	assert.Equal(t, false, isValidWebURL("ht://mmm.io"))
	assert.Equal(t, false, isValidWebURL("https://"))
	assert.Equal(t, false, isValidWebURL(""))
}
