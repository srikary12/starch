import Carbon.HIToolbox
import Foundation
import Security

// MARK: - Keychain

/// Thin wrapper over the generic-password Keychain.
///
/// This is the only place an API key is ever stored. It never goes into
/// UserDefaults, never onto disk in the Go layer, and never into a log. The
/// daemon receives it over the socket at `POST /v1/session` and holds it in
/// memory for the life of the process.
public struct Keychain: Sendable {
    public enum KeychainError: Error, LocalizedError {
        case unexpectedStatus(OSStatus)

        public var errorDescription: String? {
            switch self {
            case let .unexpectedStatus(status):
                let detail = SecCopyErrorMessageString(status, nil) as String?
                return detail.map { "Keychain error: \($0)" } ?? "Keychain error \(status)"
            }
        }
    }

    public let service: String

    public init(service: String = Brand.bundleIdentifier) {
        self.service = service
    }

    private func query(account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    /// Stores or replaces the secret for `account`.
    public func set(_ secret: String, account: String) throws {
        let data = Data(secret.utf8)

        // Try an in-place update first so the item keeps its existing ACL and
        // the user is not re-prompted.
        let updateStatus = SecItemUpdate(
            query(account: account) as CFDictionary,
            [kSecValueData as String: data] as CFDictionary
        )
        if updateStatus == errSecSuccess { return }
        guard updateStatus == errSecItemNotFound else {
            throw KeychainError.unexpectedStatus(updateStatus)
        }

        var attributes = query(account: account)
        attributes[kSecValueData as String] = data
        // The app is a menu bar utility the user drives interactively, so
        // there is no reason for the key to be readable while locked.
        attributes[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlocked

        let addStatus = SecItemAdd(attributes as CFDictionary, nil)
        guard addStatus == errSecSuccess else {
            throw KeychainError.unexpectedStatus(addStatus)
        }
    }

    /// Returns the secret for `account`, or nil if there is none.
    public func get(account: String) throws -> String? {
        var q = query(account: account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne

        var item: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &item)
        switch status {
        case errSecSuccess:
            guard let data = item as? Data else { return nil }
            return String(decoding: data, as: UTF8.self)
        case errSecItemNotFound:
            return nil
        default:
            throw KeychainError.unexpectedStatus(status)
        }
    }

    /// Removes the secret for `account`. Absent is not an error.
    public func delete(account: String) throws {
        let status = SecItemDelete(query(account: account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainError.unexpectedStatus(status)
        }
    }

    /// Whether a secret exists, without reading it.
    ///
    /// Uses a metadata-only query so the settings window can show "key saved"
    /// without decrypting anything or triggering an access prompt.
    public func hasSecret(account: String) -> Bool {
        var q = query(account: account)
        q[kSecReturnData as String] = false
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        return SecItemCopyMatching(q as CFDictionary, nil) == errSecSuccess
    }
}

// MARK: - Providers

/// Providers the daemon can talk to. Two implementations cover nearly
/// everything: the Anthropic Messages API, and any OpenAI-compatible chat
/// completions endpoint (OpenAI, OpenRouter, Groq, Together, Ollama, LM Studio).
public enum ProviderID: String, CaseIterable, Sendable, Codable {
    case anthropic
    case openAICompatible = "openai_compatible"

    public var displayName: String {
        switch self {
        case .anthropic: "Anthropic"
        case .openAICompatible: "OpenAI-compatible"
        }
    }

    public var defaultBaseURL: String {
        switch self {
        case .anthropic: "https://api.anthropic.com"
        case .openAICompatible: "https://api.openai.com/v1"
        }
    }

    public var defaultModel: String {
        switch self {
        case .anthropic: "claude-sonnet-5"
        case .openAICompatible: "gpt-4o-mini"
        }
    }

    /// Keychain account name, so switching providers does not clobber the
    /// other one's key.
    public var keychainAccount: String { "api-key.\(rawValue)" }

    /// Whether a local endpoint means no key is needed. Ollama and LM Studio
    /// accept anything, which matters for users who will not send work text to
    /// a third party at all.
    public var requiresAPIKey: Bool {
        switch self {
        case .anthropic: true
        case .openAICompatible: false
        }
    }
}

// MARK: - Hot key

/// A global hot key, stored as Carbon key code plus Carbon modifier mask.
///
/// Carbon values rather than Cocoa ones because `RegisterEventHotKey` is what
/// consumes them, and converting once at the edge beats converting on every
/// registration.
public struct HotKeySpec: Sendable, Equatable, Codable {
    public struct Modifiers: OptionSet, Sendable, Equatable, Codable {
        public let rawValue: UInt32
        public init(rawValue: UInt32) { self.rawValue = rawValue }

        public static let command = Modifiers(rawValue: UInt32(cmdKey))
        public static let shift = Modifiers(rawValue: UInt32(shiftKey))
        public static let option = Modifiers(rawValue: UInt32(optionKey))
        public static let control = Modifiers(rawValue: UInt32(controlKey))
    }

    public var keyCode: UInt32
    public var modifiers: Modifiers

    public init(keyCode: UInt32, modifiers: Modifiers) {
        self.keyCode = keyCode
        self.modifiers = modifiers
    }

    /// ⌃⌥⌘P. Chosen to be unlikely to collide with an app shortcut.
    public static let `default` = HotKeySpec(
        keyCode: UInt32(kVK_ANSI_P),
        modifiers: [.control, .option, .command]
    )

    /// Keys safe to bind behind a single modifier: they carry no character and
    /// shadow nothing people type. Everything else needs two modifiers.
    private static let singleModifierSafeKeys: Set<Int> = [
        kVK_Space,
        kVK_F1, kVK_F2, kVK_F3, kVK_F4, kVK_F5, kVK_F6,
        kVK_F7, kVK_F8, kVK_F9, kVK_F10, kVK_F11, kVK_F12,
    ]

    public var isValid: Bool { invalidReason == nil }

    /// Why this combination cannot be used as a global hot key, or nil.
    ///
    /// This is stricter than it looks, deliberately. The binding is global, so
    /// a bad choice does not just fail to work — it silently breaks a shortcut
    /// in every other app, with no clue as to why.
    public var invalidReason: String? {
        let primary = modifiers.intersection([.command, .control, .option])

        if primary.isEmpty {
            return "A shortcut needs Command, Control or Option. Without one it would "
                + "swallow ordinary typing in every app."
        }

        // One modifier plus a character key shadows something people rely on:
        // ⌘P is Print, ⌃A jumps to line start in every text field, and ⌥P
        // types π. Function keys and Space are the exceptions.
        if primary.rawValue.nonzeroBitCount == 1,
           !Self.singleModifierSafeKeys.contains(Int(keyCode))
        {
            return "\(displayString) would override that shortcut in every app — ⌘P would "
                + "stop Print working everywhere. Add a second modifier."
        }

        return nil
    }

    /// Menu-style rendering, e.g. "⌃⌥⌘P".
    public var displayString: String {
        var out = ""
        if modifiers.contains(.control) { out += "⌃" }
        if modifiers.contains(.option) { out += "⌥" }
        if modifiers.contains(.shift) { out += "⇧" }
        if modifiers.contains(.command) { out += "⌘" }
        out += Self.keyName(for: keyCode)
        return out
    }

    /// Name for a key code.
    ///
    /// A static ANSI table rather than `UCKeyTranslate`, so the result is
    /// deterministic and testable. The cost is that a non-ANSI layout may see
    /// the ANSI letter for a physical key; the hot key still fires on the same
    /// physical key, which is what Carbon registers.
    static func keyName(for keyCode: UInt32) -> String {
        if let name = namedKeys[Int(keyCode)] { return name }
        return "Key \(keyCode)"
    }

    private static let namedKeys: [Int: String] = [
        kVK_ANSI_A: "A", kVK_ANSI_B: "B", kVK_ANSI_C: "C", kVK_ANSI_D: "D",
        kVK_ANSI_E: "E", kVK_ANSI_F: "F", kVK_ANSI_G: "G", kVK_ANSI_H: "H",
        kVK_ANSI_I: "I", kVK_ANSI_J: "J", kVK_ANSI_K: "K", kVK_ANSI_L: "L",
        kVK_ANSI_M: "M", kVK_ANSI_N: "N", kVK_ANSI_O: "O", kVK_ANSI_P: "P",
        kVK_ANSI_Q: "Q", kVK_ANSI_R: "R", kVK_ANSI_S: "S", kVK_ANSI_T: "T",
        kVK_ANSI_U: "U", kVK_ANSI_V: "V", kVK_ANSI_W: "W", kVK_ANSI_X: "X",
        kVK_ANSI_Y: "Y", kVK_ANSI_Z: "Z",
        kVK_ANSI_0: "0", kVK_ANSI_1: "1", kVK_ANSI_2: "2", kVK_ANSI_3: "3",
        kVK_ANSI_4: "4", kVK_ANSI_5: "5", kVK_ANSI_6: "6", kVK_ANSI_7: "7",
        kVK_ANSI_8: "8", kVK_ANSI_9: "9",
        kVK_ANSI_Minus: "-", kVK_ANSI_Equal: "=",
        kVK_ANSI_LeftBracket: "[", kVK_ANSI_RightBracket: "]",
        kVK_ANSI_Backslash: "\\", kVK_ANSI_Semicolon: ";", kVK_ANSI_Quote: "'",
        kVK_ANSI_Comma: ",", kVK_ANSI_Period: ".", kVK_ANSI_Slash: "/",
        kVK_ANSI_Grave: "`",
        kVK_Return: "↩", kVK_Tab: "⇥", kVK_Space: "Space", kVK_Delete: "⌫",
        kVK_ForwardDelete: "⌦", kVK_Escape: "⎋",
        kVK_LeftArrow: "←", kVK_RightArrow: "→", kVK_UpArrow: "↑", kVK_DownArrow: "↓",
        kVK_Home: "↖", kVK_End: "↘", kVK_PageUp: "⇞", kVK_PageDown: "⇟",
        kVK_F1: "F1", kVK_F2: "F2", kVK_F3: "F3", kVK_F4: "F4",
        kVK_F5: "F5", kVK_F6: "F6", kVK_F7: "F7", kVK_F8: "F8",
        kVK_F9: "F9", kVK_F10: "F10", kVK_F11: "F11", kVK_F12: "F12",
    ]
}

// MARK: - Preferences

/// Non-secret settings.
///
/// Nothing sensitive belongs in here: this is backed by UserDefaults, which is
/// a plist in the user's home directory. The API key lives in `Keychain`.
public struct Preferences: Sendable, Equatable, Codable {
    public var provider: ProviderID
    public var model: String
    public var baseURL: String
    public var hotKey: HotKeySpec
    public var hasCompletedOnboarding: Bool
    public var debugLogging: Bool

    public init(
        provider: ProviderID = .anthropic,
        model: String? = nil,
        baseURL: String? = nil,
        hotKey: HotKeySpec = .default,
        hasCompletedOnboarding: Bool = false,
        debugLogging: Bool = false
    ) {
        self.provider = provider
        self.model = model ?? provider.defaultModel
        self.baseURL = baseURL ?? provider.defaultBaseURL
        self.hotKey = hotKey
        self.hasCompletedOnboarding = hasCompletedOnboarding
        self.debugLogging = debugLogging
    }
}

/// Loads and saves `Preferences` as a single JSON blob in UserDefaults.
///
/// One blob rather than a key per field so that adding a setting cannot leave
/// a half-migrated defaults domain behind.
///
/// Not `Sendable`: UserDefaults is thread-safe but unannotated, and this is
/// only ever touched from the main actor, so there is nothing to gain from
/// asserting otherwise.
public struct PreferencesStore {
    static let defaultsKey = "preferences.v1"

    private let defaults: UserDefaults

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    public func load() -> Preferences {
        guard let data = defaults.data(forKey: Self.defaultsKey) else {
            return Preferences()
        }
        do {
            var preferences = try JSONDecoder().decode(Preferences.self, from: data)
            // A stored shortcut can predate the current validity rules, or
            // come from a hand-edited plist. Registering an unsafe one would
            // break that key in every app, so fall back rather than honour it.
            if !preferences.hotKey.isValid {
                Log.app.error(
                    "stored shortcut \(preferences.hotKey.displayString, privacy: .public) is unsafe, reverting to default"
                )
                preferences.hotKey = .default
                save(preferences)
            }
            return preferences
        } catch {
            // Corrupt or from a future build. Defaults are always usable, and
            // losing settings beats refusing to launch.
            Log.app.error("preferences unreadable, using defaults: \(error.localizedDescription, privacy: .public)")
            return Preferences()
        }
    }

    public func save(_ preferences: Preferences) {
        do {
            defaults.set(try JSONEncoder().encode(preferences), forKey: Self.defaultsKey)
        } catch {
            Log.app.error("could not save preferences: \(error.localizedDescription, privacy: .public)")
        }
    }
}
