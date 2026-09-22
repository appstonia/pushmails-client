// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Appstonia

//go:build !windows

package main

// runningAsService is a Windows notion. Everywhere else the supervisor —
// systemd, OpenRC, a terminal — starts the process the ordinary way and stops
// it with a signal, which the console path already handles.
func runningAsService() bool { return false }

func runService() error { panic("not reachable: runningAsService is false") }
