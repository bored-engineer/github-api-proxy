package main

import (
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

type LoggingTransport struct {
	Base http.RoundTripper
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	resp, err := t.Base.RoundTrip(req)

	duration := time.Since(start)

	if err != nil {
		log.Error().
			Err(err).
			Str("method", req.Method).
			Str("url", req.URL.String()).
			Dur("duration", duration).
			Msg("HTTP request failed")
		return nil, err
	}

	log.Info().
		Str("method", req.Method).
		Str("url", req.URL.String()).
		Int("status", resp.StatusCode).
		Dur("duration", duration).
		Msg("HTTP request")

	return resp, nil
}
