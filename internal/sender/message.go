// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package sender turns an email into MIME, signs it with DKIM and hands it to
// SMTP.
package sender

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
)

type Message struct {
	From     string // "Name <address>" as it appears in the header
	FromAddr string // bare address — used for the SMTP envelope
	To       string
	Headers  []Header
	Body     []byte
}

type Header struct {
	Name  string
	Value string
}

// The body is multipart/alternative: a plain-text part is generated for clients
// that cannot render HTML, and spam filters penalise HTML-only messages.
func Build(job agent.Job, hostname string) (*Message, error) {
	if job.To == "" || job.FromEmail == "" {
		return nil, errors.New("sender or recipient address is empty")
	}

	from := formatAddress(job.FromName, job.FromEmail)

	headers := []Header{
		{"From", from},
		{"To", job.To},
		{"Subject", encodeHeader(job.Subject)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", messageID(job.MessageID, hostname)},
		{"MIME-Version", "1.0"},
	}
	if job.ReplyTo != nil && *job.ReplyTo != "" {
		headers = append(headers, Header{"Reply-To", *job.ReplyTo})
	}

	// Gmail and Outlook show this header as an "unsubscribe" button and expect
	// it on bulk mail. The address comes from the server; scraping the HTML is
	// only a fallback, because a link left as plain text scrapes to nothing and
	// the header would silently disappear.
	if url := unsubscribeURL(job); url != "" {
		headers = append(headers,
			Header{"List-Unsubscribe", "<" + url + ">"},
			Header{"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"},
		)
	}

	boundary, err := randomBoundary()
	if err != nil {
		return nil, err
	}
	headers = append(headers, Header{
		"Content-Type", `multipart/alternative; boundary="` + boundary + `"`,
	})

	// Reduce every header value to a single line, in one pass so that adding a
	// header cannot silently skip it. Must run BEFORE signing: the DKIM
	// signature is computed from this slice.
	for i := range headers {
		headers[i].Value = stripHeaderBreaks(headers[i].Value)
	}

	var body strings.Builder
	text := htmlToText(job.HTML)
	if job.Preheader != nil && *job.Preheader != "" {
		text = *job.Preheader + "\r\n\r\n" + text
	}

	writePart(&body, boundary, "text/plain; charset=UTF-8", text)
	writePart(&body, boundary, "text/html; charset=UTF-8", job.HTML)
	fmt.Fprintf(&body, "--%s--\r\n", boundary)

	return &Message{
		From:     from,
		FromAddr: job.FromEmail,
		To:       job.To,
		Headers:  headers,
		Body:     []byte(body.String()),
	}, nil
}

func (m *Message) Bytes() []byte {
	var out strings.Builder
	for _, h := range m.Headers {
		fmt.Fprintf(&out, "%s: %s\r\n", h.Name, h.Value)
	}
	out.WriteString("\r\n")
	out.Write(m.Body)
	return []byte(out.String())
}

func writePart(b *strings.Builder, boundary, contentType, content string) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	fmt.Fprintf(b, "Content-Type: %s\r\n", contentType)
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.WriteString(toQuotedPrintable(content))
	b.WriteString("\r\n")
}

// toQuotedPrintable makes UTF-8 content 7-bit safe; older relays mangle an
// 8-bit body.
func toQuotedPrintable(s string) string {
	var out strings.Builder
	w := quotedprintable.NewWriter(&out)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return out.String()
}

// encodeHeader applies RFC 2047 to non-ASCII headers; without it, accented or
// non-Latin subject lines arrive garbled.
func encodeHeader(value string) string {
	value = stripHeaderBreaks(value)
	if isASCII(value) {
		return value
	}
	return mime.QEncoding.Encode("UTF-8", value)
}

// A header is written as `Name: value\r\n`, so a break left in the value ends
// the header early and turns what follows into A HEADER OF ITS OWN — an
// injected Bcc, for one. Subject and sender name arrive as free text, so this
// is the last thing between a job and that.
//
// The server checks this too and is the real gate; this pass is here so the
// worker can guarantee on its own that what it puts on the wire is a valid
// message. Stripping beats rejecting: a space where a break used to be is a
// better outcome than mail that never goes out.
func stripHeaderBreaks(value string) string {
	if !strings.ContainsAny(value, "\r\n") {
		return value
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

func formatAddress(name, addr string) string {
	if name == "" {
		return addr
	}
	return fmt.Sprintf("%s <%s>", encodeHeader(name), addr)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// messageID carries the central message_id, so bounces can be matched back.
func messageID(id, hostname string) string {
	if hostname == "" {
		hostname = "localhost"
	}
	return fmt.Sprintf("<%s@%s>", id, hostname)
}

func randomBoundary() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("could not generate a MIME boundary: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

var unsubscribeRe = regexp.MustCompile(`href="([^"]*unsubscribe[^"]*)"`)

// unsubscribeURL prefers the address the server sent; the HTML link is a
// fallback for older server versions.
func unsubscribeURL(job agent.Job) string {
	if url := strings.TrimSpace(job.UnsubscribeURL); url != "" {
		return url
	}
	return extractUnsubscribeURL(job.HTML)
}

func extractUnsubscribeURL(html string) string {
	if m := unsubscribeRe.FindStringSubmatch(html); len(m) == 2 {
		return strings.ReplaceAll(m[1], "&amp;", "&")
	}
	return ""
}

var (
	blockTagRe = regexp.MustCompile(`(?i)</(p|div|tr|h[1-6]|li)>|<br\s*/?>`)
	stripTagRe = regexp.MustCompile(`(?s)<[^>]*>`)
	// Go's RE2 engine has no backreferences (\1), so every tag is spelled out.
	dropTagRe = regexp.MustCompile(
		`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<head[^>]*>.*?</head>`)
	multiSpace   = regexp.MustCompile(`[ \t]+`)
	multiNewline = regexp.MustCompile(`\n{3,}`)
)

// Not a full HTML parser and does not need to be: the goal is a readable
// fallback.
func htmlToText(html string) string {
	s := dropTagRe.ReplaceAllString(html, "")
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = stripTagRe.ReplaceAllString(s, "")

	replacer := strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<",
		"&gt;", ">", "&quot;", `"`, "&#39;", "'", "&#34;", `"`,
	)
	s = replacer.Replace(s)

	s = multiSpace.ReplaceAllString(s, " ")
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	s = strings.Join(lines, "\n")
	s = multiNewline.ReplaceAllString(s, "\n\n")

	return strings.TrimSpace(s)
}
