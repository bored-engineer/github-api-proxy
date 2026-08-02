package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"

	ghratelimit "github.com/bored-engineer/github-rate-limit-http-transport"
)

// RateLimitSource pairs a *ghratelimit.Transport with the same credential
// labels (client_id/installation_id/hashed_token) used to identify it in
// Prometheus metrics, so RateLimitsHandler can report its rate limits under
// the same identity.
type RateLimitSource struct {
	ClientID       string
	InstallationID string
	HashedToken    string
	Transport      *ghratelimit.Transport
}

// RateLimitResult is the JSON representation of a single RateLimitSource's
// rate limits (or fetch error) returned by RateLimitsHandler.
type RateLimitResult struct {
	ClientID       string                                     `json:"client_id,omitempty"`
	InstallationID string                                     `json:"installation_id,omitempty"`
	HashedToken    string                                     `json:"hashed_token,omitempty"`
	Resources      map[ghratelimit.Resource]*ghratelimit.Rate `json:"resources,omitempty"`
	Error          string                                     `json:"error,omitempty"`
}

// RateLimitsHandler returns an http.HandlerFunc that, on every request, issues a live GET
// to u for every source's transport in parallel and returns the freshly observed rate
// limits (or a per-source fetch error) as a JSON array.
func RateLimitsHandler(sources []RateLimitSource, u *url.URL) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		results := make([]RateLimitResult, len(sources))
		var wg sync.WaitGroup
		for idx, source := range sources {
			wg.Go(func() {
				result := RateLimitResult{
					ClientID:       source.ClientID,
					InstallationID: source.InstallationID,
					HashedToken:    source.HashedToken,
				}
				if err := source.Transport.Limits.Fetch(r.Context(), source.Transport, u); err != nil {
					result.Error = err.Error()
				} else {
					resources := make(map[ghratelimit.Resource]*ghratelimit.Rate)
					for resource, rate := range source.Transport.Limits.Iter() {
						resources[resource] = rate
					}
					result.Resources = resources
				}
				results[idx] = result
			})
		}
		wg.Wait()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	}
}
