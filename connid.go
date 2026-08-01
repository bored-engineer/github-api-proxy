package main

import (
	"context"
	"net"
	"net/http"

	"github.com/rs/xid"
)

// idConn wraps a net.Conn with a unique identifier, assigned once when
// dialed. Since http.Transport reuses the same net.Conn value across
// pooled requests, ConnID returns that same identifier for the lifetime of
// the connection, however many requests reuse it.
type idConn struct {
	net.Conn
	id string
}

// ConnID returns the identifier WithConnID's dialer assigned to conn, or ""
// if conn wasn't dialed through one.
func ConnID(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	c, _ := conn.(*idConn)
	if c == nil {
		return ""
	}
	return c.id
}

// WithConnID returns a shallow clone of base whose DialContext tags each
// newly dialed connection with a unique ID (an xid, same scheme as
// RequestIDTransport) retrievable via ConnID.
func WithConnID(base *http.Transport) *http.Transport {
	t := base.Clone()
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idConn{Conn: conn, id: xid.New().String()}, nil
	}
	return t
}
