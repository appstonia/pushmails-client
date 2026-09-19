// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package mta

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// replies decides what the fake server answers at each step; an empty string
// means the default success reply.
type replies struct {
	mail string
	rcpt string
	data string
}

// session is what the server actually saw, so a test can prove the envelope
// reached the wire rather than trusting the return value alone.
type session struct {
	mu   sync.Mutex
	ehlo string
	from string
	to   string
	body string
}

func (s *session) snapshot() session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return session{ehlo: s.ehlo, from: s.from, to: s.to, body: s.body}
}

// fakeMX speaks just enough SMTP to accept or refuse a message. STARTTLS is
// never advertised, so the client stays in the clear and the test needs no
// certificate.
func fakeMX(t *testing.T, r replies) (host string, port string, seen *session) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	seen = &session{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn, r, seen)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", fmt.Sprint(addr.Port), seen
}

func serve(conn net.Conn, r replies, seen *session) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	write := func(line string) { fmt.Fprintf(conn, "%s\r\n", line) }

	write("220 fake ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))

		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			seen.mu.Lock()
			seen.ehlo = strings.TrimSpace(line[4:])
			seen.mu.Unlock()
			write("250-fake")
			write("250 SIZE 10240000")
		case strings.HasPrefix(command, "MAIL FROM"):
			if r.mail != "" {
				write(r.mail)
				continue
			}
			seen.mu.Lock()
			seen.from = strings.TrimSpace(line[10:])
			seen.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(command, "RCPT TO"):
			if r.rcpt != "" {
				write(r.rcpt)
				continue
			}
			seen.mu.Lock()
			seen.to = strings.TrimSpace(line[8:])
			seen.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(command, "DATA"):
			write("354 send it")
			var body strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}
			if r.data != "" {
				write(r.data)
				continue
			}
			seen.mu.Lock()
			seen.body = body.String()
			seen.mu.Unlock()
			write("250 Accepted")
		case strings.HasPrefix(command, "RSET"), strings.HasPrefix(command, "NOOP"):
			write("250 OK")
		case strings.HasPrefix(command, "QUIT"):
			write("221 Bye")
			return
		default:
			write("500 Unrecognised")
		}
	}
}

// directTo builds a Direct whose DNS answers with the given hosts and whose
// pool dials the test port instead of 25.
func directTo(port string, hosts ...string) *Direct {
	d := NewDirect("mail.example.test", nil, 5*time.Second)
	d.pool.port = port
	d.resolver.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		records := make([]*net.MX, 0, len(hosts))
		for i, host := range hosts {
			records = append(records, &net.MX{Host: host, Pref: uint16(10 + i)})
		}
		return records, nil
	}
	return d
}

func testDelivery(to string) Delivery {
	return Delivery{
		EnvelopeFrom: "news@example.test",
		To:           to,
		Domain:       "example.test",
		MaxPerConn:   10,
		Data:         []byte("Subject: hello\r\n\r\nbody\r\n"),
	}
}

// The whole point of direct mode: the message reaches the recipient's server
// with no other software in the path, and the answer we report is that
// server's answer.
func TestDirectDelivers(t *testing.T) {
	host, port, seen := fakeMX(t, replies{})
	d := directTo(port, host)
	defer d.Close()

	outcome := d.Deliver(context.Background(), testDelivery("person@example.test"))
	if !outcome.Delivered {
		t.Fatalf("not delivered: %v", outcome.Err)
	}

	got := seen.snapshot()
	if got.from != "<news@example.test>" {
		t.Errorf("envelope sender = %q", got.from)
	}
	if got.to != "<person@example.test>" {
		t.Errorf("recipient = %q", got.to)
	}
	if !strings.Contains(got.body, "Subject: hello") {
		t.Errorf("the message body never reached the server: %q", got.body)
	}
	// The EHLO name is what the receiver matches against the PTR of our
	// address; sending the wrong one costs reputation on every message.
	if got.ehlo != "mail.example.test" {
		t.Errorf("EHLO = %q, want mail.example.test", got.ehlo)
	}
}

// A 5xx at RCPT is about the mailbox: permanent, and the walk stops because
// the next server would only say the same thing.
func TestDirectRecipientRejectionIsPermanent(t *testing.T) {
	host, port, _ := fakeMX(t, replies{rcpt: "550 5.1.1 User unknown"})
	d := directTo(port, host)
	defer d.Close()

	outcome := d.Deliver(context.Background(), testDelivery("nobody@example.test"))
	if outcome.Delivered {
		t.Fatal("a rejected message was reported as delivered")
	}
	if !outcome.Permanent {
		t.Error("a recipient-stage 5xx was not permanent")
	}
	if outcome.Code != 550 {
		t.Errorf("Code = %d, want 550", outcome.Code)
	}
}

// A 5xx at MAIL FROM is about US, not the recipient. Treating it as a bounce
// would suppress healthy subscribers the day our address is blocklisted.
func TestDirectSessionRejectionIsNotPermanent(t *testing.T) {
	host, port, _ := fakeMX(t, replies{mail: "550 5.7.1 Sender address rejected"})
	d := directTo(port, host)
	defer d.Close()

	outcome := d.Deliver(context.Background(), testDelivery("person@example.test"))
	if outcome.Permanent {
		t.Fatal("a session-stage 5xx was reported as a permanent bounce")
	}
	if !outcome.SessionRejected() {
		t.Error("a session-stage 5xx was not counted as a session rejection")
	}
}

// The MX list is walked in order. A server that refuses US does not end the
// attempt: another one may well accept, and the message is not at fault.
func TestDirectFallsBackToNextMX(t *testing.T) {
	refusingHost, refusingPort, _ := fakeMX(t, replies{mail: "550 5.7.1 Go away"})
	acceptingHost, acceptingPort, seen := fakeMX(t, replies{})

	// Both fakes listen on their own port, so the pool needs a per-host port.
	// Keeping the resolver honest is simpler: give the hosts distinct names
	// and let the dialler resolve them through a stub.
	d := NewDirect("mail.example.test", nil, 5*time.Second)
	defer d.Close()
	d.resolver.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{
			{Host: "primary.example.test", Pref: 10},
			{Host: "backup.example.test", Pref: 20},
		}, nil
	}
	d.pool.port = refusingPort
	ports := map[string]string{
		"primary.example.test": net.JoinHostPort(refusingHost, refusingPort),
		"backup.example.test":  net.JoinHostPort(acceptingHost, acceptingPort),
	}
	d.pool.dialContext = func(ctx context.Context, dialer *net.Dialer, host string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", ports[host])
	}

	outcome := d.Deliver(context.Background(), testDelivery("person@example.test"))
	if !outcome.Delivered {
		t.Fatalf("the backup MX was never tried: %v", outcome.Err)
	}
	if got := seen.snapshot(); got.to != "<person@example.test>" {
		t.Errorf("the backup server did not receive the message (to=%q)", got.to)
	}
}

// A domain that publishes a null MX accepts no mail at all (RFC 7505). That is
// a fact about the address, not a passing problem, so it bounces instead of
// being retried for days.
func TestDirectNullMXIsPermanent(t *testing.T) {
	d := NewDirect("mail.example.test", nil, 5*time.Second)
	defer d.Close()
	d.resolver.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{{Host: ".", Pref: 0}}, nil
	}

	outcome := d.Deliver(context.Background(), testDelivery("person@example.test"))
	if !outcome.Permanent {
		t.Error("a null MX did not produce a permanent failure")
	}
	if outcome.Stage != StageResolve {
		t.Errorf("Stage = %v, want resolve", outcome.Stage)
	}
}

// Sessions are reused across messages: reconnecting per message costs a round
// trip every time and reads as aggressive to the receiving side.
func TestDirectReusesSession(t *testing.T) {
	host, port, _ := fakeMX(t, replies{})
	d := directTo(port, host)
	defer d.Close()

	for i := range 3 {
		outcome := d.Deliver(context.Background(), testDelivery("person@example.test"))
		if !outcome.Delivered {
			t.Fatalf("message %d not delivered: %v", i, outcome.Err)
		}
	}

	d.pool.mu.Lock()
	idle := len(d.pool.idle)
	d.pool.mu.Unlock()
	if idle == 0 {
		t.Error("no connection was kept for reuse")
	}
}
