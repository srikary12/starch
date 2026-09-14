# Starch

Rewrite selected text anywhere on macOS, without leaving the app you are in.

Select text in any app, press a shortcut (or right-click → **Starch**), and a
small overlay shows a rewrite as it streams in. Return replaces the text in
place, Escape cancels. That is the whole product: it exists to kill the
copy → switch to ChatGPT → paste → prompt → copy → switch back → paste loop,
and it is only worth using if it is faster than that loop.

Bring your own API key. There is no account, no hosted backend, and no
telemetry.

---

## Privacy

This section is a commitment, not a description of intent. If any of it stops
being true, that is a bug worth filing.

- **No telemetry.** No analytics, no crash reporting, no update pings, no
  "anonymous usage statistics". Starch makes no network connection of its own,
  ever.
- **No backend.** There is no Starch server to talk to. No account, no sign-in,
  no sync.
- **Your key stays in the Keychain.** It is never written to `UserDefaults`,
  never written to disk by the Go layer, and never logged. The helper process
  receives it over a local socket and holds it in memory only, for the lifetime
  of that process. The app keeps a copy in memory too, for its own lifetime, so
  that a helper restart does not mean a Keychain prompt mid-sentence — both are
  process memory, and neither is ever written anywhere.
- **Your text goes to one place: the endpoint you configured.** Nowhere else.
  Point it at a local Ollama or LM Studio instance and nothing leaves the
  machine at all.
- **Nothing you rewrite is stored.** Not the selection, not the result, not a
  history of either. The text exists in memory for the length of one request
  and is gone. There is no local database to erase because there is nothing
  kept to put in one.
- **The local socket is not a network port.** The helper listens on a Unix
  domain socket with mode `0600` inside a `0700` directory, with a random
  handshake token generated at spawn. There is no TCP listener on any port,
  which is deliberate: a loopback port would be reachable by every process on
  the machine.

Everything above is verifiable — the source is right here, and
[`api/README.md`](api/README.md) documents every byte that crosses the process
boundary.

---

## How it works

Everything except text capture, hotkeys, overlay UI and secret storage is
OS-independent, so that part is written once in Go and each platform gets a
thin native shell.

```
┌──────────────────────────────────┐
│  Swift shell (macOS)             │   hotkey · Services menu · text capture
│                                  │   overlay UI · Keychain · daemon lifecycle
└───────────────┬──────────────────┘
                │  HTTP + SSE over a Unix domain socket
┌───────────────▼──────────────────┐
│  starchd (Go)                    │   providers · streaming · prompts
│                                  │   presets
└──────────────────────────────────┘
```

`starchd` is a local daemon on your machine, not a service anyone operates. The
shell spawns it at login, supervises it, and makes sure it dies with the app.

Future Windows and Linux shells speak the same wire contract; only the top box
gets rewritten.

The split also pays for itself on latency. A long-lived daemon keeps a warm
HTTP client with keep-alive to your provider, so every rewrite after the first
skips the TLS handshake — worth 100–200ms, a real fraction of the budget. The
IPC hop costs a millisecond or two and does not matter.

**Performance target:** first token visible within 500ms of the trigger, a
2–3 sentence rewrite complete under 1.5s. Past about two seconds people go back
to the ChatGPT tab and the product is dead.

---

## Install

Starch is installed by building it. Needs macOS 13+, [Go](https://go.dev) 1.23+
and Xcode 16+.

```sh
git clone https://github.com/srikary12/starch
cd starch
make install
```

That builds the daemon and the app, puts it in `/Applications`, and registers
the right-click **Starch** entry — which macOS only discovers from there. The
app is a menu bar utility with no Dock icon, so look in the status bar, not the
Dock. Open **Settings…** from its menu and add your API key.

### Why there is no DMG or Homebrew cask

Both are possible; neither is better than this. macOS quarantines anything
downloaded, and clearing that needs an Apple Developer ID certificate at
$99/yr. Without one, a downloaded Starch fails its first launch with *"Starch
is damaged and can't be opened"* until you run `xattr -dr com.apple.quarantine`
by hand — and Homebrew removed the `--no-quarantine` escape hatch, so a cask
does not avoid it either. A locally built app is never quarantined, so it just
runs. A two-minute build beats a scary dialog.

A Homebrew *formula* would sidestep the download but not the problem: formula
`post_install` is sandboxed and cannot write to `/Applications`, which is where
this app has to live for the Services entry to exist at all.

### Other targets

```sh
make            # build without installing
make run        # build and launch in place
make test       # Go and Swift test suites
make check      # vet, gofmt and both suites — what CI runs
make help       # every target
```

Builds are stamped with `git describe`, so the menu bar names the exact commit
you are running — `Helper connected · starchd 0.2.1 (v1)`, or
`starchd 0.2.1-4-g9f3c2a1-dirty` for an uncommitted working tree.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development quirks worth knowing
before you lose an hour to one of them.

---

## Configuration

Settings live in the menu bar → **Settings…**

- **Provider** — Anthropic, Google AI Studio, OpenAI, or anything else
  OpenAI-compatible. That last category covers OpenRouter, Groq, Ollama and LM
  Studio with a single implementation. Local models matter here: they are the
  answer for anyone who will not send work messages to a third party.
- **Endpoint and model** — dropdowns, filled from a list the helper serves. The
  models on offer follow the endpoint, because that is where they live:
  `api.openai.com` serves GPT models, `localhost:11434` serves whatever you
  have pulled into Ollama. Both fields still take a typed value. The list is
  curated and therefore always a little behind, so a model released after your
  build is never out of reach — type its name and it is used exactly as typed.
- **Thinking** — shown only for models that have a reasoning setting, carrying
  the levels that model accepts, and defaulting to **Low**. Current models
  think at medium or high unless told otherwise, which on a one-sentence
  rewrite is most of the wait and little of the benefit.
- **API key** — stored in the Keychain. Local endpoints generally do not need
  one.
- **Shortcut** — default `⌃⌥⌘P`, rebindable.

- **Presets** — editable JSON at
  `~/Library/Application Support/Starch/presets.json`. Settings has buttons to
  open it or reveal it in Finder. The file is created with the five defaults on
  first run, and an edit applies to the next rewrite without restarting
  anything. Break the JSON and Starch keeps working on the built-in presets,
  telling you which line is wrong.

---

## Permissions

Starch asks for **Accessibility** access, and explains why before asking. macOS
puts reading the current selection, and writing a replacement back, behind that
permission. It reads the text you have selected when you trigger it, and
nothing else: no keystroke logging, no background monitoring.

The right-click → **Starch** route needs no permissions at all, which makes it
the lower-commitment way to try Starch first. It does need one switch, though:
macOS registers every new third-party Services entry **switched off**, so the
first time you install Starch the menu item will not appear anywhere until you
turn it on.

**System Settings → Keyboard → Keyboard Shortcuts → Services → Text →** tick
**Starch**. Then quit and reopen the app you want to use it in — apps read the
Services list once at launch.

Starch detects this and says so in its menu and its set-up guide, rather than
leaving you to right-click and conclude the feature is broken. You can give the
service its own keyboard shortcut on that same screen.

---

## Roadmap

| | | |
|---|---|---|
| **M0** | Two skeletons — daemon, menu bar app, handshake, hotkey, onboarding | ✅ done |
| **M1** | Text capture: Accessibility path plus clipboard fallback, both triggers | ✅ done |
| **M2** | The loop: providers, streaming rewrite, overlay, replace in place | ✅ done |
| **M3** | Presets, including a neutral-business-English one | ✅ done |
| **M4** | CI on every push, and tagged source releases | ✅ done |
| **M5** | Model and endpoint pickers, and a thinking-effort control | ✅ done |

Not in v1: accounts, sync, analytics, auto-update, fine-tuning, or a custom
keyboard. No Windows or Linux shell yet either, though the Go layer is written
assuming they are coming.

**Also not in v1: a local voice profile.** It was planned — accepted rewrites
kept in a local database, the nearest few injected as examples so output drifts
towards how you write. It was cut. Keeping a history of everything you rewrite
is a real cost to the privacy guarantee above, and it is not one that pays for
itself before the app is packaged and in people's hands. Presets already cover
the common ground.

---

## License

[Apache-2.0](LICENSE).
