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

// FromContext returns the value stored in ctx under key, or the zero value of T if none was stored.
func FromContext[T any](ctx context.Context, key any) T {
	v, _ := ctx.Value(key).(T)
	return v
}
