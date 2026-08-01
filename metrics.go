package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	Latency = promauto.NewSummaryVec(prometheus.SummaryOpts{
		Name:      "latency_seconds",
		Subsystem: "github",
		Help:      "The latency of the GitHub API",
		Objectives: map[float64]float64{
			// Track the p50, p75, p90, p95 and p99
			0.50: 0.050,
			0.75: 0.025,
			0.90: 0.010,
			0.95: 0.005,
			0.99: 0.001,
		},
	}, []string{"status"})
)

// LatencyTransport records latency metrics based on the status code the
// upstream server actually returned for this specific network round trip
// -- as opposed to whatever transport chain wraps it further out (e.g.
// LoggingTransport, around a caching layer that may substitute a cached
// body/status for a 304 Not Modified response).
type LatencyTransport struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *LatencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.Base.RoundTrip(req)
	duration := time.Since(start)
	if resp != nil {
		Latency.WithLabelValues(strconv.Itoa(resp.StatusCode)).Observe(duration.Seconds())
	}
	return resp, err
}
