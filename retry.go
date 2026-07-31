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

// retryDelay returns how long to wait before the next attempt, honoring a
// Retry-After header if the response provided one (in seconds), otherwise
// falling back to exponential backoff (starting at wait, doubling each
// attempt, capped at max) with full jitter.
func retryDelay(attempt int, resp *http.Response, wait, max time.Duration) time.Duration {
	if resp != nil {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	backoff := wait << attempt
	if backoff <= 0 || backoff > max {
		backoff = max
	}
	return time.Duration(rand.Int64N(int64(backoff)))
}

// RetryTransport retries requests that fail with a transient network error
// or a retryable status code (429, or 5xx other than 501), using
// exponential backoff with jitter, honoring Retry-After when present.
//
// Requests with a body are never retried: safely replaying one would
// require buffering it, and the body may already have been partially sent
// by the previous attempt.
type RetryTransport struct {
	Base       http.RoundTripper
	MaxRetries int
	// RateRetry, if true, retries 429 Too Many Requests responses until
	// Retry-After (falling back to backoff if absent) clears, without
	// counting against MaxRetries or giving up.
	RateRetry bool
	// Wait is the initial backoff delay (before jitter/doubling).
	Wait time.Duration
	// MaxWait caps the backoff delay.
	MaxWait time.Duration
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

		timer := time.NewTimer(retryDelay(attempt, resp, t.Wait, t.MaxWait))
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
