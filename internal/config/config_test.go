// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.cfg")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseConfigLine(t *testing.T) {
	tests := []struct {
		name, in, wantKey, wantValue string
		wantErr                      bool
	}{
		{"plain", "SMTP_HOST=mail.example.com", "SMTP_HOST", "mail.example.com", false},
		{"spaces around =", "SMTP_PORT = 587", "SMTP_PORT", "587", false},
		{"export prefix", "export SMTP_PORT=25", "SMTP_PORT", "25", false},
		{"comment", "# a comment", "", "", false},
		{"blank", "   ", "", "", false},
		{"trailing comment", "LOG_LEVEL=debug # noisy", "LOG_LEVEL", "debug", false},
		// A '#' is legal inside a token or password, so quoting has to protect it.
		{"quoted keeps hash", `PUSHMAILS_TOKEN="pm_live_a#b"`, "PUSHMAILS_TOKEN", "pm_live_a#b", false},
		{"single quotes are literal", `SMTP_PASSWORD='a\nb'`, "SMTP_PASSWORD", `a\nb`, false},
		{"escapes inside double quotes", `DKIM_KEY_PATH="C:\\keys\\dkim.pem"`, "DKIM_KEY_PATH", `C:\keys\dkim.pem`, false},
		{"no equals sign", "SMTP_HOST mail", "", "", true},
		{"empty key", "=value", "", "", true},
		{"unterminated quote", `SMTP_PASSWORD="abc`, "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, err := parseConfigLine(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseConfigLine(%q) accepted an invalid line", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfigLine(%q): %v", tt.in, err)
			}
			if key != tt.wantKey || value != tt.wantValue {
				t.Errorf("parseConfigLine(%q) = (%q, %q), want (%q, %q)",
					tt.in, key, value, tt.wantKey, tt.wantValue)
			}
		})
	}
}

// A file written by a Windows editor starts with a BOM; without stripping it,
// the first setting in the file would silently never apply.
func TestLoadStripsBOM(t *testing.T) {
	path := writeConfig(t, "\ufeffPUSHMAILS_API=https://api.example.com\nPUSHMAILS_TOKEN=t\nBOUNCE_DOMAIN=bounce.example.test\n")
	cfg, err := Load([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != "https://api.example.com" {
		t.Errorf("APIURL = %q", cfg.APIURL)
	}
}

func TestLoadFromConfigFile(t *testing.T) {
	path := writeConfig(t, `
# central system
PUSHMAILS_API=https://api.example.com
PUSHMAILS_TOKEN=pm_live_from_file
BOUNCE_DOMAIN=bounce.example.test

SMTP_HOST=mail.example.com
SMTP_PORT=587
SMTP_TLS=true
SMTP_TIMEOUT=45s
CONCURRENCY=8
`)
	cfg, err := Load([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Token != "pm_live_from_file" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.SMTP.Host != "mail.example.com" || cfg.SMTP.Port != 587 {
		t.Errorf("SMTP = %s:%d", cfg.SMTP.Host, cfg.SMTP.Port)
	}
	if !cfg.SMTP.TLS {
		t.Error("SMTP_TLS from the file was not applied")
	}
	if cfg.SMTP.Timeout != 45*time.Second {
		t.Errorf("Timeout = %v", cfg.SMTP.Timeout)
	}
	if cfg.Concurrency != 8 {
		t.Errorf("Concurrency = %d", cfg.Concurrency)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile = %q, want %q", cfg.ConfigFile, path)
	}
}

// Precedence is the whole point of the layering: a unit file or container that
// passes an environment variable must not lose to a file left on the server,
// and a flag typed by hand beats both.
func TestPrecedence(t *testing.T) {
	path := writeConfig(t, "PUSHMAILS_API=https://file.example.com\n"+
		"PUSHMAILS_TOKEN=from-file\nSMTP_HOST=file.example.com\n"+
		"BOUNCE_DOMAIN=bounce.example.test\n")

	t.Setenv("SMTP_HOST", "env.example.com")

	cfg, err := Load([]string{"-config", path, "-api", "https://flag.example.com", "-bounce-domain", "bounce.example.test"})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.APIURL != "https://flag.example.com" {
		t.Errorf("flag lost to file/env: APIURL = %q", cfg.APIURL)
	}
	if cfg.SMTP.Host != "env.example.com" {
		t.Errorf("environment lost to file: SMTP_HOST = %q", cfg.SMTP.Host)
	}
	if cfg.Token != "from-file" {
		t.Errorf("file was not used for an unset value: Token = %q", cfg.Token)
	}
}

// A named file that cannot be read has to fail loudly: carrying on means
// running with settings the operator did not intend.
func TestMissingNamedConfigFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.cfg")
	if _, err := Load([]string{"-config", missing, "-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test"}); err == nil {
		t.Error("a missing -config file was accepted")
	}

	t.Setenv("PUSHMAILS_CONFIG", missing)
	if _, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test"}); err == nil {
		t.Error("a missing PUSHMAILS_CONFIG file was accepted")
	}
}

// The build-time default is different: a machine configured purely from flags
// or the environment has no file at all, and that is a supported setup.
func TestMissingDefaultConfigIsFine(t *testing.T) {
	old := DefaultConfigPath
	DefaultConfigPath = filepath.Join(t.TempDir(), "absent.cfg")
	t.Cleanup(func() { DefaultConfigPath = old })

	cfg, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test"})
	if err != nil {
		t.Fatalf("a missing default config file was treated as an error: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want empty", cfg.ConfigFile)
	}
}

func TestDefaultConfigPathOverride(t *testing.T) {
	old := DefaultConfigPath
	t.Cleanup(func() { DefaultConfigPath = old })

	DefaultConfigPath = "/etc/pushmails/client.cfg"
	if got := defaultConfigPath(); got != "/etc/pushmails/client.cfg" {
		t.Errorf("the linker-provided path was ignored: %q", got)
	}

	// Empty means "use the platform default", so a plain `go build` still
	// looks somewhere sensible.
	DefaultConfigPath = ""
	if defaultConfigPath() == "" {
		t.Error("no platform default config path")
	}
}

func TestValidateRejectsIncompleteConfig(t *testing.T) {
	path := writeConfig(t, "PUSHMAILS_API=ftp://api.example.com\n")
	if _, err := Load([]string{"-config", path}); err == nil {
		t.Error("a config without a token and with a bad scheme was accepted")
	}
}

// Direct delivery is the default: this exists so mail leaves the customer's
// own machine, and requiring them to ask for that would make the ordinary case
// the one you have to configure.
func TestDefaultModeIsDirect(t *testing.T) {
	cfg, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeDirect {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeDirect)
	}
	if !cfg.DirectMode() {
		t.Error("DirectMode() disagrees with Mode")
	}
}

// A misspelled mode has to stop the process. Falling back to a default would
// send mail by a route the operator did not choose, and both routes have very
// different consequences for their infrastructure.
func TestUnknownModeRejected(t *testing.T) {
	_, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test", "-mode", "diretc"})
	if err == nil {
		t.Fatal("an unknown -mode was accepted")
	}
	if !strings.Contains(err.Error(), "-mode") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// The EHLO name is matched against the reverse DNS of the connecting address.
// A bare label can never match one, so it is refused at startup rather than
// costing reputation on every message.
func TestEHLOMustBeQualified(t *testing.T) {
	if _, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test",
		"-ehlo", "mailer"}); err == nil {
		t.Error("an unqualified -ehlo was accepted")
	}
	if _, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test",
		"-ehlo", "mail.example.com"}); err != nil {
		t.Errorf("a qualified -ehlo was rejected: %v", err)
	}
}

func TestSourceIPMustParse(t *testing.T) {
	if _, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test",
		"-source-ip", "not-an-address"}); err == nil {
		t.Error("an invalid -source-ip was accepted")
	}
	if _, err := Load([]string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test",
		"-source-ip", "192.0.2.10"}); err != nil {
		t.Errorf("a valid -source-ip was rejected: %v", err)
	}
}

// The SMTP settings only have to hold up in relay mode. Validating them in
// direct mode would refuse a perfectly good configuration over a port nobody
// is going to dial.
func TestSMTPValidatedOnlyInRelayMode(t *testing.T) {
	args := []string{"-api", "https://a.example.com", "-token", "t", "-bounce-domain", "bounce.example.test", "-smtp-port", "0"}

	if _, err := Load(args); err != nil {
		t.Errorf("direct mode rejected an unused SMTP port: %v", err)
	}
	if _, err := Load(append(args, "-mode", "relay")); err == nil {
		t.Error("relay mode accepted an invalid SMTP port")
	}
}

// Bounce handling is not optional. Without an envelope domain of its own,
// every rejection that arrives after the receiving server said 250 goes to the
// From mailbox, nobody reads it, and the dead addresses stay on the list.
func TestBounceDomainRequired(t *testing.T) {
	if _, err := Load([]string{"-api", "https://a.example.com", "-token", "t"}); err == nil {
		t.Fatal("a configuration without a bounce domain was accepted")
	}

	// It is a domain, not a mailbox: the local part is built per message and
	// carries the message id.
	for _, bad := range []string{"bounce@example.test", "localhost", "a b.test"} {
		_, err := Load([]string{"-api", "https://a.example.com", "-token", "t",
			"-bounce-domain", bad})
		if err == nil {
			t.Errorf("-bounce-domain %q was accepted", bad)
		}
	}
}
