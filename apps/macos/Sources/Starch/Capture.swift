import AppKit
@preconcurrency import ApplicationServices
import Carbon.HIToolbox
import StarchKit

// Reading the current selection out of whatever app the user is in.
//
// Two strategies, tried in order. The Accessibility API is preferred because
// it touches nothing the user owns; the clipboard round-trip is the fallback
// for the large amount of Electron and web content where AX cannot see the
// selection.
//
// Worth being precise about permissions, because it is easy to get backwards:
// *both* hot-key strategies need Accessibility. AX obviously does, and posting
// a synthetic Cmd-C through CGEvent also requires the process to be trusted.
// The clipboard fallback is a fallback for apps that do not implement AX
// properly, not a way around the permission. Only the Services path — where
// macOS hands us the text itself — works with no permission at all.

// MARK: - Accessibility

enum AccessibilityCapture {
    struct Selection {
        let element: AXUIElement
        let text: String
        /// Whether `kAXSelectedTextAttribute` can be written back on this
        /// element. Probed, not assumed: plenty of apps expose the selection
        /// read-only, and knowing before we try is what lets the replace path
        /// fall through cleanly in M2.
        let isReplaceable: Bool
    }

    enum Failure: Error, LocalizedError {
        case notTrusted
        case noFocusedElement(AXError)
        case attributeUnavailable(AXError)
        case notAString
        case emptySelection

        var errorDescription: String? {
            switch self {
            case .notTrusted:
                "Accessibility access has not been granted."
            case let .noFocusedElement(code):
                "No focused element (\(code.explanation))."
            case let .attributeUnavailable(code):
                "The app does not expose its selection (\(code.explanation))."
            case .notAString:
                "The selection is not text."
            case .emptySelection:
                "Nothing is selected."
            }
        }
    }

    /// How long to wait for an app to answer an Accessibility query.
    ///
    /// AX calls are synchronous IPC into the target app and will happily block
    /// for the system default of six seconds. An app that cannot answer in a
    /// quarter of a second is not worth waiting for — the clipboard fallback
    /// will beat it, and the whole budget to first token is 500ms.
    static let messagingTimeout: Float = 0.25

    static func read() throws -> Selection {
        guard AXIsProcessTrusted() else { throw Failure.notTrusted }

        let system = AXUIElementCreateSystemWide()
        AXUIElementSetMessagingTimeout(system, messagingTimeout)

        var focusedValue: CFTypeRef?
        let focusStatus = AXUIElementCopyAttributeValue(
            system, kAXFocusedUIElementAttribute as CFString, &focusedValue
        )
        guard focusStatus == .success,
              let focused = focusedValue,
              CFGetTypeID(focused) == AXUIElementGetTypeID()
        else {
            throw Failure.noFocusedElement(focusStatus)
        }
        let element = focused as! AXUIElement
        AXUIElementSetMessagingTimeout(element, messagingTimeout)

        var selectedValue: CFTypeRef?
        let selectionStatus = AXUIElementCopyAttributeValue(
            element, kAXSelectedTextAttribute as CFString, &selectedValue
        )
        guard selectionStatus == .success else {
            throw Failure.attributeUnavailable(selectionStatus)
        }
        guard let text = selectedValue as? String else { throw Failure.notAString }
        guard !text.isEmpty else { throw Failure.emptySelection }

        var settable: DarwinBoolean = false
        AXUIElementIsAttributeSettable(element, kAXSelectedTextAttribute as CFString, &settable)

        return Selection(element: element, text: text, isReplaceable: settable.boolValue)
    }
}

extension AXError {
    /// Short name for an AXError, since the raw values are opaque in a log.
    var explanation: String {
        switch self {
        case .success: "success"
        case .apiDisabled: "Accessibility API disabled"
        case .invalidUIElement: "invalid element"
        case .cannotComplete: "app did not respond"
        case .attributeUnsupported: "attribute unsupported"
        case .noValue: "no value"
        case .notImplemented: "app does not implement Accessibility"
        case .actionUnsupported: "action unsupported"
        case .parameterizedAttributeUnsupported: "parameterized attribute unsupported"
        case .notEnoughPrecision: "not enough precision"
        case .failure: "failure"
        case .illegalArgument: "illegal argument"
        case .invalidUIElementObserver: "invalid observer"
        case .notificationUnsupported: "notification unsupported"
        case .notificationAlreadyRegistered: "notification already registered"
        case .notificationNotRegistered: "notification not registered"
        @unknown default: "AXError \(rawValue)"
        }
    }
}

// MARK: - Synthetic keystrokes

enum Keystroke {
    /// Waits for the user to let go of the modifiers they triggered with.
    ///
    /// This is the bug that would otherwise ship: a synthetic Cmd-C posted
    /// while ⌃⌥⌘ is still physically held arrives at the host app as ⌃⌥⌘C,
    /// which almost nothing treats as copy. The event's own flags do not
    /// override the physically-held ones at the HID tap. Waiting is the fix.
    ///
    /// Returns whether the modifiers actually cleared.
    @discardableResult
    static func waitForModifiersToClear(timeout: Duration = .milliseconds(400)) async -> Bool {
        let deadline = ContinuousClock.now.advanced(by: timeout)
        while ContinuousClock.now < deadline {
            if heldModifiers.isEmpty { return true }
            try? await Task.sleep(for: .milliseconds(5))
        }
        return heldModifiers.isEmpty
    }

    private static var heldModifiers: CGEventFlags {
        CGEventSource.flagsState(.combinedSessionState)
            .intersection([.maskCommand, .maskControl, .maskAlternate, .maskShift])
    }

    /// Posts a key down/up pair to the system event tap.
    ///
    /// Requires Accessibility trust; without it the events are silently
    /// dropped, which is why callers check first.
    static func send(key: CGKeyCode, flags: CGEventFlags) {
        let source = CGEventSource(stateID: .combinedSessionState)
        guard let down = CGEvent(keyboardEventSource: source, virtualKey: key, keyDown: true),
              let up = CGEvent(keyboardEventSource: source, virtualKey: key, keyDown: false)
        else {
            Log.capture.error("could not construct synthetic key event")
            return
        }
        down.flags = flags
        up.flags = flags
        down.post(tap: .cghidEventTap)
        up.post(tap: .cghidEventTap)
    }

    static func sendCommandC() { send(key: CGKeyCode(kVK_ANSI_C), flags: .maskCommand) }
    static func sendCommandV() { send(key: CGKeyCode(kVK_ANSI_V), flags: .maskCommand) }
}

// MARK: - Orchestration

@MainActor
final class SelectionCapturer {
    enum Strategy: String {
        /// Read straight out of the app through the Accessibility API.
        case accessibility
        /// Driven through a synthetic copy and the clipboard.
        case clipboard
        /// Handed to us by macOS via the Services menu.
        case services
    }

    struct Capture {
        let text: String
        let strategy: Strategy
        /// Captured *before* anything else happens, so the overlay can hand
        /// focus back to the right app before pasting.
        let sourceApp: NSRunningApplication?
        let replaceableViaAX: Bool
        let element: AXUIElement?
        let duration: Duration

        var appName: String { sourceApp?.localizedName ?? "an unknown app" }

        /// A short, elided preview for on-screen feedback.
        ///
        /// Deliberately never logged: the selection is the most sensitive
        /// thing this app touches, and the unified log outlives the process.
        var preview: String {
            let flattened = text
                .replacingOccurrences(of: "\n", with: " ")
                .trimmingCharacters(in: .whitespacesAndNewlines)
            return flattened.count <= 60 ? flattened : String(flattened.prefix(59)) + "…"
        }
    }

    enum Failure: Error, LocalizedError {
        case notTrusted
        case bothStrategiesFailed(accessibility: String, clipboard: String)

        var errorDescription: String? {
            switch self {
            case .notTrusted:
                "Grant Accessibility access to use the keyboard shortcut. "
                    + "Right-click → \(Brand.name) works without it."
            case let .bothStrategiesFailed(accessibility, clipboard):
                "Could not read the selection. Accessibility: \(accessibility) Clipboard: \(clipboard)"
            }
        }
    }

    private let clipboard = ClipboardCapture()

    /// Captures the selection for the hot-key trigger.
    func capture() async -> Result<Capture, Failure> {
        // Before anything else: the overlay must be able to give focus back to
        // the app the user was actually in, and any step below can change what
        // is frontmost.
        let sourceApp = NSWorkspace.shared.frontmostApplication
        let started = ContinuousClock.now

        guard AXIsProcessTrusted() else {
            // Not just an AX failure: the clipboard fallback posts synthetic
            // events, which needs the same permission. There is nothing to
            // fall back to, so say so plainly rather than timing out.
            Log.capture.error("capture attempted without Accessibility trust")
            return .failure(.notTrusted)
        }

        var accessibilityReason = "not attempted."
        do {
            let selection = try AccessibilityCapture.read()
            let capture = Capture(
                text: selection.text,
                strategy: .accessibility,
                sourceApp: sourceApp,
                replaceableViaAX: selection.isReplaceable,
                element: selection.element,
                duration: started.duration(to: .now)
            )
            log(capture)
            return .success(capture)
        } catch {
            accessibilityReason = error.localizedDescription
            Log.capture.info("accessibility path unavailable: \(accessibilityReason, privacy: .public)")
        }

        // Fall through to the clipboard. The modifier wait is what makes the
        // synthetic copy actually read as a copy.
        do {
            let released = await Keystroke.waitForModifiersToClear()
            if !released {
                Log.capture.info("modifiers still held; synthetic copy may be misread")
            }

            let text = try await clipboard.captureText { Keystroke.sendCommandC() }
            // No second AX probe here. We are on this path precisely because
            // the AX read failed, so a re-probe would almost certainly fail
            // the same way — and pay another messagingTimeout to do it, on the
            // path that is already the slow one. Assume paste-only; M2 can try
            // an AX write opportunistically and fall back if it does not take.
            let capture = Capture(
                text: text,
                strategy: .clipboard,
                sourceApp: sourceApp,
                replaceableViaAX: false,
                element: nil,
                duration: started.duration(to: .now)
            )
            log(capture)
            return .success(capture)
        } catch {
            let clipboardReason = error.localizedDescription
            Log.capture.error(
                """
                both capture strategies failed — \
                ax: \(accessibilityReason, privacy: .public) \
                clipboard: \(clipboardReason, privacy: .public)
                """
            )
            return .failure(.bothStrategiesFailed(
                accessibility: accessibilityReason, clipboard: clipboardReason
            ))
        }
    }

    /// Wraps text macOS handed us through the Services menu.
    ///
    /// No capture work is needed — the selection is already on the pasteboard
    /// macOS passed in — but the AX element is still probed so the replace
    /// path has somewhere to write. That probe needs the permission; without
    /// it the capture still succeeds and replacement falls back to pasting.
    func capture(fromService text: String) -> Capture {
        let started = ContinuousClock.now
        let sourceApp = NSWorkspace.shared.frontmostApplication
        let selection = try? AccessibilityCapture.read()

        let capture = Capture(
            text: text,
            strategy: .services,
            sourceApp: sourceApp,
            replaceableViaAX: selection?.isReplaceable ?? false,
            element: selection?.element,
            duration: started.duration(to: .now)
        )
        log(capture)
        return capture
    }

    /// Metadata only. The selected text never reaches the log.
    private func log(_ capture: Capture) {
        let millis = Double(capture.duration.components.attoseconds) / 1e15
        Log.capture.info(
            """
            captured via \(capture.strategy.rawValue, privacy: .public) \
            from \(capture.sourceApp?.bundleIdentifier ?? "unknown", privacy: .public) \
            in \(millis, format: .fixed(precision: 1))ms, \
            \(capture.text.count) chars, \
            ax-replaceable=\(capture.replaceableViaAX, privacy: .public)
            """
        )
    }
}

// MARK: - Services

/// Backs the right-click → Starch entry.
///
/// Note what this deliberately does *not* do: macOS will replace the selection
/// for you if the handler writes text back to the pasteboard, with no
/// Accessibility work at all. That is tempting and wrong — it forces a
/// blocking call with no streaming overlay. Instead the text is handed to the
/// same flow the hot key uses, so there is one code path for replacement and
/// one UX.
@MainActor
final class ServicesProvider: NSObject {
    var onSelection: ((String) -> Void)?

    @objc func rewriteSelection(
        _ pasteboard: NSPasteboard,
        userData: String?,
        error: AutoreleasingUnsafeMutablePointer<NSString?>
    ) {
        guard let text = pasteboard.string(forType: .string), !text.isEmpty else {
            Log.capture.error("services invoked with no text on the pasteboard")
            error.pointee = "Select some text first." as NSString
            return
        }
        Log.capture.info("services invoked, \(text.count) chars")
        onSelection?(text)
    }
}
