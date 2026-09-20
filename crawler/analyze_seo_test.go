package crawler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seoRoundTripFunc func(*http.Request) (*http.Response, error)

func (f seoRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func analyzeSEOHTML(t *testing.T, markup string) SEO {
	t.Helper()

	const rootURL = "https://example.test/"

	client := &http.Client{
		Transport: seoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			assert.Equal(t, rootURL, req.URL.String())

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/html; charset=utf-8"},
				},
				Body:    io.NopCloser(strings.NewReader(markup)),
				Request: req,
			}, nil
		}),
	}

	payload, err := Analyze(context.Background(), Options{
		URL:        rootURL,
		HTTPClient: client,
	})
	require.NoError(t, err)

	var report struct {
		Pages []struct {
			URL        string          `json:"url"`
			HTTPStatus int             `json:"http_status"`
			Status     string          `json:"status"`
			SEO        json.RawMessage `json:"seo"`
		} `json:"pages"`
	}
	require.NoError(t, json.Unmarshal(payload, &report))
	require.Len(t, report.Pages, 1)

	page := report.Pages[0]
	assert.Equal(t, rootURL, page.URL)
	assert.Equal(t, http.StatusOK, page.HTTPStatus)
	assert.Equal(t, "ok", page.Status)

	require.NotEmpty(t, page.SEO, "seo field is required")

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(page.SEO, &fields))
	require.NotNil(t, fields, "seo value must be an object")

	for _, key := range []string{
		"has_title",
		"title",
		"has_description",
		"description",
		"has_h1",
	} {
		require.Contains(t, fields, key, "missing required field %s", key)
		require.NotEqual(t, "null", strings.TrimSpace(string(fields[key])),
			"%s cannot be null", key)
	}

	var seo SEO
	require.NoError(t, json.Unmarshal(page.SEO, &seo))
	return seo
}

func TestAnalyzeSEO(t *testing.T) {
	tests := []struct {
		name string
		html string
		want SEO
	}{
		{
			name: "all elements present",
			html: `
				<!doctype html>
				<html>
					<head>
						<title>Example Test</title>
						<meta name="description" content="A useful description">
					</head>
					<body>
						<h1>Main heading</h1>
					</body>
				</html>
			`,
			want: SEO{
				HasTitle:       true,
				Title:          "Example Test",
				HasDescription: true,
				Description:    "A useful description",
				HasH1:          true,
			},
		},
		{
			name: "all elements absent",
			html: `
				<html>
					<head></head>
					<body><p>Just a page</p></body>
				</html>
			`,
			want: SEO{},
		},
		{
			name: "only title",
			html: `<html><head><title>Only title</title></head></html>`,
			want: SEO{
				HasTitle: true,
				Title:    "Only title",
			},
		},
		{
			name: "only description",
			html: `
				<html>
					<head>
						<meta name="description" content="Only description">
					</head>
				</html>
			`,
			want: SEO{
				HasDescription: true,
				Description:    "Only description",
			},
		},
		{
			name: "only h1",
			html: `<html><body><h1>Only heading</h1></body></html>`,
			want: SEO{
				HasH1: true,
			},
		},
		{
			name: "empty elements still count as present",
			html: `
				<html>
					<head>
						<title></title>
						<meta name="description" content="">
					</head>
					<body><h1></h1></body>
				</html>
			`,
			want: SEO{
				HasTitle:       true,
				HasDescription: true,
				HasH1:          true,
			},
		},
		{
			name: "description without content attribute",
			html: `
				<html>
					<head><meta name="description"></head>
					<body></body>
				</html>
			`,
			want: SEO{
				HasDescription: true,
			},
		},
		{
			name: "HTML entities are decoded",
			html: `
				<html>
					<head>
						<title>Tom &amp; Jerry &#8212; &#x41;</title>
						<meta name="description"
							content="&quot;Cats&quot; &amp; dogs &#169;">
					</head>
					<body><h1>Tom &amp; Jerry</h1></body>
				</html>
			`,
			want: SEO{
				HasTitle:       true,
				Title:          "Tom & Jerry — A",
				HasDescription: true,
				Description:    `"Cats" & dogs ©`,
				HasH1:          true,
			},
		},
		{
			name: "entities are not decoded twice",
			html: `
				<html>
					<head>
						<title>Literal &amp;amp;</title>
						<meta name="description" content="Literal &amp;lt;">
					</head>
				</html>
			`,
			want: SEO{
				HasTitle:       true,
				Title:          "Literal &amp;",
				HasDescription: true,
				Description:    "Literal &lt;",
			},
		},
		{
			name: "unrelated elements do not count",
			html: `
				<html>
					<head>
						<meta name="keywords" content="go, crawler">
						<meta property="og:description" content="Social description">
					</head>
					<body>
						<h2>Not an h1</h2>
						<p title="Not a title element">Text</p>
						<!--
							<title>Commented title</title>
							<meta name="description" content="Commented description">
							<h1>Commented heading</h1>
						-->
					</body>
				</html>
			`,
			want: SEO{},
		},
		{
			name: "HTML tag and attribute names are case insensitive",
			html: `
				<HTML>
					<HEAD>
						<TITLE>Uppercase tags</TITLE>
						<META NAME="description" CONTENT="Description">
					</HEAD>
					<BODY><H1>Heading</H1></BODY>
				</HTML>
			`,
			want: SEO{
				HasTitle:       true,
				Title:          "Uppercase tags",
				HasDescription: true,
				Description:    "Description",
				HasH1:          true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := analyzeSEOHTML(t, tt.html)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAnalyzeSEOTextConventions(t *testing.T) {
	t.Run("trim and collapse whitespace", func(t *testing.T) {
		got := analyzeSEOHTML(t, `
			<html>
				<head>
					<title>
						Example     Test
						Page
					</title>
					<meta name="description" content="
						A     useful
						description
					">
				</head>
				<body><h1>Heading</h1></body>
			</html>
		`)

		assert.Equal(t, SEO{
			HasTitle:       true,
			Title:          "Example Test Page",
			HasDescription: true,
			Description:    "A useful description",
			HasH1:          true,
		}, got)
	})

	t.Run("whitespace-only text becomes empty", func(t *testing.T) {
		got := analyzeSEOHTML(t, `
			<html>
				<head>
					<title>
					</title>
					<meta name="description" content="    ">
				</head>
				<body><h1>   </h1></body>
			</html>
		`)

		assert.Equal(t, SEO{
			HasTitle:       true,
			HasDescription: true,
			HasH1:          true,
		}, got)
	})

	t.Run("first title and description win", func(t *testing.T) {
		got := analyzeSEOHTML(t, `
			<html>
				<head>
					<title>First title</title>
					<title>Second title</title>
					<meta name="description" content="First description">
					<meta name="description" content="Second description">
				</head>
				<body>
					<h1>First heading</h1>
					<h1>Second heading</h1>
				</body>
			</html>
		`)

		assert.Equal(t, SEO{
			HasTitle:       true,
			Title:          "First title",
			HasDescription: true,
			Description:    "First description",
			HasH1:          true,
		}, got)
	})

	// TODO: check if the requirement is valid
	t.Run("empty first elements are not replaced by later ones", func(t *testing.T) {
		got := analyzeSEOHTML(t, `
			<html>
				<head>
					<title></title>
					<title>Second title</title>
					<meta name="description" content="">
					<meta name="description" content="Second description">
				</head>
			</html>
		`)

		assert.Equal(t, SEO{
			HasTitle:       true,
			HasDescription: true,
		}, got)
	})
}
