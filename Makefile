# Starch — system-wide text rewriter.
#
#   make            build everything
#   make test       run the Go and Swift test suites
#   make run        build and launch the app
#   make help       list every target

APP_NAME  := Starch
BUNDLE_ID := dev.starch.Starch
MACOS_DIR := apps/macos
LINUX_DIR := apps/linux

BUILD_DIR := build
DAEMON    := $(BUILD_DIR)/starchd
APP       := $(MACOS_DIR)/build/$(APP_NAME).app

# Stamped from the tag, because everyone installs this by building it. Without
# that, every report says "0.1.0-dev" and there is no way to ask which build
# someone is on. --dirty marks uncommitted work so a local experiment is never
# mistaken for a release. Falls back for a source tarball, which has no .git.
GIT_VERSION := $(shell git describe --tags --dirty --always 2>/dev/null | sed 's/^v//')
VERSION   ?= $(if $(GIT_VERSION),$(GIT_VERSION),0.1.0-dev)
ARCH      ?= arm64
SIGN_IDENTITY ?= -

# Trimmed paths and no symbol table: the daemon is shipped inside the bundle
# and nothing benefits from embedding the build machine's directory layout.
GO_LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: help build daemon app run install test test-go test-swift \
        test-linux-shell linux linux-install \
        fmt vet check clean register-services reset-permissions logs

## help: list available targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## build: build the daemon and the app bundle
build: app

## daemon: build starchd
daemon:
	@mkdir -p $(BUILD_DIR)
	@# Removed rather than overwritten: a previous ARCH=universal leaves a fat
	@# binary here, and `go build -o` refuses to overwrite a file it did not
	@# produce — so switching architectures failed with "already exists and is
	@# not an object file" until you knew to run `make clean`.
	@rm -f $(DAEMON)
ifeq ($(ARCH),universal)
	@# CGO_ENABLED=0 keeps cross-compilation to Windows and Linux a one-liner,
	@# and constrains any future dependency to pure Go for the same reason.
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(BUILD_DIR)/starchd-arm64 ./cmd/starchd
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(BUILD_DIR)/starchd-amd64 ./cmd/starchd
	lipo -create -output $(DAEMON) $(BUILD_DIR)/starchd-arm64 $(BUILD_DIR)/starchd-amd64
	@rm -f $(BUILD_DIR)/starchd-arm64 $(BUILD_DIR)/starchd-amd64
else
	CGO_ENABLED=0 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(DAEMON) ./cmd/starchd
endif
	@echo "built $(DAEMON) ($(VERSION))"

## app: build the macOS app bundle with the daemon inside it
app: daemon
	@$(MAKE) -C $(MACOS_DIR) app \
		DAEMON="$(CURDIR)/$(DAEMON)" \
		VERSION="$(VERSION)" \
		ARCH="$(ARCH)" \
		SIGN_IDENTITY="$(SIGN_IDENTITY)"

## linux: build the daemon and the Linux shell (run this on Linux)
##        The macOS targets above build an app bundle and do not apply here.
linux: daemon
	@$(MAKE) -C $(LINUX_DIR) build \
		OUTDIR="$(CURDIR)/$(BUILD_DIR)" \
		VERSION="$(VERSION)"

## linux-install: install the Linux shell and daemon into PREFIX/bin
##                PREFIX defaults to ~/.local, so no root is needed.
linux-install: daemon
	@$(MAKE) -C $(LINUX_DIR) install \
		OUTDIR="$(CURDIR)/$(BUILD_DIR)" \
		VERSION="$(VERSION)"

## run: build and launch the app (quits any running copy first)
run: app
	@pkill -x $(APP_NAME) 2>/dev/null || true
	@open "$(APP)"
	@echo "launched. It is a menu bar app — look in the status bar, not the Dock."

## install: copy the app to /Applications and register it
##          Services discovery in M1 needs the app to live here.
install: app
	@pkill -x $(APP_NAME) 2>/dev/null || true
	@rm -rf "/Applications/$(APP_NAME).app"
	@mv "$(APP)" /Applications/
	@$(MAKE) register-services
	@echo "installed /Applications/$(APP_NAME).app"

## test: run every test suite
test: test-go test-linux-shell test-swift

## test-go: run the Go tests with the race detector
test-go:
	go test -race ./...

## test-linux-shell: run the Linux shell's Go tests
##                    Its own module, so the root suite does not reach it. It
##                    builds and runs on macOS too, which is the only reason it
##                    can be developed here at all.
test-linux-shell:
	@$(MAKE) -C $(LINUX_DIR) test

## test-swift: run the Swift tests
test-swift:
	@$(MAKE) -C $(MACOS_DIR) test

## fmt: format the Go sources
fmt:
	gofmt -w .
	@$(MAKE) -C $(LINUX_DIR) fmt

## vet: run go vet and check formatting, in both modules
vet:
	go vet ./...
	@$(MAKE) -C $(LINUX_DIR) vet
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

## check: vet plus the full test suite
check: vet test

## register-services: make the right-click Services entry discoverable
##                    macOS caches the Services list aggressively and generally
##                    only picks up an app living in /Applications. Expect to
##                    need this, and a re-login, more than once.
register-services:
	@/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister \
		-f "/Applications/$(APP_NAME).app" 2>/dev/null || true
	@/System/Library/CoreServices/pbs -flush
	@echo "flushed the Services cache. If the menu entry is still missing, log out and back in."

## reset-permissions: revoke Accessibility so the next launch re-prompts
##                    TCC keys the grant to the code signature, so every
##                    ad-hoc rebuild is a new app as far as macOS is concerned.
reset-permissions:
	@tccutil reset Accessibility $(BUNDLE_ID) || true
	@echo "reset Accessibility for $(BUNDLE_ID)"

## logs: stream the app and daemon logs
logs:
	@echo "streaming logs for $(BUNDLE_ID) — ^C to stop"
	@/usr/bin/log stream --style compact --predicate 'subsystem == "$(BUNDLE_ID)"'

## clean: remove all build output
clean:
	rm -rf $(BUILD_DIR)
	@$(MAKE) -C $(MACOS_DIR) clean
	@$(MAKE) -C $(LINUX_DIR) clean
