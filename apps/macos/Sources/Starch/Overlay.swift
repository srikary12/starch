import AppKit
@preconcurrency import ApplicationServices
import Carbon.HIToolbox
import StarchKit

// The overlay: a small panel near the selection showing the rewrite as it
// streams, with Return to accept and Escape to cancel.
//
// Two constraints shape all of this. It must never take focus from the app the
// user is in — the moment it does, the frontmost app changes and the
// replacement has nowhere to land. And it must never replace text silently:
// the user sees the rewrite before it lands, every time.

@MainActor
final class OverlayController {
    enum Outcome {
        case accepted(String)
        case cancelled
    }

    /// Called when the user accepts. The overlay is already hidden by then.
    var onAccept: ((String) -> Void)?
    /// Called on Escape or a click elsewhere.
    var onCancel: (() -> Void)?
    /// Called when the user asks for a different preset.
    var onCyclePreset: (() -> Void)?

    private let panel: NSPanel
    private let textView = NSTextView()
    private let scrollView = NSScrollView()
    private let presetLabel = UI.secondary("")
    private let hintLabel = UI.secondary("")
    private let spinner = NSProgressIndicator()

    private var monitor: Any?
    private var streaming = false
    private var currentText = ""

    init() {
        // .nonactivatingPanel is the whole trick: the panel can show and take
        // clicks without its application becoming active, so the app the user
        // was typing in stays frontmost and stays the paste target.
        panel = NSPanel(
            contentRect: NSRect(x: 0, y: 0, width: 460, height: 160),
            styleMask: [.nonactivatingPanel, .fullSizeContentView, .borderless],
            backing: .buffered,
            defer: true
        )
        panel.isFloatingPanel = true
        panel.level = .floating
        panel.hidesOnDeactivate = false
        panel.becomesKeyOnlyIfNeeded = true
        panel.worksWhenModal = true
        panel.isMovableByWindowBackground = true
        panel.backgroundColor = .clear
        panel.isOpaque = false
        panel.hasShadow = true
        // Show above full-screen apps, and follow the user between Spaces.
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .transient]

        panel.contentView = buildContent()
    }

    private func buildContent() -> NSView {
        let container = NSVisualEffectView()
        container.material = .popover
        container.blendingMode = .behindWindow
        container.state = .active
        container.wantsLayer = true
        container.layer?.cornerRadius = 12
        container.layer?.masksToBounds = true
        container.layer?.borderWidth = 1
        container.layer?.borderColor = NSColor.separatorColor.cgColor

        textView.isEditable = false
        textView.isSelectable = true
        textView.drawsBackground = false
        textView.font = .systemFont(ofSize: 13)
        textView.textContainerInset = NSSize(width: 4, height: 4)

        scrollView.documentView = textView
        scrollView.drawsBackground = false
        scrollView.hasVerticalScroller = true
        scrollView.autohidesScrollers = true

        spinner.style = .spinning
        spinner.controlSize = .small
        spinner.isDisplayedWhenStopped = false

        let header = NSStackView(views: [presetLabel, NSView(), spinner])
        header.orientation = .horizontal
        header.distribution = .fill

        hintLabel.stringValue = "Return to replace · Esc to cancel · Tab for another style"

        let stack = NSStackView(views: [header, scrollView, hintLabel])
        stack.orientation = .vertical
        stack.spacing = 8
        stack.edgeInsets = NSEdgeInsets(top: 12, left: 14, bottom: 12, right: 14)
        stack.translatesAutoresizingMaskIntoConstraints = false

        container.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            stack.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            stack.topAnchor.constraint(equalTo: container.topAnchor),
            stack.bottomAnchor.constraint(equalTo: container.bottomAnchor),
            scrollView.heightAnchor.constraint(greaterThanOrEqualToConstant: 64),
        ])
        return container
    }

    // MARK: Presentation

    /// Shows the overlay near the selection and begins a streaming state.
    func begin(presetName: String, near capture: SelectionCapturer.Capture) {
        currentText = ""
        streaming = true
        textView.string = ""
        textView.textColor = .labelColor
        presetLabel.stringValue = presetName
        hintLabel.stringValue = "Return to replace · Esc to cancel · Tab for another style"
        spinner.startAnimation(nil)

        position(near: capture)
        // orderFrontRegardless, not makeKeyAndOrderFront: the latter would
        // activate this app and lose the source app's focus.
        panel.orderFrontRegardless()
        installKeyMonitor()
    }

    func append(_ text: String) {
        currentText += text
        textView.string = currentText
        textView.scrollToEndOfDocument(nil)
    }

    /// Replaces the accumulated text with the daemon's authoritative version.
    func finish(full: String) {
        streaming = false
        spinner.stopAnimation(nil)
        if !full.isEmpty {
            currentText = full
            textView.string = full
        }
        hintLabel.stringValue = "Return to replace · Esc to discard · Tab for another style"
    }

    /// Shows a failure in place, rather than vanishing and leaving the user to
    /// guess whether anything happened.
    func showError(_ message: String) {
        streaming = false
        spinner.stopAnimation(nil)
        currentText = ""
        textView.string = message
        textView.textColor = .secondaryLabelColor
        hintLabel.stringValue = "Esc to dismiss"
    }

    func hide() {
        removeKeyMonitor()
        spinner.stopAnimation(nil)
        panel.orderOut(nil)
    }

    var isVisible: Bool { panel.isVisible }

    // MARK: Keyboard

    /// The panel is not key, so it receives no keystrokes through the
    /// responder chain. A local monitor is the way to get them — it sees
    /// events destined for this app before they are dispatched.
    private func installKeyMonitor() {
        removeKeyMonitor()
        monitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            guard let self, self.panel.isVisible else { return event }
            switch Int(event.keyCode) {
            case kVK_Return, kVK_ANSI_KeypadEnter:
                self.accept()
                return nil
            case kVK_Escape:
                self.cancel()
                return nil
            case kVK_Tab:
                self.onCyclePreset?()
                return nil
            default:
                return event
            }
        }
    }

    private func removeKeyMonitor() {
        if let monitor { NSEvent.removeMonitor(monitor) }
        monitor = nil
    }

    private func accept() {
        // Accepting mid-stream would replace the selection with a half
        // sentence. Ignore it until the terminal event has landed.
        guard !streaming, !currentText.isEmpty else { return }
        let text = currentText
        hide()
        onAccept?(text)
    }

    private func cancel() {
        hide()
        onCancel?()
    }

    // MARK: Positioning

    /// Places the panel near the selection, falling back to the mouse.
    private func position(near capture: SelectionCapturer.Capture) {
        let anchor = selectionRect(for: capture) ?? NSRect(origin: NSEvent.mouseLocation, size: .zero)
        let size = panel.frame.size

        // Below the selection by default; the text being rewritten stays
        // visible, which matters when judging the rewrite.
        var origin = NSPoint(x: anchor.minX, y: anchor.minY - size.height - 8)

        let screen = NSScreen.screens.first { $0.frame.contains(anchor.origin) }
            ?? NSScreen.main
        if let visible = screen?.visibleFrame {
            // Flip above the selection rather than run off the bottom.
            if origin.y < visible.minY {
                origin.y = anchor.maxY + 8
            }
            origin.x = min(max(origin.x, visible.minX + 8), visible.maxX - size.width - 8)
            origin.y = min(max(origin.y, visible.minY + 8), visible.maxY - size.height - 8)
        }
        panel.setFrameOrigin(origin)
    }

    /// The selection's on-screen rectangle, in Cocoa coordinates.
    ///
    /// Only available on the Accessibility path, and not from every app even
    /// then — which is why every caller has a mouse-location fallback.
    private func selectionRect(for capture: SelectionCapturer.Capture) -> NSRect? {
        guard let element = capture.element else { return nil }

        var rangeValue: CFTypeRef?
        guard AXUIElementCopyAttributeValue(
            element, kAXSelectedTextRangeAttribute as CFString, &rangeValue
        ) == .success, let rangeValue else { return nil }

        var boundsValue: CFTypeRef?
        guard AXUIElementCopyParameterizedAttributeValue(
            element,
            kAXBoundsForRangeParameterizedAttribute as CFString,
            rangeValue,
            &boundsValue
        ) == .success, let boundsValue else { return nil }

        var rect = CGRect.zero
        guard CFGetTypeID(boundsValue) == AXValueGetTypeID(),
              AXValueGetValue(boundsValue as! AXValue, .cgRect, &rect)
        else { return nil }

        // Accessibility reports a top-left origin against the primary screen;
        // Cocoa windows use bottom-left. Without this flip the panel lands as
        // far from the selection as the selection is from the top of the
        // screen — which looks like the positioning simply not working.
        guard let primary = NSScreen.screens.first else { return nil }
        let flippedY = primary.frame.maxY - rect.maxY
        return NSRect(x: rect.minX, y: flippedY, width: rect.width, height: rect.height)
    }
}
