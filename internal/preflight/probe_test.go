// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package preflight

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
)

type stubRelay struct{ err error }

func (s stubRelay) Probe() error { return s.err }

func TestCheckRelay(t *testing.T) {
	if ok, msg := CheckRelay(stubRelay{}); !ok || msg != "" {
		t.Errorf("healthy relay reported as (%v, %q)", ok, msg)
	}
	ok, msg := CheckRelay(stubRelay{err: errors.New("connection refused")})
	if ok || msg == "" {
		t.Errorf("unreachable relay reported as (%v, %q)", ok, msg)
	}
}

// A probe that never ran must not read as one that failed: with no target
// configured every node would otherwise show a permanent warning.
func TestPort25SkippedWithoutTarget(t *testing.T) {
	status, errText := Port25(context.Background(), "  ", time.Second)
	if status != agent.Port25Skipped || errText != "" {
		t.Errorf("Port25 without a target = (%q, %q)", status, errText)
	}
}

func TestPort25Reachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	status, errText := Port25(context.Background(), listener.Addr().String(), time.Second)
	if status != agent.Port25OK {
		t.Errorf("Port25 = (%q, %q), want ok", status, errText)
	}
}

// A refusal means the host answered — nothing was filtered, so this is an
// error, not evidence that egress is blocked. Reporting it as "blocked" would
// pause a perfectly healthy customer.
func TestPort25RefusedIsErrorNotBlocked(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close() // nothing is listening now

	status, _ := Port25(context.Background(), address, time.Second)
	if status != agent.Port25Error {
		t.Errorf("Port25 against a closed port = %q, want error", status)
	}
}

func TestRelayIsLocal(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"mail.example.com", false},
		{"203.0.113.10", false},
	}
	for _, tt := range tests {
		if got := RelayIsLocal(tt.host); got != tt.want {
			t.Errorf("RelayIsLocal(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
