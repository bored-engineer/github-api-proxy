package main

import (
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	ghtransport "github.com/bored-engineer/github-conditional-http-transport"
	ghratelimit "github.com/bored-engineer/github-rate-limit-http-transport"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type (
	ctxXID                struct{}
	ctxConnXID            struct{}
	ctxConnLocalAddr      struct{}
	ctxAuthClientID       struct{}
	ctxAuthInstallationID struct{}
)

// sanitizeHeaders returns a copy of headers with the Authorization value
// (if any) replaced by its hash, safe to log verbatim (matches the same
// hashed_token value already surfaced via the "auth" object).
func sanitizeHeaders(headers http.Header) http.Header {
	sanitized := headers.Clone()
	if authorization := sanitized.Get("Authorization"); authorization != "" {
		sanitized.Set("Authorization", ghtransport.HashToken(authorization))
	}
	return sanitized
}

// addAddrFields splits a "host:port" address and sets its "ip"/"port"
// fields directly on d, falling back to hostport unchanged as the ip (with
// no port) if it can't be split (e.g. it's already a bare host). Does
// nothing if hostport is empty (e.g. the address wasn't known).
func addAddrFields(d *zerolog.Event, hostport string) {
	if hostport == "" {
		return
	}
	ip, port, err := net.SplitHostPort(hostport)
	if err != nil {
		ip, port = hostport, ""
	}
	if ip != "" {
		d.Str("ip", ip)
	}
	if port != "" {
		d.Str("port", port)
	}
}

// logAddrDict builds an "ip"/"port" Dict for hostport, or nil if hostport
// is empty (e.g. the address wasn't known, so the field should be omitted
// entirely).
func logAddrDict(hostport string) *zerolog.Event {
	if hostport == "" {
		return nil
	}
	d := zerolog.Dict()
	addAddrFields(d, hostport)
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
	d := zerolog.Dict()
	if clientID := FromContext[string](req.Context(), ctxAuthClientID{}); clientID != "" {
		d.Str("client_id", clientID)
	}
	if installationID := FromContext[string](req.Context(), ctxAuthInstallationID{}); installationID != "" {
		d.Str("installation_id", installationID)
	}
	if authorization := req.Header.Get("Authorization"); authorization != "" {
		d.Str("hashed_token", ghtransport.HashToken(authorization))
	}
	return d
}

// logCacheStatus parses resp's "Cache-Status" header (RFC 9211), as set by
// github-conditional-http-transport on every response, into a Dict with
// etag/last_modified/hit/stored/reason. Returns nil if the header is empty,
// meaning caching wasn't in play for this response at all. Example header
// values: "github-conditional-http-transport; hit" or "...; fwd=uri-miss;
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
	if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" {
		if t, err := http.ParseTime(lastModified); err == nil {
			d.Time("last_modified", t)
		}
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

// logRateLimitDict parses resp's rate-limit headers into the "ratelimit"
// object of a log line. Returns nil if resp doesn't carry rate-limit
// headers (e.g. it's not a GitHub API response).
func logRateLimitDict(resp *http.Response) *zerolog.Event {
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
	// LogRequestLocalAddr controls whether the listener's own address
	// (the one the client connected to) is logged as
	// request.conn.local.
	LogRequestLocalAddr bool
	// LogRequestRemoteAddr controls whether the client's address (plus
	// the incoming connection's id) is logged as request.conn.remote.
	LogRequestRemoteAddr bool
	// LogResponseLocalAddr controls whether the address the proxy dialed
	// out from (e.g. matching a configured --src-ip) is logged as
	// response.conn.local.
	LogResponseLocalAddr bool
	// LogResponseRemoteAddr controls whether the upstream GitHub
	// server's address (plus the connection's id) is logged as
	// response.conn.remote.
	LogResponseRemoteAddr bool
	// LogResponseConnReuse controls whether connection reuse stats (reused,
	// was_idle, idle_time) are logged under response.conn.remote,
	// describing whether the upstream connection used for this request
	// was freshly dialed or reused from the pool.
	LogResponseConnReuse bool
	// LogRequestHeaders controls whether the full (sanitized) request
	// headers are logged in the "request" object.
	LogRequestHeaders bool
	// LogResponseHeaders controls whether the full (sanitized) response
	// headers are logged in the "response" object.
	LogResponseHeaders bool
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Trace the underlying connection so we can log its details (local
	// address, remote address, reuse, idle time) for this specific
	// attempt, regardless of whether a response (or only an error) comes
	// back.
	var gotConn *httptrace.GotConnInfo
	if t.LogResponseLocalAddr || t.LogResponseRemoteAddr || t.LogResponseConnReuse {
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
	if id := FromContext[xid.ID](req.Context(), ctxXID{}); !id.IsNil() {
		reqDict.Str("id", id.String())
	}
	reqDict.Str("method", req.Method)
	reqDict.Str("scheme", req.URL.Scheme)
	reqDict.Str("host", req.URL.Host)
	reqDict.Str("path", req.URL.Path)
	if req.URL.RawQuery != "" {
		reqDict.Str("query", req.URL.RawQuery)
	}
	if t.LogRequestLocalAddr || t.LogRequestRemoteAddr {
		conn := zerolog.Dict()
		if t.LogRequestLocalAddr {
			if local := logAddrDict(FromContext[string](req.Context(), ctxConnLocalAddr{})); local != nil {
				conn.Dict("local", local)
			}
		}
		if t.LogRequestRemoteAddr {
			if remote := logAddrDict(req.RemoteAddr); remote != nil {
				if id := FromContext[xid.ID](req.Context(), ctxConnXID{}); !id.IsNil() {
					remote.Str("id", id.String())
				}
				conn.Dict("remote", remote)
			}
		}
		reqDict.Dict("conn", conn)
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
		if gotConn != nil && gotConn.Conn != nil && (t.LogResponseLocalAddr || t.LogResponseRemoteAddr || t.LogResponseConnReuse) {
			conn := zerolog.Dict()
			if t.LogResponseLocalAddr {
				if local := logAddrDict(gotConn.Conn.LocalAddr().String()); local != nil {
					conn.Dict("local", local)
				}
			}
			if t.LogResponseRemoteAddr || t.LogResponseConnReuse {
				remote := zerolog.Dict()
				if t.LogResponseRemoteAddr {
					addAddrFields(remote, gotConn.Conn.RemoteAddr().String())
					if xc, ok := gotConn.Conn.(*xidConn); ok {
						remote.Str("id", xc.id.String())
					}
				}
				if t.LogResponseConnReuse {
					remote.Bool("reused", gotConn.Reused)
					remote.Bool("was_idle", gotConn.WasIdle)
					if gotConn.WasIdle {
						// IdleTime is only meaningful when WasIdle is true.
						remote.Dur("idle_time", gotConn.IdleTime)
					}
				}
				conn.Dict("remote", remote)
			}
			respDict.Dict("conn", conn)
		}
		if cache := logCacheStatus(resp); cache != nil {
			respDict.Dict("cache", cache)
		}
		if resp.ContentLength > 0 {
			respDict.Int64("content_length", resp.ContentLength)
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
		if rl := logRateLimitDict(resp); rl != nil {
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
