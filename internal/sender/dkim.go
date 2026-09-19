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
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DKIMSigner signs messages per RFC 6376.
//
// Hand-written to keep the dependency list empty. Canonicalisation is
// relaxed/relaxed: it survives the harmless whitespace edits relays make along
// the way, where simple breaks signatures constantly.
type DKIMSigner struct {
	key      *rsa.PrivateKey
	domain   string
	selector string
}

// Headers to sign, in order: verifiers follow the h= tag. Absent ones are
// skipped.
var signedHeaders = []string{
	"from", "to", "subject", "date", "message-id",
	"mime-version", "content-type", "reply-to",
	// RFC 8058 one-click unsubscribe only counts when DKIM covers these two.
	// Unsigned, Gmail hides the button and the message fails the bulk-sender
	// requirement: writing the header without signing it is writing nothing.
	"list-unsubscribe", "list-unsubscribe-post",
}

// NewDKIMSignerFromPEM builds a signer from a key the central system issued.
//
// The key arrives over the API and stays in memory: it is never written to
// disk, so there is no file on the customer's machine to leak, back up or
// forget to rotate.
func NewDKIMSignerFromPEM(raw []byte, domain, selector string) (*DKIMSigner, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("the DKIM key for %s is not valid PEM", domain)
	}

	key, err := parsePrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	return &DKIMSigner{key: key, domain: domain, selector: selector}, nil
}

// parsePrivateKey accepts PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE
// KEY"); the usual tools emit either.
func parsePrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("could not parse the DKIM key (tried PKCS#1 and PKCS#8): %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the DKIM key is not an RSA key (%T)", parsed)
	}
	return key, nil
}

// Sign prepends a DKIM-Signature header. It goes first because verifiers
// include the header itself, with an empty b=, when recomputing the hash.
func (s *DKIMSigner) Sign(msg *Message) error {
	bodyHash := sha256.Sum256(canonicalizeBody(msg.Body))

	present := make(map[string]string, len(msg.Headers))
	for _, h := range msg.Headers {
		present[strings.ToLower(h.Name)] = h.Value
	}

	var included []string
	for _, name := range signedHeaders {
		if _, ok := present[name]; ok {
			included = append(included, name)
		}
	}

	// b= is filled in once the signature is computed.
	sigHeader := fmt.Sprintf(
		"v=1; a=rsa-sha256; c=relaxed/relaxed; d=%s; s=%s; t=%d; h=%s; bh=%s; b=",
		s.domain,
		s.selector,
		time.Now().Unix(),
		strings.Join(included, ":"),
		base64.StdEncoding.EncodeToString(bodyHash[:]),
	)

	var toSign strings.Builder
	for _, name := range included {
		toSign.WriteString(canonicalizeHeader(name, present[name]))
		toSign.WriteString("\r\n")
	}
	// No CRLF on the last line: the signed data ends with the canonicalised
	// DKIM-Signature header.
	toSign.WriteString(canonicalizeHeader("dkim-signature", sigHeader))

	digest := sha256.Sum256([]byte(toSign.String()))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return fmt.Errorf("could not produce the DKIM signature: %w", err)
	}

	msg.Headers = append([]Header{{
		Name:  "DKIM-Signature",
		Value: sigHeader + base64.StdEncoding.EncodeToString(signature),
	}}, msg.Headers...)

	return nil
}

var headerWhitespace = regexp.MustCompile(`[ \t]+`)

// canonicalizeHeader: relaxed header canonicalisation (RFC 6376 §3.4.2) —
// lowercase name, folded lines joined, whitespace runs collapsed, edges
// trimmed.
func canonicalizeHeader(name, value string) string {
	value = strings.ReplaceAll(value, "\r\n", "")
	value = strings.ReplaceAll(value, "\n", "")
	value = headerWhitespace.ReplaceAllString(value, " ")
	return strings.ToLower(name) + ":" + strings.TrimSpace(value)
}

// canonicalizeBody: relaxed body canonicalisation (RFC 6376 §3.4.4) — trailing
// whitespace dropped, whitespace runs collapsed, trailing empty lines removed.
// A non-empty body ends with a single CRLF.
func canonicalizeBody(body []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")

	for i, line := range lines {
		lines[i] = strings.TrimRight(headerWhitespace.ReplaceAllString(line, " "), " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		// An empty body canonicalises to a single CRLF.
		return []byte("\r\n")
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}
