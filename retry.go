package main

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// isRetryableStatusCode reports whether code indicates a transient failure
// worth retrying: GitHub's rate limit response, or a server error other
// than 501 Not Implemented (which is permanent).
func isRetryableStatusCode(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code != http.StatusNotImplemented)
}

// isRetryableError reports whether err (returned from a RoundTrip) is a
// transient network error worth retrying, as opposed to the caller giving
// up (context canceled/expired).
func isRetryableError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// rateLimitReset returns the duration until the rate limit in resp's
// X-RateLimit-Reset header (a Unix timestamp) resets, or false if the
// header is missing or invalid.
func rateLimitReset(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("X-RateLimit-Reset")
	if v == "" {
		return 0, false
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	if d := time.Until(time.Unix(sec, 0)); d > 0 {
		return d, true
	}
	return 0, true
}

// retryDelay returns how long to wait before the next attempt. If
// untilReset is true and resp has an X-RateLimit-Reset header, it waits
// until that reset time; otherwise it honors a Retry-After header if the
// response provided one (in seconds), falling back to capped exponential
// backoff with full jitter.
func retryDelay(attempt int, resp *http.Response, untilReset bool) time.Duration {
	if resp != nil {
		if untilReset {
			if d, ok := rateLimitReset(resp); ok {
				return d
			}
		}
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	const (
		base = 250 * time.Millisecond
		cap_ = 30 * time.Second
	)
	backoff := base << attempt
	if backoff <= 0 || backoff > cap_ {
		backoff = cap_
	}
	return time.Duration(rand.Int64N(int64(backoff)))
}

// RetryTransport retries requests that fail with a transient network error
// or a retryable status code (429, or 5xx other than 501), using capped
// exponential backoff with jitter, honoring Retry-After when present.
//
// Requests with a body are never retried: safely replaying one would
// require buffering it, and the body may already have been partially sent
// by the previous attempt.
type RetryTransport struct {
	Base       http.RoundTripper
	MaxRetries int
	// RateRetry, if true, retries 429 Too Many Requests responses until
	// the rate limit resets (per X-RateLimit-Reset, falling back to
	// Retry-After/backoff if absent), without counting against
	// MaxRetries or giving up.
	RateRetry bool
}

func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Body != http.NoBody {
		return t.Base.RoundTrip(req)
	}

	var attempt, retries int
	for {
		resp, err := t.Base.RoundTrip(req)

		rateLimited := err == nil && t.RateRetry && resp.StatusCode == http.StatusTooManyRequests

		var retryable bool
		switch {
		case err != nil:
			retryable = isRetryableError(err)
		case rateLimited:
			retryable = true
		default:
			retryable = isRetryableStatusCode(resp.StatusCode)
		}
		if !retryable || (!rateLimited && retries >= t.MaxRetries) {
			return resp, err
		}

		timer := time.NewTimer(retryDelay(attempt, resp, rateLimited))
		attempt++
		if !rateLimited {
			retries++
		}
		select {
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return resp, err
		}

		// We're retrying: drain and close the previous response so its
		// connection can be reused.
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
}
