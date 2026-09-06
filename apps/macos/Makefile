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
ARCH ?= arm64
ifeq ($(ARCH),universal)
SWIFT_ARCH_FLAGS := --arch arm64 --arch x86_64
else
SWIFT_ARCH_FLAGS := --arch $(ARCH)
endif

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
	swift build -c $(CONFIG) $(SWIFT_ARCH_FLAGS)

## test: run the Swift unit tests
test:
	@if ! test -d "$$(xcode-select -p)/Platforms/MacOSX.platform/Developer/Library/Frameworks/XCTest.framework"; then \
		echo "error: XCTest not found. It ships with Xcode, not Command Line Tools."; \
		echo "       Install Xcode, then: sudo xcode-select -s /Applications/Xcode.app"; \
		exit 1; \
	fi
	swift test

print-bin-path:
	@swift build -c $(CONFIG) $(SWIFT_ARCH_FLAGS) --show-bin-path

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

	@cp "$$(swift build -c $(CONFIG) $(SWIFT_ARCH_FLAGS) --show-bin-path)/$(APP_NAME)" "$(CONTENTS)/MacOS/$(APP_NAME)"
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
