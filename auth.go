package main

import (
	"context"
	"net/http"
)

// clientIDContextKey is the context.Context key under which the client ID is stored.
type clientIDContextKey struct{}

// installationIDContextKey is the context.Context key under which the installation ID is stored.
type installationIDContextKey struct{}

// ClientIDFromContext returns the client ID previously stored in ctx by
// AuthTransport, or "" if none was stored (e.g. the request was
// authenticated by a personal access token, which has no client ID).
func ClientIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clientIDContextKey{}).(string)
	return id
}

// InstallationIDFromContext returns the installation ID previously stored
// in ctx by AuthTransport, or "" if none was stored (e.g. the request
// wasn't authenticated by a GitHub App installation).
func InstallationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(installationIDContextKey{}).(string)
	return id
}

// AuthTransport identifies which configured credential (ClientID/
// InstallationID) authenticated each request and stores it in the
// request's context, so LoggingTransport can log it without needing any
// knowledge of authentication itself.
type AuthTransport struct {
	ClientID       string
	InstallationID string
	Base           http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if t.ClientID != "" {
		ctx = context.WithValue(ctx, clientIDContextKey{}, t.ClientID)
	}
	if t.InstallationID != "" {
		ctx = context.WithValue(ctx, installationIDContextKey{}, t.InstallationID)
	}
	return t.Base.RoundTrip(req.WithContext(ctx))
}
