package main

import (
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// cacheFields renders the "cache" object of a log line, parsed from the
// "Cache-Status" header (RFC 9211) that github-conditional-http-transport
// sets on every response.
type cacheFields struct {
	ETag   string
	Hit    bool
	Stored bool
	Reason string
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler.
func (c cacheFields) MarshalZerologObject(e *zerolog.Event) {
	if c.ETag != "" {
		e.Str("etag", c.ETag)
	}
	e.Bool("hit", c.Hit)
	e.Bool("stored", c.Stored)
	if c.Reason != "" {
		e.Str("reason", c.Reason)
	}
}

// parseCacheStatus parses the parameters of a "Cache-Status" header value
// (RFC 9211) as set by github-conditional-http-transport, e.g.
// "github-conditional-http-transport; hit" or "...; fwd=uri-miss;
// fwd-status=200; stored". ok is false if header is empty, meaning caching
// wasn't in play for this response at all.
func parseCacheStatus(header string) (fields cacheFields, ok bool) {
	if header == "" {
		return cacheFields{}, false
	}
	for _, param := range strings.Split(header, ";")[1:] { // skip the leading cache name
		switch param = strings.TrimSpace(param); {
		case param == "hit":
			fields.Hit = true
		case param == "stored":
			fields.Stored = true
		case strings.HasPrefix(param, "fwd="):
			fields.Reason = strings.TrimPrefix(param, "fwd=")
		}
	}
	return fields, true
}

// rateLimitFields renders the "ratelimit" object of a log line.
type rateLimitFields struct {
	ghratelimit.Rate
	Resource string
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler.
func (r rateLimitFields) MarshalZerologObject(e *zerolog.Event) {
	e.Uint64("limit", r.Limit)
	e.Uint64("used", r.Used)
	e.Uint64("remaining", r.Remaining)
	e.Uint64("reset", r.Reset)
	if r.Resource != "" {
		e.Str("resource", r.Resource)
	}
}

// addrFields renders an "ip"/"port" pair as a nested object.
type addrFields struct {
	IP   string
	Port string
}

// Empty reports whether neither field was populated (e.g. the address
// wasn't known, so the object should be omitted entirely).
func (a addrFields) Empty() bool {
	return a.IP == "" && a.Port == ""
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler.
func (a addrFields) MarshalZerologObject(e *zerolog.Event) {
	if a.IP != "" {
		e.Str("ip", a.IP)
	}
	if a.Port != "" {
		e.Str("port", a.Port)
	}
}

// connFields renders the "conn" object of a log line, describing the
// underlying network connection used for this specific attempt (captured
// via httptrace.GotConnInfo).
type connFields struct {
	// ID uniquely identifies the underlying connection (assigned once when
	// it's dialed, via WithConnID), stable across every request that
	// reuses it, so they can be correlated in logs.
	ID string
	// Remote is the address of the upstream (GitHub) server this
	// connection is actually established with.
	Remote  addrFields
	Reused  bool
	WasIdle bool
	// IdleTime reports how long the connection was previously idle, only
	// meaningful (and only logged) when WasIdle is true.
	IdleTime time.Duration
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler.
func (c connFields) MarshalZerologObject(e *zerolog.Event) {
	if c.ID != "" {
		e.Str("id", c.ID)
	}
	if !c.Remote.Empty() {
		e.Object("remote", c.Remote)
	}
	e.Bool("reused", c.Reused)
	e.Bool("was_idle", c.WasIdle)
	if c.WasIdle {
		e.Dur("idle_time", c.IdleTime)
	}
}

// requestFields renders the "request" object of a log line.
type requestFields struct {
	ID     string
	Method string
	Scheme string
	Host   string
	Path   string
	// Query is the (sanitized) raw query string, without the leading "?".
	Query string
	// Incoming is the address of the client that connected to the proxy
	// (i.e. the inbound *http.Request's RemoteAddr).
	Incoming addrFields
	// Conn describes the underlying network connection used for this
	// attempt, non-nil only if --log-conn was set (since it requires an
	// httptrace.ClientTrace on every request).
	Conn      *connFields
	UserAgent string
	// Source is the IP this request was dialed from, set only if --src-ip was used.
	Source addrFields
	Auth   AuthFields
	// Headers holds the full (sanitized) request headers, non-nil only if
	// --log-request-headers was set.
	Headers http.Header
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler.
func (r requestFields) MarshalZerologObject(e *zerolog.Event) {
	if r.ID != "" {
		e.Str("id", r.ID)
	}
	e.Str("method", r.Method)
	e.Str("scheme", r.Scheme)
	e.Str("host", r.Host)
	e.Str("path", r.Path)
	if r.Query != "" {
		e.Str("query", r.Query)
	}
	if !r.Incoming.Empty() {
		e.Object("incoming", r.Incoming)
	}
	if r.Conn != nil {
		e.Object("conn", *r.Conn)
	}
	if r.UserAgent != "" {
		e.Str("user_agent", r.UserAgent)
	}
	if !r.Source.Empty() {
		e.Object("source", r.Source)
	}
	e.Object("auth", r.Auth)
	if r.Headers != nil {
		e.Interface("headers", r.Headers)
	}
}

// responseFields renders the "response" object of a log line.
type responseFields struct {
	// Status is the status code ultimately returned for the request (e.g.
	// after a cached body/status is substituted for a 304 Not Modified
	// response).
	Status int
	// Cache holds the parsed "Cache-Status" header (RFC 9211), nil if the
	// header wasn't present (caching wasn't in play for this response).
	Cache       *cacheFields
	Size        int64
	RequestID   string
	MediaType   string
	ContentType string
	RateLimit   *rateLimitFields
	// Headers holds the full (sanitized) response headers, non-nil only if
	// --log-response-headers was set.
	Headers http.Header
}

// MarshalZerologObject implements zerolog.LogObjectMarshaler.
func (r responseFields) MarshalZerologObject(e *zerolog.Event) {
	e.Int("status", r.Status)
	if r.Cache != nil {
		e.Object("cache", *r.Cache)
	}
	if r.Size > 0 {
		e.Int64("size", r.Size)
	}
	if r.RequestID != "" {
		e.Str("request_id", r.RequestID)
	}
	if r.MediaType != "" {
		e.Str("media_type", r.MediaType)
	}
	if r.ContentType != "" {
		e.Str("content_type", r.ContentType)
	}
	if r.RateLimit != nil {
		e.Object("ratelimit", *r.RateLimit)
	}
	if r.Headers != nil {
		e.Interface("headers", r.Headers)
	}
}

// LoggingTransport logs each request/response and records latency metrics.
type LoggingTransport struct {
	Base http.RoundTripper
	// LogRateLimit controls whether requests to /rate_limit are logged;
	// they are typically noisy since they're issued on a fixed interval
	// to poll rate limit status rather than in response to real traffic.
	LogRateLimit bool
	// LocalIP is the source IP this transport dials from, logged as the
	// "source" object in the "request" object when non-empty (i.e. when
	// --src-ip was used).
	LocalIP string
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

	// Add the request details.
	reqFields := requestFields{
		ID:        RequestIDFromContext(req.Context()),
		Method:    req.Method,
		Scheme:    req.URL.Scheme,
		Host:      req.URL.Host,
		Path:      req.URL.Path,
		Query:     sanitizeQuery(req.URL.RawQuery),
		UserAgent: req.Header.Get("User-Agent"),
		Auth:      AuthFieldsFromContext(req.Context()),
	}
	if t.LogAddr {
		incomingIP, incomingPort := splitHostPort(req.RemoteAddr)
		reqFields.Incoming = addrFields{IP: incomingIP, Port: incomingPort}
		reqFields.Source = addrFields{IP: t.LocalIP}
	}
	if t.LogConn && gotConn != nil {
		connInfo := connFields{
			ID:       ConnID(gotConn.Conn),
			Reused:   gotConn.Reused,
			WasIdle:  gotConn.WasIdle,
			IdleTime: gotConn.IdleTime,
		}
		if gotConn.Conn != nil {
			connInfo.Remote.IP, connInfo.Remote.Port = splitHostPort(gotConn.Conn.RemoteAddr().String())
		}
		reqFields.Conn = &connInfo
	}
	if t.LogRequestHeaders {
		reqFields.Headers = sanitizeHeaders(req.Header)
	}
	evt = evt.Object("request", reqFields)

	// If the response is not nil, add the response details.
	if resp != nil {
		fields := responseFields{
			Status:      resp.StatusCode,
			Size:        resp.ContentLength,
			RequestID:   resp.Header.Get("X-Github-Request-Id"),
			MediaType:   resp.Header.Get("X-Github-Media-Type"),
			ContentType: resp.Header.Get("Content-Type"),
		}
		if cache, ok := parseCacheStatus(resp.Header.Get("Cache-Status")); ok {
			cache.ETag = resp.Header.Get("Etag")
			fields.Cache = &cache
		}
		if rate, err := ghratelimit.ParseRate(resp.Header); err == nil {
			fields.RateLimit = &rateLimitFields{
				Rate:     rate,
				Resource: resp.Header.Get("X-Ratelimit-Resource"),
			}
		}
		if t.LogResponseHeaders {
			fields.Headers = sanitizeHeaders(resp.Header)
		}
		evt = evt.Object("response", fields)
	}

	// Fire the log event.
	evt.Msg("HTTP request")

	return resp, err
}
