# Contributing

## Prerequisites

| | |
|---|---|
| macOS | 13.0 or later |
| Go | 1.23+ (`CGO_ENABLED=0` — see below) |
| Xcode | required to **run tests** |

Command Line Tools alone will build and run the app, but ships neither XCTest
nor swift-testing, so `make test-swift` cannot work. If you have Xcode
installed but not selected, the Makefile points at it automatically; to make it
permanent:

```sh
sudo xcode-select -s /Applications/Xcode.app
```

## Everyday commands

```sh
make            # build daemon + app bundle
make run        # build and launch
make test       # Go (with -race) and Swift suites
make check      # vet + gofmt check + tests
make logs       # stream app and daemon logs
make clean
make help       # everything else
```

## Layout

```
cmd/starchd/            daemon entrypoint
internal/
  brand/                the product name, in one place
  config/               environment-driven configuration
  server/               UDS listener, routes, SSE encoder
api/                    the wire contract — other shells depend on it
apps/macos/
  Sources/StarchKit/    testable logic: framing, client, lifecycle, settings
  Sources/Starch/       AppKit shell: menu bar, windows, hot key
  Tests/
```

`internal/` must stay OS-independent. Anything macOS-specific that leaks in
there is a bug — the whole point of the split is that Windows and Linux shells
reuse it unchanged. `os.UserConfigDir` and friends are the portable way to do
per-platform paths; build tags are a last resort.

Changing anything in `api/` is a contract change. Read the versioning rules at
the bottom of [`api/README.md`](api/README.md) first.

## Dependency policy

**Ask before adding any third-party dependency**, in either language. Today the
Go module has zero and the Swift package has zero; both use only the standard
library and system frameworks. The one dependency already agreed is
`modernc.org/sqlite` for M4, chosen because it is pure Go and keeps
`CGO_ENABLED=0` — which is what makes cross-compiling to Windows and Linux a
one-line build.

## Quirks that will otherwise cost you an hour

### Services registration is finicky

*(Relevant from M1, when the right-click entry lands.)*

macOS caches the Services list aggressively and generally only notices an app
that lives in `/Applications`. After changing anything in `NSServices`:

```sh
make install            # copies to /Applications and registers
make register-services  # lsregister -f + pbs -flush
```

If the menu entry still does not appear, log out and back in. This is normal
and not a sign you have done something wrong.

Three failure modes look identical to "registration is broken" and none of them
are. Check all three before touching the plist — in this order, because this is
the order of likelihood:

**macOS ships the service switched off.** This is the usual answer. A newly
registered third-party text service is disabled by default: it appears in
System Settings → Keyboard → Keyboard Shortcuts → Services → Text with its
checkbox clear, and appears nowhere else until ticked. Nothing is logged and
nothing is wrong. Check the live state with:

```sh
defaults read pbs NSServicesStatus
```

An absent `NSServicesStatus` key, or no entry for
`dev.starch.Starch - Starch - rewriteSelection`, means never-configured, which
behaves as off. `ServicesMenu.state()` reads exactly this, and the app surfaces
it in the menu bar and the set-up guide.

Note the key format: `"<bundle id> - <menu item title> - <NSMessage>"`. The menu
title is part of it, so renaming the entry in `NSServices` silently resets every
existing user's choice and hands them a fresh unticked service with no
explanation.

The other two:

**The host app caches the Services menu at launch.** An app that was already
running when the service was registered will not show it until you quit and
reopen *that* app — not Starch. This is the usual explanation for an entry that
is provably registered and still invisible.

**`NSReturnTypes` hides the item everywhere read-only.** A service declaring
return types is only enabled where the selection is editable, because macOS
needs somewhere to put what comes back. It is easy to read the declaration as
*enabling* placement in editable contexts; it does the opposite, restricting to
them. Starch is send-only and must stay that way — it does its own replacement.

To check what is actually registered, rather than what you think you declared:

```sh
/System/Library/CoreServices/pbs -dump | grep -A14 dev.starch.Starch
```

To exercise the handler without a context menu at all:

```swift
// swift thisfile.swift — reaches the service directly, bypassing menu display
import AppKit
let pb = NSPasteboard(name: .init("Probe"))
pb.clearContents()
pb.setString("some text", forType: .string)
print(NSPerformService("Starch", pb) ? "dispatched" : "service not found")
```

That separates *is the service wired up* from *is macOS showing it*, which are
different problems with different fixes.

Worth documenting for users, too: any Service can be given its own shortcut in
**System Settings → Keyboard → Keyboard Shortcuts → Services**.

### An LSUIElement app still needs a main menu

No menu bar is shown, so it looks like there is nothing to install. But AppKit
routes ⌘X/⌘C/⌘V/⌘A/⌘Z through the main menu's key equivalents, and without a
main menu those keys do nothing in any text field the app owns — including the
one people paste an API key into. `AppDelegate.installEditMenu` exists for
that, and Edit cannot be the first item because macOS treats the first as the
application menu.

### Accessibility re-prompts on every rebuild

TCC keys the permission grant to the app's code signature. The default build is
ad-hoc signed, and an ad-hoc signature changes on every build, so macOS treats
each build as a brand new app and asks again.

```sh
make reset-permissions   # tccutil reset Accessibility dev.starch.Starch
```

To avoid the re-prompt entirely, create a self-signed code signing certificate
in Keychain Access (*Certificate Assistant → Create a Certificate*, type *Code
Signing*) and build with it:

```sh
make app SIGN_IDENTITY="Starch Dev"
```

The signature is then stable across rebuilds and the grant sticks.

### The Keychain may prompt on each rebuild too, for the same reason

A new signature means a new identity as far as the Keychain ACL is concerned. A
stable signing identity fixes this as well.

### The bundle identifier is frozen

`dev.starch.Starch` appears in `Info.plist` and in `Brand.bundleIdentifier`,
and the app asserts they agree at launch in debug builds. Changing it silently
de-authorises Accessibility and orphans Keychain items for every existing
install. Do not.

### Unix socket paths are capped at ~104 bytes

`sun_path` is 104 bytes on Darwin. Both the daemon and the shell check this up
front and fail with a readable message, because `bind` otherwise fails with a
bare `EINVAL`. If you write a test that puts a socket in `t.TempDir()`, it will
intermittently blow the limit — use the short-path helper in
`internal/server/server_test.go` instead.

### `log` may be shadowed in your shell

If `log show` prints `too many arguments`, your shell has its own `log`. Use
`/usr/bin/log`, which is what `make logs` does.

## Tests

Go tests are table-driven and must run with no network. The daemon is designed
to be testable against an `httptest` provider stub.

Swift tests cover what can be tested without a window server: HTTP framing,
preferences, hot key encoding. The framing tests replay every fixture at six
chunk sizes including one byte at a time — that is deliberate, because parsing
correctly regardless of read boundaries is the entire job and the failure mode
otherwise is silent truncation.

Capture and overlay behaviour genuinely cannot be automated. Those live in
[MANUAL_TESTS.md](MANUAL_TESTS.md), which is expected to be run and updated,
not treated as decoration.

Run `make check` before opening a PR.

## Commits

Small commits with clear messages. The message should say **why**, not restate
the diff — if a decision has a trade-off, the commit that makes it is the right
place to record the reasoning.

If you hit a wall — the Accessibility API turning out to be a dead end in some
major app, Services registration refusing to cooperate — say so early in an
issue rather than building an elaborate workaround. Changing the plan is
cheaper than inheriting the workaround.
