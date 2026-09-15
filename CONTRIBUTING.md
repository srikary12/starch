# Contributing

## Prerequisites

| | |
|---|---|
| macOS | 13.0 or later, to build the macOS app |
| Linux | any X11 desktop, plus a keyring providing `org.freedesktop.secrets` |
| Go | 1.23+ (`CGO_ENABLED=0` — see below) |
| Xcode | required to **run the Swift tests** |

Only the macOS app needs a Mac. The daemon and the Linux shell are pure Go and
build anywhere, which is why the Linux shell can be developed on a Mac — its
tests run there too, against a private `dbus-daemon` and a stand-in keyring.

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
make test       # every suite: Go, the Linux shell, Swift
make check      # vet + gofmt check + tests
make logs       # stream app and daemon logs
make clean
make help       # everything else

make linux          # build the daemon and the Linux shell
make linux-install  # into ~/.local/bin; PREFIX overrides
make windows        # cross-build starchd.exe and starch.exe, from any host
```

## Layout

```
cmd/starchd/            daemon entrypoint
internal/
  brand/                the product name, in one place
  catalog/              the curated provider, endpoint and model table
  config/               environment-driven configuration
  server/               UDS listener, routes, SSE encoder
api/                    the wire contract — other shells depend on it
apps/macos/
  Sources/StarchKit/    testable logic: framing, client, lifecycle, settings
  Sources/Starch/       AppKit shell: menu bar, windows, hot key
  Tests/
apps/desktop/           the Go shells, one module — see below
  cmd/starch/           the shell binary, one per platform from this entrypoint
  internal/client/      the wire contract, over the Unix socket
  internal/daemon/      spawning and supervising starchd
  internal/secret/      the API key, in the OS secret store
  internal/settings/    preferences, and the hot key spelling
  internal/rewrite/     the session handshake and its one retry
  internal/cli/         `starch config` and `starch rewrite`
  internal/x11/         Linux: hot key, selection, overlay
  internal/win32/       Windows: hot key, selection, overlay
```

`apps/desktop` is a **separate Go module**, the way `apps/macos` is a separate
Swift package. That is what keeps the root module's dependency list empty:
`starchd` is the process users run with an API key in memory, and "it depends
on nothing" is worth being able to say without qualification. A `replace`
directive lets the shell import `internal/brand`, `internal/config` and
`internal/catalog` from the daemon rather than restating them — which is why
its compiled-in provider defaults cannot go stale the way the macOS shell's
did.

**One module for both Go shells, not one each.** Most of a shell is not
platform-specific — the client, the supervisor, settings, the session handshake
and the terminal interface are the same code on Linux and Windows — and Go's
`internal` rule would stop a sibling module from importing any of it, so the
alternative is two copies that drift. Only `internal/x11` and `internal/win32`
are built per platform, behind build tags.

`internal/` must stay OS-independent. Anything macOS-specific that leaks in
there is a bug — the whole point of the split is that Windows and Linux shells
reuse it unchanged. `os.UserConfigDir` and friends are the portable way to do
per-platform paths; build tags are a last resort.

Changing anything in `api/` is a contract change. Read the versioning rules at
the bottom of [`api/README.md`](api/README.md) first.

### Keeping the model catalog current

`internal/catalog` is a hand-maintained table, served to every shell at
`GET /v1/models`. When a provider ships a model, that file is the only place to
add it — and the only place that records which thinking levels each model
accepts, because no provider's own `/models` endpoint reports that uniformly.

Its tests check the table against itself: a default that names a model absent
from its own list, a default effort the model would reject, a duplicated id.
They cannot check that the names are current — nothing can do that but a
person with the provider's documentation open. Which is also why every shell
must keep its model field typeable: a stale table must never be able to lock
someone out of a model their endpoint serves.

## Dependency policy

**Ask before adding any third-party dependency**, in any of the three modules.
The root Go module has zero and the Swift package has zero, and both must stay
that way: the daemon is the process holding an API key, and everything it links
is something to audit.

The Linux shell is the one agreed exception, because Go's standard library has
no X11, no D-Bus and no font rasteriser. Three are approved, all pure Go so
that `CGO_ENABLED=0` still holds:

| | |
|---|---|
| `github.com/jezek/xgb` | the X11 protocol. No dependencies of its own. |
| `github.com/godbus/dbus/v5` | the Secret Service, and the tray icon |
| `golang.org/x/image` | rasterising text in the overlay |

Nothing else. In particular no GTK or Qt binding: the shell draws its own
overlay, which needs no widgets, and that is why settings are a terminal
command rather than a window.

## The Linux shell targets X11 first

This is a narrowing made on purpose, and it is worth writing down why, because
"just add Wayland" looks like a small follow-up and is not.

Linux is not one target for this product. It is three, and they differ on
exactly the three things the shell does — take a global shortcut, read the
selection, put text back.

**Reading the selection is *better* than on macOS, on two of the three.** On
X11, and on Wayland under KWin or a wlroots compositor, selecting text already
puts it in the PRIMARY selection: readable with no synthetic Ctrl-C, no
clipboard to save and restore, and no permission of any kind. GNOME's Mutter
implements neither `wlr-data-control` nor its successor `ext-data-control-v1`,
so GNOME under Wayland is the one place that cannot.

**Putting text back is where Wayland costs something.** The accessibility API
is the natural route — AT-SPI2 is D-Bus, so it works identically under X11 and
Wayland — but [no Chromium element exposes `org.a11y.atspi.EditableText`
anywhere in its tree](https://xa11y.dev/explanation/accessibility-quirks/).
That rules out every Electron application, VS Code, Slack, Discord and Chrome
itself, which is the same verdict the M1 matrix reached on macOS: **paste is
the common path, not the fallback.** Synthetic input is therefore load-bearing,
and on Wayland that means the RemoteDesktop portal — a permission dialog, a
persistent remote-control indicator under GNOME, and restore tokens that [do
not survive a reboot under
KDE](https://www.mail-archive.com/kde-bugs-dist@kde.org/msg877541.html). Going
through XWayland's XTEST bridge instead produces ["Allow remote interaction"
popups every few minutes with almost zero
context](https://www.semicomplete.com/blog/xdotool-and-exploring-wayland-fragmentation/).
There is no Wayland equivalent of granting Accessibility once and never
thinking about it again.

**The overlay cannot follow the selection under GNOME.** Mutter [implements no
layer-shell](https://gitlab.gnome.org/GNOME/mutter/-/work_items/973) and
Wayland gives clients no global coordinates. Cursor-anchored is possible on X11
and on KWin/wlroots, and not on GNOME.

Two smaller ones: there is no system tray under GNOME without a shell extension
(Ubuntu ships one, Fedora does not), and the global shortcut does have a portal
— `xdg-desktop-portal-gnome` 48+, KDE, Hyprland — with a universal fallback of
binding a command in the desktop's own keyboard settings, which is the same
shape as the Services checkbox macOS already needs.

So X11 buys a complete, prompt-free experience today, on XFCE, Cinnamon, MATE
and KDE's X11 session, and it is the only way to have the whole loop working
before deciding what Wayland is worth. The cost is stated plainly: **GNOME
compile-disabled its X11 session in 49 and removed it in 50**, so this does not
reach a GNOME desktop at all, and it is not a long-term answer on its own.
Wayland is a separate decision with its own trade-offs, not a later chore.

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

### The desktop shells are a second Go module

`go test ./...` at the root does not reach them, and neither does `go vet`. Both
root targets shell out to `apps/desktop` — `make vet` and `make test` cover
everything, but a bare `go test ./...` quietly covers less than it looks like.

Running `starch rewrite` spawns a daemon of its own, on its own socket named
for the process. Two daemons must never contend for one socket, and a one-shot
command could not authenticate to a daemon someone else spawned anyway: the
handshake token exists only in that parent's memory.

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

Run `make check` before opening a PR. That is exactly what CI runs, so a green
`make check` locally means a green CI.

## CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every push to
`main` and every pull request:

| Job | What it protects |
|---|---|
| **Go** | `make vet` (vet plus gofmt) and the suite under `-race` |
| **Swift** | the Swift suite, on the toolchain it prints |
| **App bundle** | a universal build, then checks both binaries carry both slices, the version really got substituted into `Info.plist`, the bundle identifier has not moved, and the signature seals |
| **Daemon cross-compiles** | builds `starchd` for darwin, linux and windows, and runs the Go suite on Linux |
| **Linux shell** | vet, the suite under `-race`, both Linux architectures, and that the two binaries install where each other expects |

The last two exist because the README claims the Go layer is ready for other
platforms, and now that one of them is half-built the claim is testable.
An unchecked claim like that rots in a week.

The Linux job installs `dbus` before running the suite. The Secret Service
tests skip themselves when no session bus is present, and a skipped test reads
as a pass, so the job asserts afterwards that the locked-keyring case actually
ran. That suite is the only thing in the repository depending on software
outside it.

The bundle job's checks are the interesting ones — each is a failure that
otherwise ships silently. A universal build that quietly produced one
architecture is only discovered by an Intel user. A failed `sed` ships a
bundle whose version reads `__VERSION__`. And a changed bundle identifier
de-authorises every existing install's Accessibility grant and Keychain ACL.

## Releasing

Tag it. [`.github/workflows/release.yml`](.github/workflows/release.yml) does
the rest.

```sh
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

The workflow builds and tests the tagged tree on a clean machine *before*
publishing anything, then creates the GitHub release with install instructions
and a generated changelog. GitHub attaches the source archives itself — there
is nothing else to upload, because Starch is installed by building it.

`workflow_dispatch` runs the same verification and publishes nothing, which is
how to check a release will succeed without spending a tag on it.

Tags are the version: `make` stamps builds with `git describe`, so `v0.2.0`
becomes `0.2.0` and a later commit becomes `0.2.0-3-g1a2b3c4`. Nothing else
needs editing to cut a release — there is no version constant to bump.

## Commits

Small commits with clear messages. The message should say **why**, not restate
the diff — if a decision has a trade-off, the commit that makes it is the right
place to record the reasoning.

If you hit a wall — the Accessibility API turning out to be a dead end in some
major app, Services registration refusing to cooperate — say so early in an
issue rather than building an elaborate workaround. Changing the plan is
cheaper than inheriting the workaround.
