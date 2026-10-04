package crawler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goldenRoot = "http://example.test"

const goldenReport = `{
  "root_url": "http://example.test",
  "depth": 1,
  "generated_at": "<time>",
  "pages": [
    {
      "url": "http://example.test",
      "depth": 0,
      "http_status": 200,
      "status": "ok",
      "error": "",
      "seo": {
        "has_title": true,
        "title": "Example title",
        "has_description": true,
        "description": "Example description",
        "has_h1": true
      },
      "broken_links": [
        {
          "url": "http://example.test/missing",
          "status_code": 404,
          "error": "Not Found"
        }
      ],
      "assets": [
        {
          "url": "http://example.test/static/logo.png",
          "type": "image",
          "status_code": 200,
          "size_bytes": 12345,
          "error": ""
        }
      ],
      "discovered_at": "<time>"
    }
  ]
}
`

var timeFieldRe = regexp.MustCompile(`"(generated_at|discovered_at)":(\s*)"([^"]*)"`)

func normalizeTimes(t *testing.T, payload []byte) []byte {
	t.Helper()

	return timeFieldRe.ReplaceAllFunc(payload, func(m []byte) []byte {
		parts := timeFieldRe.FindSubmatch(m)
		_, err := time.Parse(time.RFC3339, string(parts[3]))
		assert.NoError(t, err, "%s is not ISO8601: %s", parts[1], parts[3])
		return []byte(`"` + string(parts[1]) + `":` + string(parts[2]) + `"<time>"`)
	})
}

func analyzeGolden(t *testing.T, indent bool) []byte {
	t.Helper()

	client, _ := newStubClient(t, map[string]stubReply{
		goldenRoot: htmlReply(`
			<html>
			<head>
				<title>Example title</title>
				<meta name="description" content="Example description">
			</head>
			<body>
				<h1>Example</h1>
				<a href="/missing">Missing</a>
				<img src="/static/logo.png">
			</body>
			</html>
		`),
		goldenRoot + "/missing": {
			status: http.StatusNotFound,
		},
		goldenRoot + "/static/logo.png": {
			status:        http.StatusOK,
			contentType:   "image/png",
			contentLength: 12345,
		},
	})

	payload, err := Analyze(context.Background(), Options{
		URL:        goldenRoot,
		Depth:      1,
		IndentJSON: indent,
		HTTPClient: client,
	})
	require.NoError(t, err)
	return payload
}

func TestReportJSON(t *testing.T) {
	t.Run("matches golden report", func(t *testing.T) {
		payload := analyzeGolden(t, true)

		assert.Equal(t, goldenReport, string(normalizeTimes(t, payload)))
	})

	t.Run("indent changes only formatting", func(t *testing.T) {
		indented := normalizeTimes(t, analyzeGolden(t, true))
		plain := normalizeTimes(t, analyzeGolden(t, false))

		require.True(t, json.Valid(indented))
		require.True(t, json.Valid(plain))
		assert.NotEqual(t, string(indented), string(plain))
		assert.NotContains(t, string(bytes.TrimSuffix(plain, []byte("\n"))), "\n")

		var compacted bytes.Buffer
		require.NoError(t, json.Compact(&compacted, indented))
		assert.Equal(t,
			string(bytes.TrimSuffix(plain, []byte("\n"))),
			compacted.String(),
		)
	})

	t.Run("output ends with a single newline", func(t *testing.T) {
		for _, indent := range []bool{false, true} {
			payload := analyzeGolden(t, indent)

			assert.True(t, bytes.HasSuffix(payload, []byte("}\n")), "indent=%v", indent)
			assert.False(t, bytes.HasSuffix(payload, []byte("\n\n")), "indent=%v", indent)
		}
	})
}
