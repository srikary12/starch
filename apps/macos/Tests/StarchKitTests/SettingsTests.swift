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

    /// A hot key with no modifier would swallow the plain key system-wide, and
    /// shift alone would swallow every capital letter.
    @Test("a hot key must carry command, control or option")
    func validity() {
        let p = UInt32(kVK_ANSI_P)
        #expect(HotKeySpec(keyCode: p, modifiers: []).isValid == false)
        #expect(HotKeySpec(keyCode: p, modifiers: [.shift]).isValid == false)
        #expect(HotKeySpec(keyCode: p, modifiers: [.command]).isValid)
        #expect(HotKeySpec(keyCode: p, modifiers: [.control]).isValid)
        #expect(HotKeySpec(keyCode: p, modifiers: [.option]).isValid)
        #expect(HotKeySpec(keyCode: p, modifiers: [.shift, .command]).isValid)
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
        #expect(ProviderID.openAICompatible.rawValue == "openai_compatible")
    }

    @Test("every provider offers a usable default endpoint and model")
    func defaults() {
        for provider in ProviderID.allCases {
            #expect(!provider.defaultModel.isEmpty)
            #expect(URL(string: provider.defaultBaseURL) != nil)
            #expect(provider.defaultBaseURL.hasPrefix("https://"))
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
                hotKey: HotKeySpec(keyCode: UInt32(kVK_ANSI_R), modifiers: [.command, .shift]),
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
