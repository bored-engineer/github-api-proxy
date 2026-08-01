package main

import (
	"context"
	"net/http"
)

// ContextTransport injects Value into each request's context under Key,
// before delegating to Base.
type ContextTransport struct {
	Base  http.RoundTripper
	Key   any
	Value any
}

// RoundTrip implements http.RoundTripper.
func (t *ContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := context.WithValue(req.Context(), t.Key, t.Value)
	return t.Base.RoundTrip(req.WithContext(ctx))
}
