// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	version string
}

// pollWait is how long the server holds a claim call open; the HTTP timeout
// must stay above it, or a normal long-poll reads as a failure.
func New(baseURL, token, version string, pollWait time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		version: version,
		http: &http.Client{
			Timeout: pollWait + 30*time.Second,
			Transport: &http.Transport{
				// The claim loop keeps hitting the same host; reuse the
				// connection instead of a TLS handshake every round.
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *Client) SetPollWait(pollWait time.Duration) {
	c.http.Timeout = pollWait + 30*time.Second
}

func (c *Client) Hello(ctx context.Context, hostname string) (*HelloResponse, error) {
	var resp HelloResponse
	err := c.post(ctx, "/api/agent/hello", HelloRequest{
		Version:  c.version,
		Hostname: hostname,
	}, &resp)
	return &resp, err
}

func (c *Client) Claim(ctx context.Context, batchSize int) (*ClaimResponse, error) {
	var resp ClaimResponse
	err := c.post(ctx, "/api/agent/jobs/claim", ClaimRequest{BatchSize: batchSize}, &resp)
	return &resp, err
}

func (c *Client) Ack(ctx context.Context, results []Result) (*AckResponse, error) {
	var resp AckResponse
	err := c.post(ctx, "/api/agent/jobs/ack", AckRequest{Results: results}, &resp)
	return &resp, err
}

// DKIMKeys fetches the signing keys for the given sending domains.
//
// The keys belong to this account and are held in memory only; they are asked
// for per batch rather than all at once, so a domain that is not sending today
// does not have its private key on the wire.
func (c *Client) DKIMKeys(ctx context.Context, domains []string) (*DKIMKeysResponse, error) {
	path := "/api/agent/dkim-keys"
	if len(domains) > 0 {
		path += "?domains=" + url.QueryEscape(strings.Join(domains, ","))
	}

	var resp DKIMKeysResponse
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return &resp, err
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("could not encode request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("could not build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "pushmails-client/"+c.version)

	resp, err := c.http.Do(req)
	if err != nil {
		// Wrapped as an APIError so Retryable() marks it worth another try.
		return &APIError{Code: "network_error", Message: err.Error()}
	}
	defer resp.Body.Close()

	// Capped: a misbehaving server must not exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &APIError{Code: "read_error", Message: err.Error()}
	}

	if resp.StatusCode >= 400 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		if jsonErr := json.Unmarshal(raw, apiErr); jsonErr != nil || apiErr.Code == "" {
			apiErr.Code = "http_error"
			apiErr.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return apiErr
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("could not decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) Preflight(ctx context.Context, req PreflightRequest) (*PreflightResponse, error) {
	var resp PreflightResponse
	err := c.post(ctx, "/api/agent/preflight", req, &resp)
	return &resp, err
}

func (c *Client) Version() string { return c.version }
