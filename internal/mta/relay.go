// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package mta

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/appstonia/pushmails-client/internal/config"
)

// Relay hands every message to one SMTP server you nominate and lets it do the
// delivering.
//
// Use it when something else on your network is already the mail path: a
// hardened Postfix, a smarthost your provider requires, an appliance that has
// to see outgoing mail. The trade is that "sent" then means "the relay
// accepted it", not "the recipient's server accepted it" — whatever happens
// afterwards arrives as a bounce in the envelope sender's mailbox, and the
// central system never learns about it.
type Relay struct {
	cfg config.SMTP

	// One connection, shared; concurrent workers take turns.
	mu     sync.Mutex
	client *smtp.Client
}

func NewRelay(cfg config.SMTP) *Relay {
	return &Relay{cfg: cfg}
}

// Deliver sends the message through the relay.
//
// The connection stays open between messages: reconnecting for every email
// adds up over thousands of them and pressures the relay.
func (r *Relay) Deliver(ctx context.Context, del Delivery) Outcome {
	if err := ctx.Err(); err != nil {
		return Classify(err, StageSession)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	outcome := r.deliver(del)
	if outcome.Delivered {
		return outcome
	}

	// Reconnect once: over a long run the relay may close an idle connection
	// under us.
	if IsConnectionError(outcome.Err) {
		r.closeLocked()
		return r.deliver(del)
	}
	return outcome
}

func (r *Relay) deliver(del Delivery) Outcome {
	client, err := r.connectLocked()
	if err != nil {
		return Classify(err, StageSession)
	}
	return transactRelay(client, r.cfg.Timeout, del)
}

// transactRelay mirrors the direct-mode transaction so both paths reach the
// same verdict for the same reply. The split between what is about us and what
// is about the recipient is identical: up to and including MAIL FROM the relay
// is answering about our envelope sender and our credentials.
func transactRelay(client *smtp.Client, timeout time.Duration, del Delivery) Outcome {
	if err := client.Reset(); err != nil {
		return Classify(err, StageSession)
	}
	if err := client.Mail(del.EnvelopeFrom); err != nil {
		return Classify(err, StageSession)
	}
	if err := client.Rcpt(del.To); err != nil {
		return Classify(err, StageRecipient)
	}

	w, err := client.Data()
	if err != nil {
		return Classify(err, StageRecipient)
	}
	if _, err := w.Write(del.Data); err != nil {
		_ = w.Close()
		return Classify(err, StageRecipient)
	}
	if err := w.Close(); err != nil {
		return Classify(err, StageRecipient)
	}
	return Outcome{Delivered: true}
}

// Probe dials, greets, negotiates TLS and authenticates — a real send minus the
// message.
//
// The connection is kept, so a successful probe warms the pool instead of
// costing a round trip.
func (r *Relay) Probe() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.connectLocked(); err != nil {
		return Classify(err, StageSession).Err
	}
	return nil
}

func (r *Relay) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		_ = r.client.Quit()
		r.client = nil
	}
}

func (r *Relay) closeLocked() {
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
}

func (r *Relay) connectLocked() (*smtp.Client, error) {
	if r.client != nil {
		if err := r.client.Noop(); err == nil {
			return r.client, nil
		}
		r.closeLocked()
	}

	addr := net.JoinHostPort(r.cfg.Host, strconv.Itoa(r.cfg.Port))

	var conn net.Conn
	var err error
	if r.cfg.TLS {
		conn, err = tls.DialWithDialer(
			&net.Dialer{Timeout: r.cfg.Timeout}, "tcp", addr, r.tlsConfig())
	} else {
		conn, err = net.DialTimeout("tcp", addr, r.cfg.Timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("could not connect to the SMTP server (%s): %w", addr, err)
	}
	// Every command has to finish inside this window; a stalled server must
	// not block a worker forever.
	_ = conn.SetDeadline(time.Now().Add(r.cfg.Timeout))

	client, err := smtp.NewClient(conn, r.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("could not open the SMTP session: %w", err)
	}

	if !r.cfg.TLS {
		// Upgrade when offered; otherwise carry on in the clear, which is
		// normal for a local relay on localhost:25.
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(r.tlsConfig()); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}

	if r.cfg.Username != "" {
		if err := r.authenticate(client); err != nil {
			_ = client.Close()
			return nil, err
		}
	}

	r.client = client
	return client, nil
}

func (r *Relay) authenticate(client *smtp.Client) error {
	ok, mechs := client.Extension("AUTH")
	if !ok {
		return errors.New("a username was given but the server does not support authentication")
	}

	// PLAIN first, LOGIN as fallback. net/smtp refuses to send credentials
	// over an unencrypted connection, and that is deliberate.
	if strings.Contains(mechs, "PLAIN") {
		auth := smtp.PlainAuth("", r.cfg.Username, r.cfg.Password, r.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP authentication failed: %w", err)
		}
		return nil
	}
	if strings.Contains(mechs, "LOGIN") {
		if err := client.Auth(&loginAuth{r.cfg.Username, r.cfg.Password}); err != nil {
			return fmt.Errorf("SMTP authentication failed: %w", err)
		}
		return nil
	}
	return fmt.Errorf("no supported authentication mechanism (server offers: %s)", mechs)
}

func (r *Relay) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         r.cfg.Host,
		InsecureSkipVerify: r.cfg.SkipVerify,
		MinVersion:         tls.VersionTLS12,
	}
}

// loginAuth implements AUTH LOGIN. net/smtp ships only PLAIN and CRAM-MD5,
// while some servers (Exchange in particular) accept only LOGIN.
type loginAuth struct {
	username, password string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("AUTH LOGIN cannot be used on an unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("unexpected server challenge: %q", fromServer)
	}
}
