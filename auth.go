package main

import (
	"context"
	"net/http"

	ghtransport "github.com/bored-engineer/github-conditional-http-transport"
	"github.com/rs/zerolog"
)

// authContextKey is the context.Context key under which AuthFields is stored.
type authContextKey struct{}

// AuthFields identifies which credential authenticated a request, for
// logging purposes. Fields are left empty when they don't apply to the
// credential type (e.g. InstallationID for OAuth clients and personal
// access tokens).
type AuthFields struct {
	ClientID       string
	InstallationID string
	HashedToken    string
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler, rendering the
// "auth" object of a log line.
func (a AuthFields) MarshalZerologObject(e *zerolog.Event) {
	if a.ClientID != "" {
		e.Str("client_id", a.ClientID)
	}
	if a.InstallationID != "" {
		e.Str("installation_id", a.InstallationID)
	}
	if a.HashedToken != "" {
		e.Str("hashed_token", a.HashedToken)
	}
}

// AuthFieldsFromContext returns the AuthFields previously stored in ctx by
// AuthTransport, or the zero value if none was stored.
func AuthFieldsFromContext(ctx context.Context) AuthFields {
	fields, _ := ctx.Value(authContextKey{}).(AuthFields)
	return fields
}

// AuthTransport computes the AuthFields for each request (using its
// Authorization header, already set by the oauth2/basic-auth transport it's
// wrapped in) and stores them in the request's context, so LoggingTransport
// can log them without needing any knowledge of authentication itself.
type AuthTransport struct {
	ClientID       string
	InstallationID string
	Base           http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	fields := AuthFields{ClientID: t.ClientID, InstallationID: t.InstallationID}
	if authorization := req.Header.Get("Authorization"); authorization != "" {
		fields.HashedToken = ghtransport.HashToken(authorization)
	}
	ctx := context.WithValue(req.Context(), authContextKey{}, fields)
	return t.Base.RoundTrip(req.WithContext(ctx))
}
