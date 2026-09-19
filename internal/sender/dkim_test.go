// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package sender

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/appstonia/pushmails-client/internal/agent"
)

func writeTestKey(t *testing.T, pkcs8 bool) ([]byte, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("could not generate a key: %v", err)
	}

	var block *pem.Block
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("could not encode PKCS#8: %v", err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	}

	return pem.EncodeToMemory(block), key
}

// Both PEM formats must be accepted; the usual tools emit either one.
func TestNewDKIMSignerAcceptsBothPEMFormats(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		name := "PKCS#1"
		if pkcs8 {
			name = "PKCS#8"
		}
		t.Run(name, func(t *testing.T) {
			raw, _ := writeTestKey(t, pkcs8)
			if _, err := NewDKIMSignerFromPEM(raw, "example.com", "pm"); err != nil {
				t.Fatalf("the %s format was rejected: %v", name, err)
			}
		})
	}
}

func TestNewDKIMSignerRejectsGarbage(t *testing.T) {
	if _, err := NewDKIMSignerFromPEM([]byte("this is not PEM"), "example.com", "pm"); err == nil {
		t.Error("invalid PEM was accepted")
	}
}

// Proves the signature actually verifies: this repeats what a verifier does
// (canonicalise → hash → check against the public key). Producing a signature
// is easy; producing a CORRECT one is the hard part, and that is what is
// tested here.
func TestDKIMSignatureVerifies(t *testing.T) {
	raw, key := writeTestKey(t, false)
	signer, err := NewDKIMSignerFromPEM(raw, "example.com", "pm")
	if err != nil {
		t.Fatal(err)
	}

	replyTo := "support@example.com"
	msg, err := Build(agent.Job{
		MessageID: "msg-1",
		To:        "recipient@example.com",
		FromName:  "Test Sender",
		FromEmail: "sender@example.com",
		ReplyTo:   &replyTo,
		Subject:   "Hello Renée — non-ASCII subject",
		HTML:      `<p>Hello <b>Renée</b>! <a href="https://example.com/unsubscribe?m=1">Unsubscribe</a></p>`,
	}, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if err := signer.Sign(msg); err != nil {
		t.Fatalf("signing failed: %v", err)
	}

	// DKIM-Signature has to be prepended.
	if msg.Headers[0].Name != "DKIM-Signature" {
		t.Fatalf("the first header is not DKIM-Signature: %s", msg.Headers[0].Name)
	}
	sigValue := msg.Headers[0].Value

	// --- act like a verifier ---

	bIndex := strings.LastIndex(sigValue, "b=")
	if bIndex < 0 {
		t.Fatal("the signature has no b= tag")
	}
	sigB64 := sigValue[bIndex+2:]
	unsigned := sigValue[:bIndex+2] // with an empty b=

	signature, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("the signature is not base64: %v", err)
	}

	// 1) The body hash must match bh=.
	bodyHash := sha256.Sum256(canonicalizeBody(msg.Body))
	wantBH := "bh=" + base64.StdEncoding.EncodeToString(bodyHash[:])
	if !strings.Contains(sigValue, wantBH) {
		t.Error("bh= does not match the body hash")
	}

	// 2) The headers listed in h= must be canonicalised and signed in order.
	hStart := strings.Index(sigValue, "h=") + 2
	hEnd := strings.Index(sigValue[hStart:], ";") + hStart
	signedNames := strings.Split(sigValue[hStart:hEnd], ":")

	byName := map[string]string{}
	for _, h := range msg.Headers {
		byName[strings.ToLower(h.Name)] = h.Value
	}

	var toVerify strings.Builder
	for _, name := range signedNames {
		toVerify.WriteString(canonicalizeHeader(name, byName[name]))
		toVerify.WriteString("\r\n")
	}
	toVerify.WriteString(canonicalizeHeader("dkim-signature", unsigned))

	digest := sha256.Sum256([]byte(toVerify.String()))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the DKIM signature did not verify: %v", err)
	}
}

// If the signed body is modified afterwards, verification has to fail.
func TestDKIMDetectsBodyTampering(t *testing.T) {
	raw, _ := writeTestKey(t, false)
	signer, _ := NewDKIMSignerFromPEM(raw, "example.com", "pm")

	msg, err := Build(agent.Job{
		MessageID: "msg-2", To: "a@example.com",
		FromEmail: "b@example.com", Subject: "Subject", HTML: "<p>Original</p>",
	}, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Sign(msg); err != nil {
		t.Fatal(err)
	}

	sigValue := msg.Headers[0].Value
	msg.Body = append(msg.Body, []byte("injected content")...)

	newHash := sha256.Sum256(canonicalizeBody(msg.Body))
	tamperedBH := "bh=" + base64.StdEncoding.EncodeToString(newHash[:])
	if strings.Contains(sigValue, tamperedBH) {
		t.Error("bh= still matches even though the body changed")
	}
}

func TestCanonicalizeHeader(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"Subject", "Hello", "subject:Hello"},
		{"SUBJECT", "  padded  ", "subject:padded"},
		{"To", "a@b.com,   c@d.com", "to:a@b.com, c@d.com"},
		{"X-Fold", "line1\r\n line2", "x-fold:line1 line2"},
	}
	for _, tt := range tests {
		if got := canonicalizeHeader(tt.name, tt.value); got != tt.want {
			t.Errorf("canonicalizeHeader(%q, %q) = %q, want %q",
				tt.name, tt.value, got, tt.want)
		}
	}
}

func TestCanonicalizeBody(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"trailing empty lines are dropped", "hello\r\n\r\n\r\n", "hello\r\n"},
		{"trailing whitespace is dropped", "hello   \r\n", "hello\r\n"},
		{"runs of whitespace collapse", "a    b\r\n", "a b\r\n"},
		{"an empty body becomes a single CRLF", "", "\r\n"},
		{"a single line ends with CRLF", "one", "one\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(canonicalizeBody([]byte(tt.in))); got != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
		})
	}
}
