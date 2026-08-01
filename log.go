package main

import (
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"time"

	ghtransport "github.com/bored-engineer/github-conditional-http-transport"
	ghratelimit "github.com/bored-engineer/github-rate-limit-http-transport"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// splitHostPort splits a "host:port" address into its parts, returning
// hostport unchanged as the host (with an empty port) if it can't be split
// (e.g. it's already a bare host, or empty).
func splitHostPort(hostport string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return host, port
}

// sensitiveHeaders are header names redacted by sanitizeHeaders before
// logging, since their values are credentials rather than metadata.
var sensitiveHeaders = []string{"Authorization", "Cookie", "Set-Cookie"}

// sanitizeHeaders returns a copy of headers with sensitiveHeaders redacted,
// safe to log verbatim (e.g. the Authorization header holds the raw
// credential itself, already surfaced non-reversibly via the "auth" object).
func sanitizeHeaders(headers http.Header) http.Header {
	sanitized := headers.Clone()
	for _, name := range sensitiveHeaders {
		if sanitized.Get(name) != "" {
			sanitized.Set(name, "REDACTED")
		}
	}
	return sanitized
}

// sensitiveQueryParams are query parameter names redacted by sanitizeQuery
// before logging, since GitHub's (legacy, but still accepted) query-string
// authentication forms carry a credential there rather than in a header.
var sensitiveQueryParams = []string{"access_token", "client_secret"}

// sanitizeQuery returns rawQuery with sensitiveQueryParams values redacted,
// safe to log verbatim. If rawQuery isn't well-formed enough to parse, or
// none of sensitiveQueryParams are present, it's returned unchanged.
func sanitizeQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	var redacted bool
	for _, name := range sensitiveQueryParams {
		if values.Has(name) {
			values.Set(name, "REDACTED")
			redacted = true
		}
	}
	if !redacted {
		return rawQuery
	}
	return values.Encode()
}

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

// UpstreamStatusTransport records latency metrics based on the status code
// the upstream server actually returned for this specific network round
// trip -- as opposed to whatever transport chain wraps it further out (e.g.
// LoggingTransport, around a caching layer that may substitute a cached
// body/status for a 304 Not Modified response).
type UpstreamStatusTransport struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *UpstreamStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.Base.RoundTrip(req)
	duration := time.Since(start)
	if resp != nil {
		Latency.WithLabelValues(strconv.Itoa(resp.StatusCode)).Observe(duration.Seconds())
	}
	return resp, err
}

// logAddr builds a Dict for an "ip"/"port" pair, or nil if both are empty
// (e.g. the address wasn't known, so the field should be omitted entirely).
func logAddr(ip, port string) *zerolog.Event {
	if ip == "" && port == "" {
		return nil
	}
	d := zerolog.Dict()
	if ip != "" {
		d.Str("ip", ip)
	}
	if port != "" {
		d.Str("port", port)
	}
	return d
}

// logConn builds the "conn" object of a log line from an
// httptrace.GotConnInfo, describing the underlying network connection used
// for this specific attempt. Returns nil if info is nil (e.g. an error
// occurred before a connection was ever obtained).
func logConn(info *httptrace.GotConnInfo) *zerolog.Event {
	if info == nil {
		return nil
	}
	d := zerolog.Dict()
	if id := ConnID(info.Conn); id != "" {
		d.Str("id", id)
	}
	if info.Conn != nil {
		if remote := logAddr(splitHostPort(info.Conn.RemoteAddr().String())); remote != nil {
			d.Dict("remote", remote)
		}
	}
	d.Bool("reused", info.Reused)
	d.Bool("was_idle", info.WasIdle)
	if info.WasIdle {
		// IdleTime is only meaningful when WasIdle is true.
		d.Dur("idle_time", info.IdleTime)
	}
	return d
}

// logAuth builds the "auth" object of a log line, identifying which
// credential authenticated req: ClientID/InstallationID come from context
// (set by AuthTransport for whichever configured credential is in use),
// while HashedToken is computed directly from the request's current
// Authorization header, so it's populated even for a pass-through request
// authenticated by the client's own header rather than a configured
// --auth-* credential.
func logAuth(req *http.Request) *zerolog.Event {
	auth := AuthFieldsFromContext(req.Context())
	d := zerolog.Dict()
	if auth.ClientID != "" {
		d.Str("client_id", auth.ClientID)
	}
	if auth.InstallationID != "" {
		d.Str("installation_id", auth.InstallationID)
	}
	if authorization := req.Header.Get("Authorization"); authorization != "" {
		d.Str("hashed_token", ghtransport.HashToken(authorization))
	}
	return d
}

// logCacheStatus parses resp's "Cache-Status" header (RFC 9211), as set by
// github-conditional-http-transport on every response, into a Dict with
// etag/hit/stored/reason. Returns nil if the header is empty, meaning
// caching wasn't in play for this response at all. Example header values:
// "github-conditional-http-transport; hit" or "...; fwd=uri-miss;
// fwd-status=200; stored".
func logCacheStatus(resp *http.Response) *zerolog.Event {
	header := resp.Header.Get("Cache-Status")
	if header == "" {
		return nil
	}
	d := zerolog.Dict()
	if etag := resp.Header.Get("Etag"); etag != "" {
		d.Str("etag", etag)
	}
	var hit, stored bool
	var reason string
	for _, param := range strings.Split(header, ";")[1:] { // skip the leading cache name
		switch param = strings.TrimSpace(param); {
		case param == "hit":
			hit = true
		case param == "stored":
			stored = true
		case strings.HasPrefix(param, "fwd="):
			reason = strings.TrimPrefix(param, "fwd=")
		}
	}
	d.Bool("hit", hit)
	d.Bool("stored", stored)
	if reason != "" {
		d.Str("reason", reason)
	}
	return d
}

// logRateLimit parses resp's rate-limit headers into the "ratelimit"
// object of a log line. Returns nil if resp doesn't carry rate-limit
// headers (e.g. it's not a GitHub API response).
func logRateLimit(resp *http.Response) *zerolog.Event {
	rate, err := ghratelimit.ParseRate(resp.Header)
	if err != nil {
		return nil
	}
	d := zerolog.Dict()
	d.Uint64("limit", rate.Limit)
	d.Uint64("used", rate.Used)
	d.Uint64("remaining", rate.Remaining)
	d.Uint64("reset", rate.Reset)
	if resource := resp.Header.Get("X-Ratelimit-Resource"); resource != "" {
		d.Str("resource", resource)
	}
	return d
}

// LoggingTransport logs each request/response and records latency metrics.
type LoggingTransport struct {
	Base http.RoundTripper
	// LogRateLimit controls whether requests to /rate_limit are logged;
	// they are typically noisy since they're issued on a fixed interval
	// to poll rate limit status rather than in response to real traffic.
	LogRateLimit bool
	// LogAddr controls whether the "incoming" and "source" address objects
	// are logged.
	LogAddr bool
	// LogConn controls whether the "conn" object (the underlying network
	// connection's remote address, reuse, and idle time) is logged. This
	// requires an httptrace.ClientTrace on every request, so it's kept
	// separate from LogAddr.
	LogConn bool
	// LogRequestHeaders controls whether the full (sanitized) request
	// headers are logged in the "request" object.
	LogRequestHeaders bool
	// LogResponseHeaders controls whether the full (sanitized) response
	// headers are logged in the "response" object.
	LogResponseHeaders bool
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Trace the underlying connection so we can log its details (remote
	// address, reuse, idle time) for this specific attempt, regardless of
	// whether a response (or only an error) comes back.
	var gotConn *httptrace.GotConnInfo
	if t.LogConn {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				gotConn = &info
			},
		}))
	}

	// Perform the request, tracking how long it takes.
	start := time.Now()
	resp, err := t.Base.RoundTrip(req)
	duration := time.Since(start)

	// Skip logging if the request is for the rate limit API, unless enabled.
	if !t.LogRateLimit && req.URL.Path == "/rate_limit" {
		return resp, err
	}

	// Initialize the log event (error vs info) with the duration.
	var evt *zerolog.Event
	if err != nil {
		evt = log.Error().Err(err)
	} else {
		evt = log.Info()
	}
	evt = evt.Dur("duration", duration)

	// Build the "request" object.
	reqDict := zerolog.Dict()
	if id := RequestIDFromContext(req.Context()); id != "" {
		reqDict.Str("id", id)
	}
	reqDict.Str("method", req.Method)
	reqDict.Str("scheme", req.URL.Scheme)
	reqDict.Str("host", req.URL.Host)
	reqDict.Str("path", req.URL.Path)
	if query := sanitizeQuery(req.URL.RawQuery); query != "" {
		reqDict.Str("query", query)
	}
	if t.LogAddr {
		if incoming := logAddr(splitHostPort(req.RemoteAddr)); incoming != nil {
			reqDict.Dict("incoming", incoming)
		}
		if source := logAddr(SrcIPFromContext(req.Context()), ""); source != nil {
			reqDict.Dict("source", source)
		}
	}
	if t.LogConn {
		if conn := logConn(gotConn); conn != nil {
			reqDict.Dict("conn", conn)
		}
	}
	if userAgent := req.Header.Get("User-Agent"); userAgent != "" {
		reqDict.Str("user_agent", userAgent)
	}
	reqDict.Dict("auth", logAuth(req))
	if t.LogRequestHeaders {
		reqDict.Interface("headers", sanitizeHeaders(req.Header))
	}
	evt = evt.Dict("request", reqDict)

	// If the response is not nil, add the response details.
	if resp != nil {
		respDict := zerolog.Dict()
		respDict.Int("status", resp.StatusCode)
		if cache := logCacheStatus(resp); cache != nil {
			respDict.Dict("cache", cache)
		}
		if resp.ContentLength > 0 {
			respDict.Int64("size", resp.ContentLength)
		}
		if requestID := resp.Header.Get("X-Github-Request-Id"); requestID != "" {
			respDict.Str("request_id", requestID)
		}
		if mediaType := resp.Header.Get("X-Github-Media-Type"); mediaType != "" {
			respDict.Str("media_type", mediaType)
		}
		if contentType := resp.Header.Get("Content-Type"); contentType != "" {
			respDict.Str("content_type", contentType)
		}
		if rl := logRateLimit(resp); rl != nil {
			respDict.Dict("ratelimit", rl)
		}
		if t.LogResponseHeaders {
			respDict.Interface("headers", sanitizeHeaders(resp.Header))
		}
		evt = evt.Dict("response", respDict)
	}

	// Fire the log event.
	evt.Msg("HTTP request")

	return resp, err
}
