package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runCLI(t *testing.T, args ...string) []byte {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	oldArgs, oldStdout := os.Args, os.Stdout
	os.Args = append([]string{"hexlet-go-crawler"}, args...)
	os.Stdout = w
	t.Cleanup(func() {
		os.Args, os.Stdout = oldArgs, oldStdout
	})

	runErr := run()
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, runErr)
	return out
}

func TestCLIPrintsJSONAsIs(t *testing.T) {
	const url = "http://127.0.0.1:1"

	for name, args := range map[string][]string{
		"plain":    {"--retries", "0", url},
		"indented": {"--retries", "0", "--IndentJSON", url},
	} {
		t.Run(name, func(t *testing.T) {
			out := runCLI(t, args...)

			require.True(t, bytes.HasPrefix(out, []byte("{")), "unexpected text before JSON: %q", out)
			require.True(t, bytes.HasSuffix(out, []byte("}\n")), "JSON must end with a single newline: %q", out)
			assert.False(t, bytes.HasSuffix(out, []byte("\n\n")))
			assert.True(t, json.Valid(out))

			var report struct {
				RootURL string `json:"root_url"`
				Pages   []struct {
					URL   string `json:"url"`
					Error string `json:"error"`
				} `json:"pages"`
			}
			require.NoError(t, json.Unmarshal(out, &report))
			assert.Equal(t, url, report.RootURL)
			require.Len(t, report.Pages, 1)
			assert.Contains(t, report.Pages[0].Error, "loopback")
		})
	}
}
