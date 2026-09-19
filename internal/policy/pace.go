// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package policy

import (
	"strings"
	"sync"
	"time"
)

// Pacing is the minimum gap between messages that share a sending domain and a
// receiving infrastructure.
//
// It differs from Throttle in direction: Throttle penalises AFTER a refusal,
// pacing spaces things out BEFORE one. Providers count rate against the sender
// identity (the DKIM domain), and emptying fifty messages into Gmail in a few
// seconds costs reputation without ever producing a 4xx to react to.
//
// In-process only. Two machines sending as the same domain go twice as fast;
// coordinating that centrally is a different problem.

// mxProviders reduces a primary MX host to a provider identity.
//
// Why the host name alone will not do: one infrastructure answers to many
// names. gmail.com points at gmail-smtp-in.l.google.com while a company on
// Workspace points at aspmx.l.google.com, and Google counts both against the
// same sender. Separate keys would multiply the gap by the number of corporate
// domains in the batch.
//
// Why not the public suffix list: this repository has no dependencies, and the
// list answers the wrong question anyway — mx.hosting.example may serve
// thousands of unrelated customers under one registrable domain. For
// infrastructure we do not recognise the host name itself is the key: at worst
// the grouping is incomplete, which is far better than tying two unrelated
// recipients together.
var mxProviders = []struct {
	suffix   string
	identity string
}{
	{"google.com", "google.com"},
	{"googlemail.com", "google.com"},
	// Covers hotmail/outlook.com and Office 365
	// (*.mail.protection.outlook.com) alike.
	{"outlook.com", "outlook.com"},
	{"hotmail.com", "outlook.com"},
	{"yahoodns.net", "yahoodns.net"},
	{"yandex.net", "yandex.net"},
	{"yandex.ru", "yandex.net"},
	{"icloud.com", "icloud.com"},
}

// mxIntervals is the gap per provider identity. Starting points: the large
// providers tolerate a high rate, while Microsoft and Yahoo rate-limit sudden
// bursts sooner.
var mxIntervals = map[string]time.Duration{
	"google.com":   250 * time.Millisecond,
	"yandex.net":   500 * time.Millisecond,
	"outlook.com":  time.Second,
	"yahoodns.net": time.Second,
	"icloud.com":   time.Second,
}

// Gap for infrastructure we do not recognise. Small corporate servers are the
// most sensitive of all: dozens of messages in a few seconds means greylisting
// or a rate limit.
const defaultPaceInterval = 2 * time.Second

// A key untouched for this long is forgotten. Comfortably longer than the
// largest interval — a forgotten key just means "you may send now".
const paceIdle = 10 * time.Minute

// MXIdentity maps a primary MX host to its pacing key.
func MXIdentity(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, p := range mxProviders {
		if host == p.suffix || strings.HasSuffix(host, "."+p.suffix) {
			return p.identity
		}
	}
	return host
}

// IntervalForMX is the gap for a provider identity.
func IntervalForMX(identity string) time.Duration {
	if interval, ok := mxIntervals[identity]; ok {
		return interval
	}
	return defaultPaceInterval
}

// Pacer spreads out the send moments for each (sending domain, MX identity)
// pair.
//
// It does not sleep, it RESERVES: the lock is held only for the arithmetic and
// the waiting is the caller's business. Parallel workers on the same key take
// consecutive slots instead of queueing on a mutex.
type Pacer struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func NewPacer() *Pacer {
	return &Pacer{last: map[string]time.Time{}}
}

// Reserve claims the next send moment for a key.
//
// When deadline is non-zero and the slot falls past it, nothing is reserved
// and ok is false: a slot that will never be used would push everything behind
// it further out. The moment is still returned, because the caller needs it to
// tell the central system when to offer the job again.
func (p *Pacer) Reserve(senderDomain, mx string, now, deadline time.Time) (time.Time, bool) {
	key := strings.ToLower(senderDomain) + "|" + mx
	interval := IntervalForMX(mx)

	p.mu.Lock()
	defer p.mu.Unlock()

	slot := now
	if last, ok := p.last[key]; ok {
		if next := last.Add(interval); next.After(slot) {
			slot = next
		}
	}
	if !deadline.IsZero() && slot.After(deadline) {
		return slot, false
	}
	p.last[key] = slot
	return slot, true
}

// Sweep forgets keys nobody has used for a while. A campaign touches thousands
// of domains; without this the map grows for as long as the process lives.
func (p *Pacer) Sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, last := range p.last {
		if now.Sub(last) > paceIdle {
			delete(p.last, key)
		}
	}
}
