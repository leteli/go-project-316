package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const timerTolerance = 15 * time.Millisecond

type recorder struct {
	mu    sync.Mutex
	start time.Time
	at    []time.Duration
}

func (r *recorder) hit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at = append(r.at, time.Since(r.start))
}

func (r *recorder) stamps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, len(r.at))
	copy(out, r.at)
	return out
}

func (r *recorder) gaps() []time.Duration {
	stamps := r.stamps()
	if len(stamps) < 2 {
		return nil
	}
	out := make([]time.Duration, 0, len(stamps)-1)
	for i := 1; i < len(stamps); i++ {
		out = append(out, stamps[i]-stamps[i-1])
	}
	return out
}

func newSite(t *testing.T, links int) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rec.hit()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = fmt.Fprint(w, "<html><head><title>root</title></head><body>")
		for i := 1; i <= links; i++ {
			_, _ = fmt.Fprintf(w, `<a href="/p%d">p%d</a>`, i, i)
		}
		_, _ = fmt.Fprint(w, "</body></html>")
	})
	for i := 1; i <= links; i++ {
		mux.HandleFunc(fmt.Sprintf("/p%d", i), func(w http.ResponseWriter, r *http.Request) {
			rec.hit()
			w.Header().Set("Content-Type", "text/html")
			if r.Method == http.MethodHead {
				return
			}
			_, _ = fmt.Fprint(w, "<html><head><title>leaf</title></head><body>leaf</body></html>")
		})
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rec.start = time.Now()
	return srv, rec
}

func expectedRequests(links int) int { return 1 + 2*links }

func baseOpts(url string) Options {
	return Options{
		URL:        url,
		Depth:      1,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func mustAnalyze(t *testing.T, opts Options) Report {
	t.Helper()
	payload, err := Analyze(context.Background(), opts)
	require.NoError(t, err, "Analyze must succeed")

	var rep Report
	require.NoError(t, json.Unmarshal(payload, &rep), "report must be valid JSON")
	return rep
}

func TestDelayIsRespectedBetweenRequests(t *testing.T) {
	const links = 4
	const delay = 100 * time.Millisecond

	srv, rec := newSite(t, links)
	opts := baseOpts(srv.URL)
	opts.Delay = delay.String()

	mustAnalyze(t, opts)

	require.Len(t, rec.stamps(), expectedRequests(links), "all pages and links must be requested")
	for i, gap := range rec.gaps() {
		assert.GreaterOrEqual(t, gap, delay-timerTolerance,
			"requests %d and %d are closer than the configured delay", i, i+1)
	}
}

func TestRequestCountDoesNotExceedRate(t *testing.T) {
	const links = 20
	const rps = 10

	srv, rec := newSite(t, links)
	opts := baseOpts(srv.URL)
	opts.RPS = rps

	start := time.Now()
	mustAnalyze(t, opts)
	elapsed := time.Since(start)

	allowed := int(elapsed.Seconds()*rps) + 2
	assert.LessOrEqual(t, len(rec.stamps()), allowed,
		"more requests than rps=%d allows within %v", rps, elapsed)
}

func TestRPSTakesPrecedenceOverDelay(t *testing.T) {
	const links = 4
	const rps = 20
	const wantInterval = time.Second / rps

	srv, rec := newSite(t, links)
	opts := baseOpts(srv.URL)
	opts.Delay = "1s"
	opts.RPS = rps

	start := time.Now()
	mustAnalyze(t, opts)
	elapsed := time.Since(start)

	total := expectedRequests(links)
	require.Len(t, rec.stamps(), total)
	assert.Less(t, elapsed, time.Duration(total)*wantInterval*4,
		"run took %v: delay seems to be applied instead of rps", elapsed)
	for i, gap := range rec.gaps() {
		assert.GreaterOrEqual(t, gap, wantInterval-timerTolerance,
			"requests %d and %d are closer than rps=%d allows", i, i+1, rps)
	}
}

func TestNoLimitIsNotThrottled(t *testing.T) {
	const links = 10

	srv, rec := newSite(t, links)
	opts := baseOpts(srv.URL)

	start := time.Now()
	mustAnalyze(t, opts)
	elapsed := time.Since(start)

	total := expectedRequests(links)
	require.Len(t, rec.stamps(), total)
	assert.Less(t, elapsed, time.Duration(total)*5*time.Millisecond,
		"unlimited run of %d requests took %v: unexpected waiting", total, elapsed)
}

func TestReportIsIdenticalWithAndWithoutRateLimit(t *testing.T) {
	const links = 5

	srvFast, _ := newSite(t, links)
	fast := mustAnalyze(t, baseOpts(srvFast.URL))

	srvSlow, _ := newSite(t, links)
	slowOpts := baseOpts(srvSlow.URL)
	slowOpts.Delay = "20ms"
	slow := mustAnalyze(t, slowOpts)

	require.Len(t, slow.Pages, len(fast.Pages), "rate limiting must not change the page count")
	for i := range slow.Pages {
		assert.Equal(t, "ok", slow.Pages[i].Status,
			"page %s failed: %s", slow.Pages[i].URL, slow.Pages[i].Error)
		assert.Equal(t, fast.Pages[i].Depth, slow.Pages[i].Depth, "depth differs at page %d", i)
	}
}

func TestCancelStopsRateLimitWait(t *testing.T) {
	srv, _ := newSite(t, 5)
	opts := baseOpts(srv.URL)
	opts.Delay = "10s"

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
	case <-time.After(time.Second):
		require.Fail(t, "Analyze did not return within 1s after cancel: the limiter ignores ctx.Done()")
	}
}

func TestInvalidRateOptions(t *testing.T) {
	srv, _ := newSite(t, 1)

	cases := map[string]func(*Options){
		"unparsable delay": func(o *Options) { o.Delay = "200" },
		"negative delay":   func(o *Options) { o.Delay = "-1s" },
		"negative rps":     func(o *Options) { o.RPS = -5 },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := baseOpts(srv.URL)
			mutate(&opts)

			_, err := Analyze(context.Background(), opts)
			assert.Error(t, err, "invalid rate options must be rejected")
		})
	}
}

func TestLimiterReservesDistinctSlots(t *testing.T) {
	const callers = 8
	const interval = 20 * time.Millisecond

	limiter, err := newHTTPRateLimiter(&http.Client{}, 0, interval.String())
	require.NoError(t, err)

	waits := make([]time.Duration, callers)
	var wg sync.WaitGroup
	for i := range waits {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			waits[i] = limiter.reserveSlot()
		}(i)
	}
	wg.Wait()

	var maxWait time.Duration
	for _, w := range waits {
		if w > maxWait {
			maxWait = w
		}
	}
	assert.InDelta(t, float64(time.Duration(callers-1)*interval), float64(maxWait),
		float64(timerTolerance), "the last caller must wait for its own slot, not share one")
}
