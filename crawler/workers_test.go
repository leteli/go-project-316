package crawler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pageSpec struct {
	links  []string
	status int
	ctype  string
	delay  time.Duration
}

type siteHit struct {
	method string
	path   string
}

type siteRecorder struct {
	mu          sync.Mutex
	hits        []siteHit
	inFlight    int
	maxInFlight int
}

func (s *siteRecorder) enter(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, siteHit{method: r.Method, path: r.URL.Path})
	s.inFlight++
	if s.inFlight > s.maxInFlight {
		s.maxInFlight = s.inFlight
	}
}

func (s *siteRecorder) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
}

func (s *siteRecorder) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

func newGraphSite(t *testing.T, pages map[string]pageSpec) (*httptest.Server, *siteRecorder) {
	t.Helper()
	rec := &siteRecorder{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.enter(r)
		defer rec.leave()

		p, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
		ct := p.ctype
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		if p.status != 0 && p.status != http.StatusOK {
			w.WriteHeader(p.status)
			return
		}
		if r.Method == http.MethodHead {
			return
		}
		var b strings.Builder
		b.WriteString("<html><head><title>t</title></head><body>")
		for _, l := range p.links {
			_, _ = fmt.Fprintf(&b, `<a href=%q>l</a>`, l)
		}
		b.WriteString("</body></html>")
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestLevelIsProcessedByWholeWorkerPool(t *testing.T) {
	const leaves = 8
	const workers = 4
	const handlerDelay = 40 * time.Millisecond

	pages := map[string]pageSpec{}
	links := make([]string, 0, leaves)
	for i := 1; i <= leaves; i++ {
		p := fmt.Sprintf("/p%d", i)
		links = append(links, p)
		pages[p] = pageSpec{delay: handlerDelay}
	}
	pages["/"] = pageSpec{links: links}

	srv, rec := newGraphSite(t, pages)
	opts := baseOpts(srv.URL)
	opts.Workers = workers

	start := time.Now()
	rep := mustAnalyze(t, opts)
	elapsed := time.Since(start)

	require.Len(t, rep.Pages, leaves+1)
	assert.LessOrEqual(t, rec.peak(), workers,
		"more than %d requests were in flight: the pool size is not respected", workers)
	assert.Greater(t, rec.peak(), 1, "requests never overlapped: the level is crawled sequentially")
	assert.Less(t, elapsed, time.Duration(leaves)*handlerDelay,
		"crawl took %v, which is no better than sequential", elapsed)
}

func TestCancelDoesNotLeakWorkers(t *testing.T) {
	pages := map[string]pageSpec{}
	links := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		p := fmt.Sprintf("/p%d", i)
		links = append(links, p)
		pages[p] = pageSpec{delay: 100 * time.Millisecond}
	}
	pages["/"] = pageSpec{links: links}

	srv, _ := newGraphSite(t, pages)
	opts := baseOpts(srv.URL)
	opts.Workers = 4

	runtime.GC()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Analyze(ctx, opts)
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Analyze did not return within 2s after cancel")
	}

	opts.HTTPClient.CloseIdleConnections()

	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		got := runtime.NumGoroutine()
		if got <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked after cancel: before=%d, after=%d", before, got)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
