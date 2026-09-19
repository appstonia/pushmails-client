// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package mta

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// Resolver looks up a domain's mail servers and caches the answer.
//
// The cache is not an optimisation: one campaign sends thousands of messages
// to the same domain, and a query per message is both slow and rude to the
// resolver.
type Resolver struct {
	// The DNS calls are fields so a test can exercise MX ordering, the
	// fallback to A records and the cache without touching a real resolver.
	lookupMX   func(ctx context.Context, domain string) ([]*net.MX, error)
	lookupHost func(ctx context.Context, domain string) ([]string, error)
	ttl        time.Duration

	mu    sync.Mutex
	cache map[string]*mxEntry
}

type mxEntry struct {
	hosts     []string
	expiresAt time.Time
	// Failures are cached too: when a domain does not exist, going back to DNS
	// for every one of its thousand queued messages helps nobody.
	err error
}

const (
	defaultMXTTL = 15 * time.Minute
	// Failures expire sooner: a freshly created domain may still be
	// propagating.
	failureMXTTL = 2 * time.Minute
)

func NewResolver(ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = defaultMXTTL
	}
	return &Resolver{
		lookupMX:   net.DefaultResolver.LookupMX,
		lookupHost: net.DefaultResolver.LookupHost,
		ttl:        ttl,
		cache:      map[string]*mxEntry{},
	}
}

// Lookup returns the domain's mail servers in preference order.
func (r *Resolver) Lookup(ctx context.Context, domain string) ([]string, error) {
	now := time.Now()

	r.mu.Lock()
	if entry, ok := r.cache[domain]; ok && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.hosts, entry.err
	}
	r.mu.Unlock()

	hosts, err := r.resolve(ctx, domain)

	ttl := r.ttl
	if err != nil {
		ttl = failureMXTTL
	}
	r.mu.Lock()
	r.cache[domain] = &mxEntry{hosts: hosts, err: err, expiresAt: now.Add(ttl)}
	r.mu.Unlock()

	return hosts, err
}

func (r *Resolver) resolve(ctx context.Context, domain string) ([]string, error) {
	records, err := r.lookupMX(ctx, domain)
	if err == nil && len(records) > 0 {
		sort.SliceStable(records, func(i, j int) bool {
			return records[i].Pref < records[j].Pref
		})
		hosts := make([]string, 0, len(records))
		for _, record := range records {
			host := trimDot(record.Host)
			// A lone "." means the domain accepts no mail at all (RFC 7505).
			if host == "" {
				continue
			}
			hosts = append(hosts, host)
		}
		if len(hosts) > 0 {
			return hosts, nil
		}
		return nil, &PermanentError{
			Message: fmt.Sprintf("%s accepts no mail (null MX)", domain),
		}
	}

	// With no MX the domain itself is the mail server (RFC 5321 §5.1).
	if addrs, aErr := r.lookupHost(ctx, domain); aErr == nil && len(addrs) > 0 {
		return []string{domain}, nil
	}

	// Whether the DNS failure is permanent decides the message's fate:
	// NXDOMAIN says the address is invalid (bounce), a timeout says our
	// resolver could not be reached (try again).
	var dnsErr *net.DNSError
	if err != nil && errorAs(err, &dnsErr) && dnsErr.IsNotFound {
		return nil, &PermanentError{
			Message: fmt.Sprintf("domain %s does not exist", domain),
		}
	}
	if err == nil {
		err = fmt.Errorf("no MX record")
	}
	return nil, fmt.Errorf("could not resolve %s: %w", domain, err)
}

func trimDot(host string) string {
	for len(host) > 0 && host[len(host)-1] == '.' {
		host = host[:len(host)-1]
	}
	return host
}

func errorAs(err error, target **net.DNSError) bool {
	dnsErr, ok := err.(*net.DNSError)
	if ok {
		*target = dnsErr
	}
	return ok
}
