// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package runner

import (
	"testing"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
	"github.com/appstonia/pushmails-client/internal/config"
)

// The cadence is the server's decision; these bounds only stop a bug on either
// side from turning every installation into a one-second polling loop.
func TestClampInterval(t *testing.T) {
	tests := []struct {
		in, want time.Duration
	}{
		{0, minPreflightInterval},
		{time.Second, minPreflightInterval},
		{5 * time.Minute, 5 * time.Minute},
		{24 * time.Hour, maxPreflightInterval},
		{-time.Second, minPreflightInterval},
	}
	for _, tt := range tests {
		if got := clampInterval(tt.in); got != tt.want {
			t.Errorf("clampInterval(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestBackoffDuration(t *testing.T) {
	if got := backoffDuration(1); got != 2*time.Second {
		t.Errorf("first backoff = %v", got)
	}
	if got := backoffDuration(100); got != 60*time.Second {
		t.Errorf("backoff is not capped: %v", got)
	}
}

// The batch is sized to the measured rate so it finishes inside the lease: an
// overrun batch is taken back by the server and sent twice.
func TestNextBatchSize(t *testing.T) {
	r := &Runner{cfg: &config.Config{BatchSize: 200}, maxBatch: 200, lease: time.Minute}

	// No measurement yet: claim the configured ceiling.
	if got := r.nextBatchSize(); got != 200 {
		t.Errorf("without a rate = %d, want 200", got)
	}

	// 2 msg/s over a 60s lease, aiming at half of it.
	r.rate = 2
	if got := r.nextBatchSize(); got != 60 {
		t.Errorf("with rate 2 = %d, want 60", got)
	}

	// A very slow relay still claims something, and never more than the ceiling.
	r.rate = 0.01
	if got := r.nextBatchSize(); got != minBatch {
		t.Errorf("slow relay = %d, want %d", got, minBatch)
	}
	r.rate = 1000
	if got := r.nextBatchSize(); got != 200 {
		t.Errorf("fast relay = %d, want 200 (the ceiling)", got)
	}
}

// Both ceilings apply: the server's limit protects the queue, the flag is what
// the operator asked for.
func TestApplyLimits(t *testing.T) {
	r := &Runner{cfg: &config.Config{BatchSize: 100}, maxBatch: 100}

	r.applyLimits(&agent.HelloResponse{MaxBatchSize: 500, LeaseSeconds: 120})
	if r.maxBatch != 100 {
		t.Errorf("a higher server limit raised the batch: %d", r.maxBatch)
	}
	if r.lease != 2*time.Minute {
		t.Errorf("lease = %v, want 2m", r.lease)
	}

	r.applyLimits(&agent.HelloResponse{MaxBatchSize: 25, LeaseSeconds: 0})
	if r.maxBatch != 25 {
		t.Errorf("a lower server limit was ignored: %d", r.maxBatch)
	}
	if r.lease != 2*time.Minute {
		t.Errorf("a missing lease overwrote the known one: %v", r.lease)
	}
}

// The message id has to travel inside the envelope address. A bounce that
// arrives hours after the receiving server said 250 carries nothing else that
// identifies the message it belongs to, so without this there is no way to
// suppress the address it came from.
func TestEnvelopeSender(t *testing.T) {
	job := agent.Job{
		MessageID: "0b6f2e5c-1f4a-4c3e-9d21-7b8f0a1c2d3e",
		FromEmail: "news@example.test",
	}

	got := envelopeSender(job, "bounce.example.test")
	want := "bounce+0b6f2e5c-1f4a-4c3e-9d21-7b8f0a1c2d3e@bounce.example.test"
	if got != want {
		t.Errorf("envelope = %q, want %q", got, want)
	}

	// Configuration refuses an empty bounce domain at startup, so this is the
	// belt-and-braces path: fall back to the From address rather than build a
	// syntactically broken envelope that every server would refuse.
	if got := envelopeSender(job, ""); got != "news@example.test" {
		t.Errorf("without a bounce domain the envelope was %q", got)
	}
	if got := envelopeSender(agent.Job{FromEmail: "news@example.test"}, "bounce.example.test"); got != "news@example.test" {
		t.Errorf("without a message id the envelope was %q", got)
	}
}
