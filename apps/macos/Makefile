# Assembles the macOS app bundle.
#
# SwiftPM cannot emit a .app, so this target does the bundling, signing and
# Launch Services chores. Normally driven from the repository root Makefile,
# which builds the daemon first and passes its path in.

APP_NAME   := Starch
BUNDLE_ID  := dev.starch.Starch
VERSION    ?= 0.0.0-dev
CONFIG     ?= release

# Path to a prebuilt starchd, supplied by the root Makefile.
DAEMON     ?=

# Ad-hoc by default. Pass a real identity to keep TCC grants across rebuilds:
#   make app SIGN_IDENTITY="Starch Dev"
# See CONTRIBUTING.md — an ad-hoc signature changes on every build, so macOS
# treats each build as a new app and re-prompts for Accessibility.
SIGN_IDENTITY ?= -

# arm64, x86_64, or universal.
#
# A universal build compiles each slice on its own and lipos them together,
# rather than passing `--arch arm64 --arch x86_64` in one invocation. That
# combination fails outright on Xcode 16.4 — "Unexpected duplicate tasks",
# "SWIFT_VERSION '' is unsupported" — and succeeds on Xcode 26, so a build that
# works here would break for anyone a version behind. Per-slice works on both,
# and it is already how the root Makefile builds the Go daemon, so there is one
# universal-binary story rather than two.
ARCH ?= arm64
ifeq ($(ARCH),universal)
SLICES := arm64 x86_64
else
SLICES := $(ARCH)
endif
FIRST_SLICE := $(firstword $(SLICES))

BUILD_DIR := build
APP       := $(BUILD_DIR)/$(APP_NAME).app
CONTENTS  := $(APP)/Contents

# XCTest ships with Xcode, not with Command Line Tools. If the active developer
# directory has no XCTest but Xcode is installed, point at it rather than
# failing with "no such module 'XCTest'".
XCTEST_PATH := $(shell xcode-select -p 2>/dev/null)/Platforms/MacOSX.platform/Developer/Library/Frameworks/XCTest.framework
ifeq ($(wildcard $(XCTEST_PATH)),)
ifneq ($(wildcard /Applications/Xcode.app),)
export DEVELOPER_DIR := /Applications/Xcode.app/Contents/Developer
endif
endif

.PHONY: all app build test clean print-bin-path

all: app

## build: compile the Swift executable only
build:
	@for slice in $(SLICES); do \
		echo "swift build -c $(CONFIG) --arch $$slice"; \
		swift build -c $(CONFIG) --arch $$slice || exit 1; \
	done

## test: run the Swift unit tests
test:
	@if ! test -d "$$(xcode-select -p)/Platforms/MacOSX.platform/Developer/Library/Frameworks/XCTest.framework"; then \
		echo "error: XCTest not found. It ships with Xcode, not Command Line Tools."; \
		echo "       Install Xcode, then: sudo xcode-select -s /Applications/Xcode.app"; \
		exit 1; \
	fi
	swift test

print-bin-path:
	@swift build -c $(CONFIG) --arch $(FIRST_SLICE) --show-bin-path

## app: assemble, populate and sign $(APP)
app: build
	@if [ -z "$(DAEMON)" ]; then \
		echo "error: DAEMON is not set. Run 'make app' from the repository root."; \
		exit 1; \
	fi
	@if [ ! -x "$(DAEMON)" ]; then \
		echo "error: no daemon binary at $(DAEMON). Run 'make daemon' first."; \
		exit 1; \
	fi
	@rm -rf "$(APP)"
	@mkdir -p "$(CONTENTS)/MacOS" "$(CONTENTS)/Resources"

	@# lipo even for one slice: it copies in that case, so there is a single
	@# code path rather than a branch that only the universal build exercises.
	@slices=""; \
	for slice in $(SLICES); do \
		slices="$$slices $$(swift build -c $(CONFIG) --arch $$slice --show-bin-path)/$(APP_NAME)"; \
	done; \
	lipo -create -output "$(CONTENTS)/MacOS/$(APP_NAME)" $$slices
	@cp "$(DAEMON)" "$(CONTENTS)/Resources/starchd"
	@chmod +x "$(CONTENTS)/Resources/starchd"

	@sed 's/__VERSION__/$(VERSION)/g' Resources/Info.plist > "$(CONTENTS)/Info.plist"
	@printf 'APPL????' > "$(CONTENTS)/PkgInfo"

	@# Sign inside out: nested code first, then the bundle that contains it.
	@# --deep is deprecated and gets this wrong, so do it explicitly.
	@codesign --force --sign "$(SIGN_IDENTITY)" \
		--identifier "$(BUNDLE_ID).starchd" \
		"$(CONTENTS)/Resources/starchd"
	@codesign --force --sign "$(SIGN_IDENTITY)" \
		--identifier "$(BUNDLE_ID)" \
		"$(APP)"
	@codesign --verify --strict "$(APP)"

	@echo "built $(APP) ($(ARCH), version $(VERSION), signed as '$(SIGN_IDENTITY)')"

clean:
	rm -rf .build $(BUILD_DIR)
