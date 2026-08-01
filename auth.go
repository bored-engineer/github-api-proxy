package main

import (
	"context"
	"net/http"
)

// authContextKey is the context.Context key under which AuthFields is stored.
type authContextKey struct{}

// AuthFields identifies which credential authenticated a request, for
// logging purposes. Fields are left empty when they don't apply to the
// credential type (e.g. InstallationID for OAuth clients and personal
// access tokens). The "hashed_token" log field is computed separately (see
// logAuth in log.go), directly from the request's Authorization header,
// rather than stored here.
type AuthFields struct {
	ClientID       string
	InstallationID string
}

// AuthFieldsFromContext returns the AuthFields previously stored in ctx by
// AuthTransport, or the zero value if none was stored.
func AuthFieldsFromContext(ctx context.Context) AuthFields {
	fields, _ := ctx.Value(authContextKey{}).(AuthFields)
	return fields
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
	fields := AuthFields{ClientID: t.ClientID, InstallationID: t.InstallationID}
	ctx := context.WithValue(req.Context(), authContextKey{}, fields)
	return t.Base.RoundTrip(req.WithContext(ctx))
}
