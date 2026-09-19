// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package agent is the protocol client that talks to the central PushMails API.
package agent

// This file mirrors the server's contract. Field names must match its `json`
// tags exactly; a field changes on both sides or on neither.

type HelloRequest struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
}

type HelloResponse struct {
	NodeID           string `json:"node_id"`
	Name             string `json:"name"`
	Mode             string `json:"mode"`
	RateLimitPerHour int32  `json:"rate_limit_per_hour"`
	MaxBatchSize     int    `json:"max_batch_size"`
	// A claimed batch must be acknowledged within this window.
	LeaseSeconds int `json:"lease_seconds"`
	// How long the server holds a claim call open.
	PollWaitSeconds int `json:"poll_wait_seconds"`
	// Sending addresses registered for this node. Usually empty here: your own
	// MTA picks the outgoing address, not this process. Mirrored anyway, so a
	// registered address your relay never uses can be spotted in the log.
	IPs []IP `json:"ips"`
}

type IP struct {
	IP string `json:"ip"`
	// Reverse DNS name; may be empty.
	PTR string `json:"ptr"`
	// true when the address is reserved for a single account.
	Dedicated bool `json:"dedicated"`
	// "warmup" | "active" — paused addresses are never returned.
	Status string `json:"status"`
	// Warm-up ceiling for the day; 0 = no limit.
	DailyLimit int `json:"daily_limit"`
	// Blocklist status measured centrally: "clean" | "listed" | "unknown".
	// Informational — dropping an address is the server's call, not ours.
	Reputation string `json:"reputation"`
	// Zones that list the address, for diagnosis.
	ListedZones []string `json:"listed_zones"`
}

const (
	ReputationClean   = "clean"
	ReputationListed  = "listed"
	ReputationUnknown = "unknown"
)

type ClaimRequest struct {
	BatchSize int `json:"batch_size"`
}

// Merge fields are already filled in centrally; the client does no template
// processing.
type Job struct {
	MessageID  string `json:"message_id"`
	CampaignID string `json:"campaign_id"`
	To         string `json:"to"`
	// Outgoing address the server picked. Empty for self-hosted sending: this
	// client binds whatever its own configuration names, so there is nothing
	// here to honour.
	SourceIP  *string `json:"source_ip"`
	FromName  string  `json:"from_name"`
	FromEmail string  `json:"from_email"`
	ReplyTo   *string `json:"reply_to"`
	Subject   string  `json:"subject"`
	Preheader *string `json:"preheader"`
	HTML      string  `json:"html"`
	// Goes into the List-Unsubscribe header. The server supplies it because
	// scraping the HTML misses links left as plain text, and the header would
	// then silently disappear.
	UnsubscribeURL string `json:"unsubscribe_url"`
}

type ClaimResponse struct {
	Jobs []Job `json:"jobs"`
	// Sending allowance left for the current hour.
	RemainingQuota int64 `json:"remaining_quota"`
	// Wait suggested by the server when the response is empty.
	RetryAfterSeconds int `json:"retry_after_seconds"`
	// Set when the server refuses to hand out work because this node is not in
	// a state to send; the reason is a machine-readable finding code.
	//
	// This is the gate that cannot be skipped: when preflight is unreachable
	// the client keeps sending, so claim has to carry the same verdict.
	Blocked       bool    `json:"blocked"`
	BlockedReason *string `json:"blocked_reason"`
}

// Result statuses:
//
//   - sent      delivered.
//   - failed    temporary; the server requeues until the attempt budget runs out.
//   - bounced   permanent; never retried.
//   - released  claimed but never attempted. Not a failed attempt, so reporting
//     it as failed would burn one of the message's attempts for nothing.
const (
	StatusSent     = "sent"
	StatusFailed   = "failed"
	StatusBounced  = "bounced"
	StatusReleased = "released"
)

type Result struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	// SMTP reply code; 0 = none (connection failed or timed out). Kept apart
	// from the error text because the server aggregates on the code, while the
	// wording differs from provider to provider.
	SMTPCode int `json:"smtp_code,omitempty"`
	// Suggested wait for "released" results. Without it the job comes straight
	// back on the next claim and a release → claim hot loop forms.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
	// Address the message actually left from, when the sender binds one.
	// Always empty here: this client sends from the machine it runs on and
	// does not choose between addresses. Part of the shared protocol, so the
	// field exists either way.
	SourceIP string `json:"source_ip,omitempty"`
}

type AckRequest struct {
	Results []Result `json:"results"`
}

type AckResponse struct {
	// How many results the server applied. Fewer than sent means some leases
	// had already expired and those jobs went to another node.
	Accepted int `json:"accepted"`
}

// DKIMKey is one sending domain's signing key.
type DKIMKey struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
	// PEM-encoded private key. Held in memory, never written to disk.
	PrivateKey string `json:"private_key"`
}

type DKIMKeysResponse struct {
	Keys []DKIMKey `json:"keys"`
	// How long the keys may be cached before asking again. A domain verified
	// in the panel then starts being signed without a restart.
	TTLSeconds int `json:"ttl_seconds"`
}

type APIError struct {
	Code       string `json:"error"`
	Message    string `json:"message"`
	StatusCode int    `json:"-"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// A rejected token will not fix itself; 5xx and network errors may.
func (e *APIError) Retryable() bool {
	return e.StatusCode == 0 || e.StatusCode >= 500 || e.StatusCode == 429
}

// ---------------------------------------------------------------- preflight

// PreflightRequest reports what only this process can measure: whether its own
// relay answers, whether outbound port 25 works from this host, and which DKIM
// identity it signs with. The server already knows everything else.
type PreflightRequest struct {
	Version  string          `json:"version"`
	Hostname string          `json:"hostname"`
	Probes   PreflightProbes `json:"probes"`
	DKIM     PreflightDKIM   `json:"dkim"`
	// Domain the envelope sender is built on. Reported so the server can say
	// whether bounces sent there actually reach it: the record it verifies and
	// the value configured here are set in two different places, and when they
	// drift the mail still goes out while the bounces quietly do not come
	// back.
	BounceDomain string `json:"bounce_domain"`
}

type PreflightProbes struct {
	// Pointer because "not reported" and "unreachable" are different answers,
	// and the server must not read silence as a failure.
	RelayReachable *bool  `json:"relay_reachable"`
	RelayError     string `json:"relay_error,omitempty"`
	// Whether the relay runs on this machine. If it does not, this host's own
	// port 25 says nothing about whether mail can leave.
	RelayLocal bool `json:"relay_local"`
	// "ok" | "blocked" | "error" | "skipped"
	Port25Egress string `json:"port25_egress"`
	Port25Target string `json:"port25_target,omitempty"`
	Port25Error  string `json:"port25_error,omitempty"`
}

type PreflightDKIM struct {
	Enabled  bool   `json:"enabled"`
	Domain   string `json:"domain,omitempty"`
	Selector string `json:"selector,omitempty"`
}

// Port 25 probe outcomes. "skipped" is a real value: no target means no probe,
// and a probe that never ran must not read as one that failed.
const (
	Port25OK      = "ok"
	Port25Blocked = "blocked"
	Port25Error   = "error"
	Port25Skipped = "skipped"
)

// Finding is one reason the node is blocked, or one thing worth fixing.
type Finding struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Measured centrally, not here.
type IPReputation struct {
	IP          string   `json:"ip"`
	Status      string   `json:"status"`
	ListedZones []string `json:"listed_zones"`
	CheckedAt   *string  `json:"checked_at"`
}

// Port25Probe says what to dial and how often. An empty target means "do not
// probe" — the address is an operator's choice, never a hardcoded one.
type Port25Probe struct {
	Target          string `json:"target"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// PreflightResponse is the server's verdict. This client reports facts and
// obeys; keeping the policy in one place is what stops the rules from drifting
// between the panel, the central pool and every customer's installation.
type PreflightResponse struct {
	CanSend             bool           `json:"can_send"`
	CheckedAt           string         `json:"checked_at"`
	RecheckAfterSeconds int            `json:"recheck_after_seconds"`
	Blockers            []Finding      `json:"blockers"`
	Warnings            []Finding      `json:"warnings"`
	IPs                 []IPReputation `json:"ips"`
	Port25Probe         Port25Probe    `json:"port25_probe"`
}
