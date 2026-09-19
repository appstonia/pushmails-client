// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package sender

import (
	"mime"
	"net/mail"
	"strings"
	"testing"

	"github.com/appstonia/pushmails-client/internal/agent"
)

// The generated message has to be readable by a standard parser — mail clients
// apply the same rules.
func TestBuildProducesParseableMessage(t *testing.T) {
	preheader := "Preview text"
	msg, err := Build(agent.Job{
		MessageID: "msg-1",
		To:        "recipient@example.com",
		FromName:  "PushMails Test",
		FromEmail: "sender@example.com",
		Subject:   "Hello Renée",
		Preheader: &preheader,
		HTML:      "<p>Hello <b>Renée</b>, welcome aboard.</p>",
	}, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := mail.ReadMessage(strings.NewReader(string(msg.Bytes())))
	if err != nil {
		t.Fatalf("could not parse the message: %v", err)
	}

	if got := parsed.Header.Get("To"); got != "recipient@example.com" {
		t.Errorf("To = %q", got)
	}

	// A non-ASCII subject must be RFC 2047 encoded and decode back cleanly.
	subject := parsed.Header.Get("Subject")
	if !strings.HasPrefix(subject, "=?UTF-8?") {
		t.Errorf("non-ASCII subject was not encoded: %q", subject)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
	if err != nil {
		t.Fatalf("could not decode the subject: %v", err)
	}
	if decoded != "Hello Renée" {
		t.Errorf("decoded subject = %q, want %q", decoded, "Hello Renée")
	}

	contentType := parsed.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/alternative") {
		t.Errorf("Content-Type = %q, want multipart/alternative", contentType)
	}

	// The From header must parse as an address.
	addr, err := mail.ParseAddress(parsed.Header.Get("From"))
	if err != nil {
		t.Fatalf("could not parse From: %v", err)
	}
	if addr.Address != "sender@example.com" {
		t.Errorf("From address = %q", addr.Address)
	}

	body := string(msg.Body)
	if !strings.Contains(body, "text/plain") || !strings.Contains(body, "text/html") {
		t.Error("one of the body parts is missing")
	}
	// The preheader belongs at the top of the plain-text alternative.
	if !strings.Contains(body, "Preview") {
		t.Error("preheader was not prepended to the plain-text part")
	}
}

// When an unsubscribe link exists, the List-Unsubscribe header must be there —
// Gmail and Outlook expect it on bulk mail.
func TestBuildAddsListUnsubscribe(t *testing.T) {
	msg, err := Build(agent.Job{
		MessageID: "m", To: "a@example.com", FromEmail: "b@example.com",
		Subject: "Subject",
		HTML:    `<a href="https://example.com/unsubscribe?m=abc&amp;t=1">Unsubscribe</a>`,
	}, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}

	var value string
	for _, h := range msg.Headers {
		if h.Name == "List-Unsubscribe" {
			value = h.Value
		}
	}
	if value == "" {
		t.Fatal("List-Unsubscribe header is missing")
	}
	// The HTML entity has to become a real & or the link breaks.
	if !strings.Contains(value, "m=abc&t=1") {
		t.Errorf("List-Unsubscribe = %q, &amp; was not decoded", value)
	}
}

func TestBuildWithoutUnsubscribeLink(t *testing.T) {
	msg, err := Build(agent.Job{
		MessageID: "m", To: "a@example.com", FromEmail: "b@example.com",
		Subject: "Subject", HTML: "<p>no link here</p>",
	}, "h")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range msg.Headers {
		if h.Name == "List-Unsubscribe" {
			t.Error("List-Unsubscribe was added without a link")
		}
	}
}

// The real rule: the server supplies the address. When the template leaves the
// link as PLAIN TEXT, scraping returned nothing and the header disappeared
// silently — and since Gmail/Yahoo require it on bulk mail, that hits delivery
// directly.
func TestBuildPrefersJobUnsubscribeURL(t *testing.T) {
	msg, err := Build(agent.Job{
		MessageID: "m", To: "a@example.com", FromEmail: "b@example.com",
		Subject:        "Subject",
		HTML:           `<p>To unsubscribe: https://example.com/unsubscribe?m=abc</p>`,
		UnsubscribeURL: "https://example.com/unsubscribe?m=abc&t=1",
	}, "h")
	if err != nil {
		t.Fatal(err)
	}

	var value string
	for _, h := range msg.Headers {
		if h.Name == "List-Unsubscribe" {
			value = h.Value
		}
	}
	if value != "<https://example.com/unsubscribe?m=abc&t=1>" {
		t.Errorf("List-Unsubscribe = %q", value)
	}
}

func TestBuildRejectsEmptyAddresses(t *testing.T) {
	if _, err := Build(agent.Job{FromEmail: "a@example.com"}, "h"); err == nil {
		t.Error("a job without a recipient was accepted")
	}
	if _, err := Build(agent.Job{To: "a@example.com"}, "h"); err == nil {
		t.Error("a job without a sender was accepted")
	}
}

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"tags are dropped", "<p>Hello</p>", "Hello"},
		{"block tag becomes a newline", "<p>one</p><p>two</p>", "one\ntwo"},
		{"br becomes a newline", "one<br>two", "one\ntwo"},
		{"script is dropped", "<script>evil()</script><p>good</p>", "good"},
		{"style is dropped", "<style>p{color:red}</style><p>text</p>", "text"},
		{"entities are decoded", "<p>a &amp; b &lt;c&gt;</p>", "a & b <c>"},
		{"nbsp becomes a space", "<p>a&nbsp;b</p>", "a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := htmlToText(tt.in); got != tt.want {
				t.Errorf("htmlToText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEncodeHeader(t *testing.T) {
	if got := encodeHeader("Plain ASCII"); got != "Plain ASCII" {
		t.Errorf("an ASCII header was encoded needlessly: %q", got)
	}
	got := encodeHeader("Grüße aus Köln")
	if !strings.HasPrefix(got, "=?UTF-8?") {
		t.Errorf("a non-ASCII header was not encoded: %q", got)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(got)
	if err != nil || decoded != "Grüße aus Köln" {
		t.Errorf("encoding did not round-trip: %q → %q (%v)", got, decoded, err)
	}
}

// The server rejects these values now, but the queue still holds rows written
// before that check landed — the worker has to stand on its own.
func TestBuildStripsHeaderInjection(t *testing.T) {
	replyTo := "reply@example.com\r\nX-Forged: yes"
	msg, err := Build(agent.Job{
		MessageID: "msg-inject",
		To:        "recipient@example.com",
		FromName:  "Acme\r\nX-Name: forged",
		FromEmail: "sender@example.com",
		Subject:   "Campaign\r\nBcc: outside@attacker.example",
		ReplyTo:   &replyTo,
		HTML:      "<p>body</p>",
	}, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// The test is not whether "Bcc:" appears in the text — stripping turns the
	// break into a space, so that string sits harmlessly inside the subject
	// value. What matters is how many HEADERS a parser sees: if the injected
	// lines are read as headers of their own, the hole is still open.
	parsed, err := mail.ReadMessage(strings.NewReader(string(msg.Bytes())))
	if err != nil {
		t.Fatalf("could not parse message: %v", err)
	}
	for _, injected := range []string{"Bcc", "X-Forged", "X-Name"} {
		if got := parsed.Header.Get(injected); got != "" {
			t.Errorf("injected %s header appeared: %q", injected, got)
		}
	}
	if got := parsed.Header.Get("Subject"); strings.ContainsAny(got, "\r\n") {
		t.Errorf("line break left in subject: %q", got)
	}

	// Stripping must happen BEFORE signing: the DKIM signature is computed
	// from msg.Headers, so a break left in the slice would make the signature
	// cover text we never put on the wire.
	for _, h := range msg.Headers {
		if strings.ContainsAny(h.Value, "\r\n") {
			t.Errorf("line break left in header slice: %s = %q", h.Name, h.Value)
		}
	}
}
