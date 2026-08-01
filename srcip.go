package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
)

// RoundRobinTransport balances requests across multiple http.RoundTrippers,
// selecting one round-robin for each request.
type RoundRobinTransport struct {
	Transports []http.RoundTripper
	next       uint64
}

func (t *RoundRobinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	idx := atomic.AddUint64(&t.next, 1) - 1
	return t.Transports[idx%uint64(len(t.Transports))].RoundTrip(req)
}

// RoundRobin returns transports[0] directly if it's the only one, otherwise
// a RoundRobinTransport balancing across all of them per-request.
func RoundRobin(transports []http.RoundTripper) http.RoundTripper {
	if len(transports) == 1 {
		return transports[0]
	}
	return &RoundRobinTransport{Transports: transports}
}

// srcIPContextKey is the context.Context key under which the source IP is stored.
type srcIPContextKey struct{}

// SrcIPFromContext returns the source IP previously stored in ctx by
// SrcIPTransport, or "" if none was stored (i.e. --src-ip wasn't used).
func SrcIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(srcIPContextKey{}).(string)
	return ip
}

// SrcIPTransport records which source IP a request was dialed from in its
// context, so LoggingTransport can log it without needing to know which
// source IP its own chain corresponds to.
type SrcIPTransport struct {
	IP   string
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *SrcIPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := context.WithValue(req.Context(), srcIPContextKey{}, t.IP)
	return t.Base.RoundTrip(req.WithContext(ctx))
}

// SrcIPTransports parses addrs and returns one dedicated http.Transport
// (and connection pool) per source IP, so each source IP gets its own set
// of connections rather than sharing/reusing dials. If addrs is empty,
// base is returned directly as the sole transport.
func SrcIPTransports(addrs []string, base http.RoundTripper) ([]http.RoundTripper, error) {
	if len(addrs) == 0 {
		return []http.RoundTripper{base}, nil
	}
	transports := make([]http.RoundTripper, len(addrs))
	for idx, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			return nil, fmt.Errorf("net.ParseIP failed: %q", addr)
		}
		dialer := &net.Dialer{
			LocalAddr: &net.TCPAddr{IP: ip, Port: 0},
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = dialer.DialContext
		transports[idx] = transport
	}
	return transports, nil
}
