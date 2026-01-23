package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"testing"

	ghratelimit "github.com/bored-engineer/github-rate-limit-http-transport"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/oauth2"
)

type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	panic("unexpected RoundTrip call")
}

type stubTokenSource struct {
	token string
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: s.token}, nil
}

func TestConfigurePATTransport(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectToken string
		expectID    string
	}{
		{
			name:        "custom identifier",
			input:       "id123:token-value",
			expectToken: "token-value",
			expectID:    "id123",
		},
		{
			name:        "hashed identifier",
			input:       "token-value",
			expectToken: "token-value",
			expectID:    hashToken("token-value"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RateLimitRemaining.Reset()
			RateLimitReset.Reset()

			base := &stubRoundTripper{}

			got := configurePatTransport(tt.input, base, nil)
			if len(got) != 1 {
				t.Fatalf("expected one transport, got %d", len(got))
			}

			transport := got[0]
			oauthTransport, ok := transport.Base.(*oauth2.Transport)
			if !ok {
				t.Fatalf("expected oauth2.Transport base, got %T", transport.Base)
			}
			if oauthTransport.Base != base {
				t.Fatalf("expected provided base transport to be used")
			}

			token, err := oauthTransport.Source.Token()
			if err != nil {
				t.Fatalf("token retrieval failed: %v", err)
			}
			if token.AccessToken != tt.expectToken {
				t.Fatalf("unexpected token: got %q want %q", token.AccessToken, tt.expectToken)
			}

			resource := ghratelimit.Resource("core")
			rate := &ghratelimit.Rate{Remaining: 42, Reset: 123}
			transport.Limits.Notify(&http.Response{}, resource, rate)

			gauge := RateLimitRemaining.WithLabelValues(tt.expectID, resource.String())
			if got := testutil.ToFloat64(gauge); got != float64(rate.Remaining) {
				t.Fatalf("remaining gauge mismatch: got %v want %v", got, rate.Remaining)
			}

			resetGauge := RateLimitReset.WithLabelValues(tt.expectID, resource.String())
			if got := testutil.ToFloat64(resetGauge); got != float64(rate.Reset) {
				t.Fatalf("reset gauge mismatch: got %v want %v", got, rate.Reset)
			}
		})
	}
}

func TestSetUpOauthTransport(t *testing.T) {
	origBasic := ghauthBasic
	defer func() { ghauthBasic = origBasic }()

	RateLimitRemaining.Reset()
	RateLimitReset.Reset()

	base := &stubRoundTripper{}
	authTransport := &stubRoundTripper{}

	var gotID, gotSecret string
	ghauthBasic = func(rt http.RoundTripper, clientID, clientSecret string) (http.RoundTripper, error) {
		if rt != base {
			t.Fatalf("unexpected base transport")
		}
		gotID, gotSecret = clientID, clientSecret
		return authTransport, nil
	}

	balancing := configureOauthTransport("client-id:client-secret", base, nil)
	if len(balancing) != 1 {
		t.Fatalf("expected one transport, got %d", len(balancing))
	}

	transport := balancing[0]
	if transport.Base != authTransport {
		t.Fatalf("expected auth transport to be used")
	}
	if gotID != "client-id" || gotSecret != "client-secret" {
		t.Fatalf("unexpected credentials captured: %q/%q", gotID, gotSecret)
	}

	resource := ghratelimit.Resource("core")
	rate := &ghratelimit.Rate{Remaining: 7, Reset: 11}
	transport.Limits.Notify(&http.Response{}, resource, rate)

	if got := testutil.ToFloat64(RateLimitRemaining.WithLabelValues("client-id", resource.String())); got != float64(rate.Remaining) {
		t.Fatalf("remaining gauge mismatch: got %v want %v", got, rate.Remaining)
	}
	if got := testutil.ToFloat64(RateLimitReset.WithLabelValues("client-id", resource.String())); got != float64(rate.Reset) {
		t.Fatalf("reset gauge mismatch: got %v want %v", got, rate.Reset)
	}
}

func TestSetupGitHubApp(t *testing.T) {
	origApp := ghauthApp
	defer func() { ghauthApp = origApp }()

	RateLimitRemaining.Reset()
	RateLimitReset.Reset()

	base := &stubRoundTripper{}
	ts := &stubTokenSource{token: "app-token"}

	var gotApp, gotInstall, gotKey string
	ghauthApp = func(ctx context.Context, appID, installationID, privateKey string) (oauth2.TokenSource, error) {
		gotApp, gotInstall, gotKey = appID, installationID, privateKey
		return ts, nil
	}

	balancing := configureGitHubApp(context.Background(), "app-1:inst-2:pk", base, nil)
	if len(balancing) != 1 {
		t.Fatalf("expected one transport, got %d", len(balancing))
	}

	transport := balancing[0]
	oauthTransport, ok := transport.Base.(*oauth2.Transport)
	if !ok {
		t.Fatalf("expected oauth2.Transport base, got %T", transport.Base)
	}
	if oauthTransport.Base != base {
		t.Fatalf("expected provided base transport to be used")
	}
	if oauthTransport.Source != ts {
		t.Fatalf("unexpected token source")
	}
	if gotApp != "app-1" || gotInstall != "inst-2" || gotKey != "pk" {
		t.Fatalf("unexpected app params captured: %q/%q/%q", gotApp, gotInstall, gotKey)
	}

	resource := ghratelimit.Resource("core")
	rate := &ghratelimit.Rate{Remaining: 9, Reset: 13}
	transport.Limits.Notify(&http.Response{}, resource, rate)

	if got := testutil.ToFloat64(RateLimitRemaining.WithLabelValues("app-1:inst-2", resource.String())); got != float64(rate.Remaining) {
		t.Fatalf("remaining gauge mismatch: got %v want %v", got, rate.Remaining)
	}
	if got := testutil.ToFloat64(RateLimitReset.WithLabelValues("app-1:inst-2", resource.String())); got != float64(rate.Reset) {
		t.Fatalf("reset gauge mismatch: got %v want %v", got, rate.Reset)
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.StdEncoding.EncodeToString(sum[:])
}
