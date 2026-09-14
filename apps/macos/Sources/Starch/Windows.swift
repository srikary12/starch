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
        // NSTextField(labelWithString:) is not selectable by default, which
        // makes the two labels people most need — the presets path and a JSON
        // parse error naming a line — impossible to copy out of the window.
        // Selectable everywhere rather than case by case: a label worth
        // reading is a label worth copying, and it costs nothing.
        field.isSelectable = true
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

    /// A dropdown that can also be typed into.
    ///
    /// A combo box rather than a pop-up button because the daemon's list is
    /// curated, and therefore always a little behind: a model released after
    /// this build, a private gateway's URL, or whatever someone has pulled
    /// into Ollama all have to stay reachable. The list is a convenience,
    /// never a whitelist — a pure dropdown would lock those people out of
    /// their own configuration.
    static func comboBox(placeholder: String, width: CGFloat = 280) -> NSComboBox {
        let box = NSComboBox()
        box.placeholderString = placeholder
        box.isEditable = true
        box.completes = true
        box.widthAnchor.constraint(equalToConstant: width).isActive = true
        return box
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
final class SettingsWindowController: NSWindowController, NSWindowDelegate, NSComboBoxDelegate {
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
    private let modelBox = UI.comboBox(placeholder: "model name")
    private let baseURLBox = UI.comboBox(placeholder: "https://…")
    // The same two settings as plain fields, shown instead of the combo boxes
    // when there is nothing to suggest. Exactly one of each pair is ever
    // visible; NSStackView drops the hidden one out of the layout.
    private let modelField = UI.textField("", placeholder: "model name")
    private let baseURLField = UI.textField("", placeholder: "https://…")
    private let effortPopUp = NSPopUpButton()
    private let sourceNoteLabel = UI.secondary("")
    private let presetStatusLabel = UI.secondary("")
    private let editPresetsButton = NSButton(title: "Edit presets.json…", target: nil, action: nil)
    private let revealPresetsButton = NSButton(title: "Show in Finder", target: nil, action: nil)

    private let apiKeyField = NSSecureTextField()
    private let keyStatusLabel = UI.secondary("")
    private let hotKeyButton = NSButton(title: "", target: nil, action: nil)
    private let debugCheckbox = NSButton(checkboxWithTitle: "Verbose logging (Console.app)", target: nil, action: nil)

    private var recordingMonitor: Any?

    /// The daemon's provider and model table. Empty until it answers, which
    /// every picker treats as "no suggestions" rather than as an error — the
    /// fields still work, they just stop offering anything.
    private var catalog: ModelCatalog = .empty

    /// The Thinking row, hidden outright for a model with no reasoning
    /// controls. Hidden rather than disabled: a greyed-out control implies the
    /// model has a setting we are refusing to show, when in fact asking for
    /// one is what some of those models reject.
    private var effortRow: NSView?

    /// Shown in the effort picker for "send nothing and let the endpoint
    /// decide". The escape hatch for a gateway that rejects the parameter.
    private static let endpointDefaultTitle = "Endpoint default"

    init(preferences: Preferences, store: PreferencesStore, keychain: Keychain) {
        self.preferences = preferences
        self.store = store
        self.keychain = keychain
        super.init(window: UI.window(title: "\(Brand.name) Settings", size: NSSize(width: 520, height: 520)))
        window?.delegate = self
        buildUI()
        refresh()
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { fatalError("not supported") }

    func show() {
        refreshKeyStatus()
        refreshPresetStatus()
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        // orderFrontRegardless as well, because this window is sometimes
        // opened out of another window's close. An app whose last window has
        // just gone is not active for a moment, and makeKeyAndOrderFront on an
        // inactive app orders behind the active one — which put Settings
        // silently behind whatever the user was working in.
        window?.orderFrontRegardless()
        // Activated last, not first. An accessory app has no window to make
        // key until one has been ordered in, so activating ahead of that does
        // nothing and the window arrives in a background app.
        NSApp.activate(ignoringOtherApps: true)

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

        modelBox.delegate = self
        baseURLBox.delegate = self
        modelField.delegate = self
        baseURLField.delegate = self
        effortPopUp.target = self
        effortPopUp.action = #selector(effortChanged)
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

        let effortRow = UI.hstack([UI.fieldLabel("Thinking"), effortPopUp])
        self.effortRow = effortRow

        let content = UI.vstack([
            UI.heading("Model"),
            UI.hstack([UI.fieldLabel("Provider"), providerPopUp]),
            // Endpoint above Model because that is the order they depend in:
            // the models on offer are a property of the endpoint, not of the
            // provider. api.openai.com serves GPT models; localhost:11434
            // serves whatever this machine has pulled.
            UI.hstack([UI.fieldLabel("Endpoint"), baseURLBox, baseURLField]),
            UI.hstack([UI.fieldLabel("Model"), modelBox, modelField]),
            UI.hstack([UI.fieldLabel(""), sourceNoteLabel]),
            effortRow,
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
        hotKeyButton.title = preferences.hotKey.displayString
        debugCheckbox.state = preferences.debugLogging ? .on : .off
        refreshSuggestions()
        refreshKeyStatus()
    }

    /// Hands the window the daemon's catalog, once it has answered.
    func update(catalog: ModelCatalog) {
        guard catalog != self.catalog else { return }
        self.catalog = catalog
        refreshSuggestions()
    }

    /// Repopulates both lists and the Thinking row from the catalog.
    ///
    /// It deliberately never overwrites what is in the fields. Someone halfway
    /// through typing a model name this build has never heard of is exactly
    /// the case a curated list has to keep working.
    private func refreshSuggestions() {
        let provider = catalog.provider(preferences.provider)

        present(
            suggestions: provider?.endpoints.map(\.url) ?? [],
            value: preferences.baseURL,
            in: baseURLBox,
            or: baseURLField
        )

        let models = catalog.models(provider: preferences.provider, baseURL: preferences.baseURL)
        present(
            suggestions: models.map(\.id),
            value: preferences.model,
            in: modelBox,
            or: modelField
        )

        refreshSourceNote(provider: provider, models: models)
        refreshEffort()
    }

    /// Shows a dropdown when there is something to drop down, and a plain text
    /// field when there is not.
    ///
    /// An empty NSComboBox still draws its arrow, and clicking it opens
    /// nothing. For an OpenAI-compatible endpoint that is not a transient
    /// state but the permanent one — what a local Ollama holds cannot be known
    /// from here — so the control itself changes rather than implying there is
    /// a list behind it. The same applies to any endpoint the catalog does not
    /// know, and to every field while the helper is still starting.
    private func present(
        suggestions: [String],
        value: String,
        in box: NSComboBox,
        or field: NSTextField
    ) {
        box.removeAllItems()
        box.addItems(withObjectValues: suggestions)

        let offering = !suggestions.isEmpty
        box.isHidden = !offering
        field.isHidden = offering

        // Both are kept in step so a swap never reveals a stale value — but
        // never the one being typed into. The catalog can arrive mid-word, and
        // overwriting the field under the cursor would lose what was typed.
        for control in [box, field] where control.currentEditor() == nil {
            if control.stringValue != value { control.stringValue = value }
        }
    }

    /// The line under the model field: what the selected model is called, or
    /// why the list is empty.
    ///
    /// The lists hold bare ids, because an editable field's value has to be
    /// the thing that goes on the wire. This is where the friendly name and
    /// the daemon's note go instead.
    private func refreshSourceNote(provider: CatalogProvider?, models: [CatalogModel]) {
        let endpoint = provider?.endpoint(url: preferences.baseURL)

        if let model = models.first(where: { $0.id == preferences.model }) {
            sourceNoteLabel.stringValue = model.label
        } else if let note = endpoint?.note, !note.isEmpty {
            sourceNoteLabel.stringValue = note
        } else if endpoint == nil, !catalog.isEmpty {
            sourceNoteLabel.stringValue = "Custom endpoint — type the name of a model it serves."
        } else if models.contains(where: { $0.id == preferences.model }) == false, !models.isEmpty {
            sourceNoteLabel.stringValue = "Not in the list — it will still be used exactly as typed."
        } else {
            sourceNoteLabel.stringValue = ""
        }
    }

    /// Shows the Thinking row for a model that has reasoning controls, with
    /// exactly the levels that model accepts.
    private func refreshEffort() {
        guard let thinking = catalog.thinking(
            provider: preferences.provider,
            baseURL: preferences.baseURL,
            model: preferences.model
        ) else {
            effortRow?.isHidden = true
            // And stop sending one. Some models reject the parameter outright,
            // so a level left over from a previous selection is not harmless.
            if !preferences.thinkingEffort.isEmpty {
                preferences.thinkingEffort = ""
                commit()
            }
            return
        }

        effortRow?.isHidden = false
        effortPopUp.removeAllItems()
        effortPopUp.addItem(withTitle: Self.endpointDefaultTitle)
        effortPopUp.lastItem?.representedObject = ""
        for level in thinking.levels {
            effortPopUp.addItem(withTitle: level.capitalized)
            effortPopUp.lastItem?.representedObject = level
        }

        // A thinking model with nothing chosen starts at the catalog's
        // default, which is "low" nearly everywhere. This is an inline
        // rewriter on a 500ms first-token budget and rewriting a sentence is
        // not a reasoning task, so the alternative is every rewrite silently
        // paying for reasoning nobody asked for.
        if preferences.thinkingEffort.isEmpty, thinking.levels.contains(thinking.defaultLevel) {
            preferences.thinkingEffort = thinking.defaultLevel
            commit()
        }

        let index = effortPopUp.itemArray.firstIndex {
            ($0.representedObject as? String) == preferences.thinkingEffort
        }
        effortPopUp.selectItem(at: index ?? 0)
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
        // hand-typed endpoint. Both the catalog's answer and the compiled-in
        // fallback count as untouched: a setting saved before the catalog
        // existed is still not a customisation.
        let wasDefaultModel = preferences.model == preferences.provider.defaultModel
            || preferences.model == defaultModel(for: preferences.provider, baseURL: preferences.baseURL)
        let wasDefaultURL = preferences.baseURL == preferences.provider.defaultBaseURL
            || preferences.baseURL == defaultBaseURL(for: preferences.provider)

        preferences.provider = provider
        if wasDefaultURL { preferences.baseURL = defaultBaseURL(for: provider) }
        if wasDefaultModel { preferences.model = defaultModel(for: provider, baseURL: preferences.baseURL) }
        // The old provider's level means nothing to the new one — Gemini has
        // no "xhigh" and Anthropic no "minimal" — and sending one it does not
        // know is a rejected request. refreshEffort() puts the new model's
        // own default back.
        preferences.thinkingEffort = ""

        refresh()
        commit()
    }

    /// The endpoint to start a provider from: the catalog's first, falling
    /// back to the compiled-in one for when the daemon has not answered yet.
    private func defaultBaseURL(for provider: ProviderID) -> String {
        catalog.provider(provider)?.endpoints.first?.url ?? provider.defaultBaseURL
    }

    private func defaultModel(for provider: ProviderID, baseURL: String) -> String {
        catalog.provider(provider)?.endpoint(url: baseURL)?.defaultModel ?? provider.defaultModel
    }

    /// A list selection. `stringValue` is not yet updated when this fires, so
    /// the chosen item is read directly — reading the field here is the
    /// classic way to get a combo box that lags one selection behind.
    func comboBoxSelectionDidChange(_ notification: Notification) {
        guard let box = notification.object as? NSComboBox,
              let value = box.objectValueOfSelectedItem as? String
        else { return }

        switch box {
        case baseURLBox: chooseEndpoint(value)
        case modelBox: chooseModel(value)
        default: return
        }
    }

    func controlTextDidEndEditing(_ notification: Notification) {
        guard let field = notification.object as? NSTextField else { return }
        let value = field.stringValue.trimmingCharacters(in: .whitespaces)
        switch field {
        case modelBox, modelField: chooseModel(value)
        case baseURLBox, baseURLField: chooseEndpoint(value)
        default: return
        }
    }

    private func chooseEndpoint(_ url: String) {
        let changed = url != preferences.baseURL
        preferences.baseURL = url
        baseURLBox.stringValue = url

        // Only when the endpoint actually changed, and only when the model it
        // leaves behind is not one this endpoint serves. Resetting on every
        // end-editing would throw away a model deliberately typed for a known
        // endpoint the moment the user clicked away.
        if changed,
           let endpoint = catalog.provider(preferences.provider)?.endpoint(url: url),
           endpoint.model(id: preferences.model) == nil,
           let fallback = endpoint.defaultModel
        {
            preferences.model = fallback
            modelBox.stringValue = fallback
            preferences.thinkingEffort = ""
        }

        refreshSuggestions()
        commit()
    }

    private func chooseModel(_ id: String) {
        if id != preferences.model {
            // Levels differ per model, so one chosen for the previous model
            // may not exist here. refreshEffort() reinstates this model's own
            // default rather than leaving a level it would reject.
            preferences.thinkingEffort = ""
        }
        preferences.model = id
        modelBox.stringValue = id

        refreshSuggestions()
        commit()
    }

    @objc private func effortChanged() {
        preferences.thinkingEffort = (effortPopUp.selectedItem?.representedObject as? String) ?? ""
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
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        // The first-run case: an LSUIElement app is not activated by being
        // launched — it has no Dock icon to have been clicked — so the guide
        // would open behind whatever the user is in and go unread.
        window?.orderFrontRegardless()
        NSApp.activate(ignoringOtherApps: true)
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
