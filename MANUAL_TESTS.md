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
| 20 | Press `⌃⌥⌘P` in TextEdit | Menu shows `Shortcut fired at <time> in TextEdit` |
| 21 | Confirm the keystroke was consumed | No `p` is typed into the document. This is the reason for Carbon over a global monitor. |
| 22 | Repeat in Notes, Mail, Slack, Chrome, VS Code, Terminal | Fires in all; frontmost app name is correct |
| 23 | Settings → click the shortcut → press `⌃⌥⌘K` | Button updates; new shortcut fires; old one does not |
| 24 | Try to record a bare letter with no modifier | Rejected with an explanation |
| 25 | Press Escape while recording | Recording cancels, previous shortcut kept |
| 26 | Try to record a shortcut another app owns | Readable "already claimed" message, not a silent failure |
| 27 | Quit the app, press the shortcut | Nothing happens; no stale registration |

### Settings and Keychain

Keychain is not unit-tested — reading and writing real items can raise an
interactive prompt that would hang an unattended run — so it is covered here.

| # | Check | Expected |
|---|---|---|
| 28 | Enter an API key, click Save | Field clears; status says a key is saved |
| 29 | Keychain Access → search `dev.starch.Starch` | Item exists, of kind *application password* |
| 30 | `defaults read dev.starch.Starch` | Preferences present. **No key material anywhere.** |
| 31 | Quit and relaunch, open Settings | Status still reports a saved key |
| 32 | Click Remove | Status flips to no key; the Keychain item is gone |
| 33 | Switch provider with defaults untouched | Model and endpoint follow the new provider |
| 34 | Type a custom endpoint, then switch provider | Custom value is **not** silently overwritten |
| 35 | Save a key per provider, switch between them | Each provider keeps its own key |
| 36 | Toggle verbose logging | Daemon restarts; `make logs` shows debug lines |

### Housekeeping

| # | Check | Expected |
|---|---|---|
| 37 | `make check` | Everything passes |
| 38 | `codesign --verify --strict --verbose=2 /Applications/Starch.app` | Valid |
| 39 | Read `make logs` output after a full session | No API key, no user text, anywhere |

---

## M1 — capture *(not yet implemented)*

The point of this table is the middle column. Report which strategy each app
actually takes; the Accessibility API is read-only or silently broken across a
lot of Electron and web content, and knowing exactly where is the deliverable.

| App | Expected path | AX read | AX write | Clipboard fallback | Notes |
|---|---|---|---|---|---|
| TextEdit | Accessibility | | | | |
| Notes | Accessibility | | | | |
| Mail | Accessibility | | | | |
| Safari (textarea) | Accessibility | | | | |
| Safari (contenteditable) | ? | | | | |
| Chrome | likely clipboard | | | | |
| Slack | likely clipboard | | | | |
| VS Code | likely clipboard | | | | |
| Terminal / iTerm | clipboard, read-only | | | | Replacement may be impossible |
| Obsidian | ? | | | | |

Also verify, for every app above:

- The original clipboard is restored — including on **every** error path.
  Silently eating someone's clipboard is the fastest way to get uninstalled.
- The frontmost app is captured *before* the overlay appears and reactivated
  before pasting, or the paste goes nowhere.
- When both strategies fail, the overlay says so rather than failing silently.
- `Cmd-Z` in the host app undoes the replacement cleanly.

---

## M2 — the loop *(not yet implemented)*

- First token visible within 500ms of the trigger; 2–3 sentence rewrite under
  1.5s. Measure with the debug timing flag, and record real numbers.
- Escape cancels and the upstream generation actually stops — verify against
  the provider's usage dashboard, not just the UI.
- A 401 from the provider shows a readable message, not a raw error string.
- The overlay never steals focus from the source app.
- Text is never replaced without the user seeing the rewrite first.
