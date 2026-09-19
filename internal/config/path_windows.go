// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package config

import (
	"os"
	"path/filepath"
)

// platformConfigPath is where a packaged build looks when no path was baked in
// at link time.
//
// ProgramData is read from the environment rather than hardcoded to
// C:\ProgramData: the folder is relocatable, and a machine that moved it would
// otherwise get a path that does not exist. The literal is only the fallback
// for the rare case where the variable is missing.
func platformConfigPath() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "PushMails", "client.cfg")
}
