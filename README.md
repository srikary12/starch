# Starch

Rewrite selected text anywhere on macOS, without leaving the app you are in.

Select text in any app, press a shortcut (or right-click → **Starch**), and a
small overlay shows a rewrite as it streams in. Return replaces the text in
place, Escape cancels. That is the whole product: it exists to kill the
copy → switch to ChatGPT → paste → prompt → copy → switch back → paste loop,
and it is only worth using if it is faster than that loop.

Bring your own API key. There is no account, no hosted backend, and no
telemetry.

> **Status: pre-release (M1).** The app runs in the menu bar, supervises its
> local helper, and captures the selected text from either trigger — the
> keyboard shortcut or right-click → Starch. **It does not rewrite anything
> yet**; it shows you what it captured and how. The first real rewrite lands in
> M2. See [Roadmap](#roadmap).

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
  of that process.
- **Your text goes to one place: the endpoint you configured.** Nowhere else.
  Point it at a local Ollama or LM Studio instance and nothing leaves the
  machine at all.
- **The voice profile is local.** Accepted rewrites are stored in a SQLite file
  on your machine to make future output sound like you (M4). It never leaves
  the device, and there is a button to erase it.
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
│                                  │   presets · voice profile · cache
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

## Building

Requires macOS 13+, [Go](https://go.dev) 1.23+, and Xcode (for the Swift test
frameworks — Command Line Tools alone can build the app but cannot run
`make test`).

```sh
git clone https://github.com/srikary12/starch
cd starch
make            # build the daemon and the app bundle
make run        # build and launch
make test       # Go and Swift test suites
make help       # every target
```

The app is a menu bar utility with no Dock icon — look in the status bar.

`make install` copies it to `/Applications` and registers it. The right-click
**Starch** entry needs this: macOS only discovers Services from apps installed
there, and caches the list aggressively.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development quirks worth knowing
before you lose an hour to one of them.

---

## Configuration

Settings live in the menu bar → **Settings…**

- **Provider** — Anthropic, or anything OpenAI-compatible. One implementation
  covers OpenAI, OpenRouter, Groq, Together, Ollama and LM Studio. Local models
  matter here: they are the answer for anyone who will not send work messages
  to a third party.
- **API key** — stored in the Keychain. Local endpoints generally do not need
  one.
- **Shortcut** — default `⌃⌥⌘P`, rebindable.

Presets will be editable JSON at
`~/Library/Application Support/Starch/presets.json` (M3).

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
| **M2** | The loop: providers, streaming rewrite, overlay, replace in place | |
| **M3** | Presets, including a neutral-business-English one | |
| **M4** | Local voice profile — output that sounds like you, not like an LLM | |
| **M5** | Notarised DMG and a Homebrew cask | |

Not in v1: accounts, sync, analytics, auto-update, fine-tuning, or a custom
keyboard. No Windows or Linux shell yet either, though the Go layer is written
assuming they are coming.

---

## License

[Apache-2.0](LICENSE).
