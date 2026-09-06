import AppKit
@preconcurrency import ApplicationServices
import Carbon.HIToolbox
import StarchKit

// MARK: - Global hot key

/// Registers the global hot key through Carbon's `RegisterEventHotKey`.
///
/// The modern-looking alternative, `NSEvent.addGlobalMonitorForEvents`, is the
/// wrong tool: a global monitor observes events but cannot consume them, so
/// the keystroke would also reach whatever app the user is typing in. Carbon's
/// hot key API consumes the event and needs no extra permission. It is old,
/// not deprecated, and still the correct answer.
@MainActor
final class HotKeyManager {
    enum HotKeyError: Error, LocalizedError {
        case invalidCombination
        case registrationFailed(OSStatus)

        var errorDescription: String? {
            switch self {
            case .invalidCombination:
                "A shortcut needs at least one of Command, Control or Option."
            case let .registrationFailed(status):
                status == OSStatus(eventHotKeyExistsErr)
                    ? "That shortcut is already claimed by another app."
                    : "Could not register the shortcut (Carbon error \(status))."
            }
        }
    }

    private var hotKeyRef: EventHotKeyRef?
    private var handlerRef: EventHandlerRef?
    private var onFire: (() -> Void)?

    /// Distinguishes our hot key from any other Carbon client in-process.
    private static let signature: OSType = {
        let chars = Array("STCH".utf8)
        return chars.reduce(OSType(0)) { ($0 << 8) | OSType($1) }
    }()

    private(set) var current: HotKeySpec?

    /// Replaces any existing registration with `spec`.
    func register(_ spec: HotKeySpec, onFire: @escaping () -> Void) throws {
        guard spec.isValid else { throw HotKeyError.invalidCombination }
        unregister()
        self.onFire = onFire

        installHandlerIfNeeded()

        var ref: EventHotKeyRef?
        let id = EventHotKeyID(signature: Self.signature, id: 1)
        let status = RegisterEventHotKey(
            spec.keyCode,
            spec.modifiers.rawValue,
            id,
            GetApplicationEventTarget(),
            0,
            &ref
        )
        guard status == noErr, let ref else {
            self.onFire = nil
            throw HotKeyError.registrationFailed(status)
        }

        hotKeyRef = ref
        current = spec
        Log.app.info("registered hot key \(spec.displayString, privacy: .public)")
    }

    func unregister() {
        if let hotKeyRef {
            UnregisterEventHotKey(hotKeyRef)
            self.hotKeyRef = nil
        }
        current = nil
        onFire = nil
    }

    private func installHandlerIfNeeded() {
        guard handlerRef == nil else { return }

        var eventType = EventTypeSpec(
            eventClass: OSType(kEventClassKeyboard),
            eventKind: UInt32(kEventHotKeyPressed)
        )
        // A C function pointer cannot capture, so self travels through
        // userData. Unretained: the manager outlives the handler, and
        // retaining here would make the cycle permanent.
        let context = Unmanaged.passUnretained(self).toOpaque()

        InstallEventHandler(
            GetApplicationEventTarget(),
            { _, _, userData -> OSStatus in
                guard let userData else { return OSStatus(eventNotHandledErr) }
                let manager = Unmanaged<HotKeyManager>.fromOpaque(userData).takeUnretainedValue()
                // Carbon dispatches this on the main thread from the app's
                // run loop, so this is a real assertion, not a hop. Avoiding
                // the hop matters: this is the first millisecond of the
                // latency budget.
                MainActor.assumeIsolated { manager.fire() }
                return noErr
            },
            1,
            &eventType,
            context,
            &handlerRef
        )
    }

    private func fire() {
        onFire?()
    }

    // No deinit cleanup: a nonisolated deinit cannot touch main-actor state,
    // and this manager is owned by the app delegate for the whole process
    // lifetime anyway. `applicationWillTerminate` calls `unregister`.
}

extension HotKeySpec {
    /// Builds a spec from a recorded key event, translating Cocoa modifier
    /// flags into the Carbon mask `RegisterEventHotKey` wants.
    init(event: NSEvent) {
        var modifiers: Modifiers = []
        let flags = event.modifierFlags
        if flags.contains(.command) { modifiers.insert(.command) }
        if flags.contains(.control) { modifiers.insert(.control) }
        if flags.contains(.option) { modifiers.insert(.option) }
        if flags.contains(.shift) { modifiers.insert(.shift) }
        self.init(keyCode: UInt32(event.keyCode), modifiers: modifiers)
    }
}

// MARK: - Accessibility permission

// `kAXTrustedCheckOptionPrompt` is imported from C as a mutable global, so
// Swift 6 will not let concurrent code touch it directly. HIServices writes it
// once at load time and never again, which is why the import above is marked
// @preconcurrency and why reading it into an immutable global here is safe.
private let axTrustedCheckOptionPrompt = kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String

/// The Accessibility (AXUIElement) permission.
///
/// Required for the hot key path, because reading the selection and writing
/// the rewrite back both go through the Accessibility API. This app is asking
/// to read what people type, so it is never requested without an explanation
/// on screen first.
@MainActor
enum Accessibility {
    static var isTrusted: Bool { AXIsProcessTrusted() }

    /// Asks macOS to show the permission dialog.
    ///
    /// The dialog only appears once per app identity; afterwards this is a
    /// no-op and the user has to go to System Settings, which is why the
    /// onboarding window always offers that route too.
    @discardableResult
    static func requestTrust() -> Bool {
        AXIsProcessTrustedWithOptions([axTrustedCheckOptionPrompt: true] as CFDictionary)
    }

    static func openSystemSettings() {
        let url = URL(
            string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
        )!
        NSWorkspace.shared.open(url)
    }

    /// Polls the trust state and reports changes.
    ///
    /// macOS has no reliable public notification for a permission grant, and
    /// the grant does not take effect in-process until it is re-read, so
    /// polling while a window is showing the status is the pragmatic option.
    /// Nothing polls when no window is open.
    @MainActor
    final class Watcher {
        private var timer: Timer?
        private var lastValue: Bool
        private var onChange: ((Bool) -> Void)?

        init() { lastValue = Accessibility.isTrusted }

        /// Owners must call `stop`; there is no deinit cleanup because a
        /// main-actor deinit cannot touch isolated state. Both call sites do
        /// so from `windowWillClose` and `applicationWillTerminate`.
        func start(interval: TimeInterval = 1.0, onChange: @escaping (Bool) -> Void) {
            stop()
            self.onChange = onChange
            timer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated { self?.tick() }
            }
        }

        func stop() {
            timer?.invalidate()
            timer = nil
            onChange = nil
        }

        private func tick() {
            let now = Accessibility.isTrusted
            guard now != lastValue else { return }
            lastValue = now
            Log.permissions.info("accessibility trust changed to \(now, privacy: .public)")
            onChange?(now)
        }
    }
}
