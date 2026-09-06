import AppKit
import Foundation

// The clipboard fallback for text capture, and the machinery that guarantees
// the user gets their clipboard back.
//
// Silently eating someone's clipboard is the fastest way to get uninstalled,
// so the restore is structural rather than something each call site has to
// remember: `withSavedClipboard` restores in a `defer`, and every path out —
// success, timeout, thrown error, cancellation — goes through it.

// MARK: - Snapshot

/// A deep copy of a pasteboard's contents.
///
/// Items are copied out as raw `Data` per type rather than held as
/// `NSPasteboardItem` objects, because an item belonging to a pasteboard is
/// invalidated the moment that pasteboard is cleared — which is exactly what a
/// copy round-trip does.
/// Holding bytes rather than pasteboard objects also makes this `Sendable`:
/// every stored property is a value type.
public struct PasteboardSnapshot: Sendable {
    /// One entry per pasteboard item, mapping each declared type to its bytes.
    private let items: [[NSPasteboard.PasteboardType: Data]]

    /// `changeCount` at capture time, so a caller can tell whether anything
    /// actually wrote to the pasteboard afterwards.
    public let changeCount: Int

    /// Types that were declared but produced no data.
    ///
    /// Promised content — file promises, lazily-provided flavours — cannot be
    /// snapshotted without asking the owning app to materialise it, which we
    /// will not do behind the user's back. Those flavours are lost on restore,
    /// and this records that it happened rather than hiding it.
    public let unreadableTypes: [NSPasteboard.PasteboardType]

    public init(capturing pasteboard: NSPasteboard) {
        changeCount = pasteboard.changeCount

        var items: [[NSPasteboard.PasteboardType: Data]] = []
        var unreadable: [NSPasteboard.PasteboardType] = []

        for item in pasteboard.pasteboardItems ?? [] {
            var copied: [NSPasteboard.PasteboardType: Data] = [:]
            for type in item.types {
                if let data = item.data(forType: type) {
                    copied[type] = data
                } else {
                    unreadable.append(type)
                }
            }
            if !copied.isEmpty { items.append(copied) }
        }

        self.items = items
        self.unreadableTypes = unreadable
    }

    /// True when the pasteboard held nothing we could capture.
    public var isEmpty: Bool { items.isEmpty }

    /// Puts the captured contents back, replacing whatever is there now.
    ///
    /// Restoring necessarily bumps `changeCount` again — there is no way to
    /// write a pasteboard without doing so.
    public func restore(to pasteboard: NSPasteboard) {
        pasteboard.clearContents()
        guard !items.isEmpty else { return }

        let restored = items.map { fields -> NSPasteboardItem in
            let item = NSPasteboardItem()
            for (type, data) in fields {
                item.setData(data, forType: type)
            }
            return item
        }
        pasteboard.writeObjects(restored)
    }
}

// MARK: - Clipboard round trip

/// Captures the current selection by driving a copy through the clipboard.
///
/// This is the fallback for apps where the Accessibility API cannot read the
/// selection — much of Electron and web content. It is second choice because
/// it has to touch global state the user owns.
@MainActor
public final class ClipboardCapture {
    public enum Failure: Error, LocalizedError, Equatable {
        /// Nothing wrote to the pasteboard in time. Usually means there was no
        /// selection, or the app ignores the copy command.
        case noSelection
        /// Something was copied, but it was not text.
        case notText

        public var errorDescription: String? {
            switch self {
            case .noSelection: "Could not read the selection — is any text selected?"
            case .notText: "The selection is not text."
            }
        }
    }

    /// How long to wait for the host app to service the copy.
    ///
    /// Generous enough for a slow Electron app, short enough that a failure
    /// still leaves room in the 500ms first-token budget.
    public static let defaultTimeout: Duration = .milliseconds(300)

    private let pasteboard: NSPasteboard
    private let timeout: Duration
    private let pollInterval: Duration

    public init(
        pasteboard: NSPasteboard = .general,
        timeout: Duration = ClipboardCapture.defaultTimeout,
        pollInterval: Duration = .milliseconds(8)
    ) {
        self.pasteboard = pasteboard
        self.timeout = timeout
        self.pollInterval = pollInterval
    }

    /// Runs `body` with the clipboard saved, restoring it however `body` exits.
    ///
    /// Exposed because the replace path (M2) needs the same guarantee across a
    /// different operation: write the rewrite, paste it, then put the user's
    /// clipboard back.
    public func withSavedClipboard<T>(_ body: (PasteboardSnapshot) async throws -> T) async rethrows -> T {
        let snapshot = PasteboardSnapshot(capturing: pasteboard)
        defer {
            snapshot.restore(to: pasteboard)
            if !snapshot.unreadableTypes.isEmpty {
                Log.capture.info(
                    "restored clipboard; \(snapshot.unreadableTypes.count) promised flavour(s) could not be preserved"
                )
            }
        }
        return try await body(snapshot)
    }

    /// Saves the clipboard, asks the host app to copy, reads the text back, and
    /// restores the clipboard.
    ///
    /// `triggerCopy` is injected rather than hard-coded so the synthetic
    /// keystroke stays in the app layer and this stays testable.
    public func captureText(triggerCopy: () throws -> Void) async throws -> String {
        try await withSavedClipboard { snapshot in
            try triggerCopy()

            guard try await waitForWrite(after: snapshot.changeCount) else {
                throw Failure.noSelection
            }
            guard let text = pasteboard.string(forType: .string), !text.isEmpty else {
                throw Failure.notText
            }
            return text
        }
    }

    /// Polls `changeCount` until something writes, or the timeout elapses.
    private func waitForWrite(after changeCount: Int) async throws -> Bool {
        let deadline = ContinuousClock.now.advanced(by: timeout)
        while ContinuousClock.now < deadline {
            if pasteboard.changeCount != changeCount { return true }
            // Sleeping yields the main actor, so a slow host app stalls the
            // capture without freezing the UI.
            try await Task.sleep(for: pollInterval)
        }
        // One last look: the write may have landed inside the final interval.
        return pasteboard.changeCount != changeCount
    }
}
