// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
)

// Keyring holds the signing keys the central system issues, one per sending
// domain.
//
// Why the keys come from there rather than a file on this machine: the panel
// publishes a DKIM public key in your DNS when a domain is verified, and the
// matching private key is the one the whole service signs that domain with. A
// key generated locally would not match what your DNS advertises, so receivers
// would compute the signature against the wrong public key and report
// dkim=fail — while the panel still showed the domain as verified. One key per
// domain, one place it comes from.
//
// Keys are asked for per batch and never written to disk. A domain that is not
// sending today does not have its private key on the wire.
type Keyring struct {
	client *agent.Client

	mu      sync.RWMutex
	entries map[string]entry
}

// entry is one domain's cached answer. signer may be nil: "the server has no
// key for this domain" is cached too, or a domain without one would be asked
// for again on every batch.
type entry struct {
	signer    *DKIMSigner
	expiresAt time.Time
}

// Fallback when the server states no TTL of its own.
const defaultKeyTTL = 15 * time.Minute

// Most domains asked for in one request; matches the server's own limit.
const maxDomainsPerRequest = 100

func NewKeyring(client *agent.Client) *Keyring {
	return &Keyring{client: client, entries: map[string]entry{}}
}

// Ensure fetches whatever keys the given From addresses need.
//
// Called with the batch in hand, so only domains that are missing or stale go
// to the server. Errors are logged, not returned: a failed refresh leaves the
// previous key in place, and signing with a key we already hold beats stopping
// the batch. A domain we have never held a key for fails per message, in
// sendOne, where the message can be handed back individually.
func (k *Keyring) Ensure(ctx context.Context, fromEmails []string) {
	missing := k.missingDomains(fromEmails)
	if len(missing) == 0 {
		return
	}

	for start := 0; start < len(missing); start += maxDomainsPerRequest {
		end := min(start+maxDomainsPerRequest, len(missing))
		k.fetch(ctx, missing[start:end])
	}
}

func (k *Keyring) missingDomains(fromEmails []string) []string {
	now := time.Now()

	k.mu.RLock()
	defer k.mu.RUnlock()

	seen := make(map[string]struct{}, len(fromEmails))
	missing := make([]string, 0, 4)
	for _, email := range fromEmails {
		domain := domainOf(email)
		if domain == "" {
			continue
		}
		if _, dup := seen[domain]; dup {
			continue
		}
		seen[domain] = struct{}{}
		if e, ok := k.entries[domain]; ok && now.Before(e.expiresAt) {
			continue
		}
		missing = append(missing, domain)
	}
	return missing
}

func (k *Keyring) fetch(ctx context.Context, domains []string) {
	resp, err := k.client.DKIMKeys(ctx, domains)
	if err != nil {
		var apiErr *agent.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			// The endpoint is closed to this token. Older servers only served
			// the central pool; nothing can be signed until that one is
			// updated, and sending stops rather than going out unsigned.
			slog.Error("this server does not issue signing keys to self-hosted " +
				"nodes; it needs updating before mail can be signed")
			return
		}
		slog.Warn("could not fetch signing keys, carrying on with what we have",
			"domains", domains, "error", err)
		return
	}

	ttl := time.Duration(resp.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultKeyTTL
	}
	expiresAt := time.Now().Add(ttl)

	signers := make(map[string]*DKIMSigner, len(resp.Keys))
	for _, key := range resp.Keys {
		if key.Selector == "" || key.PrivateKey == "" {
			continue
		}
		signer, err := NewDKIMSignerFromPEM([]byte(key.PrivateKey), key.Domain, key.Selector)
		if err != nil {
			// One unusable key must not leave the other domains unsigned.
			slog.Error("could not load the signing key",
				"domain", key.Domain, "error", err)
			continue
		}
		signers[strings.ToLower(key.Domain)] = signer
	}

	k.mu.Lock()
	for _, domain := range domains {
		// Domains missing from the answer are recorded too (signer nil): "no
		// key" holds for the TTL rather than being asked again every batch.
		k.entries[domain] = entry{signer: signers[domain], expiresAt: expiresAt}
	}
	k.mu.Unlock()

	slog.Info("signing keys loaded",
		"asked", len(domains), "received", len(signers), "ttl", ttl)
}

// For returns the signer for a From address, or an error naming what is wrong.
//
// A parent domain's key is never used for a subdomain: DKIM's d= has to align
// with From, and assuming relaxed alignment would produce a signature that
// fails verification. Better to say so than to send something that arrives
// broken.
//
// An expired entry is still used. Refreshing is Ensure's job; going to the
// network at send time would put a delivery behind an HTTP request.
func (k *Keyring) For(fromEmail string) (*DKIMSigner, error) {
	domain := domainOf(fromEmail)
	if domain == "" {
		return nil, fmt.Errorf("sender address has no domain: %q", fromEmail)
	}

	k.mu.RLock()
	e, known := k.entries[domain]
	k.mu.RUnlock()

	if e.signer != nil {
		return e.signer, nil
	}
	if known {
		return nil, fmt.Errorf(
			"no signing key for %s; verify the domain's DKIM record in the panel", domain)
	}
	return nil, fmt.Errorf("signing key for %s has not been fetched yet", domain)
}

// Domains lists the domains a key is currently held for. Reported at startup
// and in the health check.
func (k *Keyring) Domains() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()

	out := make([]string, 0, len(k.entries))
	for domain, e := range k.entries {
		if e.signer != nil {
			out = append(out, domain)
		}
	}
	return out
}

func domainOf(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}
