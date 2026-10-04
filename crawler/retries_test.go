package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error {
	b.closed.Store(true)
	return nil
}

func fakeResponse(r *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{},
		Body:       body,
		Request:    r,
	}
}

func newStatusServer(t *testing.T, statuses ...int) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{start: time.Now()}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rec.hit()
		i := int(calls.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		w.WriteHeader(statuses[i])
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newTestLimiter(t *testing.T, client *http.Client, retries int) *HTTPRateLimiter {
	t.Helper()
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	l, err := newHTTPRateLimiter(client, 0, 0, retries)
	require.NoError(t, err, "limiter must be created")
	return l
}

func runGet(t *testing.T, ctx context.Context, l *HTTPRateLimiter, url string) (*http.Response, error) {
	t.Helper()
	resp, err := l.runThrottledRequestWithRetries(ctx, ReqParams{url: url, method: http.MethodGet})
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

func TestStatusRetryability(t *testing.T) {
	cases := map[int]bool{
		200: false, 204: false, 301: false, 400: false, 401: false, 403: false, 404: false, 410: false,
		408: true, 425: true, 429: true,
		500: true, 502: true, 503: true, 504: true,
		501: false, 505: false, 599: false,
	}
	for code, want := range cases {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			assert.Equal(t, want, isRetryableStatus(code), "unexpected retry decision for status %d", code)
		})
	}
}

func TestBackoffGrowsExponentially(t *testing.T) {
	assert.Equal(t, 200*time.Millisecond, backoff(1))
	assert.Equal(t, 400*time.Millisecond, backoff(2))
	assert.Equal(t, 800*time.Millisecond, backoff(3))
}

func TestBackoffIsCappedAndPositive(t *testing.T) {
	for _, attempt := range []int{6, 10, 30, 40, 62, 63, 64, 100} {
		d := backoff(attempt)
		assert.Greater(t, d, time.Duration(0), "backoff(%d) must be positive (overflow?)", attempt)
		assert.LessOrEqual(t, d, MaxDelay, "backoff(%d) must not exceed MaxDelay", attempt)
	}
}

func TestJitterStaysWithinBounds(t *testing.T) {
	const limit = 200 * time.Millisecond
	for i := 0; i < 1000; i++ {
		d := jitter(limit)
		require.GreaterOrEqual(t, d, time.Duration(0), "jitter must not be negative")
		require.Less(t, d, limit, "jitter must be below the cap")
	}
}

func TestRetriesOnRetryableStatuses(t *testing.T) {
	for _, code := range []int{408, 425, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv, rec := newStatusServer(t, code, http.StatusOK)
			l := newTestLimiter(t, nil, 1)

			resp, err := runGet(t, context.Background(), l, srv.URL)

			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode, "request must succeed after a retry")
			assert.Len(t, rec.stamps(), 2, "status %d must trigger exactly one retry", code)
		})
	}
}

func TestNoRetryOnNonRetryableStatuses(t *testing.T) {
	for _, code := range []int{200, 204, 301, 400, 401, 403, 404, 410, 501, 505} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv, rec := newStatusServer(t, code, http.StatusOK)
			l := newTestLimiter(t, &http.Client{
				Timeout:       5 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}, 3)

			resp, err := runGet(t, context.Background(), l, srv.URL)

			require.NoError(t, err)
			assert.Equal(t, code, resp.StatusCode, "status must be returned as is")
			assert.Len(t, rec.stamps(), 1, "status %d must not be retried", code)
		})
	}
}

func TestRetriesExhaustedReturnsLastResponse(t *testing.T) {
	const retries = 3
	srv, rec := newStatusServer(t, http.StatusServiceUnavailable)
	l := newTestLimiter(t, nil, retries)

	resp, err := runGet(t, context.Background(), l, srv.URL)

	require.NoError(t, err, "exhausted retries with a response must not be an error")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Len(t, rec.stamps(), retries+1, "expected 1 initial request plus %d retries", retries)
}

func TestZeroRetriesMakesSingleAttempt(t *testing.T) {
	srv, rec := newStatusServer(t, http.StatusInternalServerError)
	l := newTestLimiter(t, nil, 0)

	resp, err := runGet(t, context.Background(), l, srv.URL)

	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Len(t, rec.stamps(), 1, "retries=0 must make exactly one request")
}

func TestBackoffWaitIsBoundedByCap(t *testing.T) {
	const retries = 3
	const slack = 50 * time.Millisecond

	srv, rec := newStatusServer(t, http.StatusInternalServerError)
	l := newTestLimiter(t, nil, retries)

	_, err := runGet(t, context.Background(), l, srv.URL)
	require.NoError(t, err)

	gaps := rec.gaps()
	require.Len(t, gaps, retries, "expected one gap per retry")
	for i, gap := range gaps {
		limit := backoff(i + 1)
		assert.LessOrEqual(t, gap, limit+slack,
			"wait before retry %d exceeds the jitter cap of %v", i+1, limit)
	}
}

func TestRetriesOnTimeoutError(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) <= 2 {
			return nil, timeoutError{}
		}
		return fakeResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(""))), nil
	})}
	l := newTestLimiter(t, client, 2)

	resp, err := runGet(t, context.Background(), l, "http://example.test/")

	require.NoError(t, err, "request must succeed after transient timeouts")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 3, calls.Load(), "expected 2 timed out attempts and 1 successful")
}

func TestTimeoutAfterAllRetriesIsReturned(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, timeoutError{}
	})}
	l := newTestLimiter(t, client, 2)

	resp, err := runGet(t, context.Background(), l, "http://example.test/")

	require.Error(t, err, "persistent timeout must be returned")
	assert.Nil(t, resp)
	assert.EqualValues(t, 3, calls.Load(), "expected 1 initial request plus 2 retries")
}

func TestRealClientTimeoutIsRetried(t *testing.T) {
	rec := &recorder{start: time.Now()}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		rec.hit()
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	l := newTestLimiter(t, &http.Client{Timeout: 50 * time.Millisecond}, 1)

	resp, err := runGet(t, context.Background(), l, srv.URL)

	require.Error(t, err, "timeout must be returned after retries are exhausted")
	assert.Nil(t, resp)
	assert.Len(t, rec.stamps(), 2, "client timeout must trigger one retry")
}

func TestNonTimeoutErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("connection reset")
	})}
	l := newTestLimiter(t, client, 5)

	resp, err := runGet(t, context.Background(), l, "http://example.test/")

	require.Error(t, err, "non-timeout error must be returned")
	assert.Nil(t, resp)
	assert.EqualValues(t, 1, calls.Load(), "non-timeout errors must not be retried")
}

func TestContextErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return nil, ctx.Err()
	})}
	l := newTestLimiter(t, client, 5)

	resp, err := runGet(t, ctx, l, "http://example.test/")

	require.Error(t, err, "canceled request must return an error")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, resp)
	assert.EqualValues(t, 1, calls.Load(), "context errors must not be retried")
}

func TestCancelDuringBackoffStopsRetries(t *testing.T) {
	const retries = 10
	srv, rec := newStatusServer(t, http.StatusInternalServerError)
	l := newTestLimiter(t, nil, retries)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	_, err := runGet(t, ctx, l, srv.URL)

	require.Error(t, err, "canceled retry loop must return an error")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second, "cancel must interrupt the backoff wait")
	assert.Less(t, len(rec.stamps()), retries+1, "retry loop must stop after cancel")
}

func TestCanceledContextMakesNoRequests(t *testing.T) {
	srv, rec := newStatusServer(t, http.StatusOK)
	l := newTestLimiter(t, nil, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runGet(t, ctx, l, srv.URL)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, rec.stamps(), "no request must be sent with a canceled context")
}

func TestDiscardedResponseBodiesAreClosed(t *testing.T) {
	const retries = 2
	var bodies []*trackedBody
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b := &trackedBody{Reader: strings.NewReader("busy")}
		bodies = append(bodies, b)
		return fakeResponse(r, http.StatusServiceUnavailable, b), nil
	})}
	l := newTestLimiter(t, client, retries)

	resp, err := runGet(t, context.Background(), l, "http://example.test/")
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, bodies, retries+1)

	for i := 0; i < retries; i++ {
		assert.True(t, bodies[i].closed.Load(),
			"body of discarded attempt %d must be closed to avoid connection leaks", i+1)
	}
	assert.False(t, bodies[retries].closed.Load(),
		"body of the returned response must stay open for the caller")
}
