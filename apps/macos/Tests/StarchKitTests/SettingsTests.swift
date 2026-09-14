import Carbon.HIToolbox
import Foundation
import Testing

@testable import StarchKit

// Keychain is deliberately not exercised here. Reading and writing real
// generic-password items from a test binary can raise an interactive access
// prompt, which would hang an unattended run. It is covered in MANUAL_TESTS.md
// instead.

@Suite("Hot key spec")
struct HotKeySpecTests {
    @Test("the default is control-option-command-P")
    func defaultSpec() {
        let spec = HotKeySpec.default
        #expect(spec.keyCode == UInt32(kVK_ANSI_P))
        #expect(spec.modifiers.contains(.control))
        #expect(spec.modifiers.contains(.option))
        #expect(spec.modifiers.contains(.command))
        #expect(!spec.modifiers.contains(.shift))
        #expect(spec.displayString == "⌃⌥⌘P")
        #expect(spec.isValid)
    }

    @Test("modifiers render in the order macOS uses in menus")
    func displayOrder() {
        // Apple's order is control, option, shift, command.
        let spec = HotKeySpec(keyCode: UInt32(kVK_ANSI_K), modifiers: [.command, .shift, .option, .control])
        #expect(spec.displayString == "⌃⌥⇧⌘K")
    }

    @Test("named keys render as symbols rather than numbers", arguments: [
        (kVK_Space, "Space"), (kVK_Return, "↩"), (kVK_Escape, "⎋"),
        (kVK_Tab, "⇥"), (kVK_Delete, "⌫"), (kVK_LeftArrow, "←"),
        (kVK_F5, "F5"), (kVK_ANSI_Slash, "/"), (kVK_ANSI_0, "0"),
    ])
    func namedKeys(keyCode: Int, expected: String) {
        let spec = HotKeySpec(keyCode: UInt32(keyCode), modifiers: [.command])
        #expect(spec.displayString == "⌘" + expected)
    }

    @Test("an unmapped key code degrades to something readable, not a crash")
    func unknownKey() {
        let spec = HotKeySpec(keyCode: 9_999, modifiers: [.command])
        #expect(spec.displayString == "⌘Key 9999")
    }

    /// No modifier at all would swallow the plain key everywhere, and shift
    /// alone would swallow every capital letter.
    @Test("a hot key must carry command, control or option")
    func requiresPrimaryModifier() {
        let p = UInt32(kVK_ANSI_P)
        #expect(HotKeySpec(keyCode: p, modifiers: []).isValid == false)
        #expect(HotKeySpec(keyCode: p, modifiers: [.shift]).isValid == false)
    }

    /// Regression: the recorder used to accept ⌘P, which silently stopped
    /// Print working in every app on the machine. A global binding with one
    /// modifier and a character key always shadows something.
    @Test("a single modifier with a character key is refused", arguments: [
        HotKeySpec.Modifiers.command, .control, .option,
    ])
    func singleModifierWithCharacterKeyRefused(modifier: HotKeySpec.Modifiers) {
        for keyCode in [kVK_ANSI_P, kVK_ANSI_C, kVK_ANSI_0, kVK_ANSI_Slash, kVK_Return] {
            let spec = HotKeySpec(keyCode: UInt32(keyCode), modifiers: modifier)
            #expect(spec.isValid == false, "\(spec.displayString) should be refused")
            #expect(spec.invalidReason != nil)
        }
    }

    @Test("two modifiers make a character key acceptable")
    func twoModifiersAccepted() {
        let p = UInt32(kVK_ANSI_P)
        #expect(HotKeySpec(keyCode: p, modifiers: [.command, .option]).isValid)
        #expect(HotKeySpec(keyCode: p, modifiers: [.control, .command]).isValid)
        #expect(HotKeySpec.default.isValid)
        // Shift is not a primary modifier: ⇧⌘P is still only one of them.
        #expect(HotKeySpec(keyCode: p, modifiers: [.shift, .command]).isValid == false)
    }

    /// Space and the function keys carry no character, so a single modifier is
    /// safe — this is the ⌥Space shape that launchers use.
    @Test("space and function keys are fine behind one modifier")
    func singleModifierSafeKeys() {
        #expect(HotKeySpec(keyCode: UInt32(kVK_Space), modifiers: [.option]).isValid)
        #expect(HotKeySpec(keyCode: UInt32(kVK_F5), modifiers: [.command]).isValid)
        #expect(HotKeySpec(keyCode: UInt32(kVK_F12), modifiers: [.control]).isValid)
    }

    @Test("the reason names the offending shortcut so the message is actionable")
    func invalidReasonIsSpecific() throws {
        let spec = HotKeySpec(keyCode: UInt32(kVK_ANSI_P), modifiers: [.command])
        let reason = try #require(spec.invalidReason)
        #expect(reason.contains("⌘P"))
    }

    @Test("modifier raw values are the Carbon ones RegisterEventHotKey expects")
    func carbonRawValues() {
        #expect(HotKeySpec.Modifiers.command.rawValue == UInt32(cmdKey))
        #expect(HotKeySpec.Modifiers.shift.rawValue == UInt32(shiftKey))
        #expect(HotKeySpec.Modifiers.option.rawValue == UInt32(optionKey))
        #expect(HotKeySpec.Modifiers.control.rawValue == UInt32(controlKey))
    }

    @Test("survives a Codable round trip")
    func codableRoundTrip() throws {
        let spec = HotKeySpec(keyCode: UInt32(kVK_ANSI_J), modifiers: [.control, .shift])
        let decoded = try JSONDecoder().decode(HotKeySpec.self, from: JSONEncoder().encode(spec))
        #expect(decoded == spec)
    }
}

@Suite("Providers")
struct ProviderTests {
    /// These strings cross the wire to the daemon; changing one is a contract
    /// change, not a rename.
    @Test("raw values match the wire contract")
    func rawValues() {
        #expect(ProviderID.anthropic.rawValue == "anthropic")
        #expect(ProviderID.openAI.rawValue == "openai")
        #expect(ProviderID.openAICompatible.rawValue == "openai_compatible")
    }

    @Test("every provider offers a usable default endpoint and model")
    func defaults() throws {
        for provider in ProviderID.allCases {
            #expect(!provider.defaultModel.isEmpty)
            let url = try #require(URL(string: provider.defaultBaseURL))
            // Not https everywhere any more: with OpenAI split out, the
            // compatible category defaults to a local Ollama, and localhost has
            // no certificate to present.
            #expect(url.scheme == "https" || url.host == "localhost")
            #expect(!provider.displayName.isEmpty)
        }
    }

    @Test("each provider gets its own Keychain account so keys do not collide")
    func distinctKeychainAccounts() {
        let accounts = ProviderID.allCases.map(\.keychainAccount)
        #expect(Set(accounts).count == accounts.count)
    }
}

@Suite("Preferences")
struct PreferencesTests {
    /// An isolated defaults domain, removed afterwards, so tests never touch
    /// the developer's real settings.
    private func withTempDefaults(_ body: (UserDefaults) throws -> Void) rethrows {
        let suite = "starch.tests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        try body(defaults)
    }

    @Test("defaults come from the selected provider")
    func providerDefaults() {
        let anthropic = Preferences(provider: .anthropic)
        #expect(anthropic.model == ProviderID.anthropic.defaultModel)
        #expect(anthropic.baseURL == ProviderID.anthropic.defaultBaseURL)
        #expect(anthropic.hotKey == .default)
        #expect(anthropic.hasCompletedOnboarding == false)
    }

    @Test("an explicit model or base URL overrides the provider default")
    func explicitOverrides() {
        let prefs = Preferences(provider: .openAICompatible,
                                model: "llama3.1",
                                baseURL: "http://localhost:11434/v1")
        #expect(prefs.model == "llama3.1")
        #expect(prefs.baseURL == "http://localhost:11434/v1")
    }

    @Test("saved preferences load back identically")
    func roundTrip() {
        withTempDefaults { defaults in
            let store = PreferencesStore(defaults: defaults)
            let saved = Preferences(
                provider: .openAICompatible,
                model: "qwen2.5",
                baseURL: "http://localhost:1234/v1",
                thinkingEffort: "medium",
                hotKey: HotKeySpec(keyCode: UInt32(kVK_ANSI_R), modifiers: [.command, .option]),
                hasCompletedOnboarding: true,
                debugLogging: true
            )
            store.save(saved)
            #expect(store.load() == saved)
        }
    }

    @Test("an empty defaults domain yields the defaults, not a crash")
    func loadWithNothingStored() {
        withTempDefaults { defaults in
            #expect(PreferencesStore(defaults: defaults).load() == Preferences())
        }
    }

    /// Settings written by a future build, or corrupted on disk, must not stop
    /// the app launching. Losing settings beats refusing to start.
    @Test("unreadable stored preferences fall back to defaults")
    func corruptStoredData() {
        withTempDefaults { defaults in
            defaults.set(Data("not json".utf8), forKey: PreferencesStore.defaultsKey)
            #expect(PreferencesStore(defaults: defaults).load() == Preferences())
        }
    }

    /// Regression: a ⌘P binding stored before the validity rules tightened
    /// must not be registered on the next launch — it would keep Print broken
    /// system-wide with no indication why.
    @Test("an unsafe stored shortcut is repaired on load and rewritten")
    func unsafeStoredHotKeyRepaired() throws {
        try withTempDefaults { defaults in
            let store = PreferencesStore(defaults: defaults)
            let unsafe = HotKeySpec(keyCode: UInt32(kVK_ANSI_P), modifiers: [.command])

            // Write it directly: save() is not the vector, a stale plist is.
            var prefs = Preferences()
            prefs.hotKey = unsafe
            defaults.set(try JSONEncoder().encode(prefs), forKey: PreferencesStore.defaultsKey)

            #expect(store.load().hotKey == .default)
            // And the repair is persisted, not re-derived on every launch.
            let reread = try #require(defaults.data(forKey: PreferencesStore.defaultsKey))
            #expect(try JSONDecoder().decode(Preferences.self, from: reread).hotKey == .default)
        }
    }

    /// Preferences are stored as one JSON blob, so a field added in a new
    /// version must not make every existing user's settings undecodable — they
    /// would silently revert to defaults on upgrade, which for this field
    /// means a different provider and a different model.
    @Test("settings written before thinking effort existed still load")
    func decodesSettingsFromAnOlderBuild() throws {
        let older = """
        {"provider":"gemini","model":"gemini-flash-latest",
         "baseURL":"https://generativelanguage.googleapis.com/v1beta",
         "presetID":"neutral","hasCompletedOnboarding":true,"debugLogging":false}
        """
        let loaded = try JSONDecoder().decode(Preferences.self, from: Data(older.utf8))

        #expect(loaded.provider == .gemini)
        #expect(loaded.model == "gemini-flash-latest")
        #expect(loaded.presetID == "neutral")
        #expect(loaded.hasCompletedOnboarding)
        // Absent means "ask for nothing", which leaves behaviour exactly as it
        // was before the setting existed.
        #expect(loaded.thinkingEffort.isEmpty)
    }

    /// The API key belongs in the Keychain. If it ever reaches UserDefaults it
    /// lands in a plaintext plist in the user's home directory.
    @Test("no preference field can hold a secret")
    func noSecretsInPreferences() throws {
        let encoded = try JSONEncoder().encode(Preferences())
        let object = try #require(
            try JSONSerialization.jsonObject(with: encoded) as? [String: Any]
        )
        let keys = Set(object.keys.map { $0.lowercased() })
        #expect(keys.isDisjoint(with: ["apikey", "api_key", "key", "secret", "token", "password"]))
    }
}

@Suite("Paths")
struct PathsTests {
    @Test("the real socket path fits inside the sun_path limit")
    func socketPathFits() throws {
        // Not a tautology: this is the actual home directory, and a long
        // username is exactly how this limit gets hit in the wild.
        let path = try Paths.socketPath()
        #expect(path.utf8.count <= Paths.maxSocketPathLength)
        #expect(path.hasSuffix("/\(Brand.socketName)"))
    }

    @Test("an overlong path is refused with a readable error")
    func overlongPathRejected() {
        let path = "/tmp/" + String(repeating: "x", count: Paths.maxSocketPathLength)
        #expect(throws: Paths.PathError.self) {
            try Paths.validate(socketPath: path)
        }
    }

    @Test("a path exactly at the limit is accepted")
    func boundaryPathAccepted() throws {
        let path = "/" + String(repeating: "x", count: Paths.maxSocketPathLength - 1)
        #expect(path.utf8.count == Paths.maxSocketPathLength)
        try Paths.validate(socketPath: path)
    }

    @Test("the support directory is created private to the user")
    func supportDirectoryIsPrivate() throws {
        let dir = try Paths.supportDirectory()
        let attributes = try FileManager.default.attributesOfItem(atPath: dir.path)
        let permissions = try #require(attributes[.posixPermissions] as? NSNumber)
        #expect(permissions.int16Value == 0o700)
    }
}

@Suite("Daemon process helpers")
struct DaemonProcessTests {
    @Test("handshake tokens are long, unpadded and unique")
    func tokenGeneration() {
        let tokens = (0..<64).map { _ in DaemonProcess.generateToken() }

        #expect(Set(tokens).count == tokens.count)
        for token in tokens {
            // 32 random bytes, base64url, padding stripped.
            #expect(token.count == 43)
            #expect(!token.contains("="))
            #expect(!token.contains("+"))
            #expect(!token.contains("/"))
            // Must survive an HTTP header value unescaped.
            #expect(token.allSatisfy { $0.isLetter || $0.isNumber || $0 == "-" || $0 == "_" })
        }
    }

    @Test("durations are formatted the way Go's ParseDuration reads them")
    func durationFormatting() {
        #expect(DaemonProcess.durationString(.seconds(30)) == "30s")
        #expect(DaemonProcess.durationString(.seconds(90)) == "90s")
        #expect(DaemonProcess.durationString(.milliseconds(1500)) == "1s")
    }

    @Test("the heartbeat runs well inside the daemon's idle timeout")
    func heartbeatFitsIdleWindow() {
        // If these ever cross, the daemon exits under a healthy shell.
        #expect(DaemonProcess.heartbeatInterval < DaemonProcess.idleTimeout / 3)
    }
}

@Suite("Presets")
struct PresetListTests {
    private let fromFile = [
        Preset(id: "pirate", name: "Pirate"),
        Preset(id: "terse", name: "Terse"),
    ]

    @Test("cycling walks the daemon's list, not the built-in one")
    func cyclesTheFetchedList() {
        #expect(Presets.next(after: "pirate", in: fromFile) == "terse")
        // And wraps.
        #expect(Presets.next(after: "terse", in: fromFile) == "pirate")
    }

    /// A preset can vanish when the file is edited mid-session. Cycling from
    /// an id that is no longer there must land somewhere real rather than
    /// leaving Tab dead.
    @Test("an unknown id cycles into the list rather than nowhere")
    func unknownIdRecovers() {
        #expect(Presets.next(after: "deleted", in: fromFile) == "pirate")
        #expect(Presets.named("deleted", in: fromFile).id == "pirate")
    }

    @Test("an empty list falls back to the built-ins instead of crashing")
    func emptyListFallsBack() {
        #expect(Presets.named("anything", in: []).id == Presets.defaultID)
        // An id that is not in the list lands on the first entry, which is
        // where cycling should resume when the current preset was deleted.
        #expect(Presets.next(after: "anything", in: []) == Presets.all[0].id)
        #expect(Presets.next(after: Presets.all[0].id, in: []) == Presets.all[1].id)
    }

    @Test("a known id resolves to its own entry")
    func knownId() {
        #expect(Presets.named("terse", in: fromFile).name == "Terse")
    }

    /// The shell decodes only id and name. Instruction text is the daemon's,
    /// so it cannot drift between the two.
    @Test("decoding ignores the instruction")
    func decodesIdAndNameOnly() throws {
        let json = Data("""
        {"presets":[{"id":"a","name":"A","instruction":"ignored here"}],
         "path":"/tmp/presets.json","source":"file"}
        """.utf8)

        let set = try JSONDecoder().decode(PresetSet.self, from: json)
        #expect(set.presets.count == 1)
        #expect(set.presets[0].id == "a")
        #expect(set.usingDefaults == false)
        #expect(set.problem == nil)
    }

    @Test("a reported problem is surfaced, not swallowed")
    func decodesProblem() throws {
        let json = Data("""
        {"presets":[{"id":"a","name":"A"}],"path":"/tmp/p.json",
         "source":"defaults","problem":"presets.json is not valid JSON (line 3)"}
        """.utf8)

        let set = try JSONDecoder().decode(PresetSet.self, from: json)
        #expect(set.usingDefaults)
        #expect(set.problem?.contains("line 3") == true)
    }
}
