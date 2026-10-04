package crawler

import "errors"

var (
	ErrorInvalidURL          = errors.New("invalid url")
	ErrorInvalidDepth        = errors.New("invalid depth value")
	ErrorInvalidDelay        = errors.New("invalid delay value")
	ErrorInvalidRetriesCout  = errors.New("invalid retries  count")
	ErrorInvalidRPS          = errors.New("invalid rps value")
	ErrorHTTPClientRequired  = errors.New("http client is required")
	ErrorNotHTML             = errors.New("not an HTML page")
	ErrorRateLimiterRequired = errors.New("http rate limiter is required")
	ErrorInvalidWorkersCount = errors.New("invalid number of workers")
)
