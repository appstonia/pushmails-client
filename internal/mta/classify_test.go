// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package mta

import (
	"errors"
	"testing"
)

// The permanent/temporary split is critical to the protocol: treating 5xx as
// temporary means retrying dead addresses forever, while treating 4xx as
// permanent loses a subscriber over a passing problem.
//
// The stage matters just as much. A 5xx at the recipient stage is about the
// mailbox; the same code at the session stage is about US — the receiving
// server or the relay refusing our address, our envelope sender or our
// credentials. Suppressing recipients over that would wipe out healthy
// addresses the day we are blocklisted.
func TestClassify(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		stage         Stage
		wantPermanent bool
		wantCode      int
	}{
		{"550 no such mailbox", errors.New("550 5.1.1 User unknown"), StageRecipient, true, 550},
		{"553 invalid recipient", errors.New("553 Invalid recipient"), StageRecipient, true, 553},
		{"554 transaction failed", errors.New("554 Transaction failed"), StageRecipient, true, 554},
		{"421 service unavailable", errors.New("421 Service not available"), StageRecipient, false, 421},
		{"450 mailbox busy", errors.New("450 Mailbox busy"), StageRecipient, false, 450},
		{"452 out of storage", errors.New("452 Insufficient storage"), StageRecipient, false, 452},
		{"550 rejected sender", errors.New("550 5.7.1 Sender rejected"), StageSession, false, 550},
		{"535 auth failed", errors.New("535 Authentication failed"), StageSession, false, 535},
		{"network error", errors.New("dial tcp: connection refused"), StageSession, false, 0},
		{"timeout", errors.New("i/o timeout"), StageRecipient, false, 0},
		{"message without a code", errors.New("something went wrong"), StageRecipient, false, 0},
		{"domain does not exist", &PermanentError{Message: "domain does not exist"}, StageResolve, true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome := Classify(tt.err, tt.stage)
			if outcome.Delivered {
				t.Fatal("an error was counted as delivered")
			}
			if outcome.Permanent != tt.wantPermanent {
				t.Errorf("Permanent = %v, want %v (%q)",
					outcome.Permanent, tt.wantPermanent, tt.err)
			}
			if outcome.Code != tt.wantCode {
				t.Errorf("Code = %d, want %d (%q)", outcome.Code, tt.wantCode, tt.err)
			}
			if outcome.Stage != tt.stage {
				t.Errorf("Stage = %v, want %v", outcome.Stage, tt.stage)
			}
		})
	}
}

// A 5xx during the session is the shape a blocklisting takes: the server turns
// us away before it ever hears the recipient. It must never be permanent, and
// it has to be countable on its own.
func TestSessionRejected(t *testing.T) {
	rejected := Classify(errors.New("550 5.7.1 Service unavailable, client host blocked"), StageSession)
	if rejected.Permanent {
		t.Error("a session-stage 5xx was marked permanent")
	}
	if !rejected.SessionRejected() {
		t.Error("a session-stage 5xx was not counted as a session rejection")
	}

	recipient := Classify(errors.New("550 5.1.1 User unknown"), StageRecipient)
	if recipient.SessionRejected() {
		t.Error("a recipient-stage rejection was counted as a session rejection")
	}
	if !recipient.Permanent {
		t.Error("a recipient-stage 5xx was not marked permanent")
	}
}

func TestClassifyNil(t *testing.T) {
	if outcome := Classify(nil, StageRecipient); !outcome.Delivered {
		t.Error("Classify(nil) did not report a delivery")
	}
}

func TestParseSMTPCode(t *testing.T) {
	tests := []struct {
		in       string
		wantCode int
		wantOK   bool
	}{
		{"550 5.1.1 User unknown", 550, true},
		{"250 OK", 250, true},
		{"  421 trimmed", 421, true},
		// Wrapped errors keep the code mid-sentence.
		{"could not open the SMTP session: 554 Rejected", 554, true},
		{"999 out of range", 0, false},
		{"100 too low", 0, false},
		{"no code here", 0, false},
		{"", 0, false},
		{"55", 0, false},
	}
	for _, tt := range tests {
		code, ok := parseSMTPCode(tt.in)
		if code != tt.wantCode || ok != tt.wantOK {
			t.Errorf("parseSMTPCode(%q) = (%d, %v), want (%d, %v)",
				tt.in, code, ok, tt.wantCode, tt.wantOK)
		}
	}
}

func TestIsConnectionError(t *testing.T) {
	connErrors := []string{
		"write: broken pipe",
		"read: connection reset by peer",
		"use of closed network connection",
		"unexpected EOF",
	}
	for _, msg := range connErrors {
		if !IsConnectionError(errors.New(msg)) {
			t.Errorf("IsConnectionError(%q) = false, should count as a connection error", msg)
		}
	}

	if IsConnectionError(nil) {
		t.Error("nil counted as a connection error")
	}
	if IsConnectionError(errors.New("550 User unknown")) {
		t.Error("an SMTP rejection counted as a connection error")
	}
	// The wrapper must not hide the underlying connection failure: the relay
	// decides whether to reconnect based on this answer, and direct delivery
	// decides whether to keep the pooled session.
	wrapped := Classify(errors.New("write: broken pipe"), StageRecipient)
	if !IsConnectionError(wrapped.Err) {
		t.Error("a connection error survived classification unrecognised")
	}
}
