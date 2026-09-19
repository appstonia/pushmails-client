// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package mta

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"sync"
	"time"
)

// conn is one pooled SMTP session.
type conn struct {
	client *smtp.Client
	raw    net.Conn
	key    connKey
	// How many messages have gone through this session. Providers cap it;
	// closing politely when we reach the cap beats waiting to be cut off.
	sent     int
	tls      bool
	lastUsed time.Time
}

// connKey is what makes a connection unique: from which local address, to
// which host. The local address belongs in the key because a session opened
// from one address cannot stand in for one opened from another.
type connKey struct {
	localIP string
	host    string
}

// Pool reuses SMTP connections.
//
// Reconnecting per message costs a round trip every time and, over thousands
// of messages, reads as aggressive to the receiving side.
type Pool struct {
	dialTimeout time.Duration
	idleTTL     time.Duration
	maxIdle     int
	// Outgoing SMTP port, always 25 in production: submission ports (587, 465)
	// want authentication and have no meaning when talking straight to an MX.
	// A field so tests can listen on an unprivileged port.
	port string
	// How the TCP connection is made. nil means the ordinary dial; a test sets
	// it to point several MX names at its own listeners.
	dialContext func(ctx context.Context, dialer *net.Dialer, host string) (net.Conn, error)

	mu   sync.Mutex
	idle map[connKey][]*conn
}

func NewPool(dialTimeout time.Duration) *Pool {
	return &Pool{
		dialTimeout: dialTimeout,
		idleTTL:     60 * time.Second,
		maxIdle:     2,
		port:        "25",
		idle:        map[connKey][]*conn{},
	}
}

func (p *Pool) get(ctx context.Context, localIP net.IP, host, ehlo string) (*conn, error) {
	key := connKey{host: host}
	if localIP != nil {
		key.localIP = localIP.String()
	}

	for {
		p.mu.Lock()
		queue := p.idle[key]
		if len(queue) == 0 {
			p.mu.Unlock()
			break
		}
		c := queue[len(queue)-1]
		p.idle[key] = queue[:len(queue)-1]
		p.mu.Unlock()

		if time.Since(c.lastUsed) > p.idleTTL {
			c.close()
			continue
		}
		// The server may have dropped an idle session without telling us; NOOP
		// is a cheap liveness check.
		if err := c.client.Noop(); err != nil {
			c.close()
			continue
		}
		return c, nil
	}

	return p.dial(ctx, key, localIP, host, ehlo)
}

func (p *Pool) dial(ctx context.Context, key connKey, localIP net.IP, host, ehlo string) (*conn, error) {
	client, raw, err := p.open(ctx, localIP, host, ehlo)
	if err != nil {
		return nil, err
	}

	c := &conn{client: client, raw: raw, key: key, lastUsed: time.Now()}

	// Opportunistic STARTTLS, with verification off. Public MX certificates
	// routinely fail to match the name we dialled (shared hosting, ageing
	// setups) and insisting would mean not delivering at all. This is what
	// outbound MTAs do, and it still beats sending in the clear.
	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsErr := client.StartTLS(&tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		})
		if tlsErr != nil {
			// The session is spoiled once STARTTLS fails; carrying on in the
			// clear means dialling again from scratch. Try once, without TLS.
			_ = client.Close()
			return p.dialPlain(ctx, key, localIP, host, ehlo)
		}
		c.tls = true
	}

	return c, nil
}

func (p *Pool) dialPlain(ctx context.Context, key connKey, localIP net.IP, host, ehlo string) (*conn, error) {
	client, raw, err := p.open(ctx, localIP, host, ehlo)
	if err != nil {
		return nil, err
	}
	return &conn{client: client, raw: raw, key: key, lastUsed: time.Now()}, nil
}

func (p *Pool) open(ctx context.Context, localIP net.IP, host, ehlo string) (*smtp.Client, net.Conn, error) {
	dialer := &net.Dialer{Timeout: p.dialTimeout}
	if localIP != nil {
		// Pins the outgoing connection to the address whose PTR matches the
		// EHLO name. If the address is not on this machine we get
		// EADDRNOTAVAIL here rather than mail from the wrong address.
		dialer.LocalAddr = &net.TCPAddr{IP: localIP}
	}

	dial := p.dialContext
	if dial == nil {
		dial = func(ctx context.Context, d *net.Dialer, host string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", net.JoinHostPort(host, p.port))
		}
	}

	raw, err := dial(ctx, dialer, host)
	if err != nil {
		return nil, nil, fmt.Errorf("could not connect to %s: %w", host, err)
	}
	_ = raw.SetDeadline(time.Now().Add(p.dialTimeout))

	client, err := smtp.NewClient(raw, host)
	if err != nil {
		_ = raw.Close()
		return nil, nil, fmt.Errorf("no SMTP greeting from %s: %w", host, err)
	}
	if ehlo == "" {
		ehlo = "localhost"
	}
	if err := client.Hello(ehlo); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("%s rejected EHLO %s: %w", host, ehlo, err)
	}
	return client, raw, nil
}

// put returns a connection to the pool, closing it once it has carried as many
// messages as the domain allows.
func (p *Pool) put(c *conn, maxPerConn int) {
	if maxPerConn > 0 && c.sent >= maxPerConn {
		c.close()
		return
	}
	c.lastUsed = time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle[c.key]) >= p.maxIdle {
		go c.close()
		return
	}
	p.idle[c.key] = append(p.idle[c.key], c)
}

// CloseIdle drops every pooled connection; used on shutdown.
func (p *Pool) CloseIdle() {
	p.mu.Lock()
	idle := p.idle
	p.idle = map[connKey][]*conn{}
	p.mu.Unlock()

	for _, queue := range idle {
		for _, c := range queue {
			c.close()
		}
	}
}

// Sweep drops connections that have been idle past their TTL.
func (p *Pool) Sweep() {
	p.mu.Lock()
	var stale []*conn
	for key, queue := range p.idle {
		kept := queue[:0]
		for _, c := range queue {
			if time.Since(c.lastUsed) > p.idleTTL {
				stale = append(stale, c)
				continue
			}
			kept = append(kept, c)
		}
		if len(kept) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = kept
		}
	}
	p.mu.Unlock()

	for _, c := range stale {
		c.close()
	}
}

func (c *conn) close() {
	// QUIT is the polite close; without it the server counts the session as
	// cut short.
	if err := c.client.Quit(); err != nil {
		_ = c.client.Close()
	}
}
