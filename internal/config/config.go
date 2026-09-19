// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

// Package config reads configuration from command-line flags and environment
// variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Root of the central PushMails API, e.g. https://api.pushmails.net
	APIURL string
	// Agent token issued by the panel (pm_live_...).
	Token string

	// How mail leaves this machine: ModeDirect or ModeRelay.
	Mode string

	// Domain the envelope sender (Return-Path) is built on, so bounces that
	// arrive after the receiving server accepted the message can be matched
	// back to it. Required: without it there is no bounce feedback at all, and
	// a list nobody cleans is the thing that eventually stops delivering.
	BounceDomain string

	Direct Direct
	SMTP   SMTP

	// How many emails are sent at the same time.
	Concurrency int
	// How many jobs to claim per round. The server reports its ceiling in `hello`.
	BatchSize int

	LogLevel string
	// Exercises the protocol without sending: jobs are released, not marked
	// sent, so no mail leaves and nothing drops out of the queue.
	DryRun bool

	// Config file that was actually read; empty when none was found. Only
	// reported so the startup log can say where the settings came from.
	ConfigFile string
}

// Delivery modes.
const (
	// Straight to the recipient's mail server. No other software involved, and
	// "sent" means the receiving server accepted the message.
	ModeDirect = "direct"
	// Through an SMTP server you nominate, which takes over from there.
	ModeRelay = "relay"
)

type Direct struct {
	// Name given in EHLO. It should be the reverse DNS name of the outgoing
	// address: receiving servers compare the two, and a mismatch costs
	// reputation before the message is even read. Defaults to the hostname.
	EHLO string
	// Address outgoing connections bind to. Empty lets the OS choose, which is
	// right on a single-homed machine. Set it when the machine has several
	// addresses and only one of them carries the PTR and is named in SPF.
	SourceIP string
	// How long one delivery may take, DNS included.
	Timeout time.Duration
}

type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	// Implicit TLS (usually port 465). When off, STARTTLS is attempted.
	TLS bool
	// Skips certificate verification. Only for local servers running a
	// self-signed certificate.
	SkipVerify bool
	Timeout    time.Duration
}

// DirectMode says whether this process does the delivering itself.
func (c *Config) DirectMode() bool { return c.Mode == ModeDirect }

// Precedence: flag > environment variable > file > built-in default. The file
// only fills in what nobody else supplied, so a systemd unit or container that
// passes SMTP_HOST does not lose to a file left behind on the server, and a
// flag typed by hand wins over both.
//
// The file path is baked in at build time (see DefaultConfigPath); -config or
// PUSHMAILS_CONFIG override it.
func Load(args []string) (*Config, error) {
	path, explicit := configFilePath(args)
	values, loaded, err := loadConfigFile(path, explicit)
	if err != nil {
		return nil, err
	}

	src := source{file: values}
	fs := flag.NewFlagSet("pushmails-client", flag.ContinueOnError)
	cfg := &Config{}
	if loaded {
		cfg.ConfigFile = path
	}

	// Registered for -help and so it is not an unknown flag; the value was
	// resolved above, before the defaults were built.
	fs.String("config", path,
		"Config file to read (KEY=value). Overrides PUSHMAILS_CONFIG")

	fs.StringVar(&cfg.APIURL, "api", src.str("PUSHMAILS_API", ""),
		"Central PushMails API address (e.g. https://api.pushmails.net)")
	fs.StringVar(&cfg.Token, "token", src.str("PUSHMAILS_TOKEN", ""),
		"Agent token issued by the panel")

	fs.StringVar(&cfg.Mode, "mode", src.str("PUSHMAILS_MODE", ModeDirect),
		"How mail leaves: direct (straight to the recipient) or relay (through your SMTP server)")

	fs.StringVar(&cfg.Direct.EHLO, "ehlo", src.str("EHLO_NAME", ""),
		"Name given in EHLO; should be the reverse DNS name of the sending address (direct mode)")
	fs.StringVar(&cfg.Direct.SourceIP, "source-ip", src.str("SOURCE_IP", ""),
		"Address outgoing connections bind to; empty lets the OS choose (direct mode)")
	fs.DurationVar(&cfg.Direct.Timeout, "direct-timeout", src.duration("DIRECT_TIMEOUT", 60*time.Second),
		"Time budget for one direct delivery, DNS included")

	fs.StringVar(&cfg.SMTP.Host, "smtp-host", src.str("SMTP_HOST", "localhost"),
		"SMTP server address (relay mode)")
	fs.IntVar(&cfg.SMTP.Port, "smtp-port", src.integer("SMTP_PORT", 25),
		"SMTP port")
	fs.StringVar(&cfg.SMTP.Username, "smtp-user", src.str("SMTP_USERNAME", ""),
		"SMTP username (no authentication when empty)")
	fs.StringVar(&cfg.SMTP.Password, "smtp-pass", src.str("SMTP_PASSWORD", ""),
		"SMTP password")
	fs.BoolVar(&cfg.SMTP.TLS, "smtp-tls", src.boolean("SMTP_TLS", false),
		"Connect with implicit TLS (usually port 465). When off, STARTTLS is attempted")
	fs.BoolVar(&cfg.SMTP.SkipVerify, "smtp-skip-verify", src.boolean("SMTP_SKIP_VERIFY", false),
		"Skip TLS certificate verification (local testing only)")
	fs.DurationVar(&cfg.SMTP.Timeout, "smtp-timeout", src.duration("SMTP_TIMEOUT", 30*time.Second),
		"SMTP operation timeout")

	fs.StringVar(&cfg.BounceDomain, "bounce-domain", src.str("BOUNCE_DOMAIN", ""),
		"Domain the envelope sender is built on, so bounces can be matched back (required)")

	fs.IntVar(&cfg.Concurrency, "concurrency", src.integer("CONCURRENCY", 4),
		"Number of emails sent concurrently")
	fs.IntVar(&cfg.BatchSize, "batch-size", src.integer("BATCH_SIZE", 50),
		"Number of jobs claimed per round")
	fs.StringVar(&cfg.LogLevel, "log-level", src.str("LOG_LEVEL", "info"),
		"Log level: debug, info, warn, error")
	fs.BoolVar(&cfg.DryRun, "dry-run", src.boolean("DRY_RUN", false),
		"Exercise the protocol without sending email")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var problems []string

	switch c.Mode {
	case ModeDirect:
		// An EHLO name is not required — the hostname stands in — but it has
		// to look like one when given: receivers match it against the PTR of
		// the connecting address, and a bare label matches nothing.
		if c.Direct.EHLO != "" && !strings.Contains(c.Direct.EHLO, ".") {
			problems = append(problems,
				"-ehlo must be a fully qualified name (e.g. mail.pushmails.net)")
		}
		if c.Direct.SourceIP != "" && net.ParseIP(c.Direct.SourceIP) == nil {
			problems = append(problems,
				fmt.Sprintf("-source-ip is not an IP address: %s", c.Direct.SourceIP))
		}
		if c.Direct.Timeout <= 0 {
			problems = append(problems, "-direct-timeout must be positive")
		}
	case ModeRelay:
		if c.SMTP.Host == "" {
			problems = append(problems, "-smtp-host cannot be empty in relay mode")
		}
		if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			problems = append(problems, "-smtp-port must be between 1 and 65535")
		}
	default:
		problems = append(problems,
			fmt.Sprintf("-mode must be %q or %q, not %q", ModeDirect, ModeRelay, c.Mode))
	}

	if c.APIURL == "" {
		problems = append(problems, "-api is required (or PUSHMAILS_API)")
	} else {
		c.APIURL = strings.TrimSuffix(c.APIURL, "/")
		if !strings.HasPrefix(c.APIURL, "http://") && !strings.HasPrefix(c.APIURL, "https://") {
			problems = append(problems, "-api must start with http:// or https://")
		}
	}
	if c.Token == "" {
		problems = append(problems, "-token is required (or PUSHMAILS_TOKEN)")
	}
	if c.Concurrency < 1 {
		problems = append(problems, "-concurrency must be at least 1")
	}
	if c.BatchSize < 1 {
		problems = append(problems, "-batch-size must be at least 1")
	}

	// Required rather than optional, and the reason is what happens without
	// it: every bounce that arrives after the receiving server said 250 goes
	// to the From mailbox, nobody reads it, dead addresses stay on the list
	// and the sending reputation they cost is the customer's own.
	switch {
	case c.BounceDomain == "":
		problems = append(problems,
			"-bounce-domain is required (or BOUNCE_DOMAIN); see the README on bounce handling")
	case !strings.Contains(c.BounceDomain, "."):
		problems = append(problems,
			fmt.Sprintf("-bounce-domain must be a domain name: %s", c.BounceDomain))
	case strings.ContainsAny(c.BounceDomain, "@ "):
		problems = append(problems,
			fmt.Sprintf("-bounce-domain is a domain, not an address: %s", c.BounceDomain))
	}

	if len(problems) > 0 {
		return errors.New("configuration error:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

// source resolves a setting: environment first, file second, built-in default
// last.
type source struct {
	file map[string]string
}

func (s source) str(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v, ok := s.file[key]; ok && v != "" {
		return v
	}
	return fallback
}

// A malformed value falls back to the default rather than failing: validate()
// then reports whatever ends up out of range, naming the setting instead of the
// type.
func (s source) integer(key string, fallback int) int {
	if v := s.str(key, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func (s source) boolean(key string, fallback bool) bool {
	if v := s.str(key, ""); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func (s source) duration(key string, fallback time.Duration) time.Duration {
	if v := s.str(key, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
