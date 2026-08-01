package main

import (
	"context"
	"net/http"

	"github.com/rs/xid"
	"github.com/rs/zerolog/hlog"
)

// RequestIDFromContext returns the request ID previously stored in ctx by
// RequestIDTransport (rendered via xid.ID.String()), or "" if none was stored.
func RequestIDFromContext(ctx context.Context) string {
	id, ok := hlog.IDFromCtx(ctx)
	if !ok {
		return ""
	}
	return id.String()
}

// RequestIDTransport assigns a unique ID (via xid, using the same
// zerolog/hlog helpers as hlog.RequestIDHandler) to each incoming request
// and stores it in its context, so LoggingTransport can log it. Since
// RetryTransport reuses the same *http.Request across attempts, every retry
// of a single incoming request shares this ID, letting them be correlated
// across log lines.
type RequestIDTransport struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *RequestIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, ok := hlog.IDFromRequest(req); !ok {
		req = req.WithContext(hlog.CtxWithID(req.Context(), xid.New()))
	}
	return t.Base.RoundTrip(req)
}
