package main

import (
	"net/http"
	"strconv"
	"time"

	ghtransport "github.com/bored-engineer/github-conditional-http-transport"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type LoggingTransport struct {
	Base http.RoundTripper
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Perform the request, tracking how long it takes.
	start := time.Now()
	resp, err := t.Base.RoundTrip(req)
	duration := time.Since(start)

	// Skip logging if the request is for the rate limit API
	if req.URL.Path == "/rate_limit" {
		return resp, err
	}

	// Initialize the log event (error vs info) with the duration.
	var evt *zerolog.Event
	if err != nil {
		evt = log.Error().Err(err)
	} else {
		evt = log.Info()
	}
	evt = evt.Dur("duration", duration)

	// Add the request details (if not nil).
	if req != nil {
		evt = evt.Str("method", req.Method)
		evt = evt.Str("url", req.URL.String())

		if req.RemoteAddr != "" {
			evt = evt.Str("remote_addr", req.RemoteAddr)
		}

		if userAgent := req.Header.Get("User-Agent"); userAgent != "" {
			evt = evt.Str("user_agent", userAgent)
		}

		if authorization := req.Header.Get("Authorization"); authorization != "" {
			evt = evt.Str("hashed_token", ghtransport.HashToken(authorization))
		}
	}

	// If the response is not nil, add the response details.
	if resp != nil {
		evt = evt.Int("status", resp.StatusCode)

		if resp.ContentLength > 0 {
			evt = evt.Int64("size", resp.ContentLength)
		}

		if requestID := resp.Header.Get("X-Github-Request-Id"); requestID != "" {
			evt = evt.Str("request_id", requestID)
		}

		if mediaType := resp.Header.Get("X-Github-Media-Type"); mediaType != "" {
			evt = evt.Str("media_type", mediaType)
		}

		if contentType := resp.Header.Get("Content-Type"); contentType != "" {
			evt = evt.Str("content_type", contentType)
		}

		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
			if i, err := strconv.ParseUint(remaining, 10, 64); err == nil {
				evt = evt.Uint64("ratelimit_remaining", i)
			}
		}

		if resource := resp.Header.Get("X-RateLimit-Resource"); resource != "" {
			evt = evt.Str("ratelimit_resource", resource)
		}
	}

	// Fire the log event.
	evt.Msg("HTTP request")

	return resp, nil
}
