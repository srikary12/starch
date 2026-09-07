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
| 37 | Type a custom endpoint, then switch provider | Custom value is **not** silently overwritten |
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

## M2 — the loop *(not yet implemented)*

- First token visible within 500ms of the trigger; 2–3 sentence rewrite under
  1.5s. Measure with the debug timing flag, and record real numbers.
- Escape cancels and the upstream generation actually stops — verify against
  the provider's usage dashboard, not just the UI.
- A 401 from the provider shows a readable message, not a raw error string.
- The overlay never steals focus from the source app.
- Text is never replaced without the user seeing the rewrite first.
