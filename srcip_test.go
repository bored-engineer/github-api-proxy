package main

import (
	"errors"
	"net/http"
	"testing"
)

type stubTransport struct {
	id  int
	err error
}

func (t *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{StatusCode: t.id}, nil
}

func TestRoundRobinSingleTransport(t *testing.T) {
	transports := []http.RoundTripper{&stubTransport{id: 1}}
	got := RoundRobin(transports)
	if got != transports[0] {
		t.Fatalf("RoundRobin with a single transport should return it directly, got %v", got)
	}
}

func TestRoundRobinTransportCyclesInOrder(t *testing.T) {
	transports := []http.RoundTripper{
		&stubTransport{id: 1},
		&stubTransport{id: 2},
		&stubTransport{id: 3},
	}
	rt := RoundRobin(transports)

	want := []int{1, 2, 3, 1, 2, 3, 1}
	for i, id := range want {
		resp, err := rt.RoundTrip(nil)
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
		if resp.StatusCode != id {
			t.Fatalf("request %d: got transport %d, want %d", i, resp.StatusCode, id)
		}
	}
}

func TestRoundRobinTransportPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	rt := RoundRobin([]http.RoundTripper{&stubTransport{err: wantErr}})
	_, err := rt.RoundTrip(nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want %v", err, wantErr)
	}
}

func TestSrcIPTransportsEmptyReturnsBase(t *testing.T) {
	base := &stubTransport{id: 42}
	transports, err := SrcIPTransports(nil, base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transports) != 1 || transports[0] != base {
		t.Fatalf("got %v, want a single-element slice containing base", transports)
	}
}

func TestSrcIPTransportsInvalidIP(t *testing.T) {
	_, err := SrcIPTransports([]string{"not-an-ip"}, http.DefaultTransport)
	if err == nil {
		t.Fatal("expected an error for an invalid IP, got nil")
	}
}

func TestSrcIPTransportsValidIPs(t *testing.T) {
	addrs := []string{"203.0.113.10", "203.0.113.11"}
	transports, err := SrcIPTransports(addrs, http.DefaultTransport)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transports) != len(addrs) {
		t.Fatalf("got %d transports, want %d", len(transports), len(addrs))
	}
	for i, rt := range transports {
		tr, ok := rt.(*http.Transport)
		if !ok {
			t.Fatalf("transport %d: got %T, want *http.Transport", i, rt)
		}
		if tr.DialContext == nil {
			t.Fatalf("transport %d: DialContext was not set", i)
		}
	}
}
