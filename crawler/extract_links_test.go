package crawler

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func extractedURLs(t *testing.T, markup, base string) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	require.NoError(t, err)
	links, err := ExtractHTTPLinksFromHTML(doc, base)
	require.NoError(t, err)

	urls := slices.Clone(links)
	return urls
}

func TestExtractHTTPLinksFromHTML(t *testing.T) {
	const documentURL = "https://site.test/blog/index.html"

	t.Run("relative root-relative and scheme-relative URLs", func(t *testing.T) {
		urls := extractedURLs(t, `
			<a href="child">Child</a>
			<a href="../up">Up</a>
			<a href="/root">Root</a>
			<a href="//cdn.test/file">CDN</a>
		`, documentURL)

		assert.Equal(t, []string{
			"https://site.test/blog/child",
			"https://site.test/up",
			"https://site.test/root",
			"https://cdn.test/file",
		}, urls)
	})

	t.Run("first base applies to all links", func(t *testing.T) {
		urls := extractedURLs(t, `
			<head>
				<link rel="stylesheet" href="early.css">
				<base href="/assets/">
				<base href="https://ignored.test/">
			</head>
			<body>
				<a href="child">Child</a>
				<a href="/root">Root</a>
			</body>
		`, documentURL)

		assert.Equal(t, []string{
			"https://site.test/assets/early.css",
			"https://site.test/assets/child",
			"https://site.test/root",
		}, urls)
	})

	t.Run("external base", func(t *testing.T) {
		urls := extractedURLs(t, `
			<base href="https://cdn.test/assets/">
			<img src="image.png">
			<script src="/app.js"></script>
		`, documentURL)

		assert.Equal(t, []string{
			"https://cdn.test/assets/image.png",
			"https://cdn.test/app.js",
		}, urls)
	})

	t.Run("query and trailing slash are preserved", func(t *testing.T) {
		urls := extractedURLs(t, `
			<a href="/page">One</a>
			<a href="/page/">Two</a>
			<a href="/page?id=1">Three</a>
			<a href="/page?id=2">Four</a>
			<a href="/page?id=1#part">Duplicate of three</a>
		`, documentURL)

		assert.Equal(t, []string{
			"https://site.test/page",
			"https://site.test/page/",
			"https://site.test/page?id=1",
			"https://site.test/page?id=2",
		}, urls)
	})

	t.Run("HTML entities and attribute case", func(t *testing.T) {
		urls := extractedURLs(t, `
			<A HREF=" /search?q=go&amp;page=2 ">Search</A>
			<!-- <a href="/ignored">Comment</a> -->
			<script>const text = '<a href="/also-ignored">';</script>
		`, documentURL)

		assert.Equal(t, []string{
			"https://site.test/search?q=go&page=2",
		}, urls)
	})

	t.Run("all currently supported resource attributes", func(t *testing.T) {
		urls := extractedURLs(t, `
			<a href="/page">Page</a>
			<link href="/style.css">
			<script src="/app.js"></script>
			<img src="/image.png">
			<video src="/video.mp4"></video>
			<audio src="/audio.mp3"></audio>
			<source src="/source.webm">
			<iframe src="/frame"></iframe>
		`, documentURL)

		assert.Equal(t, []string{
			"https://site.test/page",
			"https://site.test/style.css",
			"https://site.test/app.js",
			"https://site.test/image.png",
			"https://site.test/video.mp4",
			"https://site.test/audio.mp3",
			"https://site.test/source.webm",
			"https://site.test/frame",
		}, urls)
	})

	t.Run("unclosed tags are tolerated", func(t *testing.T) {
		urls := extractedURLs(t, `
			<div><a href="/one">One</a>
			<p><a href="/two">Two
		`, documentURL)

		assert.Equal(t, []string{
			"https://site.test/one",
			"https://site.test/two",
		}, urls)
	})

	t.Run("malformed base URL is rejected", func(t *testing.T) {
		_, err := ExtractHTTPLinksFromHTML(
			&html.Node{Type: html.DocumentNode},
			"http://[",
		)

		require.Error(t, err)
	})
}
