// SPDX-License-Identifier: MIT
// Copyright (C) 2026 Appstonia

package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// DefaultConfigPath is baked in at link time so a package can point the binary
// at the file it actually installs:
//
//	go build -ldflags "-X github.com/appstonia/pushmails-client/internal/config.DefaultConfigPath=/etc/pushmails/client.cfg"
//
// It must stay an uninitialised string for -X to take; the linker only patches
// variables declared empty or set to a constant. Left empty, the platform
// default applies, so a plain `go build` still finds the conventional location
// on Linux and Windows.
var DefaultConfigPath string

func defaultConfigPath() string {
	if DefaultConfigPath != "" {
		return DefaultConfigPath
	}
	return platformConfigPath()
}

// required is true only when the user named a file (-config or
// PUSHMAILS_CONFIG). A missing default file is fine — running purely from flags
// or the environment is supported. A named file that cannot be read fails
// loudly: carrying on means running with the wrong settings.
func loadConfigFile(path string, required bool) (map[string]string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("could not read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		key, value, err := parseConfigLine(scanner.Text())
		if err != nil {
			return nil, false, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if key == "" {
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("could not read %s: %w", path, err)
	}
	return values, true, nil
}

// Blank lines and comments return an empty key.
//
// The format is deliberately narrow: `KEY=value`, with an optional `export` in
// front so the file can be sourced by a shell or used as a systemd
// EnvironmentFile. In an unquoted value a trailing ` #` starts a comment, as in
// dotenv. Quoted values keep it, because `#` occurs inside passwords and
// tokens.
func parseConfigLine(raw string) (key, value string, err error) {
	line := strings.TrimSpace(raw)
	// A Windows editor leaves a UTF-8 BOM here; it would otherwise become part
	// of the first key name.
	line = strings.TrimPrefix(line, "\ufeff")
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", nil
	}
	line = strings.TrimPrefix(line, "export ")

	name, rest, found := strings.Cut(line, "=")
	if !found {
		return "", "", fmt.Errorf("expected `KEY=value`: %q", raw)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("empty key: %q", raw)
	}

	rest = strings.TrimSpace(rest)
	switch {
	case strings.HasPrefix(rest, `"`):
		unquoted, err := cutQuoted(rest, '"')
		if err != nil {
			return "", "", err
		}
		// Escapes expand inside double quotes, so a Windows path fits on one line.
		return name, strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`).Replace(unquoted), nil
	case strings.HasPrefix(rest, `'`):
		unquoted, err := cutQuoted(rest, '\'')
		if err != nil {
			return "", "", err
		}
		// Single quotes are literal: nothing inside is interpreted.
		return name, unquoted, nil
	default:
		if comment := strings.Index(rest, " #"); comment >= 0 {
			rest = rest[:comment]
		}
		return name, strings.TrimSpace(rest), nil
	}
}

func cutQuoted(rest string, quote byte) (string, error) {
	end := strings.LastIndexByte(rest, quote)
	if end <= 0 {
		return "", fmt.Errorf("unterminated quote: %q", rest)
	}
	return rest[1:end], nil
}

// The flag is scanned by hand before the flag set is built: the file feeds the
// flag defaults, so waiting for fs.Parse is too late. The second return value
// says whether the path was named explicitly.
func configFilePath(args []string) (path string, explicit bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name != "config" {
			continue
		}
		if inline {
			return value, true
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
	}
	if fromEnv := os.Getenv("PUSHMAILS_CONFIG"); fromEnv != "" {
		return fromEnv, true
	}
	return defaultConfigPath(), false
}
