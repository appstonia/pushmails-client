// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

//go:build !windows

package config

// platformConfigPath is where a packaged build looks when no path was baked in
// at link time. Distribution packages (deb, rpm) should still set
// DefaultConfigPath explicitly so the value matches what the package installs.
func platformConfigPath() string {
	return "/etc/pushmails/client.cfg"
}
