package network

import (
	"errors"
	"testing"
)

func TestClientGateConnCapRejectsExcess(t *testing.T) {
	g := newClientGate()
	ip := "10.0.0.1"

	for i := 0; i < maxClientConnsPerIP; i++ {
		if err := g.admitConn(ip); err != nil {
			t.Fatalf("admit %d: unexpected error: %v", i, err)
		}
	}

	err := g.admitConn(ip)
	if err == nil {
		t.Fatal("expected the connection cap to reject the excess connection")
	}

	var limitErr *ResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *ResourceLimitError, got %T", err)
	}

	if limitErr.Resource != "connections-per-ip" {
		t.Fatalf("unexpected resource: %q", limitErr.Resource)
	}

	// Releasing a slot must let a new connection in again.
	g.releaseConn(ip)
	if err := g.admitConn(ip); err != nil {
		t.Fatalf("admit after release: %v", err)
	}

	// A different IP has its own budget.
	if err := g.admitConn("10.0.0.2"); err != nil {
		t.Fatalf("admit other ip: %v", err)
	}
}

func TestClientGateRateLimiterThrottles(t *testing.T) {
	g := newClientGate()
	ip := "10.0.0.3"

	// The burst is consumed without throttling.
	for i := 0; i < clientRateBurst; i++ {
		if err := g.admitStream(ip, 0); err != nil {
			t.Fatalf("burst request %d throttled early: %v", i, err)
		}
		g.releaseStream(ip, 0)
	}

	// The next request, with no time elapsed to refill, must be throttled.
	err := g.admitStream(ip, 0)
	if err == nil {
		t.Fatal("expected the rate limiter to throttle after the burst")
	}

	var limitErr *ResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "rate-limit-per-ip" {
		t.Fatalf("expected rate-limit-per-ip error, got %v", err)
	}
}

func TestClientGatePendingBytesCap(t *testing.T) {
	g := newClientGate()
	ip := "10.0.0.4"

	if err := g.admitStream(ip, maxClientPendingBytesPerIP); err != nil {
		t.Fatalf("admit at cap: %v", err)
	}

	err := g.admitStream(ip, 1)
	if err == nil {
		t.Fatal("expected the pending-bytes cap to reject the excess request")
	}

	var limitErr *ResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "pending-bytes-per-ip" {
		t.Fatalf("expected pending-bytes-per-ip error, got %v", err)
	}
}

func TestClientGateStreamCap(t *testing.T) {
	g := newClientGate()
	ip := "10.0.0.5"

	// Raise the rate burst out of the way by admitting up to the stream cap;
	// each admit consumes one token, and the stream cap is below the burst.
	for i := 0; i < maxClientStreamsPerIP; i++ {
		if err := g.admitStream(ip, 0); err != nil {
			t.Fatalf("admit stream %d: %v", i, err)
		}
	}

	err := g.admitStream(ip, 0)
	if err == nil {
		t.Fatal("expected the stream cap to reject the excess stream")
	}

	var limitErr *ResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "streams-per-ip" {
		t.Fatalf("expected streams-per-ip error, got %v", err)
	}
}
