// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package policy keeps direct delivery within what receiving providers
// tolerate: how many connections to open, how many messages to put through
// one session, how fast to go, and how long to back off after a refusal.
//
// None of this applies in relay mode — the relay you nominate is doing the
// delivering and has its own idea of all four.
package policy

import (
	"strings"
	"sync"
	"time"
)

// Limits are the ceilings for one recipient domain.
type Limits struct {
	// Connections that may be open at once.
	MaxConns int
	// Messages one session may carry. Past the cap the connection is closed
	// and a new one opened; large providers cut long sessions themselves.
	MaxPerConn int
}

// Published or observed tolerances of the large providers.
//
// The table is in code rather than configuration on purpose: the values change
// rarely, and when they do they should travel with a release. A number typed
// wrong on one machine is a reputation problem that shows up days later.
var domainLimits = map[string]Limits{
	"gmail.com":      {MaxConns: 8, MaxPerConn: 100},
	"googlemail.com": {MaxConns: 8, MaxPerConn: 100},
	"outlook.com":    {MaxConns: 3, MaxPerConn: 30},
	"hotmail.com":    {MaxConns: 3, MaxPerConn: 30},
	"live.com":       {MaxConns: 3, MaxPerConn: 30},
	"msn.com":        {MaxConns: 3, MaxPerConn: 30},
	"yahoo.com":      {MaxConns: 4, MaxPerConn: 20},
	"yahoo.co.uk":    {MaxConns: 4, MaxPerConn: 20},
	"yandex.com":     {MaxConns: 4, MaxPerConn: 50},
	"yandex.ru":      {MaxConns: 4, MaxPerConn: 50},
	"icloud.com":     {MaxConns: 3, MaxPerConn: 20},
	"me.com":         {MaxConns: 3, MaxPerConn: 20},
	"protonmail.com": {MaxConns: 2, MaxPerConn: 20},
	"proton.me":      {MaxConns: 2, MaxPerConn: 20},
	"mail.ru":        {MaxConns: 3, MaxPerConn: 30},
	"gmx.de":         {MaxConns: 3, MaxPerConn: 30},
	"web.de":         {MaxConns: 3, MaxPerConn: 30},
}

// Conservative default for everything else. Most corporate mail servers are
// small, and aggressive concurrency earns an immediate temporary rejection.
var defaultLimits = Limits{MaxConns: 2, MaxPerConn: 25}

// LimitsFor returns the ceilings for a domain.
func LimitsFor(domain string) Limits {
	if limits, ok := domainLimits[strings.ToLower(domain)]; ok {
		return limits
	}
	return defaultLimits
}

// RecipientDomain is the lowercased domain part of an address.
func RecipientDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

// Throttle remembers which domains have been refusing us lately.
//
// Why it is needed: a 4xx means "not now". Carrying on at the same rate
// extends greylisting, and against a provider whose rate limit we just tripped
// it costs reputation. The penalty doubles with each consecutive refusal and
// the first success clears it.
type Throttle struct {
	mu    sync.Mutex
	state map[string]*throttleState
}

type throttleState struct {
	deferrals int
	until     time.Time
}

func NewThrottle() *Throttle {
	return &Throttle{state: map[string]*throttleState{}}
}

const (
	// The penalty starts here and doubles with each consecutive refusal.
	throttleBase = 15 * time.Second
	throttleMax  = 10 * time.Minute
	// How many consecutive refusals before a penalty applies. A single 4xx is
	// ordinary — a full mailbox, a moment of load. A run of them is not.
	throttleAfter = 3
)

// RemainingPenalty is how long the domain stays off limits; 0 means send now.
//
// The duration has to leave this process: when a job is handed back without
// saying when to return it, the queue offers it again on the very next claim
// and a release-claim loop forms. The penalty only means something if the
// central system knows about it too.
func (t *Throttle) RemainingPenalty(domain string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.state[domain]
	if !ok || !state.until.After(now) {
		return 0
	}
	return state.until.Sub(now)
}

// Deferred records a temporary rejection and extends the penalty if the run is
// long enough to look systematic.
func (t *Throttle) Deferred(domain string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.state[domain]
	if !ok {
		state = &throttleState{}
		t.state[domain] = state
	}
	state.deferrals++
	if state.deferrals < throttleAfter {
		return
	}

	steps := state.deferrals - throttleAfter
	if steps > 6 {
		steps = 6
	}
	penalty := throttleBase << steps
	if penalty > throttleMax {
		penalty = throttleMax
	}
	state.until = now.Add(penalty)
}

// Delivered clears the history for a domain.
func (t *Throttle) Delivered(domain string) {
	t.mu.Lock()
	delete(t.state, domain)
	t.mu.Unlock()
}
