// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package preflight measures what only this machine can: whether the configured
// relay answers and whether outbound port 25 works from here.
//
// It only MEASURES. What a result means is the server's call, so the rules stay
// in one place instead of drifting across every installation. That boundary is
// why this is a package of its own.
package preflight

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
)

// An interface so the probe is testable without an SMTP server.
type Relay interface {
	Probe() error
}

func CheckRelay(relay Relay) (reachable bool, errText string) {
	if err := relay.Probe(); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// A hint, not a verdict: with the relay on another machine, this host's egress
// says nothing about whether mail can leave. An empty target means no probe was
// configured, and the answer is "skipped" — a probe that never ran must not read
// as one that failed.
func Port25(ctx context.Context, target string, timeout time.Duration) (status, errText string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return agent.Port25Skipped, ""
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		// A timeout is the fingerprint of a provider filtering port 25: packets
		// dropped, not refused. A refusal only means nothing is listening there,
		// which says nothing about egress.
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return agent.Port25Blocked, err.Error()
		}
		return agent.Port25Error, err.Error()
	}
	// The point is that the packets get out, not to hold a session open on
	// someone else's mail server.
	_ = conn.Close()
	return agent.Port25OK, ""
}

// Decides how much the port 25 result counts: with a local relay this host's
// egress IS the mail path, with a remote one it is irrelevant.
func RelayIsLocal(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.HasSuffix(host, ".localhost")
}
