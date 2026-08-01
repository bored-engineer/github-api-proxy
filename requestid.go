package main

import (
	"context"
	"net/http"

	"github.com/rs/xid"
)

// requestIDContextKey is the context.Context key under which the request ID
// is stored (via ContextTransport).
type requestIDContextKey struct{}

// RequestIDFromContext returns the request ID previously stored in ctx by
// RequestIDTransport (rendered via xid.ID.String()), or "" if none was stored.
func RequestIDFromContext(ctx context.Context) string {
	id, ok := ctx.Value(requestIDContextKey{}).(xid.ID)
	if !ok {
		return ""
	}
	return id.String()
}

// RequestIDTransport assigns a unique ID (via xid, the same scheme used by
// zerolog/hlog's RequestIDHandler) to each incoming request and stores it in
// its context, so LoggingTransport can log it. Since RetryTransport reuses
// the same *http.Request across attempts, every retry of a single incoming
// request shares this ID, letting them be correlated across log lines. A
// fresh ContextTransport is used per request (rather than a single shared
// one) since the ID must be regenerated every time, unlike ContextTransport's
// other uses where Value is fixed for the transport's lifetime.
type RequestIDTransport struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *RequestIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct := &ContextTransport{Base: t.Base, Key: requestIDContextKey{}, Value: xid.New()}
	return ct.RoundTrip(req)
}
