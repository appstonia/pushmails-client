// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package mta

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Delivery is one message handed to a transport.
type Delivery struct {
	// Envelope sender (Return-Path). Asynchronous bounces go here, not to the
	// From header.
	EnvelopeFrom string
	To           string
	// Recipient domain, and the most messages one session may carry. Direct
	// delivery only; a relay ignores both.
	Domain     string
	MaxPerConn int
	Data       []byte
}

// Direct delivers straight to the recipient's mail server, with no relay in
// between.
//
// Why this is the default rather than handing off to a local Postfix: the ack
// contract is synchronous. When we report "sent" the central system takes it
// to mean the receiving server accepted the message. With a queue in between,
// "sent" would only mean "written to the local spool", and the real answer
// would arrive later as a bounce nobody is reading.
type Direct struct {
	resolver *Resolver
	pool     *Pool
	timeout  time.Duration
	// Address the outgoing connection binds to; nil lets the OS choose.
	localIP net.IP
	// Name given in EHLO. It should be the PTR of the outgoing address:
	// receiving servers compare the two and a mismatch costs reputation.
	ehlo string
}

func NewDirect(ehlo string, localIP net.IP, timeout time.Duration) *Direct {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Direct{
		resolver: NewResolver(0),
		pool:     NewPool(timeout),
		timeout:  timeout,
		localIP:  localIP,
		ehlo:     ehlo,
	}
}

// Deliver hands the message to the recipient's mail server.
//
// The MX list is walked in preference order. The only thing that stops the
// walk is a permanent rejection at the RECIPIENT stage: a 5.1.1 from one
// server is about the mailbox, and asking the next one produces the same
// answer. A permanent rejection during the session (a 550 on EHLO) does not
// stop it — that one is about our address, and another MX may well accept us.
func (d *Direct) Deliver(ctx context.Context, del Delivery) Outcome {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	hosts, err := d.resolver.Lookup(ctx, del.Domain)
	if err != nil {
		return Classify(err, StageResolve)
	}

	var last Outcome
	for _, host := range hosts {
		outcome := d.deliverTo(ctx, host, del)
		if outcome.Delivered {
			return outcome
		}
		if outcome.Permanent && outcome.Stage == StageRecipient {
			return outcome
		}
		last = outcome
		if ctx.Err() != nil {
			break
		}
	}

	if last.Err == nil {
		last.Err = fmt.Errorf("no mail server left to try for %s", del.Domain)
	}
	return last
}

// PrimaryMX is the domain's highest-priority mail server, used as the pacing
// key. It reads the resolver cache, so the Deliver that follows costs no extra
// DNS query.
func (d *Direct) PrimaryMX(ctx context.Context, domain string) (string, error) {
	hosts, err := d.resolver.Lookup(ctx, domain)
	if err != nil {
		return "", err
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("no mail server for %s", domain)
	}
	return hosts[0], nil
}

func (d *Direct) deliverTo(ctx context.Context, host string, del Delivery) Outcome {
	c, err := d.pool.get(ctx, d.localIP, host, d.ehlo)
	if err != nil {
		// Dial, greeting, EHLO and STARTTLS all happen in there, and all of
		// them are the session stage.
		return Classify(err, StageSession)
	}

	outcome := transact(c, d.timeout, del.EnvelopeFrom, del.To, del.Data)
	outcome.TLS = c.tls

	switch {
	case outcome.Delivered:
		c.sent++
		d.pool.put(c, del.MaxPerConn)
	case IsConnectionError(outcome.Err):
		// The session is spoiled; returning it to the pool would take the next
		// message down with it.
		c.close()
	case outcome.Stage == StageSession && outcome.Code >= 500:
		// The server refused us outright, so no further message will pass on
		// this session either.
		c.close()
	default:
		// A protocol-level rejection: the session itself is fine.
		c.sent++
		d.pool.put(c, del.MaxPerConn)
	}

	return outcome
}

// Sweep drops connections that have gone idle. Called between batches.
func (d *Direct) Sweep() { d.pool.Sweep() }

// Close releases every pooled connection.
func (d *Direct) Close() { d.pool.CloseIdle() }

// transact runs one SMTP transaction, tagging each step with its stage.
//
// Everything up to and including MAIL FROM is about our identity; from RCPT TO
// on it is about the recipient.
func transact(c *conn, timeout time.Duration, envelopeFrom, to string, data []byte) Outcome {
	// Every command has to finish inside this window; a stalled server must
	// not block a worker forever.
	_ = c.raw.SetDeadline(time.Now().Add(timeout))

	// The previous send may have been cut short; start clean.
	if err := c.client.Reset(); err != nil {
		return Classify(err, StageSession)
	}
	// "sender verify failed", "sender domain rejected" — us, not the recipient.
	if err := c.client.Mail(envelopeFrom); err != nil {
		return Classify(err, StageSession)
	}
	// From here on the answers are about the recipient's mailbox.
	if err := c.client.Rcpt(to); err != nil {
		return Classify(err, StageRecipient)
	}

	w, err := c.client.Data()
	if err != nil {
		return Classify(err, StageRecipient)
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return Classify(err, StageRecipient)
	}
	// Close ends DATA; the accept/reject answer arrives here.
	if err := w.Close(); err != nil {
		return Classify(err, StageRecipient)
	}

	return Outcome{Delivered: true}
}
