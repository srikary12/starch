# Manual tests

Some of this product cannot be honestly automated: text capture depends on how
each host app implements the Accessibility API, and overlay behaviour depends
on a window server. Rather than pretend otherwise, those checks live here.

Run the M0 list before every release, and the milestone list for whatever you
are working on. Record results in the PR — including the failures, which are
the useful part.

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

