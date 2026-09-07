import AppKit
@preconcurrency import ApplicationServices
import StarchKit

// Putting the rewrite back where the selection was.
//
// The M1 matrix decided the shape of this. Native editors (TextEdit, Notes)
// accept an Accessibility write; everything rendering web content — Safari
// included, not just Electron — does not. So paste is the common path, not the
// fallback, and its failure modes are the ones that matter.

@MainActor
struct Replacer {
    enum Method: String {
        case accessibility
        case paste
    }

    enum Failure: Error, LocalizedError {
        case noTarget
        case bothMethodsFailed(String)

        var errorDescription: String? {
            switch self {
            case .noTarget:
                "Could not find where to put the rewrite."
            case let .bothMethodsFailed(detail):
                "Could not replace the text. \(detail)"
            }
        }
    }

    private let clipboard = ClipboardCapture()

    /// Replaces the captured selection with `text`.
    func replace(_ text: String, for capture: SelectionCapturer.Capture) async -> Result<Method, Failure> {
        // Hand focus back before anything else. The overlay is non-activating
        // so focus should never have moved, but a stale frontmost app means a
        // paste lands in the wrong window — or in ours.
        await restoreFocus(to: capture.sourceApp)

        if capture.replaceableViaAX, let element = capture.element {
            if writeViaAccessibility(text, to: element) {
                Log.capture.info("replaced via accessibility, \(text.count) chars")
                return .success(.accessibility)
            }
            // Settability was probed at capture time, so a failure here means
            // the app changed its mind — worth falling through rather than
            // giving up.
            Log.capture.info("accessibility write refused; falling back to paste")
        }

        do {
            try await pasteReplacement(text)
            Log.capture.info("replaced via paste, \(text.count) chars")
            return .success(.paste)
        } catch {
            Log.capture.error("replacement failed: \(error.localizedDescription, privacy: .public)")
            return .failure(.bothMethodsFailed(error.localizedDescription))
        }
    }

    // MARK: Accessibility

    private func writeViaAccessibility(_ text: String, to element: AXUIElement) -> Bool {
        AXUIElementSetMessagingTimeout(element, AccessibilityCapture.messagingTimeout)
        let status = AXUIElementSetAttributeValue(
            element, kAXSelectedTextAttribute as CFString, text as CFTypeRef
        )
        return status == .success
    }

    // MARK: Paste

    /// Writes the rewrite to the clipboard, pastes, and puts the clipboard back.
    ///
    /// The restore is the part that has to be right. This runs on the common
    /// path now, so "we usually restore it" would mean routinely eating the
    /// user's clipboard.
    private func pasteReplacement(_ text: String) async throws {
        // Posting a synthetic Cmd-V needs Accessibility trust; without it the
        // event is dropped with no error and the paste simply does not happen.
        //
        // This is reachable, not theoretical. The Services path captures text
        // without the permission — macOS hands it to us — so right-click →
        // Starch on an untrusted install gets a rewrite, a Return, and
        // silence. Checking here is what turns that into an explanation.
        guard Accessibility.isTrusted else {
            throw PasteFailure.notTrusted
        }

        try await clipboard.withSavedClipboard { _ in
            let pasteboard = NSPasteboard.general
            pasteboard.clearContents()
            guard pasteboard.setString(text, forType: .string) else {
                throw PasteFailure.clipboardWriteRefused
            }

            Keystroke.sendCommandV()

            // The paste is asynchronous: the keystroke is delivered, then the
            // target app reads the pasteboard on its own schedule. Restoring
            // immediately would put the user's old clipboard back before the
            // app has read ours, and paste the wrong thing. This wait is the
            // price of the clipboard path.
            try? await Task.sleep(for: .milliseconds(120))
        }
    }

    /// Ways the paste path can fail detectably.
    ///
    /// Both used to be silent, which meant reporting a successful replacement
    /// that never happened — the worst possible outcome, because the user
    /// walks away believing their text was changed.
    private enum PasteFailure: Error, LocalizedError {
        case notTrusted
        case clipboardWriteRefused

        var errorDescription: String? {
            switch self {
            case .notTrusted:
                "Replacing text needs Accessibility access."
            case .clipboardWriteRefused:
                "Another app is holding the clipboard."
            }
        }
    }

    // MARK: Focus

    /// Reactivates the app the selection came from, and waits for it to stick.
    private func restoreFocus(to app: NSRunningApplication?) async {
        guard let app, !app.isActive else { return }

        app.activate()

        // Activation is asynchronous. Pasting before it completes sends the
        // keystroke to whatever is still frontmost.
        let deadline = ContinuousClock.now.advanced(by: .milliseconds(300))
        while ContinuousClock.now < deadline {
            if app.isActive { return }
            try? await Task.sleep(for: .milliseconds(10))
        }
        Log.capture.info("source app did not reactivate in time; pasting anyway")
    }
}
