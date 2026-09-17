# Manual tests

Some of this product cannot be honestly automated: text capture depends on how
each host app implements the Accessibility API, and overlay behaviour depends
on a window server. Rather than pretend otherwise, those checks live here.

Run the M0 list before every release, and the milestone list for whatever you
are working on. Record results in the PR — including the failures, which are
the useful part.

Everything up to M5 is macOS. M6 is the Linux shell and needs a Linux desktop.

```sh
make install            # test the /Applications copy, not the build directory
make logs               # in a second terminal
```

---

## M0 — skeletons

### Daemon lifecycle

| # | Check | Expected |
|---|---|---|
| 1 | Launch the app | Menu bar icon appears. No Dock icon, no app switcher entry. |
| 2 | Open the menu within a second or two | `Helper connected · starchd <version> (v1)` |
| 3 | `ls -l ~/Library/Application\ Support/Starch/` | `starchd.sock` is `srw-------`, directory is `drwx------` |
| 4 | `lsof -aPi -p $(pgrep -x starchd)` | No output. **Any TCP port here is a serious bug.** |
| 5 | `curl --unix-socket ~/Library/Application\ Support/Starch/starchd.sock http://d/healthz` | `401` with a JSON `unauthorized` envelope |
| 6 | Leave the app running for 60s | `starchd` still alive. It idle-exits at 30s, so surviving proves the authenticated heartbeat works. |
| 7 | Quit from the menu | `starchd` gone within ~1s, socket file gone |
| 8 | Relaunch, then `kill -9` the **app** | `starchd` gone within ~6s, socket file gone (orphan watchdog) |
| 9 | `kill -9` the **daemon** while the app runs | Menu briefly shows unavailable, then reconnects with a new pid |
| 10 | Menu → Restart Helper | Reconnects, new pid in the log |
| 11 | Seed a dead socket file, then launch | Daemon removes it and starts. See the recipe below. |
| 12 | Launch a second copy of the app | Second one reports a mismatch rather than stealing the socket |

Recipe for #11:

```sh
python3 -c "import socket;s=socket.socket(socket.AF_UNIX);s.bind('$HOME/Library/Application Support/Starch/starchd.sock');s.listen(1);s.close()"
```

### Permissions and onboarding

| # | Check | Expected |
|---|---|---|
| 13 | `make reset-permissions`, then relaunch | Onboarding window appears on first run |
| 14 | Read the onboarding copy | Explains *why* Accessibility is needed **before** any system dialog |
| 15 | Click *Grant Accessibility Access…* | macOS permission dialog appears |
| 16 | Grant it in System Settings, leaving the window open | Status flips to granted within ~1s without a relaunch |
| 17 | Revoke it in System Settings | Menu line updates to "not granted" within ~1s |
| 18 | Close onboarding, reopen from the menu | Opens again, shows current state |
| 19 | Second launch | Onboarding does **not** reappear |
| 19a | First launch, close the guide (Done **or** the red X) | Settings opens by itself, with the caret already in the API key field |
| 19b | Reopen the guide later from the menu, close it | Settings does **not** open — that is a different intent |
| 19c | First launch when a key is already in the Keychain (a reinstall) | Settings still opens, but the caret is not forced into the key field |

### Hot key

| # | Check | Expected |
|---|---|---|
| 20 | Press `⌃⌥⌘P` in TextEdit | Status bar icon flashes `●` for ~0.7s. Menu also shows `Shortcut fired at <time> in TextEdit`. |
| 21 | Confirm the keystroke was consumed | No `p` is typed into the document. This is the reason for Carbon over a global monitor. |
| 22 | Repeat in Notes, Mail, Slack, Chrome, VS Code, Terminal | Fires in all; frontmost app name is correct |
| 23 | Settings → click the shortcut → press `⌃⌥⌘K` | Button updates; new shortcut fires; old one does not |
| 24 | Try to record a bare letter with no modifier | Rejected with an explanation |
| 25 | Press Escape while recording | Recording cancels, previous shortcut kept |
| 26 | Try to record a shortcut another app owns | Readable "already claimed" message, not a silent failure |
| 27 | Quit the app, press the shortcut | Nothing happens; no stale registration |
| 28 | Try to record plain `⌘P` | **Refused**, naming the shortcut. A single modifier plus a character key shadows a system shortcut in every app. |
| 29 | Try `⌥Space` | Accepted — Space carries no character, so one modifier is safe |
| 30 | Hand-edit an unsafe shortcut into the plist, relaunch | Reverted to `⌃⌥⌘P`, logged as unsafe. See the recipe below. |

The status bar flash exists only because there is nothing else to see until the
overlay lands in M2. Without it a working hot key and a dead one are
indistinguishable from the keyboard, which made this table untestable.

Recipe for #30 — writes the ⌘P binding that used to be accepted:

```sh
python3 -c "
import json, subprocess
p = {'provider':'anthropic','model':'claude-sonnet-5','baseURL':'https://api.anthropic.com',
     'hotKey':{'keyCode':35,'modifiers':256},'hasCompletedOnboarding':True,'debugLogging':False}
subprocess.run(['defaults','write','dev.starch.Starch','preferences.v1',
                '-data', json.dumps(p).encode().hex()], check=True)
"
```

Then relaunch and check the log:

```sh
/usr/bin/log show --last 1m --info --predicate 'subsystem == "dev.starch.Starch"' | grep unsafe
```

### Settings and Keychain

Keychain is not unit-tested — reading and writing real items can raise an
interactive prompt that would hang an unattended run — so it is covered here.

| # | Check | Expected |
|---|---|---|
| 31 | Enter an API key, click Save | Field clears; status says a key is saved |
| 32 | Keychain Access → search `dev.starch.Starch` | Item exists, of kind *application password* |
| 33 | `defaults read dev.starch.Starch` | Preferences present. **No key material anywhere.** |
| 34 | Quit and relaunch, open Settings | Status still reports a saved key |
| 35 | Click Remove | Status flips to no key; the Keychain item is gone |
| 36 | Switch provider with defaults untouched | Model and endpoint follow the new provider |
| 37 | Type a custom endpoint, then switch provider | Replaced by the new provider's own endpoint. Deliberate since M5 — see test 101. |
| 38 | Save a key per provider, switch between them | Each provider keeps its own key |
| 39 | Toggle verbose logging | Daemon restarts; `make logs` shows debug lines |

### Housekeeping

| # | Check | Expected |
|---|---|---|
| 40 | `make check` | Everything passes |
| 41 | `codesign --verify --strict --verbose=2 /Applications/Starch.app` | Valid |
| 42 | Read `make logs` output after a full session | No API key, no user text, anywhere |

---

## M1 — capture

Replacement is M2. What M1 reports is which **read** strategy each app takes,
plus whether an AX write-back *would* be possible — probed with
`AXUIElementIsAttributeSettable` rather than attempted, so nothing is modified.

Setup:

```sh
make install                    # Services discovery needs /Applications
open /Applications/Starch.app   # registers the service on first launch
make logs                       # second terminal
```

Grant Accessibility to the `/Applications` copy — it is a different signature
from your build-directory copy, so the grant does not carry over.

Then switch the Services entry on, which macOS does **not** do for you:
System Settings → Keyboard → Keyboard Shortcuts → Services → Text → tick
**Starch**. The menu bar reports `Right-click menu: on` once it is. Quit and
reopen any app you want to test it in — apps cache the Services list at launch.

This survives rebuilds (the preference key is derived from the bundle
identifier, menu title and message, none of which change), so it is a one-time
step unless the menu title changes.

### Already verified automatically

| Check | Result |
|---|---|
| Service registered with `pbs` | ✅ correct `NSMessage`, `NSPortName`, send/return types |
| `NSPerformService("Starch", …)` reaches the handler | ✅ |
| Handler feeds the shared flow | ✅ `captured via services … 54 chars` |
| No selected text in the log | ✅ character count only |
| Clipboard restored on success, timeout, non-text, and thrown error | ✅ 14 unit tests |

### The matrix — needs you

Select a sentence, press `⌃⌥⌘P`, read the status-bar note. It reports
`strategy · chars · app · AX-writable|paste-only · preview`.

Results from 2026-09-07, macOS 26.6, Apple silicon:

| App | Expected | Read strategy | Replace | Verified |
|---|---|---|---|---|
| TextEdit | accessibility | accessibility | AX-writable | ✅ |
| Notes | accessibility | accessibility | AX-writable | ✅ |
| Safari | accessibility | **clipboard** | paste-only | ✅ **expectation was wrong** |
| Chrome | clipboard | clipboard | paste-only | ✅ |
| VS Code | clipboard | clipboard | paste-only | ✅ |
| Mail | accessibility | | | not yet run |
| Slack | clipboard | | | not yet run |
| Zen / Firefox | ? | | | not yet run |
| Terminal / iTerm | clipboard | | | not yet run |
| Obsidian | ? | | | not yet run |

The finding that matters: **Safari falls back to the clipboard for reading as
well as writing.** The AX read fails outright — web content does not usefully
expose `kAXSelectedTextAttribute` at all, readable or settable — so being a
first-party native app buys nothing. What decides is whether the text lives in
a native control or in a web view, and every browser and Electron app is the
latter.

So the split is not "native apps versus the awkward ones". It is two clean
populations: native editors get the AX path end to end, and everything
rendering web content gets the clipboard path end to end. Nothing observed so
far sits in between — no app read via AX and then refused the write.

That makes paste the *majority* replace path rather than the fallback, since
most of the writing people want rewritten happens in a browser or Electron.
Consequences for M2:

- The clipboard save/restore machinery is load-bearing, not a safety net. Its
  failure modes are user-visible in the common case, not the rare one.
- Paste replacement gives a clean `Cmd-Z` in the host app for free, which an AX
  write does not reliably do. The brief requires undo to work; the AX path is
  the one that needs checking, not the paste path.
- The source app has to be reactivated before the paste lands, and the overlay
  is non-activating precisely so that focus was never lost.
- Web-view apps pay a clipboard round-trip at **both** ends — once to capture,
  once to replace — plus the wait for the trigger's modifiers to clear. That
  whole path sits inside the 500ms first-token budget and is the one to
  instrument. The AX path is nearly free by comparison and is not the risk.

Then right-click → **Starch** in each and confirm the note says `services`.

### The guarantees

| # | Check | Expected |
|---|---|---|
| 43 | Copy something distinctive, then trigger in a clipboard-fallback app | Your clipboard is unchanged afterwards |
| 44 | Trigger with **nothing** selected | A readable "could not read the selection" note — never silence |
| 45 | Trigger in a non-text context (Finder icon view) | Same: a message, not silence |
| 46 | Revoke Accessibility, then press the shortcut | Note explains it, and onboarding opens. Right-click → Starch still works. |
| 47 | Hold `⌃⌥⌘` down for two seconds before releasing `P` | Still captures. This is the held-modifier case that would otherwise send ⌃⌥⌘C. |
| 48 | Select a 10,000-character block and trigger | Correct character count; no truncation |
| 49 | Select text with emoji and CJK | Character count matches; preview renders |
| 50 | Trigger repeatedly, fast, in the same app | No stuck state, clipboard still intact |
| 51 | Read `make logs` after all of the above | Character counts only. **No selected text anywhere.** |

Timings are in the log as `in N.Nms`. Worth recording the AX and clipboard
numbers separately — the 500ms first-token budget in M2 is built on them.

---

## M2 — the loop

Needs a real API key in Settings, or a local Ollama / LM Studio endpoint.
Everything below costs tokens except the local-model rows.

### Already verified automatically

| Check | Result |
|---|---|
| SSE parsed identically at 6 chunk sizes, incl. 1 byte at a time | ✅ both languages |
| Deltas over 64KB not truncated | ✅ the `bufio.Scanner` trap |
| Reasoning/thinking deltas never emitted as output | ✅ |
| Cancellation reaches the upstream, asserted server-side | ✅ |
| API key absent from every error path and log | ✅ |
| Full loop over the real socket against a stub provider | ✅ `first_token_ms=56` |

### The loop — needs you

| # | Check | Expected |
|---|---|---|
| 52 | Select a sentence in TextEdit, press the shortcut | Overlay appears **near the selection**, text streams in |
| 53 | Watch where the overlay lands | Near the selected text, not at a screen corner. Falls back to the mouse in web views. |
| 54 | Check the source app during streaming | It stays frontmost — its caret keeps blinking, its title bar stays active |
| 55 | Press Return | Selection is replaced in place; overlay closes |
| 56 | Press `Cmd-Z` in the host app | Undoes the replacement cleanly, in one step |
| 57 | Press Escape mid-stream | Overlay closes immediately, nothing is replaced |
| 58 | Press Return **while still streaming** | Ignored — a half sentence must never land |
| 59 | Press Tab | Cycles preset and re-runs against the same selection |
| 60 | Repeat 52–56 in Chrome, Safari, Slack, VS Code | Works via paste; clipboard intact afterwards |
| 61 | Copy something distinctive, then accept a rewrite in Chrome | Your clipboard still holds what you copied |
| 62 | Trigger from right-click → Starch | Same overlay, same behaviour |

### Cancellation actually stopping the spend

| # | Check | Expected |
|---|---|---|
| 63 | Select a long paragraph, trigger, press Escape after ~1s | Overlay closes |
| 64 | Check the provider's usage dashboard for that request | Output tokens reflect the **truncated** generation, not a full one |

Test 64 is the one that matters and the only way to verify it for real. The
unit test asserts the stub sees the disconnect; only the dashboard proves the
provider did.

### Failure modes

| # | Check | Expected |
|---|---|---|
| 65 | Put a wrong API key in Settings, trigger | Overlay says the key was rejected and names Settings. **Not** a raw error string. |
| 66 | Point at `http://localhost:99999/v1`, trigger | "Could not reach … Is it running" |
| 67 | Quit the helper mid-session, trigger | Re-handshakes and succeeds, or explains itself |
| 68 | `pkill -9 starchd`, then trigger | Same — the 428 retry path |
| 69 | Trigger with no key set at all | A readable message, not a hang |
| 69a | Revoke Accessibility, then use **right-click → Starch** and press Return | Says replacing needs Accessibility, **and** puts the rewrite on your clipboard. It must not claim success. |
| 70 | Read `make logs` after all of the above | No key, no selected text, no rewrite text. Timings and counts only. |

### Latency, against the 500ms budget

```sh
/usr/bin/log show --last 30m --info --predicate 'subsystem == "dev.starch.Starch"' | grep first_token_ms
```

Record separately for the AX path (TextEdit, Notes) and the clipboard path
(Chrome, Safari, VS Code) — the latter pays a round trip at each end plus the
modifier-release wait, and is the one at risk.

| Path | first_token_ms | total_ms | Target |
|---|---|---|---|
| AX (TextEdit) | | | <500 / <1500 |
| Clipboard (Chrome) | | | <500 / <1500 |

**Reasoning is the thing to watch.** The log now carries `thought_tokens`
alongside the timings, because reasoning is invisible in the result but can be
most of the wait — without it a slow rewrite looks like a slow network.

Gemini measured 1993ms to first token before any tuning, and now asks for
`thinkingLevel: "low"`. Re-measure and compare. If `thought_tokens` is still in
the hundreds, low is not low enough for this task and the next step is
`minimal` on the models that accept it — `gemini-2.5-flash` rejects `minimal`,
which is why `low` is the floor today.

Anthropic and OpenAI-compatible endpoints report no thinking tokens, so a zero
there means "not reported", not "none spent".

---

---

## M3 — presets

### Already verified automatically

| Check | Result |
|---|---|
| File created with the defaults on first read | ✅ |
| A hand edit applies with no restart | ✅ added a preset, appeared immediately |
| Broken JSON falls back and names the line | ✅ `line 1, column 3` |
| A broken file is never overwritten | ✅ |
| `PUT` validates and replaces | ✅ |

### Needs you

| # | Check | Expected |
|---|---|---|
| 71 | Settings → Presets | Shows the count and the full path |
| 72 | Click *Edit presets.json…* | Opens in your JSON editor |
| 73 | Click *Show in Finder* | Reveals the file |
| 74 | Add a preset to the file, save, trigger a rewrite | New preset is in the Tab cycle without restarting |
| 75 | Tab through every preset during one overlay | Each re-runs against the same selection; the name updates |
| 76 | Change an instruction, then re-run that preset | Output reflects the new wording |
| 77 | Delete the preset you currently have selected, then trigger | Falls back to a real preset rather than leaving Tab dead |
| 78 | Break the JSON deliberately, reopen Settings | Orange warning naming the line; rewriting still works on the built-ins |
| 79 | Fix the JSON, trigger again | Warning clears, your presets are back |
| 80 | Delete the file entirely, trigger | Recreated with the five defaults |

Test 77 is the one worth doing deliberately — editing the file mid-session can
delete the preset the app currently has selected, and that state is easy to
leave broken.

---

## M4 — CI and releases

### Already verified automatically

| Check | Result |
|---|---|
| Daemon cross-compiles for darwin, linux and windows | ✅ all five targets |
| Every Go test binary compiles for Linux | ✅ four packages |
| Universal bundle carries both slices, both binaries | ✅ `x86_64 arm64` |
| `Info.plist` version really substituted | ✅ `0.0.0-ci`, not `__VERSION__` |
| Bundle identifier unchanged | ✅ `dev.starch.Starch` |
| Signature seals and satisfies its designated requirement | ✅ |
| Version stamped from `git describe` | ✅ `5c36faa-dirty` |

That is the CI script itself, run locally against a real universal build — not
an approximation of it.

### Needs you

| # | Check | Expected |
|---|---|---|
| 81 | Push the branch, watch Actions | Four CI jobs, all green |
| 82 | Open a PR | The same four run against the merge commit |
| 83 | Break `gofmt` deliberately, push | The **Go** job fails, and names the file |
| 84 | Run **Release** via `workflow_dispatch` | Verify passes; **no** release is created |
| 85 | `git tag -a v0.1.0 && git push origin v0.1.0` | Release appears with install instructions and a changelog |
| 86 | Check the release's source archive | Extracts, and `make install` works from it |
| 87 | After the tag, rebuild locally | Menu bar reads `starchd 0.1.0 (v1)`, not a sha |
| 88 | Make a commit after the tag, rebuild | Reads `0.1.0-1-g<sha>` — a build that is past the tag says so |

Test 84 is worth doing before 85: it is the whole reason `workflow_dispatch` is
wired up, and a tag spent on a broken release cannot be un-spent cleanly.

Test 86 matters more than it looks. Everyone installs from that archive, and it
is the one artifact no local `make` ever exercises — a file missing from git,
or a build step that quietly depends on `.git` being present, only shows up
here.

---

## M5 — model pickers and thinking effort

### Already verified automatically

| Check | Result |
|---|---|
| `GET /v1/models` answers with no session and no API key | ✅ |
| Every catalog provider id is one `POST /v1/session` accepts | ✅ |
| No default model names a model absent from its own list | ✅ |
| No default effort is one its model would reject | ✅ |
| Each provider sends effort in its own field | ✅ `output_config.effort`, `thinkingLevel`, `reasoning_effort` |
| No effort chosen means no new field in the request | ✅ Gemini excepted, which keeps asking for its floor |
| A level chosen in Settings reaches the upstream request | ✅ end to end, over a real socket |
| Settings written before this existed still load | ✅ provider, model and endpoint all survive |

### Needs you

| # | Check | Expected |
|---|---|---|
| 89 | Open Settings with the helper running | Endpoint and Model are dropdowns, both populated |
| 90 | Open the Endpoint list, pick Ollama | Model list changes; the note explains it is empty |
| 91 | Type a model name that is not in the list, click away | Kept exactly as typed, and used on the next rewrite |
| 92 | Select a thinking model (`claude-sonnet-5`, `gemini-3.8-flash`) | **Thinking** row appears, preselected **Low** |
| 93 | Select `claude-haiku-4-5` | **Thinking** row disappears entirely |
| 94 | Set Thinking to High, rewrite | Noticeably slower than Low, and visibly more considered |
| 95 | With verbose logging on, change the level and rewrite | `session configured … thinking_effort=high` in Console |
| 96 | Set Thinking to *Endpoint default*, rewrite | Works; the log shows an empty `thinking_effort` |
| 97 | Switch provider | Endpoint and model become the new provider's defaults; Thinking resets |
| 98 | Quit the helper, then open Settings | Lists empty, every field still editable, nothing looks broken |
| 99 | Upgrade over an existing install | Your provider, model and endpoint are exactly as you left them |

| 100 | Click **into** the Endpoint field, then switch Provider | Endpoint *and* Model both visibly change to the new provider's |
| 101 | Type a custom endpoint, switch provider, switch back | Resets to the provider default — deliberate, see below |

Test 100 exists because it broke. An editable combo box holds the keyboard
focus's field editor, and a guard meant to stop an arriving catalog
overwriting a half-typed word also skipped deliberate updates — so the
setting changed, committed, and the field on screen did not move. Nothing
automated can see this: it needs a real window server and real focus.

Test 101 documents a trade-off rather than a bug. A provider switch always
adopts the new provider's own endpoint, which costs anyone using a custom
gateway a re-type if they flip provider and come back. The alternative was
worse: the previous rule preserved anything that did not look like a
default, and so carried `api.openai.com` across into Anthropic.

Test 91 is the one that matters most. The catalog is curated, so it is always a
little behind — the day a provider ships a model, typing its name has to be
enough, or this feature has made the app *less* capable than the text box it
replaced.

Test 94 is the reason the control exists at all. If High and Low feel the same,
the level is not reaching the provider, and 95 says whether it left the app.

---


## M6 — the Linux shell

Nothing here needs a Mac. Everything here needs a real Linux desktop, which is
the point: CI runs this code headlessly against a stand-in keyring and a stub
provider, so what it proves is that the code is correct, not that the product
works. These are the checks only a real session can make.

```sh
make linux-install      # ~/.local/bin/starch and starchd
```

### Configuration, against a real keyring

| # | Check | Expected |
|---|---|---|
| 102 | `starch config` on a machine with nothing set up | Prints defaults, and `API key  not set — run starch config --key` |
| 103 | `starch config --key`, type a key | Nothing echoes while typing. Key lands in the keyring. |
| 104 | Open Seahorse or KWalletManager | One entry, labelled for Starch, named for the provider |
| 105 | `starch config` again | `API key  saved in the keyring`, and **the key itself is nowhere in the output** |
| 106 | Lock the keyring, then `starch config` | Still prints everything. **No password dialog** — reading settings must not unlock |
| 107 | Lock the keyring, then `starch rewrite` | Password dialog appears once; the rewrite then proceeds |
| 108 | Dismiss that dialog instead | "the keyring prompt was dismissed", not a provider error |
| 109 | `starch config --provider gemini --key`, then switch back to anthropic | The Anthropic key is still there. One entry per provider. |
| 110 | `printf 'x' \| starch config --key` (piped) | Stored without a prompt, for provisioning scripts |
| 111 | Stop the keyring daemon entirely, `starch config` | Names GNOME Keyring, KWallet and KeePassXC. Everything else still prints. |

### The rewrite loop

| # | Check | Expected |
|---|---|---|
| 112 | `echo "thx for the update" \| starch rewrite` | Text appears **progressively**, not all at once |
| 113 | Time to the first character | Under 500ms on a warm daemon. This is the budget. |
| 114 | `starch rewrite --preset concise` and `--preset friendly` | Visibly different output |
| 115 | Ctrl-C part-way through | Stops at once. Check the provider's dashboard: the generation stopped being billed, not just displayed. |
| 116 | `starch rewrite` with no stdin, from a terminal | Waits for input, then "there is no text to rewrite" on an empty one |
| 117 | `ls $XDG_RUNTIME_DIR/starch/` during a rewrite | A `cli-<pid>.sock`, mode `srw-------` |
| 118 | The same, a second after it finishes | Gone. No daemon left holding a key. |
| 119 | `pgrep starchd` after several rewrites | Nothing. Each run cleans up after itself. |
| 120 | `lsof -aPi -p $(pgrep -x starchd)` mid-rewrite | No output. **Any TCP port here is a serious bug.** |
| 121 | Point `--endpoint` at a local Ollama and clear the key | Works with no key and no keyring at all |
| 122 | Unplug the network, `starch rewrite` | "could not connect", naming the endpoint — not a stack trace |

### Paths

| # | Check | Expected |
|---|---|---|
| 123 | `ls ~/.config/starch/` | `settings.json` and `presets.json`, both `-rw-------` |
| 124 | Nothing in `~/.config/Starch/` | Capitalised is the macOS spelling and must not appear here |
| 125 | Edit `settings.json` by hand, set `"hotkey": "Ctrl+C"` | Refused on next run, with the reason, and the default used instead |
| 126 | Truncate `settings.json` mid-file | Warns, uses defaults, still runs |
| 127 | Reboot, then `ls $XDG_RUNTIME_DIR/starch/` | Empty. Sockets do not survive a session; settings do. |

Test 115 is the one that matters most, and the one a stub provider cannot
check. Cancellation has to reach the provider, not just the screen — the
contract is explicit that a client which merely stops reading has cancelled
nothing and is still being billed.

Tests 106 and 107 are the pair worth reading together. A keyring prompt is the
one piece of desktop UI this shell cannot avoid, so it has to appear exactly
when a key is genuinely needed and never when someone is only looking.

Not here yet, because they do not exist yet: the hot key, the overlay, and
replacing text in place. Those arrive with the X11 work and bring their own
list — the interesting part of which will be the capture and replace matrix,
per application, the way M1's was on macOS.

## M7 — the Windows shell, headless half

Everything here needs a real Windows machine, and **none of it needs a
desktop**: the hot key, the overlay, the tray and replacing text in place are
not written yet. This list covers `starch config` and `starch rewrite`, which
is the whole product minus the desktop — the same milestone M6 reached on Linux
before the X11 work.

Read the M6 tests above as the baseline. What is repeated below is only what
Windows does differently, and every one of those differences is there because
the platform forced it, not because it was preferred.

CI already runs this code on `windows-latest` against a stub provider, so what
these checks add is a real user profile, a real Credential Manager, a real
reboot, and Task Manager.

```
go build -o build\starchd.exe .\cmd\starchd
cd apps\desktop
go build -o ..\..\build\starch.exe .\cmd\starch
```

There is no installer yet. `Locate()` looks for `starchd.exe` beside the shell
first, so keeping both in `build\` is enough; `set STARCH_DAEMON=<path>`
overrides it.

### Configuration, against a real Credential Manager

Credential Manager is part of the OS and decrypts under your logon session, so
it never prompts. M6's tests 106–108 — the locked-keyring pair — have no
analogue here, which is the one way this platform is easier than Linux.

| # | Check | Expected |
|---|---|---|
| 128 | `starch config` on a machine with nothing set up | Prints defaults, and `API key  not set` |
| 129 | `starch config --key`, type a key, in **cmd.exe** | Nothing echoes while typing |
| 130 | The same in **PowerShell** | Nothing echoes. Different console host, different failure. |
| 131 | The same in **Windows Terminal** | Nothing echoes |
| 132 | Control Panel → Credential Manager → Windows Credentials | One **Generic** credential, `Starch/api-key.<provider>`, persistence **Local machine** — *not* Enterprise, which would roam the key to every machine you log into |
| 133 | `starch config` again | Reports the key is saved, and **the key itself is nowhere in the output** |
| 134 | `type %APPDATA%\Starch\settings.json` | No key anywhere in it |
| 135 | `starch config --provider gemini --key`, then switch back | Each provider keeps its own key. One entry per provider. |
| 136 | `echo x\| starch config --key` (piped) | Stored without a prompt |
| 137 | Delete the entry from Credential Manager by hand, then `starch rewrite` | Asks for a key rather than failing obscurely |

### The socket, and who can reach it

The security-critical section, because this is where Windows differs most: the
daemon cannot enforce its own `0600`/`0700` there — `Chmod` only toggles the
read-only attribute and returns success having done nothing — so the shell sets
an access list instead, before the daemon starts. CI checks that in a temp
directory. These check it where it actually lives.

| # | Check | Expected |
|---|---|---|
| 138 | `icacls %LOCALAPPDATA%\Starch` | **Your account and nothing else.** No `BUILTIN\Administrators`, no `NT AUTHORITY\SYSTEM`, no `(I)` inherited entries |
| 139 | `dir %LOCALAPPDATA%\Starch` during a rewrite | A `starchd.sock` |
| 140 | Nothing in `%APPDATA%\Starch` but `settings.json` and `presets.json` | The socket must **not** be in Roaming: a roaming profile would sync it to a file server and restore it at next logon |
| 141 | Widen it by hand — `icacls %LOCALAPPDATA%\Starch /grant Everyone:F` — then run `starch rewrite` | The next start tightens it back to you alone. Check with 138 again. |
| 142 | `netstat -ano \| findstr <starchd pid>` mid-rewrite | No output. **Any TCP port here is a serious bug.** |
| 143 | From a *second* Windows account, try to read `%LOCALAPPDATA%\Starch` on the first | Access denied |

### Lifetime, which has no signal behind it

On Windows there is no graceful stop to send — `Process.Signal` implements
`Kill` and nothing else — and orphans are not reparented, so neither backstop
the other two platforms use exists. A job object replaces both, and it is
stronger: the OS kills the daemon when the shell's handle closes, for any
reason at all. These are the checks that prove it.

| # | Check | Expected |
|---|---|---|
| 144 | `tasklist \| findstr starchd` after several rewrites | Nothing. Each run cleans up. |
| 145 | **End task** on `starch.exe` from Task Manager mid-rewrite | `starchd.exe` disappears **immediately** — not in 30 seconds, which is what the idle timeout alone would give you |
| 146 | `taskkill /F /IM starchd.exe` mid-rewrite, then rewrite again | Works. The leftover `starchd.sock` is cleared, not treated as a live daemon. |
| 147 | After 146, `dir %LOCALAPPDATA%\Starch` | The stale `starchd.sock` is **expected** to still be there. Unlike Linux, the daemon is always killed and never unlinks its own. |
| 148 | Reboot, then `dir %LOCALAPPDATA%\Starch`, then `starch rewrite` | A stale socket may survive the reboot — `%LocalAppData%` is not `XDG_RUNTIME_DIR` and is not cleared. The rewrite must work anyway. |

Test 148 is the one with no counterpart anywhere else. On Linux the socket
directory is wiped with the session; here it persists indefinitely, so a stale
socket is not an edge case, it is Monday morning.

### The rewrite loop

| # | Check | Expected |
|---|---|---|
| 149 | `echo thx for the update\| starch rewrite` | Text appears **progressively** |
| 150 | Time to the first character | Under 500ms on a warm daemon |
| 151 | `starch rewrite --preset concise` and `--preset friendly` | Visibly different output |
| 152 | Ctrl-C part-way through | Stops at once. Check the provider's dashboard: **the generation stopped being billed**, not just displayed. |
| 153 | Unplug the network, `starch rewrite` | "could not connect", naming the endpoint — not a stack trace |
| 154 | Point `--endpoint` at a local Ollama and clear the key | Works with no key and no Credential Manager entry at all |

Test 152 matters as much here as 115 did on Linux, and for the same reason: a
client that merely stops reading has cancelled nothing and is still being
billed.

### Paths and the things that only break on someone else's machine

| # | Check | Expected |
|---|---|---|
| 155 | Run as a user whose name has a **space** (`C:\Users\Jane Smith`) | Everything works. Unquoted paths fail here and nowhere else. |
| 156 | Run as a user with a **long** name, and check the socket path length | Under 103 bytes. `C:\Users\<name>\AppData\Local\Starch\starchd.sock` is the budget, and the failure is a bare "invalid argument" from bind if it is blown. |
| 157 | Run as a user with a **non-ASCII** name (`C:\Users\Zoë`) | Everything works |
| 158 | Windows 10 **1803 or older** | AF_UNIX arrived in 1803. Below it, expect a clear failure to bind, not a crash. |
| 159 | Edit `settings.json` by hand, set `"hotkey": "Ctrl+C"` | Refused on next run, with the reason, and the default used |
| 160 | Truncate `settings.json` mid-file | Warns, uses defaults, still runs |
| 161 | Copy `starch.exe` to a machine that has never seen it and run it | Note whether SmartScreen blocks it. It will until the binaries are signed; this records what a first-time user actually hits. |

Tests 155–158 are the ones worth doing on someone else's machine rather than
yours. Every one of them is a path or an encoding assumption that is invisible
on a developer account called `runneradmin`.

## M8 — the Windows desktop

`starch run`. Everything above was the loop without a desktop; this is the
product. **None of it is covered by CI at all** — GitHub's `windows-latest`
runners have no guaranteed interactive desktop, so the hot key, the overlay and
every synthetic keystroke are unverified until someone presses the key.

```
go build -o build\starchd.exe .\cmd\starchd
cd apps\desktop
go build -o ..\..\build\starch.exe .\cmd\starch
..\..\build\starch.exe run
```

The tray does not exist yet, so `starch run` holds the console it was started
from. Ctrl-C stops it.

### The loop

| # | Check | Expected |
|---|---|---|
| 162 | Select text in Notepad, press the shortcut | Overlay appears near the selection, text streams in |
| 163 | Press Enter | The selection is replaced with the rewrite |
| 164 | **Ctrl-Z once**, in the host application | The original text is back, in **one** step. Not character by character. |
| 165 | Press the shortcut with nothing selected | A notice that says so, no overlay, no rewrite, nothing billed |
| 166 | Press Escape while the text is still streaming | Overlay closes at once, nothing is replaced. Check the provider dashboard: **the generation stopped being billed** |
| 167 | Press Enter while the text is still streaming | **Nothing happens.** A partial rewrite must never land in a document |
| 168 | Press Enter after it finishes | Replaces |
| 169 | Time from keypress to first character | Under 500ms on a warm daemon |

### The two guarantees, which is what this list is really for

| # | Check | Expected |
|---|---|---|
| 170 | Copy something distinctive (`ZZZ-MARKER`), select other text, rewrite it, accept | Clipboard still holds `ZZZ-MARKER` afterwards |
| 171 | The same, but press Escape instead | Clipboard still holds `ZZZ-MARKER` |
| 172 | The same, but with the daemon killed mid-stream so the rewrite fails | Clipboard still holds `ZZZ-MARKER`. **Every error path, not just the tidy ones** |
| 173 | Copy an **image**, then rewrite some text and accept | The image is **gone** — a known and documented limitation. What must *not* happen is the rewrite being left on the clipboard |
| 174 | Rewrite in a slow application (a large Word document) | The pasted text is the rewrite, never your previous clipboard. This is what the delayed-render path exists to guarantee |

### Focus, which is where the design is most likely to be wrong

| # | Check | Expected |
|---|---|---|
| 175 | Watch the title bar of the host application while the overlay is open | It stays active. The overlay must **never** take focus |
| 176 | With the overlay open, the text cursor in the host application | Still blinking where it was; the selection is still highlighted |
| 177 | With the overlay open, type an ordinary letter | It goes into the **application underneath**, not the overlay. The keyboard hook must only swallow Enter and Escape |
| 178 | With the overlay open, press Tab, arrows, Backspace | All reach the application underneath |
| 179 | After the overlay closes, press Escape in any application | Works normally. A hook left installed would swallow Escape machine-wide |
| 180 | Kill `starch.exe` while an overlay is open, then press Escape anywhere | Works normally. The hook must not outlive the process |

### Applications, the matrix

The equivalent of M1's on macOS. Capture is clipboard-only for now — UI
Automation is not implemented — so anything that refuses a synthetic Ctrl-C
fails to capture, and that is what this is measuring.

| # | Application | Capture | Replace | Single-step undo |
|---|---|---|---|---|
| 181 | Notepad | | | |
| 182 | WordPad / Word | | | |
| 183 | Chrome — a plain textarea | | | |
| 184 | Chrome — Gmail compose | | | |
| 185 | Edge | | | |
| 186 | Slack | | | |
| 187 | VS Code | | | |
| 188 | Outlook (desktop) | | | |
| 189 | Windows Terminal | | | |
| 190 | A dialog box's text field | | | |

Fill it in rather than reporting pass or fail. A blank cell is a finding.

### Edges

| # | Check | Expected |
|---|---|---|
| 191 | Select text near the **right edge** of the screen, rewrite | Overlay fully on screen and readable |
| 192 | The same near the **bottom edge** | Overlay appears **above** the selection, not covering it |
| 193 | On a second monitor, and on a monitor at 150% scaling | Overlay appears on the right screen, legible |
| 194 | Hold the shortcut down | **One** rewrite, not one per key repeat. Each repeat would be billed |
| 195 | Press the shortcut again while an overlay is already open | No second overlay, no second rewrite |
| 196 | Rewrite in an application **running as administrator** | Expect capture or paste to fail with the message about elevation — not silence. Windows blocks input from a non-elevated process to an elevated window |
| 197 | Set `"hotkey"` to something already taken (`Ctrl+Alt+Delete`) | A message naming the shortcut and saying how to change it, not a silent no-op |
| 198 | Quit with Ctrl-C, then `tasklist \| findstr starchd` | Nothing. The job object takes the daemon with the shell |

Tests 164, 167 and 173 are the three worth doing first. 164 and 167 are the two
promises this product makes about other people's documents, and 173 is a
limitation I would rather you saw deliberately than discovered by accident.

196 is the one most likely to be a real-world complaint, because Task Manager
and a great deal of IT software run elevated and the failure is otherwise
completely silent.

Not here yet: UI Automation capture, which would read the selection without
touching the clipboard at all in applications that support it, and the tray.
