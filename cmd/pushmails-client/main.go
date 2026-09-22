// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Appstonia

// Command pushmails-client sends your PushMails campaigns from your own server.
//
// It connects to the central PushMails system, pulls the emails waiting to be
// sent, delivers them through your local SMTP server and reports the results.
// The mail leaves your infrastructure, from your IP address.
//
// Distributed under the MIT license; see the LICENSE file for details.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/appstonia/pushmails-client/internal/agent"
	"github.com/appstonia/pushmails-client/internal/config"
	"github.com/appstonia/pushmails-client/internal/mta"
	"github.com/appstonia/pushmails-client/internal/runner"
)

// version is the release this source tree represents. Bump it when cutting one;
// on a tagged checkout the Makefile stamps the exact tag over it, so a build
// between releases does not claim to be the release.
//
// It travels with every request (User-Agent, hello, preflight), which is how a
// version-specific bug is traced to the installations it affects.
//
//	go build -ldflags "-X main.version=1.0.1" ./cmd/pushmails-client
var version = "1.0.0"

func main() {
	if err := run(); err != nil {
		// On a flag error the flag package already printed the usage.
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func run() error {
	// Asking for the version must not trip over config validation.
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" {
			printVersion()
			return nil
		}
	}

	// Started by the Windows service manager rather than from a console: the
	// SCM owns the lifecycle, so it builds the context instead of signals.
	// Always false on every other platform.
	if runningAsService() {
		return runService()
	}

	// A shutdown signal cancels the context; the runner finishes the batch in
	// hand, reports the results and then exits.
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runClient(ctx)
}

// runClient is everything the process does once someone has decided how it
// will be told to stop. The console path cancels ctx on a signal, the Windows
// service path on a stop request from the service manager.
func runClient(ctx context.Context) error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)

	// "Which file did it actually read" is the first question when a setting
	// does not take effect, and on a packaged install the answer is a
	// build-time default nobody typed.
	configFile := cfg.ConfigFile
	if configFile == "" {
		configFile = "(none)"
	}
	slog.Info("starting pushmails-client",
		"version", version,
		"api", cfg.APIURL,
		"mode", cfg.Mode,
		"concurrency", cfg.Concurrency,
		"config", configFile,
	)

	// Signing keys are fetched per batch from the central system, one per
	// verified sending domain. There is nothing to configure and nothing to
	// keep on disk: the key that signs a domain is the one whose public half
	// the panel published in its DNS, so a locally generated key could only
	// ever produce a signature that fails to verify.
	slog.Info("signing keys come from the central system",
		"bounce_domain", cfg.BounceDomain)

	if cfg.DryRun {
		slog.Warn("dry-run mode: no email will be sent; claimed jobs are " +
			"released back to the queue")
	}

	transport, err := buildTransport(cfg)
	if err != nil {
		return err
	}

	// Initial long-poll estimate; `hello` replaces it with the real value.
	client := agent.New(cfg.APIURL, cfg.Token, version, 25*time.Second)

	if err := runner.New(cfg, client, transport).Run(ctx); err != nil {
		return err
	}

	slog.Info("shut down")
	return nil
}

// buildTransport picks how mail leaves this machine.
//
// Direct is the default because it is the whole point of running this: the
// message goes from your server to the recipient's, and "sent" means their
// server took it. A relay is there for the setups that already have one in the
// path and have to keep it.
func buildTransport(cfg *config.Config) (runner.Transport, error) {
	if !cfg.DirectMode() {
		slog.Info("relay mode: mail is handed to your SMTP server",
			"smtp", fmt.Sprintf("%s:%d", cfg.SMTP.Host, cfg.SMTP.Port))
		return mta.NewRelay(cfg.SMTP), nil
	}

	ehlo := cfg.Direct.EHLO
	if ehlo == "" {
		name, err := os.Hostname()
		if err != nil || !strings.Contains(name, ".") {
			// Receiving servers compare the EHLO name with the PTR of the
			// address we connect from. A bare label ("mailer") matches
			// nothing, and that costs reputation on every message — worth a
			// warning, not a refusal, because plenty of installations deliver
			// fine without it.
			slog.Warn("no fully qualified EHLO name; set -ehlo to the reverse "+
				"DNS name of your sending address", "hostname", name)
		}
		ehlo = name
	}

	var localIP net.IP
	if cfg.Direct.SourceIP != "" {
		localIP = net.ParseIP(cfg.Direct.SourceIP)
	}

	slog.Info("direct mode: mail goes straight to the recipient's mail server",
		"ehlo", ehlo, "source_ip", cfg.Direct.SourceIP)
	return mta.NewDirect(ehlo, localIP, cfg.Direct.Timeout), nil
}

func printVersion() {
	fmt.Printf(`pushmails-client %s
Copyright (c) 2026 Appstonia
License MIT: https://opensource.org/licenses/MIT
There is NO WARRANTY, to the extent permitted by law.
`, version)
}

// logOutput is where the log lines go. A console build writes to stdout; the
// Windows service path replaces this with a file before anything is logged,
// because a service has no console to write to.
var logOutput io.Writer = os.Stdout

func setupLogging(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: lvl})))
}
