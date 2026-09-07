import AppKit
import Foundation

// Whether the right-click → Starch entry is switched on.
//
// macOS registers a newly-installed third-party text service as *disabled*.
// The result is that a fresh install has the item sitting in System Settings
// with its checkbox clear, and absent from every context menu — nothing about
// the app is wrong, and there is no feedback anywhere saying so.
//
// That matters more than it looks. The Services route is meant to be the
// low-commitment way to try Starch before granting Accessibility, so it is the
// first thing a new user reaches for. Right-clicking and finding nothing reads
// as "this feature is broken", not as "there is a checkbox four levels deep in
// System Settings". Hence detecting it and saying so.
//
// Deliberately read-only. The state is writable — it is just a preference in
// the `pbs` domain — but silently inserting ourselves into every app's context
// menu should be the user's explicit choice, and the format is undocumented
// enough that writing it would be fragile as well as rude.

public enum ServicesMenuState: String, Sendable {
    case enabled
    case disabled
    /// `pbs` has no record of the service. This is where a fresh install sits,
    /// and it behaves exactly like `disabled` — it is distinguished only so the
    /// UI can explain it as "not switched on yet" rather than "you turned it off".
    case notConfigured

    public var isUsable: Bool { self == .enabled }
}

public enum ServicesMenu {
    /// Must match `NSMessage` in Info.plist.
    public static let message = "rewriteSelection"

    /// The preferences domain macOS keeps Services enablement in.
    static let domain = "pbs"
    static let statusesKey = "NSServicesStatus"

    /// The key `pbs` files a service's status under.
    ///
    /// The format is `"<bundle id> - <menu item title> - <NSMessage>"`, which
    /// means it silently changes if the menu title in `NSServices` changes.
    /// Renaming the menu item therefore resets the user's choice, and they get
    /// a fresh unticked entry with no indication why.
    public static func statusKey(
        bundleIdentifier: String = Brand.bundleIdentifier,
        menuItemTitle: String = Brand.name,
        message: String = ServicesMenu.message
    ) -> String {
        "\(bundleIdentifier) - \(menuItemTitle) - \(message)"
    }

    public static func state() -> ServicesMenuState {
        // Another process owns this domain and we are reading it live, so the
        // cached copy in this process may be stale.
        CFPreferencesAppSynchronize(domain as CFString)
        let raw = CFPreferencesCopyAppValue(statusesKey as CFString, domain as CFString)
        return state(from: raw as? [String: Any], key: statusKey())
    }

    /// Split out from `state()` so the parsing is testable without touching
    /// real preferences.
    static func state(from statuses: [String: Any]?, key: String) -> ServicesMenuState {
        guard let entry = statuses?[key] as? [String: Any] else { return .notConfigured }

        // Current macOS writes both a nested presentation_modes dictionary and
        // the older flat keys. Prefer the nested one, fall back to the flat.
        if let modes = entry["presentation_modes"] as? [String: Any],
           let contextMenu = (modes["ContextMenu"] as? NSNumber)?.boolValue
        {
            return contextMenu ? .enabled : .disabled
        }
        if let flat = (entry["enabled_context_menu"] as? NSNumber)?.boolValue {
            return flat ? .enabled : .disabled
        }
        return .notConfigured
    }

    /// Opens System Settings at the Services list.
    public static func openSettings() {
        // Both spellings resolve on current macOS; the extension form is the
        // modern one, so try it first.
        let candidates = [
            "x-apple.systempreferences:com.apple.Keyboard-Settings.extension?Services",
            "x-apple.systempreferences:com.apple.preference.keyboard?Services",
        ]
        for candidate in candidates {
            if let url = URL(string: candidate), NSWorkspace.shared.open(url) { return }
        }
        Log.app.error("could not open the Services settings pane")
    }
}
