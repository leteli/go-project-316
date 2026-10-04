package crawler

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"
)

type HTTPRateLimiter struct {
	client      *http.Client
	mu          sync.Mutex
	reqInterval time.Duration
	nextReqTime time.Time
	retries     int
}

type ReqParams struct {
	url    string
	method string
}

func newHTTPRateLimiter(client *http.Client, rps int, delay string, retries int) (*HTTPRateLimiter, error) {
	if client == nil {
		return nil, ErrorHTTPClientRequired
	}
	if rps < 0 {
		return nil, ErrorInvalidRPS
	}
	if delay == "" {
		delay = "0s"
	}
	d, err := time.ParseDuration(delay)
	if err != nil || d < 0 {
		return nil, ErrorInvalidDelay
	}
	if retries < 0 {
		return nil, ErrorInvalidRetriesCout
	}
	var reqInterval time.Duration
	if rps != 0 {
		reqInterval = time.Second / time.Duration(rps)
	} else {
		reqInterval = d
	}
	return &HTTPRateLimiter{
		client:      client,
		mu:          sync.Mutex{},
		reqInterval: reqInterval,
		nextReqTime: time.Now(),
		retries:     retries,
	}, nil
}

func (r *HTTPRateLimiter) runThrottledRequestWithRetries(ctx context.Context, params ReqParams) (*http.Response, error) {
	var attempt int
	for attempt < r.retries {
		res, err := r.runThrottledRequest(ctx, params)
		attempt++
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			var netErr net.Error
			timeoutErr := errors.As(err, &netErr) && netErr.Timeout()
			if !timeoutErr {
				return res, err
			}
		}
		if res != nil {
			if !isRetryableStatus(res.StatusCode) {
				return res, err
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
			_ = res.Body.Close()
		}
		timer := time.NewTimer(jitter(backoff(attempt)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
			continue
		}
	}
	return r.runThrottledRequest(ctx, params)
}

func (r *HTTPRateLimiter) runThrottledRequest(ctx context.Context, params ReqParams) (*http.Response, error) {
	if r.reqInterval == 0 {
		return r.makeHTTPRequest(ctx, params)
	}
	left := r.reserveSlot()
	if left <= 0 {
		return r.makeHTTPRequest(ctx, params)
	}
	timer := time.NewTimer(left)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return r.makeHTTPRequest(ctx, params)
	}
}

func (r *HTTPRateLimiter) reserveSlot() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	left := time.Until(r.nextReqTime)
	if left <= 0 {
		r.nextReqTime = time.Now().Add(r.reqInterval)
	} else {
		r.nextReqTime = r.nextReqTime.Add(r.reqInterval)
	}
	return left
}

func (r *HTTPRateLimiter) makeHTTPRequest(ctx context.Context, params ReqParams) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, params.method, params.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func isRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

const BaseDelay = time.Duration(100 * time.Millisecond)
const MaxDelay = time.Duration(5 * time.Second)

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * BaseDelay
	if d <= 0 || d > MaxDelay {
		return MaxDelay
	}
	return d
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(d)))
}
