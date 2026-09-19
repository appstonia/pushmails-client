// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package mta delivers a built message over SMTP, either straight to the
// recipient's mail server or through a relay you nominate.
//
// Both paths return the same Outcome and share one classifier. A second copy
// would eventually disagree with the first about what a given 550 means, and
// that disagreement is a suppressed subscriber: the central system turns a
// permanent rejection into a dead address and undoing it is manual.
package mta

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// Stage says where in the SMTP conversation a failure arrived.
//
// The same 550 means two different things depending on where it lands, and
// this distinction decides whether a subscriber is kept or lost.
type Stage int

const (
	// Dial, greeting, EHLO, STARTTLS, AUTH and MAIL FROM. A rejection here is
	// about US: the address is blocklisted, the PTR is missing, the relay
	// refuses our credentials or envelope sender. It says nothing about the
	// recipient's mailbox.
	StageSession Stage = iota
	// RCPT TO, DATA and the terminating dot. A rejection here is about the
	// recipient.
	StageRecipient
	// MX resolution. About the recipient's domain.
	StageResolve
)

func (s Stage) String() string {
	switch s {
	case StageRecipient:
		return "recipient"
	case StageResolve:
		return "resolve"
	default:
		return "session"
	}
}

// Outcome is the result of one delivery attempt.
type Outcome struct {
	// The server accepted the message.
	Delivered bool
	// The failure is permanent: the address is invalid and must not be
	// retried. Only ever true at the recipient or resolve stage — a 5xx during
	// the session NEVER sets this.
	Permanent bool
	Stage     Stage
	// The server's reply code; 0 when it never answered (network or DNS).
	Code int
	// TLS was negotiated on the connection.
	TLS bool
	Err error
}

// SessionRejected marks the case where the server refused us before it ever
// heard the recipient — the earliest sign of a blocklisting.
//
// Temporary as far as the message is concerned (nothing is wrong with the
// address), but worth measuring on its own: a run of these is what a fresh
// listing looks like from here.
func (o Outcome) SessionRejected() bool {
	return o.Stage == StageSession && o.Code >= 500 && o.Code < 600
}

func (o Outcome) Error() string {
	if o.Err == nil {
		return ""
	}
	return o.Err.Error()
}

// PermanentError says the address can never be delivered to, whatever the
// SMTP code says. Raised by MX resolution for NXDOMAIN and null MX.
type PermanentError struct {
	Message string
}

func (e *PermanentError) Error() string { return e.Message }

// Classify splits permanent failures from temporary ones and reads the SMTP
// code out of the error.
//
// 4xx is temporary everywhere (mailbox full, greylisting, rate limit). So are
// network errors, timeouts and TLS problems: nothing permanent was said to us.
//
// 5xx depends on the stage, and that is the whole point:
//
//   - Recipient or resolve stage: about the recipient → bounced.
//   - Session stage (EHLO/AUTH/MAIL FROM): about US → failed, and the next MX
//     is tried. Counting it as a bounce would permanently suppress thousands
//     of good addresses the day our address is listed or the relay starts
//     refusing us.
func Classify(err error, stage Stage) Outcome {
	if err == nil {
		return Outcome{Delivered: true}
	}

	var permanent *PermanentError
	if errors.As(err, &permanent) {
		return Outcome{Permanent: true, Stage: stage, Err: err}
	}

	if code, ok := parseSMTPCode(err.Error()); ok {
		return Outcome{
			Permanent: code >= 500 && code < 600 && stage != StageSession,
			Stage:     stage,
			Code:      code,
			Err:       err,
		}
	}

	return Outcome{Stage: stage, Err: err}
}

// parseSMTPCode reads the code out of "550 5.1.1 User unknown". net/smtp
// returns a textproto.Error, but the type is lost once wrapped; reading the
// text works either way.
func parseSMTPCode(msg string) (int, bool) {
	msg = strings.TrimSpace(msg)
	// In a wrapped error the code sits mid-sentence: "... rejected: 550 ...".
	if idx := strings.LastIndex(msg, ": "); idx >= 0 && idx+2 < len(msg) {
		if code, ok := parseLeadingCode(msg[idx+2:]); ok {
			return code, true
		}
	}
	return parseLeadingCode(msg)
}

func parseLeadingCode(msg string) (int, bool) {
	msg = strings.TrimSpace(msg)
	if len(msg) < 3 {
		return 0, false
	}
	code, err := strconv.Atoi(msg[:3])
	if err != nil || code < 200 || code > 599 {
		return 0, false
	}
	return code, true
}

// IsConnectionError says the session is unusable: the pooled connection is
// dropped and, in direct mode, the next MX is tried.
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "EOF")
}
