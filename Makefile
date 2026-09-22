.PHONY: build test lint run clean install

# Release version. The source (cmd/pushmails-client/main.go) carries the
# truth; this only refines it. On a tagged checkout the exact tag is stamped
# in, so a binary built between releases says so instead of claiming to be the
# release. Without a tag the value is empty and the source default stands:
# --always is deliberately absent, because a bare commit hash is not a version
# and nobody can tell from one which release an installation is running.
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null)

# Where a packaged build looks for its config file. Override it to match what
# your package actually installs:
#
#   make build CONFIG_PATH=/opt/pushmails/client.cfg
#
# Left empty, the binary falls back to the platform default at runtime
# (/etc/pushmails/client.cfg on Unix, %ProgramData%\PushMails\client.cfg on
# Windows), so a plain `go build` still finds the conventional location.
CONFIG_PATH ?= /etc/pushmails/client.cfg
CONFIG_PKG  := github.com/appstonia/pushmails-client/internal/config

LDFLAGS := -s -w -X $(CONFIG_PKG).DefaultConfigPath=$(CONFIG_PATH)

# Windows builds deliberately leave the path unset: %ProgramData% is
# relocatable, so resolving it at runtime beats freezing C:\ProgramData into
# the binary.
LDFLAGS_WINDOWS := -s -w

ifneq ($(strip $(VERSION)),)
LDFLAGS         += -X main.version=$(VERSION)
LDFLAGS_WINDOWS += -X main.version=$(VERSION)
endif

build:
	go build -ldflags "$(LDFLAGS)" -o pushmails-client ./cmd/pushmails-client

test:
	go test ./...

lint:
	gofmt -l .
	go vet ./...

run: build
	./pushmails-client

install: build
	install -m 0755 pushmails-client /usr/local/bin/pushmails-client

clean:
	rm -f pushmails-client
	rm -rf dist

# Cross-compile for the common platforms — used when cutting a release.
.PHONY: release
release:
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)"         -o dist/pushmails-client-linux-amd64      ./cmd/pushmails-client
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)"         -o dist/pushmails-client-linux-arm64      ./cmd/pushmails-client
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)"         -o dist/pushmails-client-darwin-arm64     ./cmd/pushmails-client
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS_WINDOWS)" -o dist/pushmails-client-windows-amd64.exe ./cmd/pushmails-client
	GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS_WINDOWS)" -o dist/pushmails-client-windows-arm64.exe ./cmd/pushmails-client
	@echo "built into dist/"

# Packaging targets (deb, rpm) live outside this repository; the rule file is
# only present on the machines that cut releases. Users build from source —
# see "Running under systemd" and "Running on Windows" in the README.
-include packaging/linux/Makefile
