# The Linux shell. Invoked from the root Makefile; standalone targets work too.
#
#   make build      build the shell into ../../build
#   make test       the Go suite, with the race detector
#   make vet        vet plus a gofmt check
#   make install    copy the shell and the daemon into PREFIX/bin

OUTDIR  ?= $(CURDIR)/../../build
VERSION ?= 0.1.0-dev
PREFIX  ?= $(HOME)/.local

SHELL_BIN := $(OUTDIR)/starch

# The same flags the daemon is built with, and the same CGO_ENABLED=0. The
# shell has no C dependency — X11 is spoken as a protocol rather than through
# Xlib — so it cross-compiles the way the daemon does.
GO_LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt install clean

build:
	@mkdir -p $(OUTDIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(SHELL_BIN) ./cmd/starch
	@echo "built $(SHELL_BIN) ($(VERSION))"

test:
	go test -race ./...

vet:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

fmt:
	gofmt -w .

# Both binaries into one directory, because the shell looks for starchd next to
# itself first. That is the only arrangement in which a half-finished upgrade
# cannot pair a new shell with an old daemon.
install: build
	@mkdir -p $(PREFIX)/bin
	install -m 0755 $(SHELL_BIN) $(PREFIX)/bin/starch
	install -m 0755 $(OUTDIR)/starchd $(PREFIX)/bin/starchd
	@echo "installed starch and starchd into $(PREFIX)/bin"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; \
	  *) echo "note: $(PREFIX)/bin is not on your PATH";; esac

clean:
	rm -f $(SHELL_BIN)
