// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package policy

import (
	"testing"
	"time"
)

// An unknown domain has to get the conservative ceiling. Most corporate mail
// servers are small, and the cost of guessing high is a temporary rejection
// for every message in the batch.
func TestLimitsFor(t *testing.T) {
	if got := LimitsFor("gmail.com"); got.MaxConns != 8 {
		t.Errorf("gmail MaxConns = %d, want 8", got.MaxConns)
	}
	if got := LimitsFor("GMAIL.COM"); got.MaxConns != 8 {
		t.Error("the domain lookup is case sensitive")
	}
	got := LimitsFor("mail.example.test")
	if got != defaultLimits {
		t.Errorf("unknown domain got %+v, want the conservative default %+v", got, defaultLimits)
	}
}

func TestRecipientDomain(t *testing.T) {
	tests := map[string]string{
		"person@example.com":     "example.com",
		"Person@Example.COM":     "example.com",
		"  person@example.com  ": "example.com",
		// A plus tag has an @ of its own nowhere, but the last @ is still the
		// separator for quoted local parts.
		"odd@name@example.com": "example.com",
		"no-at-sign":           "",
	}
	for in, want := range tests {
		if got := RecipientDomain(in); got != want {
			t.Errorf("RecipientDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// One temporary rejection is ordinary; a run of them is not. Penalising the
// first would slow down every campaign that hits a full mailbox.
func TestThrottleIgnoresIsolatedDeferrals(t *testing.T) {
	th := NewThrottle()
	now := time.Now()

	for range throttleAfter - 1 {
		th.Deferred("example.com", now)
	}
	if penalty := th.RemainingPenalty("example.com", now); penalty != 0 {
		t.Fatalf("penalty %v applied before %d consecutive deferrals", penalty, throttleAfter)
	}

	th.Deferred("example.com", now)
	if penalty := th.RemainingPenalty("example.com", now); penalty <= 0 {
		t.Fatal("no penalty after a run of deferrals")
	}
}

// The penalty grows with the run and is capped, so a domain that is down for
// an hour does not get an unbounded hold-off.
func TestThrottleBacksOffAndCaps(t *testing.T) {
	th := NewThrottle()
	now := time.Now()

	var previous time.Duration
	for i := range 20 {
		th.Deferred("example.com", now)
		penalty := th.RemainingPenalty("example.com", now)
		if i >= throttleAfter && penalty < previous {
			t.Fatalf("penalty shrank as the run grew: %v then %v", previous, penalty)
		}
		if penalty > throttleMax {
			t.Fatalf("penalty %v exceeded the cap %v", penalty, throttleMax)
		}
		previous = penalty
	}
}

// A success clears the history: a domain that recovered must not stay
// penalised for the rest of the run.
func TestThrottleClearedByDelivery(t *testing.T) {
	th := NewThrottle()
	now := time.Now()

	for range throttleAfter + 3 {
		th.Deferred("example.com", now)
	}
	th.Delivered("example.com")
	if penalty := th.RemainingPenalty("example.com", now); penalty != 0 {
		t.Errorf("penalty %v survived a successful delivery", penalty)
	}
}

// Different infrastructures answer to different names; grouping them by
// provider is what keeps the gap meaningful for a corporate domain hosted at a
// large provider.
func TestMXIdentity(t *testing.T) {
	tests := map[string]string{
		"gmail-smtp-in.l.google.com":              "google.com",
		"aspmx.l.google.com.":                     "google.com",
		"example-com.mail.protection.outlook.com": "outlook.com",
		"mta5.am0.yahoodns.net":                   "yahoodns.net",
		"mx.unknown-host.test":                    "mx.unknown-host.test",
	}
	for host, want := range tests {
		if got := MXIdentity(host); got != want {
			t.Errorf("MXIdentity(%q) = %q, want %q", host, got, want)
		}
	}
}

// Consecutive messages on one key take consecutive slots, so a burst is spread
// out instead of arriving all at once.
func TestPacerSpacesSlots(t *testing.T) {
	p := NewPacer()
	now := time.Now()
	interval := IntervalForMX("google.com")

	first, ok := p.Reserve("example.com", "google.com", now, time.Time{})
	if !ok {
		t.Fatal("the first slot was refused")
	}
	second, ok := p.Reserve("example.com", "google.com", now, time.Time{})
	if !ok {
		t.Fatal("the second slot was refused")
	}
	if gap := second.Sub(first); gap < interval {
		t.Errorf("slots %v apart, want at least %v", gap, interval)
	}
}

// A slot past the lease is not reserved: it would never be used, and holding
// it would push every later message further out. The moment still comes back,
// because the hand-back has to tell the server when to offer the job again.
func TestPacerRefusesSlotPastDeadline(t *testing.T) {
	p := NewPacer()
	now := time.Now()
	deadline := now.Add(time.Millisecond)

	if _, ok := p.Reserve("example.com", "outlook.com", now, deadline); !ok {
		t.Fatal("the first slot was refused although it fits")
	}
	slot, ok := p.Reserve("example.com", "outlook.com", now, deadline)
	if ok {
		t.Fatal("a slot past the deadline was reserved")
	}
	if !slot.After(deadline) {
		t.Errorf("returned slot %v is not past the deadline %v", slot, deadline)
	}

	// The refused slot must not have been recorded, or it would push the next
	// caller out even though nothing was sent.
	if next, ok := p.Reserve("example.com", "outlook.com", now, time.Time{}); !ok ||
		next.After(slot) {
		t.Error("the refused slot was still recorded")
	}
}

// Separate sending domains do not share a gap: one campaign must not pace
// another one's mail.
func TestPacerKeysBySenderDomain(t *testing.T) {
	p := NewPacer()
	now := time.Now()

	first, _ := p.Reserve("one.example", "google.com", now, time.Time{})
	second, _ := p.Reserve("two.example", "google.com", now, time.Time{})
	if !first.Equal(second) {
		t.Error("two sending domains were paced against each other")
	}
}

func TestPacerSweep(t *testing.T) {
	p := NewPacer()
	now := time.Now()
	p.Reserve("example.com", "google.com", now, time.Time{})

	p.Sweep(now.Add(paceIdle + time.Minute))
	if len(p.last) != 0 {
		t.Errorf("%d keys survived the sweep", len(p.last))
	}
}
