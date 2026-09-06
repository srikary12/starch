import AppKit
import Foundation
import Testing

@testable import StarchKit

// Every test runs against a private pasteboard from NSPasteboard.withUniqueName,
// never NSPasteboard.general — a test suite that stomps the developer's actual
// clipboard would be its own bug report.

@MainActor
private func withScratchPasteboard(_ body: (NSPasteboard) throws -> Void) rethrows {
    let pasteboard = NSPasteboard.withUniqueName()
    defer { pasteboard.releaseGlobally() }
    try body(pasteboard)
}

@MainActor
private func withScratchPasteboard(_ body: (NSPasteboard) async throws -> Void) async rethrows {
    let pasteboard = NSPasteboard.withUniqueName()
    defer { pasteboard.releaseGlobally() }
    try await body(pasteboard)
}

@MainActor
private func write(_ string: String, to pasteboard: NSPasteboard) {
    pasteboard.clearContents()
    pasteboard.setString(string, forType: .string)
}

@Suite("Pasteboard snapshot")
@MainActor
struct PasteboardSnapshotTests {
    @Test("a plain string survives a clear and restore")
    func stringRoundTrip() {
        withScratchPasteboard { pasteboard in
            write("the original clipboard", to: pasteboard)
            let snapshot = PasteboardSnapshot(capturing: pasteboard)

            write("something else entirely", to: pasteboard)
            #expect(pasteboard.string(forType: .string) == "something else entirely")

            snapshot.restore(to: pasteboard)
            #expect(pasteboard.string(forType: .string) == "the original clipboard")
        }
    }

    @Test("every declared type on an item is preserved, not just the string")
    func multipleTypes() {
        withScratchPasteboard { pasteboard in
            let item = NSPasteboardItem()
            item.setString("plain", forType: .string)
            item.setString("<b>rich</b>", forType: .html)
            item.setData(Data([0xDE, 0xAD, 0xBE, 0xEF]), forType: .init("com.example.custom"))
            pasteboard.clearContents()
            pasteboard.writeObjects([item])

            let snapshot = PasteboardSnapshot(capturing: pasteboard)
            write("clobbered", to: pasteboard)
            snapshot.restore(to: pasteboard)

            #expect(pasteboard.string(forType: .string) == "plain")
            #expect(pasteboard.string(forType: .html) == "<b>rich</b>")
            #expect(pasteboard.data(forType: .init("com.example.custom")) == Data([0xDE, 0xAD, 0xBE, 0xEF]))
        }
    }

    @Test("a multi-item pasteboard keeps all of its items, in order")
    func multipleItems() {
        withScratchPasteboard { pasteboard in
            pasteboard.clearContents()
            pasteboard.writeObjects(["first", "second", "third"].map { text in
                let item = NSPasteboardItem()
                item.setString(text, forType: .string)
                return item
            })

            let snapshot = PasteboardSnapshot(capturing: pasteboard)
            write("clobbered", to: pasteboard)
            snapshot.restore(to: pasteboard)

            let restored = (pasteboard.pasteboardItems ?? []).compactMap { $0.string(forType: .string) }
            #expect(restored == ["first", "second", "third"])
        }
    }

    /// Restoring nothing must leave the pasteboard empty rather than leaving
    /// whatever the round-trip put there.
    @Test("an empty clipboard restores as empty")
    func emptyRoundTrip() {
        withScratchPasteboard { pasteboard in
            pasteboard.clearContents()
            let snapshot = PasteboardSnapshot(capturing: pasteboard)
            #expect(snapshot.isEmpty)

            write("copied during the round trip", to: pasteboard)
            snapshot.restore(to: pasteboard)

            #expect(pasteboard.string(forType: .string) == nil)
            #expect(pasteboard.pasteboardItems?.isEmpty ?? true)
        }
    }

    @Test("the snapshot records the change count it was taken at")
    func recordsChangeCount() {
        withScratchPasteboard { pasteboard in
            write("x", to: pasteboard)
            let snapshot = PasteboardSnapshot(capturing: pasteboard)
            #expect(snapshot.changeCount == pasteboard.changeCount)

            write("y", to: pasteboard)
            #expect(pasteboard.changeCount != snapshot.changeCount)
        }
    }

    /// The snapshot holds bytes, not NSPasteboardItem objects: an item is
    /// invalidated the moment its pasteboard is cleared, which is precisely
    /// what a copy round-trip does.
    @Test("captured data survives the pasteboard being cleared underneath it")
    func survivesClear() {
        withScratchPasteboard { pasteboard in
            write("original", to: pasteboard)
            let snapshot = PasteboardSnapshot(capturing: pasteboard)

            pasteboard.clearContents()
            #expect(pasteboard.string(forType: .string) == nil)

            snapshot.restore(to: pasteboard)
            #expect(pasteboard.string(forType: .string) == "original")
        }
    }
}

@Suite("Clipboard capture")
@MainActor
struct ClipboardCaptureTests {
    private func capture(
        _ pasteboard: NSPasteboard,
        timeout: Duration = .milliseconds(120)
    ) -> ClipboardCapture {
        ClipboardCapture(pasteboard: pasteboard, timeout: timeout, pollInterval: .milliseconds(2))
    }

    @Test("returns the copied text and puts the clipboard back")
    func happyPath() async throws {
        try await withScratchPasteboard { pasteboard in
            write("USER's own clipboard", to: pasteboard)

            let text = try await capture(pasteboard).captureText {
                // Stands in for the host app servicing a synthetic Cmd-C.
                write("the selected text", to: pasteboard)
            }

            #expect(text == "the selected text")
            #expect(pasteboard.string(forType: .string) == "USER's own clipboard")
        }
    }

    /// The most important test here. If the host app ignores the copy, the user
    /// must still get their clipboard back.
    @Test("restores the clipboard when nothing is copied")
    func restoresOnTimeout() async throws {
        try await withScratchPasteboard { pasteboard in
            write("USER's own clipboard", to: pasteboard)

            await #expect(throws: ClipboardCapture.Failure.noSelection) {
                try await capture(pasteboard).captureText { /* app ignores it */ }
            }

            #expect(pasteboard.string(forType: .string) == "USER's own clipboard")
        }
    }

    @Test("restores the clipboard when the copy yields non-text")
    func restoresOnNonText() async throws {
        try await withScratchPasteboard { pasteboard in
            write("USER's own clipboard", to: pasteboard)

            await #expect(throws: ClipboardCapture.Failure.notText) {
                try await capture(pasteboard).captureText {
                    pasteboard.clearContents()
                    pasteboard.setData(Data([0x01, 0x02]), forType: .tiff)
                }
            }

            #expect(pasteboard.string(forType: .string) == "USER's own clipboard")
        }
    }

    @Test("restores the clipboard when triggering the copy itself throws")
    func restoresWhenTriggerThrows() async throws {
        struct Boom: Error {}

        try await withScratchPasteboard { pasteboard in
            write("USER's own clipboard", to: pasteboard)

            await #expect(throws: Boom.self) {
                try await capture(pasteboard).captureText { throw Boom() }
            }

            #expect(pasteboard.string(forType: .string) == "USER's own clipboard")
        }
    }

    @Test("restores a rich multi-type clipboard, not just the string flavour")
    func restoresRichClipboard() async throws {
        try await withScratchPasteboard { pasteboard in
            let item = NSPasteboardItem()
            item.setString("plain", forType: .string)
            item.setString("<i>rich</i>", forType: .html)
            pasteboard.clearContents()
            pasteboard.writeObjects([item])

            let text = try await capture(pasteboard).captureText {
                write("selection", to: pasteboard)
            }

            #expect(text == "selection")
            #expect(pasteboard.string(forType: .string) == "plain")
            #expect(pasteboard.string(forType: .html) == "<i>rich</i>")
        }
    }

    @Test("an empty user clipboard is left empty, not filled with the selection")
    func restoresEmptyClipboard() async throws {
        try await withScratchPasteboard { pasteboard in
            pasteboard.clearContents()

            let text = try await capture(pasteboard).captureText {
                write("selection", to: pasteboard)
            }

            #expect(text == "selection")
            #expect(pasteboard.string(forType: .string) == nil)
        }
    }

    /// A copy that lands late but inside the window must still be seen; the
    /// poll loop checks once more after the deadline for exactly this.
    @Test("a slow host app is still caught inside the timeout")
    func slowCopy() async throws {
        try await withScratchPasteboard { pasteboard in
            write("USER's own clipboard", to: pasteboard)

            let text = try await capture(pasteboard, timeout: .milliseconds(400)).captureText {
                Task { @MainActor in
                    try? await Task.sleep(for: .milliseconds(120))
                    write("late selection", to: pasteboard)
                }
            }

            #expect(text == "late selection")
            #expect(pasteboard.string(forType: .string) == "USER's own clipboard")
        }
    }

    @Test("withSavedClipboard restores even when the body throws")
    func savedClipboardHelper() async throws {
        struct Boom: Error {}

        try await withScratchPasteboard { pasteboard in
            write("USER's own clipboard", to: pasteboard)

            await #expect(throws: Boom.self) {
                try await capture(pasteboard).withSavedClipboard { _ in
                    write("scribbled over", to: pasteboard)
                    throw Boom()
                }
            }

            #expect(pasteboard.string(forType: .string) == "USER's own clipboard")
        }
    }
}
