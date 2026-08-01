package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/rs/xid"
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

type xidConn struct {
	net.Conn
	id xid.ID
}

// SrcIPTransports parses addrs and returns one dedicated http.Transport
// (and connection pool) per source IP, so each source IP gets its own set
// of connections rather than sharing/reusing dials. If addrs is empty,
// the default LocalAddr is used.
func SrcIPTransports(addrs []string, base http.RoundTripper) ([]http.RoundTripper, error) {
	if len(addrs) == 0 {
		addrs = []string{""} // No source IP, use default dialer LocalAddr.
	}
	transports := make([]http.RoundTripper, len(addrs))
	for idx, addr := range addrs {
		dialer := new(net.Dialer)
		ip := net.ParseIP(addr)
		if ip != nil {
			dialer.LocalAddr = &net.TCPAddr{IP: ip, Port: 0}
		} else if addr != "" {
			return nil, fmt.Errorf("net.ParseIP failed: %q", addr)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if conn != nil {
				conn = &xidConn{Conn: conn, id: xid.New()}
			}
			return conn, err
		}
		transports[idx] = transport
	}
	return transports, nil
}
