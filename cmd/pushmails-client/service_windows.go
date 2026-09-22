// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Appstonia

//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceName is what the service is registered as. The installer, `sc.exe`
// and the log lines all have to agree on it.
const serviceName = "pushmails-client"

// stopWait is how long the service manager is told to wait for a stop. A
// graceful shutdown finishes the batch in hand and reports the results, which
// in direct mode can take as long as one delivery timeout; a wait hint shorter
// than that would have the manager declare the service hung while it was doing
// exactly what it was asked to.
const stopWait = 2 * time.Minute

// runningAsService reports whether the service manager started this process,
// rather than a person at a console.
//
// The question is asked of the operating system instead of a flag: a flag can
// be left out of the registered command line, and the failure mode then is a
// service that starts, never reports its status and is killed after thirty
// seconds with "the service did not respond in a timely fashion" — the least
// informative error Windows produces.
func runningAsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		// Nothing can be logged yet: if this is a service there is no console
		// to write to. Treating it as a console run keeps the standalone case
		// working, and a real service would fail visibly in the manager.
		return false
	}
	return is
}

func runService() error {
	// A service has no stdout. Everything from here on, including a config
	// error that stops the process before it starts, has to reach a file or it
	// reaches nobody.
	closeLog, err := logToFile()
	if err != nil {
		return err
	}
	defer closeLog()

	return svc.Run(serviceName, &clientService{})
}

type clientService struct{}

// Execute is the service manager's side of the lifecycle: report status, run
// the client, and turn a stop request into a cancelled context.
func (*clientService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runClient(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				cancel()
				return false, waitForShutdown(done, status)
			default:
				// Nothing else is accepted, so nothing else should arrive.
				slog.Warn("unexpected service control request", "cmd", req.Cmd)
			}
		case err := <-done:
			// The client stopped on its own — a bad token, an unreadable
			// config. Exiting with a non-zero code is what makes the service
			// manager's recovery actions fire; returning 0 would leave a
			// service that looks stopped on purpose.
			if err != nil {
				slog.Error("client stopped", "error", err)
				return false, 1
			}
			return false, 0
		}
	}
}

// waitForShutdown keeps the service manager informed while the client finishes
// the batch in hand. Without the checkpoints a shutdown that takes longer than
// the wait hint is reported as a hung service and the process is killed —
// losing exactly the graceful report the wait is there to allow.
func waitForShutdown(done <-chan error, status chan<- svc.Status) uint32 {
	deadline := time.Now().Add(stopWait)
	checkpoint := uint32(1)

	report := func() {
		status <- svc.Status{
			State:      svc.StopPending,
			CheckPoint: checkpoint,
			WaitHint:   uint32(time.Until(deadline).Milliseconds()),
		}
		checkpoint++
	}
	report()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			if err != nil {
				slog.Error("shutdown returned an error", "error", err)
				return 1
			}
			return 0
		case <-ticker.C:
			if time.Now().After(deadline) {
				// Report it rather than hiding it: the batch was not finished
				// and the central system will hand those jobs out again when
				// the lease expires.
				slog.Error("shutdown did not finish in time", "waited", stopWait)
				return 1
			}
			report()
		}
	}
}

// logToFile points the logger at %ProgramData%\PushMails\client.log, next to
// the config file, and returns a function that closes it.
//
// PUSHMAILS_LOG_FILE overrides the path. It is read from the environment
// rather than the config file because a config error is one of the things that
// has to end up in this log.
func logToFile() (func(), error) {
	path := os.Getenv("PUSHMAILS_LOG_FILE")
	if path == "" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		path = filepath.Join(base, "PushMails", "client.log")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("log directory: %w", err)
	}

	w, err := newRollingFile(path, 10<<20)
	if err != nil {
		return nil, fmt.Errorf("log file: %w", err)
	}
	logOutput = w
	return func() { _ = w.Close() }, nil
}

// rollingFile keeps the log from growing without end on a machine nobody logs
// in to. One generation is kept: at the limit the file becomes .1 and a new
// one starts. There is no log rotation on Windows the way logrotate or the
// journal provide it, and a service that runs for a year would otherwise leave
// a file that cannot be opened.
type rollingFile struct {
	mu    sync.Mutex
	path  string
	limit int64
	size  int64
	f     *os.File
}

func newRollingFile(path string, limit int64) (*rollingFile, error) {
	r := &rollingFile{path: path, limit: limit}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rollingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rollingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size+int64(len(p)) > r.limit {
		if err := r.roll(); err != nil {
			// A failed roll must not take the log with it; keep writing to the
			// file at hand and let it exceed the limit.
			r.size = 0
		}
	}

	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rollingFile) roll() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		// Reopen the original: dropping the handle here would silence the
		// service for as long as it runs.
		_ = r.open()
		return err
	}
	return r.open()
}

func (r *rollingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
