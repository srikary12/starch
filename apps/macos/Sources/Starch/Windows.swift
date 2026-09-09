import AppKit
import StarchKit

// Programmatic AppKit throughout: no storyboards, no xibs. It keeps the whole
// UI reviewable as text and buildable without opening Xcode.

// MARK: - Small view helpers

@MainActor
enum UI {
    static func label(_ text: String, size: CGFloat = NSFont.systemFontSize, weight: NSFont.Weight = .regular, color: NSColor = .labelColor) -> NSTextField {
        let field = NSTextField(labelWithString: text)
        field.font = .systemFont(ofSize: size, weight: weight)
        field.textColor = color
        field.lineBreakMode = .byWordWrapping
        field.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        return field
    }

    static func title(_ text: String) -> NSTextField {
        label(text, size: 20, weight: .semibold)
    }

    static func heading(_ text: String) -> NSTextField {
        label(text, size: NSFont.systemFontSize, weight: .semibold)
    }

    static func secondary(_ text: String) -> NSTextField {
        label(text, size: NSFont.smallSystemFontSize, color: .secondaryLabelColor)
    }

    static func textField(_ value: String, placeholder: String, width: CGFloat = 280) -> NSTextField {
        let field = NSTextField(string: value)
        field.placeholderString = placeholder
        field.widthAnchor.constraint(equalToConstant: width).isActive = true
        return field
    }

    static func vstack(_ views: [NSView], spacing: CGFloat = 10, alignment: NSLayoutConstraint.Attribute = .leading) -> NSStackView {
        let stack = NSStackView(views: views)
        stack.orientation = .vertical
        stack.alignment = alignment
        stack.spacing = spacing
        return stack
    }

    static func hstack(_ views: [NSView], spacing: CGFloat = 8) -> NSStackView {
        let stack = NSStackView(views: views)
        stack.orientation = .horizontal
        stack.alignment = .firstBaseline
        stack.spacing = spacing
        return stack
    }

    static func separator() -> NSBox {
        let box = NSBox()
        box.boxType = .separator
        return box
    }

    /// A field label of fixed width, so rows line up without a grid view.
    static func fieldLabel(_ text: String) -> NSTextField {
        let field = label(text)
        field.alignment = .right
        field.widthAnchor.constraint(equalToConstant: 90).isActive = true
        return field
    }

    static func window(title: String, size: NSSize) -> NSWindow {
        let window = NSWindow(
            contentRect: NSRect(origin: .zero, size: size),
            styleMask: [.titled, .closable, .miniaturizable],
            backing: .buffered,
            defer: false
        )
        window.title = title
        window.isReleasedWhenClosed = false
        window.center()
        return window
    }
}

// MARK: - Settings

@MainActor
final class SettingsWindowController: NSWindowController, NSWindowDelegate, NSTextFieldDelegate {
    /// Called whenever a non-secret setting changes.
    var onPreferencesChanged: ((Preferences) -> Void)?
    /// Called when the user asks to see onboarding again.
    var onShowOnboarding: (() -> Void)?
    /// Asks the delegate for the daemon's current view of presets.json.
    var presetSet: (() -> (path: String?, count: Int, problem: String?))?

    private var preferences: Preferences
    private let store: PreferencesStore
    private let keychain: Keychain

    private let providerPopUp = NSPopUpButton()
    private let modelField = UI.textField("", placeholder: "model name")
    private let baseURLField = UI.textField("", placeholder: "https://…")
    private let presetStatusLabel = UI.secondary("")
    private let editPresetsButton = NSButton(title: "Edit presets.json…", target: nil, action: nil)
    private let revealPresetsButton = NSButton(title: "Show in Finder", target: nil, action: nil)

    private let apiKeyField = NSSecureTextField()
    private let keyStatusLabel = UI.secondary("")
    private let hotKeyButton = NSButton(title: "", target: nil, action: nil)
    private let debugCheckbox = NSButton(checkboxWithTitle: "Verbose logging (Console.app)", target: nil, action: nil)

    private var recordingMonitor: Any?

    init(preferences: Preferences, store: PreferencesStore, keychain: Keychain) {
        self.preferences = preferences
        self.store = store
        self.keychain = keychain
        super.init(window: UI.window(title: "\(Brand.name) Settings", size: NSSize(width: 520, height: 430)))
        window?.delegate = self
        buildUI()
        refresh()
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { fatalError("not supported") }

    func show() {
        refreshKeyStatus()
        refreshPresetStatus()
        NSApp.activate(ignoringOtherApps: true)
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)

        // Arriving with no key means this window was opened to collect one —
        // most often straight off the first-run guide. Start in that field
        // rather than making the user hunt for it.
        if !keychain.hasSecret(account: preferences.provider.keychainAccount) {
            window?.makeFirstResponder(apiKeyField)
        }
    }

    // MARK: Layout

    private func buildUI() {
        providerPopUp.target = self
        providerPopUp.action = #selector(providerChanged)
        for provider in ProviderID.allCases {
            providerPopUp.addItem(withTitle: provider.displayName)
            providerPopUp.lastItem?.representedObject = provider.rawValue
        }

        modelField.delegate = self
        baseURLField.delegate = self
        apiKeyField.placeholderString = "paste your API key"
        apiKeyField.widthAnchor.constraint(equalToConstant: 280).isActive = true

        let saveKey = NSButton(title: "Save", target: self, action: #selector(saveAPIKey))
        saveKey.keyEquivalent = "\r"
        let removeKey = NSButton(title: "Remove", target: self, action: #selector(removeAPIKey))

        hotKeyButton.target = self
        hotKeyButton.action = #selector(beginRecordingHotKey)
        hotKeyButton.bezelStyle = .rounded
        hotKeyButton.widthAnchor.constraint(equalToConstant: 140).isActive = true

        debugCheckbox.target = self
        editPresetsButton.target = self
        editPresetsButton.action = #selector(editPresets)
        editPresetsButton.bezelStyle = .rounded
        revealPresetsButton.target = self
        revealPresetsButton.action = #selector(revealPresets)
        revealPresetsButton.bezelStyle = .rounded
        debugCheckbox.action = #selector(debugToggled)

        let onboardingButton = NSButton(
            title: "Set-up guide and permissions…", target: self, action: #selector(showOnboarding)
        )
        onboardingButton.bezelStyle = .rounded

        let content = UI.vstack([
            UI.heading("Model"),
            UI.hstack([UI.fieldLabel("Provider"), providerPopUp]),
            UI.hstack([UI.fieldLabel("Model"), modelField]),
            UI.hstack([UI.fieldLabel("Endpoint"), baseURLField]),
            UI.hstack([UI.fieldLabel("API key"), apiKeyField, saveKey, removeKey]),
            UI.hstack([UI.fieldLabel(""), keyStatusLabel]),

            UI.separator(),
            UI.heading("Shortcut"),
            UI.hstack([UI.fieldLabel("Hot key"), hotKeyButton]),
            UI.hstack([UI.fieldLabel(""), UI.secondary(
                "The right-click → \(Brand.name) service can also be given its own shortcut in\n"
                + "System Settings → Keyboard → Keyboard Shortcuts → Services."
            )]),

            UI.separator(),

            UI.heading("Presets"),
            presetStatusLabel,
            UI.hstack([UI.fieldLabel(""), UI.hstack([editPresetsButton, revealPresetsButton], spacing: 8)]),
            UI.hstack([UI.fieldLabel(""), UI.secondary(
                "Edit the file to change the styles Tab cycles through. Changes apply to the\n"
                + "next rewrite — nothing needs restarting."
            )]),

            UI.separator(),
            UI.hstack([UI.fieldLabel(""), debugCheckbox]),
            UI.hstack([UI.fieldLabel(""), onboardingButton]),

            UI.separator(),
            UI.secondary(
                "Your key is stored in the macOS Keychain and handed to the local \(Brand.daemonExecutable) "
                + "helper in memory only.\nNo account, no telemetry: \(Brand.name) never contacts any server "
                + "except the endpoint above."
            ),
        ], spacing: 12)

        let container = NSView()
        container.addSubview(content)
        content.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            content.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: 20),
            content.trailingAnchor.constraint(lessThanOrEqualTo: container.trailingAnchor, constant: -20),
            content.topAnchor.constraint(equalTo: container.topAnchor, constant: 20),
            content.bottomAnchor.constraint(lessThanOrEqualTo: container.bottomAnchor, constant: -20),
        ])
        window?.contentView = container
    }

    // MARK: State

    private func refresh() {
        providerPopUp.selectItem(at: ProviderID.allCases.firstIndex(of: preferences.provider) ?? 0)
        modelField.stringValue = preferences.model
        baseURLField.stringValue = preferences.baseURL
        hotKeyButton.title = preferences.hotKey.displayString
        debugCheckbox.state = preferences.debugLogging ? .on : .off
        refreshKeyStatus()
    }

    private func refreshKeyStatus() {
        let account = preferences.provider.keychainAccount
        if keychain.hasSecret(account: account) {
            keyStatusLabel.stringValue = "A key is saved in the Keychain for \(preferences.provider.displayName)."
            keyStatusLabel.textColor = .secondaryLabelColor
        } else if preferences.provider.requiresAPIKey {
            keyStatusLabel.stringValue = "No key saved. \(preferences.provider.displayName) needs one."
            keyStatusLabel.textColor = .systemOrange
        } else {
            keyStatusLabel.stringValue = "No key saved. Local endpoints such as Ollama do not need one."
            keyStatusLabel.textColor = .secondaryLabelColor
        }
    }

    private func commit() {
        store.save(preferences)
        onPreferencesChanged?(preferences)
    }

    // MARK: Actions

    @objc private func providerChanged() {
        guard let raw = providerPopUp.selectedItem?.representedObject as? String,
              let provider = ProviderID(rawValue: raw), provider != preferences.provider
        else { return }

        // Carry the defaults across only when the user has not customised the
        // old ones, so switching providers does not silently discard a
        // hand-typed endpoint.
        let wasDefaultModel = preferences.model == preferences.provider.defaultModel
        let wasDefaultURL = preferences.baseURL == preferences.provider.defaultBaseURL

        preferences.provider = provider
        if wasDefaultModel { preferences.model = provider.defaultModel }
        if wasDefaultURL { preferences.baseURL = provider.defaultBaseURL }

        refresh()
        commit()
    }

    func controlTextDidEndEditing(_ notification: Notification) {
        guard let field = notification.object as? NSTextField else { return }
        switch field {
        case modelField: preferences.model = field.stringValue.trimmingCharacters(in: .whitespaces)
        case baseURLField: preferences.baseURL = field.stringValue.trimmingCharacters(in: .whitespaces)
        default: return
        }
        commit()
    }

    @objc private func debugToggled() {
        preferences.debugLogging = debugCheckbox.state == .on
        commit()
    }

    @objc private func saveAPIKey() {
        let key = apiKeyField.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !key.isEmpty else { return }
        do {
            try keychain.set(key, account: preferences.provider.keychainAccount)
            // Clear it from the field so it is not left sitting on screen.
            apiKeyField.stringValue = ""
            refreshKeyStatus()
            onPreferencesChanged?(preferences)
        } catch {
            presentError(error, context: "Could not save the key to the Keychain.")
        }
    }

    @objc private func removeAPIKey() {
        do {
            try keychain.delete(account: preferences.provider.keychainAccount)
            apiKeyField.stringValue = ""
            refreshKeyStatus()
            onPreferencesChanged?(preferences)
        } catch {
            presentError(error, context: "Could not remove the key from the Keychain.")
        }
    }

    /// Refreshes the preset summary, including any parse failure.
    ///
    /// Surfacing the problem here is the point: someone who has just edited
    /// this file and sees nothing change needs to be told their JSON is
    /// broken, not left to conclude the feature does not work.
    func refreshPresetStatus() {
        let state = presetSet?() ?? (path: nil, count: 0, problem: nil)
        let available = state.path != nil

        editPresetsButton.isEnabled = available
        revealPresetsButton.isEnabled = available

        if let problem = state.problem {
            presetStatusLabel.stringValue = "⚠︎  Using the built-in presets — \(problem)"
            presetStatusLabel.textColor = .systemOrange
        } else if let path = state.path {
            presetStatusLabel.stringValue = "\(state.count) presets · \(path)"
            presetStatusLabel.textColor = .secondaryLabelColor
        } else {
            presetStatusLabel.stringValue = "Waiting for the helper…"
            presetStatusLabel.textColor = .secondaryLabelColor
        }
    }

    @objc private func editPresets() {
        guard let path = presetSet?().path else { return }
        // NSWorkspace.open rather than a hard-coded editor: it is the user's
        // file, and it should open in whatever they use for JSON.
        NSWorkspace.shared.open(URL(fileURLWithPath: path))
    }

    @objc private func revealPresets() {
        guard let path = presetSet?().path else { return }
        NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
    }

    @objc private func showOnboarding() {
        onShowOnboarding?()
    }

    // MARK: Hot key recording

    @objc private func beginRecordingHotKey() {
        guard recordingMonitor == nil else { return }
        hotKeyButton.title = "Press a shortcut…"

        recordingMonitor = NSEvent.addLocalMonitorForEvents(matching: [.keyDown]) { [weak self] event in
            guard let self else { return event }
            MainActor.assumeIsolated {
                self.finishRecording(with: event)
            }
            return nil // consume, so the keystroke does not reach the field
        }
    }

    private func finishRecording(with event: NSEvent) {
        defer {
            if let recordingMonitor { NSEvent.removeMonitor(recordingMonitor) }
            recordingMonitor = nil
        }

        // Escape abandons recording and keeps the existing shortcut.
        if event.keyCode == 53, !event.modifierFlags.contains(.command) {
            hotKeyButton.title = preferences.hotKey.displayString
            return
        }

        let spec = HotKeySpec(event: event)
        if let reason = spec.invalidReason {
            hotKeyButton.title = preferences.hotKey.displayString
            keyStatusLabel.stringValue = reason
            keyStatusLabel.textColor = .systemOrange
            return
        }

        preferences.hotKey = spec
        hotKeyButton.title = spec.displayString
        commit()
        refreshKeyStatus()
    }

    private func presentError(_ error: Error, context: String) {
        let alert = NSAlert()
        alert.messageText = context
        alert.informativeText = error.localizedDescription
        alert.alertStyle = .warning
        if let window { alert.beginSheetModal(for: window) } else { alert.runModal() }
    }

    func windowWillClose(_ notification: Notification) {
        if let recordingMonitor { NSEvent.removeMonitor(recordingMonitor) }
        recordingMonitor = nil
    }
}

// MARK: - Onboarding

/// Explains what the app does and why it wants Accessibility, before asking.
///
/// This app reads what people type. An unexplained permission dialog ends that
/// conversation immediately, so the dialog is never raised until the reason is
/// on screen.
@MainActor
final class OnboardingWindowController: NSWindowController, NSWindowDelegate {
    var onFinished: (() -> Void)?

    private let watcher = Accessibility.Watcher()
    private let statusLabel = UI.label("")
    private let grantButton = NSButton(title: "Grant Accessibility Access…", target: nil, action: nil)
    private let settingsButton = NSButton(title: "Open System Settings", target: nil, action: nil)
    private let servicesStatusLabel = UI.label("")
    private let servicesButton = NSButton(title: "Open Services Settings…", target: nil, action: nil)

    init() {
        super.init(window: UI.window(title: "Welcome to \(Brand.name)", size: NSSize(width: 560, height: 520)))
        window?.delegate = self
        buildUI()
        refreshStatus()
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { fatalError("not supported") }

    func show() {
        refreshStatus()
        watcher.start { [weak self] _ in self?.refreshStatus() }
        NSApp.activate(ignoringOtherApps: true)
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
    }

    private func buildUI() {
        grantButton.target = self
        grantButton.action = #selector(requestAccessibility)
        servicesButton.target = self
        servicesButton.action = #selector(openServicesSettings)
        servicesButton.bezelStyle = settingsButton.bezelStyle
        grantButton.bezelStyle = .rounded
        settingsButton.target = self
        settingsButton.action = #selector(openSettings)
        settingsButton.bezelStyle = .rounded

        let done = NSButton(title: "Done", target: self, action: #selector(finish))
        done.bezelStyle = .rounded
        done.keyEquivalent = "\r"

        let content = UI.vstack([
            UI.title("Rewrite text anywhere, without leaving the app you are in"),
            UI.label(
                "Select text in any app, trigger \(Brand.name), and a small overlay shows a rewrite as it "
                + "streams in. Press Return to replace the text in place, or Escape to cancel. Nothing is "
                + "ever replaced without you seeing it first."
            ),

            UI.separator(),

            UI.heading("Two ways to trigger it"),
            UI.label("1.  The keyboard shortcut, anywhere. Needs Accessibility access."),
            UI.label("2.  Right-click → \(Brand.name), from the Services menu. Needs no permission."),
            UI.secondary(
                "The right-click route asks for no permissions, which makes it the lower-commitment "
                + "way to try \(Brand.name) first — but macOS ships every new third-party Services "
                + "entry switched off, so it has to be turned on once before it appears. You can give "
                + "it a keyboard shortcut of its own on the same screen."
            ),
            servicesStatusLabel,
            UI.hstack([servicesButton]),

            UI.separator(),

            UI.heading("Why Accessibility access"),
            UI.label(
                "macOS puts reading the current selection, and writing a replacement back into the app you "
                + "are using, behind the Accessibility permission. That is the only reason \(Brand.name) "
                + "asks for it. It reads the text you have selected when you trigger it, and nothing else — "
                + "there is no keystroke logging and no background monitoring."
            ),
            statusLabel,
            UI.hstack([grantButton, settingsButton], spacing: 10),
            UI.secondary(
                "macOS shows its permission dialog only once per app. If you have dismissed it before, "
                + "use System Settings instead."
            ),

            UI.separator(),

            UI.heading("What leaves your machine"),
            UI.label(
                "Only the text you select, and only to the model endpoint you configure yourself. There is "
                + "no \(Brand.name) account and no \(Brand.name) server. No analytics, no crash reporting, "
                + "no update checks. Your API key is kept in the macOS Keychain."
            ),

            UI.hstack([done]),
        ], spacing: 12)

        let container = NSView()
        container.addSubview(content)
        content.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            content.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: 24),
            content.trailingAnchor.constraint(equalTo: container.trailingAnchor, constant: -24),
            content.topAnchor.constraint(equalTo: container.topAnchor, constant: 24),
            content.bottomAnchor.constraint(lessThanOrEqualTo: container.bottomAnchor, constant: -20),
        ])
        window?.contentView = container
    }

    private func refreshStatus() {
        switch ServicesMenu.state() {
        case .enabled:
            servicesStatusLabel.stringValue = "✓  Right-click → \(Brand.name) is switched on."
            servicesStatusLabel.textColor = .systemGreen
            servicesButton.isEnabled = false
        case .disabled, .notConfigured:
            servicesStatusLabel.stringValue =
                "○  Not switched on yet. Tick \(Brand.name) under Text, then reopen the app you want to use it in."
            servicesStatusLabel.textColor = .secondaryLabelColor
            servicesButton.isEnabled = true
        }

        if Accessibility.isTrusted {
            statusLabel.stringValue = "✓  Accessibility access is granted. The keyboard shortcut will work."
            statusLabel.textColor = .systemGreen
            grantButton.isEnabled = false
        } else {
            statusLabel.stringValue = "○  Not granted yet. The keyboard shortcut will not fire until it is."
            statusLabel.textColor = .secondaryLabelColor
            grantButton.isEnabled = true
        }
    }

    @objc private func requestAccessibility() {
        Accessibility.requestTrust()
        // The grant lands asynchronously, and macOS may show nothing at all if
        // the dialog was dismissed before; the watcher picks it up either way.
        refreshStatus()
    }

    @objc private func openSettings() {
        Accessibility.openSystemSettings()
    }

    @objc private func openServicesSettings() {
        ServicesMenu.openSettings()
    }

    /// There is no notification for a Services enablement change, so refresh
    /// when the user comes back from System Settings.
    func windowDidBecomeKey(_ notification: Notification) {
        refreshStatus()
    }

    @objc private func finish() {
        close()
    }

    func windowWillClose(_ notification: Notification) {
        watcher.stop()
        onFinished?()
    }
}
