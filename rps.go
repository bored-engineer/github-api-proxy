package main

import (
	"net/http"

	"go.uber.org/ratelimit"
)

type RPSTransport struct {
	Limiter ratelimit.Limiter
	Base    http.RoundTripper
}

func (t *RPSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.Limiter.Take()
	return t.Base.RoundTrip(req)
}
