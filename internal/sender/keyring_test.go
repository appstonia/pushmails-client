// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package sender

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
)

// keyServer stands in for the central system, counting requests so a test can
// prove the cache is doing its job.
func keyServer(t *testing.T, keys []agent.DKIMKey, ttl int) (*agent.Client, *atomic.Int32, *atomic.Value) {
	t.Helper()

	var calls atomic.Int32
	var asked atomic.Value
	asked.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		asked.Store(r.URL.Query().Get("domains"))

		wanted := map[string]bool{}
		for _, d := range strings.Split(r.URL.Query().Get("domains"), ",") {
			wanted[d] = true
		}
		out := make([]agent.DKIMKey, 0, len(keys))
		for _, key := range keys {
			if wanted[strings.ToLower(key.Domain)] {
				out = append(out, key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agent.DKIMKeysResponse{Keys: out, TTLSeconds: ttl})
	}))
	t.Cleanup(srv.Close)

	return agent.New(srv.URL, "pm_live_test", "test", time.Second), &calls, &asked
}

func testKey(t *testing.T, domain string) agent.DKIMKey {
	t.Helper()
	raw, _ := writeTestKey(t, false)
	return agent.DKIMKey{Domain: domain, Selector: "pm", PrivateKey: string(raw)}
}

// The key that signs a domain is the one whose public half the panel published
// in DNS, so it has to come from the central system rather than a file here.
func TestKeyringSignsWithFetchedKey(t *testing.T) {
	client, _, asked := keyServer(t, []agent.DKIMKey{testKey(t, "example.test")}, 900)
	k := NewKeyring(client)

	k.Ensure(context.Background(), []string{"news@example.test"})

	if got := asked.Load().(string); got != "example.test" {
		t.Errorf("asked for %q, want only the domain in the batch", got)
	}

	signer, err := k.For("news@example.test")
	if err != nil {
		t.Fatalf("no signer after a successful fetch: %v", err)
	}

	msg := &Message{
		Headers: []Header{{"From", "news@example.test"}, {"Subject", "hello"}},
		Body:    []byte("body\r\n"),
	}
	if err := signer.Sign(msg); err != nil {
		t.Fatalf("signing failed: %v", err)
	}
	if msg.Headers[0].Name != "DKIM-Signature" {
		t.Error("the signature was not prepended")
	}
	if !strings.Contains(msg.Headers[0].Value, "d=example.test") {
		t.Errorf("the signature does not carry the sending domain: %q", msg.Headers[0].Value)
	}
}

// Keys are asked for per batch, and a domain already held is not asked for
// again until its TTL runs out: a private key belongs on the wire as rarely as
// it can be.
func TestKeyringCachesUntilTTL(t *testing.T) {
	client, calls, _ := keyServer(t, []agent.DKIMKey{testKey(t, "example.test")}, 900)
	k := NewKeyring(client)

	for range 3 {
		k.Ensure(context.Background(), []string{"news@example.test", "other@example.test"})
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d requests for one domain, want 1", got)
	}
}

// "The server has no key for this domain" is cached as well. Without that, a
// domain waiting on verification would be asked for on every single batch.
func TestKeyringCachesAbsence(t *testing.T) {
	client, calls, _ := keyServer(t, nil, 900)
	k := NewKeyring(client)

	k.Ensure(context.Background(), []string{"news@unverified.test"})
	k.Ensure(context.Background(), []string{"news@unverified.test"})

	if got := calls.Load(); got != 1 {
		t.Errorf("%d requests for a domain with no key, want 1", got)
	}
	if _, err := k.For("news@unverified.test"); err == nil {
		t.Fatal("a domain with no key returned a signer")
	}
}

// A parent domain's key must never sign a subdomain: DKIM's d= has to align
// with From, and a signature that fails verification is worse than an honest
// refusal, because it is reported as delivered.
func TestKeyringDoesNotBorrowParentKey(t *testing.T) {
	client, _, _ := keyServer(t, []agent.DKIMKey{testKey(t, "example.test")}, 900)
	k := NewKeyring(client)

	k.Ensure(context.Background(), []string{"news@example.test"})

	if _, err := k.For("news@mail.example.test"); err == nil {
		t.Error("a subdomain was signed with the parent domain's key")
	}
}

// An address with no domain cannot be signed, and saying so beats a nil
// dereference at send time.
func TestKeyringRejectsAddressWithoutDomain(t *testing.T) {
	client, _, _ := keyServer(t, nil, 900)
	k := NewKeyring(client)

	if _, err := k.For("not-an-address"); err == nil {
		t.Error("an address without a domain returned a signer")
	}
}
