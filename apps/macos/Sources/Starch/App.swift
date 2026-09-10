import AppKit
import StarchKit

// MARK: - Menu bar

@MainActor
final class MenuBarController {
    var onOpenSettings: (() -> Void)?
    var onOpenOnboarding: (() -> Void)?
    var onRestartDaemon: (() -> Void)?

    private let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    private let menu = NSMenu()

    private let connectionItem = NSMenuItem(title: "Starting…", action: nil, keyEquivalent: "")
    private let accessibilityItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")
    private let hotKeyItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")
    private let servicesItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")
    private let activityItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")
    private var flash: Task<Void, Never>?

    init() {
        if let button = statusItem.button {
            button.image = NSImage(
                systemSymbolName: "wand.and.stars",
                accessibilityDescription: Brand.name
            )
            button.image?.isTemplate = true
            if button.image == nil { button.title = Brand.name }
        }

        connectionItem.isEnabled = false
        accessibilityItem.isEnabled = false
        hotKeyItem.isEnabled = false
        activityItem.isEnabled = false
        activityItem.isHidden = true

        menu.addItem(connectionItem)
        menu.addItem(accessibilityItem)
        menu.addItem(hotKeyItem)
        menu.addItem(servicesItem)
        menu.addItem(activityItem)
        menu.addItem(.separator())

        let restart = NSMenuItem(title: "Restart Helper", action: #selector(restartDaemon), keyEquivalent: "")
        restart.target = self
        menu.addItem(restart)

        let settings = NSMenuItem(title: "Settings…", action: #selector(openSettings), keyEquivalent: ",")
        settings.target = self
        menu.addItem(settings)

        let onboarding = NSMenuItem(title: "Set-up Guide…", action: #selector(openOnboarding), keyEquivalent: "")
        onboarding.target = self
        menu.addItem(onboarding)

        menu.addItem(.separator())
        menu.addItem(NSMenuItem(title: "Quit \(Brand.name)", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q"))

        statusItem.menu = menu
    }

    func update(
        daemon: DaemonProcess.Status,
        preferences: Preferences,
        accessibilityTrusted: Bool,
        servicesState: ServicesMenuState
    ) {
        switch daemon {
        case .stopped:
            connectionItem.title = "Helper stopped"
        case .starting:
            connectionItem.title = "Helper starting…"
        case let .running(health):
            connectionItem.title = "Helper connected · \(Brand.daemonExecutable) \(health.version) (\(health.apiVersion))"
        case let .mismatch(detail):
            connectionItem.title = "Helper mismatch — \(detail)"
        case let .failed(detail):
            connectionItem.title = "Helper unavailable — \(detail)"
        }

        accessibilityItem.title = accessibilityTrusted
            ? "Accessibility: granted"
            : "Accessibility: not granted (shortcut inactive)"

        hotKeyItem.title = "Shortcut: \(preferences.hotKey.displayString)"

        // macOS ships a new third-party service switched off, so this is the
        // normal state after a fresh install, not an error. Make it actionable
        // rather than just reporting it.
        switch servicesState {
        case .enabled:
            servicesItem.title = "Right-click menu: on"
            servicesItem.action = nil
            servicesItem.target = nil
            servicesItem.isEnabled = false
        case .disabled, .notConfigured:
            servicesItem.title = "Right-click menu: off — turn on in Settings…"
            servicesItem.action = #selector(openServicesSettings)
            servicesItem.target = self
            servicesItem.isEnabled = true
        }
    }

    /// Transient line in the menu, used in M0 to make the hot key observable
    /// before there is any capture or overlay to show.
    func note(_ text: String) {
        activityItem.title = text
        activityItem.isHidden = false
    }

    /// Flashes the status bar item to show the trigger fired.
    ///
    /// The menu note alone is not enough: it requires opening the menu, so a
    /// working hot key and a dead one look identical from the keyboard. Until
    /// the overlay lands in M2 this is the only way to test the trigger by
    /// hand. It goes away when there is a real overlay to show.
    func flashTrigger(_ text: String) {
        note(text)
        guard let button = statusItem.button else { return }

        flash?.cancel()
        button.title = " ●"
        flash = Task { @MainActor [weak self] in
            try? await Task.sleep(for: .milliseconds(700))
            guard !Task.isCancelled else { return }
            self?.statusItem.button?.title = ""
        }
    }

    @objc private func openServicesSettings() { ServicesMenu.openSettings() }
    @objc private func openSettings() { onOpenSettings?() }
    @objc private func openOnboarding() { onOpenOnboarding?() }
    @objc private func restartDaemon() { onRestartDaemon?() }
}

// MARK: - Application delegate

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let store = PreferencesStore()
    private let keychain = Keychain()
    private let menuBar = MenuBarController()
    private let hotKeys = HotKeyManager()
    private let accessibilityWatcher = Accessibility.Watcher()
    private let capturer = SelectionCapturer()
    private let services = ServicesProvider()
    private let overlay = OverlayController()
    private let replacer = Replacer()

    /// The in-flight rewrite. Cancelling it tears down the socket, which the
    /// daemon turns into cancellation of the upstream model call.
    private var rewriteTask: Task<Void, Never>?
    /// Whether the daemon has been given credentials for the current settings.
    private var sessionReady = false

    /// Presets as the daemon last reported them.
    ///
    /// Seeded from the built-in list so the first trigger after launch has
    /// something to show, then replaced by whatever presets.json actually
    /// holds. The daemon is the authority; this is a cache so that Tab does
    /// not wait on a round trip.
    private var presets: [Preset] = Presets.all
    private var presetSetPath: String?
    private var presetProblem: String?

    /// API keys read this launch, by Keychain account.
    ///
    /// The daemon holds the key in memory only, so every daemon restart costs
    /// a re-handshake — and each of those used to be a fresh Keychain read.
    /// Under an unstable signing identity that means a password prompt roughly
    /// per rewrite, which is fatal to a tool whose entire claim is being faster
    /// than switching to a browser tab. A modal prompt is slower than the loop
    /// this replaces.
    ///
    /// The security cost is close to nothing: the key is already resident for
    /// the daemon's whole lifetime, so this makes it resident in two processes
    /// rather than one, and anyone who can read our memory can read the
    /// daemon's. Cleared whenever the user changes credentials.
    private var keyCache: [String: String] = [:]

    private var preferences = Preferences()
    private var daemon: DaemonProcess?
    private var settingsWindow: SettingsWindowController?
    private var onboardingWindow: OnboardingWindowController?

    /// Set while the first-run guide is on screen.
    ///
    /// Closing it then leads straight into Settings, because the guide
    /// explains what Starch needs and Settings is where it gets it — without a
    /// key the app cannot do anything at all. Reopening the guide later from
    /// the menu is a different intent and must not drag Settings along.
    private var showingFirstRunGuide = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        // A mismatch here silently breaks Accessibility grants and Keychain
        // ACLs, both of which are keyed off the bundle identifier.
        assert(
            Bundle.main.bundleIdentifier == nil || Bundle.main.bundleIdentifier == Brand.bundleIdentifier,
            "Info.plist bundle id \(Bundle.main.bundleIdentifier ?? "nil") != Brand.bundleIdentifier \(Brand.bundleIdentifier)"
        )

        preferences = store.load()

        menuBar.onOpenSettings = { [weak self] in self?.showSettings() }
        menuBar.onOpenOnboarding = { [weak self] in self?.showOnboarding() }
        menuBar.onRestartDaemon = { [weak self] in self?.restartDaemon() }

        installEditMenu()
        startDaemon()
        registerHotKey()
        registerServices()
        refreshMenu()

        // Keep the menu honest about permission changes made in System
        // Settings while the app is running.
        accessibilityWatcher.start { [weak self] _ in self?.refreshMenu() }

        if !preferences.hasCompletedOnboarding {
            showingFirstRunGuide = true
            showOnboarding()
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        hotKeys.unregister()
        accessibilityWatcher.stop()
        // Blocking, deliberately: an orphaned daemon holding an API key is
        // worse than a few milliseconds at quit.
        daemon?.stop()
    }

    // MARK: Daemon

    private func startDaemon() {
        let executable: URL
        do {
            executable = try Self.locateDaemon()
        } catch {
            Log.app.error("cannot locate daemon: \(error.localizedDescription, privacy: .public)")
            menuBar.update(
                daemon: .failed(error.localizedDescription),
                preferences: preferences,
                accessibilityTrusted: Accessibility.isTrusted,
                servicesState: ServicesMenu.state()
            )
            return
        }

        let socketPath: String
        do {
            socketPath = try Paths.socketPath()
        } catch {
            Log.app.error("cannot resolve socket path: \(error.localizedDescription, privacy: .public)")
            menuBar.update(
                daemon: .failed(error.localizedDescription),
                preferences: preferences,
                accessibilityTrusted: Accessibility.isTrusted,
                servicesState: ServicesMenu.state()
            )
            return
        }

        let daemon = DaemonProcess(
            executableURL: executable,
            socketPath: socketPath,
            debug: preferences.debugLogging
        )
        daemon.onStatusChange = { [weak self] status in
            self?.refreshMenu()
            // The preset list lives behind the daemon, so it cannot be read
            // until one is up. Status only changes on a real transition —
            // uptime is deliberately excluded from Status's == — so this fires
            // once per daemon start, not on every heartbeat.
            if status.isHealthy { self?.refreshPresetsFromDaemon() }
        }
        self.daemon = daemon
        daemon.start()
    }

    private func restartDaemon() {
        daemon?.stop()
        daemon = nil
        startDaemon()
        refreshMenu()
    }

    /// Finds the `starchd` binary.
    ///
    /// In a built app it sits in `Contents/Resources`. Under `swift run` there
    /// is no bundle, so fall back to a binary beside the executable, which is
    /// where the Makefile's dev target puts it.
    private static func locateDaemon() throws -> URL {
        if let bundled = Paths.bundledDaemon() {
            return bundled
        }

        let sibling = URL(fileURLWithPath: CommandLine.arguments[0])
            .deletingLastPathComponent()
            .appendingPathComponent(Brand.daemonExecutable)
        if FileManager.default.isExecutableFile(atPath: sibling.path) {
            return sibling
        }

        throw NSError(domain: Brand.bundleIdentifier, code: 1, userInfo: [
            NSLocalizedDescriptionKey:
                "\(Brand.daemonExecutable) not found in the app bundle. Run `make build` to assemble it.",
        ])
    }

    // MARK: Hot key

    private func registerHotKey() {
        do {
            try hotKeys.register(preferences.hotKey) { [weak self] in
                self?.hotKeyFired()
            }
        } catch {
            Log.app.error("hot key registration failed: \(error.localizedDescription, privacy: .public)")
            menuBar.note("Shortcut unavailable: \(error.localizedDescription)")
        }
    }

    private func hotKeyFired() {
        Task { @MainActor in
            switch await capturer.capture() {
            case let .success(capture):
                beginRewrite(with: capture)
            case let .failure(error):
                reportCaptureFailure(error)
            }
        }
    }

    // MARK: Main menu

    /// Installs a main menu carrying the standard edit commands.
    ///
    /// An LSUIElement app shows no menu bar, so it is easy to conclude it does
    /// not need a main menu. It does: AppKit routes ⌘X/⌘C/⌘V/⌘A/⌘Z through the
    /// main menu's key equivalents, and without one those keys do nothing in
    /// any text field the app owns. That made it impossible to paste an API
    /// key into Settings — the one thing that window exists to do.
    ///
    /// The items are given no target, so they travel the responder chain to
    /// whichever field is focused.
    private func installEditMenu() {
        let mainMenu = NSMenu()

        // macOS treats the first item as the application menu and will not
        // show a later one in its place, so Edit cannot be first.
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(
            withTitle: "Quit \(Brand.name)",
            action: #selector(NSApplication.terminate(_:)),
            keyEquivalent: "q"
        )
        appItem.submenu = appMenu
        mainMenu.addItem(appItem)

        let editItem = NSMenuItem()
        let editMenu = NSMenu(title: "Edit")
        let commands: [(String, Selector, String, NSEvent.ModifierFlags)] = [
            ("Undo", Selector(("undo:")), "z", [.command]),
            ("Redo", Selector(("redo:")), "z", [.command, .shift]),
            ("Cut", #selector(NSText.cut(_:)), "x", [.command]),
            ("Copy", #selector(NSText.copy(_:)), "c", [.command]),
            ("Paste", #selector(NSText.paste(_:)), "v", [.command]),
            ("Select All", #selector(NSText.selectAll(_:)), "a", [.command]),
        ]
        for (title, action, key, modifiers) in commands {
            if title == "Cut" { editMenu.addItem(.separator()) }
            let item = NSMenuItem(title: title, action: action, keyEquivalent: key)
            item.keyEquivalentModifierMask = modifiers
            editMenu.addItem(item)
        }
        editItem.submenu = editMenu
        mainMenu.addItem(editItem)

        NSApp.mainMenu = mainMenu
    }

    // MARK: Services

    private func registerServices() {
        services.onSelection = { [weak self] text in
            guard let self else { return }
            self.beginRewrite(with: self.capturer.capture(fromService: text))
        }
        NSApp.servicesProvider = services
        // Without this the entry only appears after a relaunch, and often not
        // even then. See `make register-services` for the rest of the ritual.
        NSUpdateDynamicServices()
    }

    // MARK: The shared flow

    /// Where both triggers converge.
    ///
    /// M1 ends here: the selection is captured and shown. The overlay, the
    /// rewrite and the replacement land in M2, and they hang off this one
    /// function so there is a single code path and a single UX.
    private func beginRewrite(with capture: SelectionCapturer.Capture) {
        // A second trigger replaces the first rather than racing it.
        rewriteTask?.cancel()

        let preset = Presets.named(preferences.presetID, in: presets)
        overlay.begin(presetName: preset.name, near: capture)

        overlay.onAccept = { [weak self] text in
            self?.applyReplacement(text, for: capture)
        }
        overlay.onCancel = { [weak self] in
            // Cancelling the task closes the socket, which is what stops the
            // generation upstream. Without this Escape would only hide the UI.
            self?.rewriteTask?.cancel()
            Log.capture.info("rewrite cancelled by the user")
        }
        overlay.onCyclePreset = { [weak self] in
            self?.cyclePreset(for: capture)
        }

        rewriteTask = Task { @MainActor [weak self] in
            await self?.stream(capture: capture, preset: preset)
        }
    }

    /// Runs one rewrite into the overlay.
    private func stream(capture: SelectionCapturer.Capture, preset: Preset) async {
        guard let client = daemon?.client else {
            overlay.showError("The helper is not running.")
            return
        }

        // The daemon holds the key in memory only, so a restarted daemon needs
        // the session again. Establishing it lazily here — rather than at
        // launch — also means a key added in Settings works without a restart.
        if !sessionReady, !(await establishSession(client: client)) { return }

        do {
            try await runStream(client: client, capture: capture, preset: preset)
        } catch let error as DaemonError where Self.isRecoverable(error) {
            // The daemon went away mid-request. It restarts in well under a
            // second — the supervisor respawns it, and Settings changes restart
            // it deliberately — so the window where this happens is small and
            // entirely invisible to the user. Surfacing it would be blaming
            // them for our own lifecycle.
            Log.capture.info("daemon unavailable; re-handshaking and retrying once")
            sessionReady = false

            // Give the supervisor time to bring it back before trying again.
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }

            guard let retryClient = daemon?.client, await establishSession(client: retryClient) else {
                return
            }
            do {
                try await runStream(client: retryClient, capture: capture, preset: preset)
            } catch is CancellationError {
                overlay.hide()
            } catch {
                overlay.showError(readableMessage(for: error))
            }
        } catch is CancellationError {
            overlay.hide()
        } catch {
            overlay.showError(readableMessage(for: error))
        }
    }

    /// Whether a failure is the daemon being momentarily absent rather than
    /// something the user needs to act on.
    private static func isRecoverable(_ error: DaemonError) -> Bool {
        switch error {
        case let .api(_, code, _):
            // Its session died with it; the key lives only in its memory.
            code == "no_session"
        case .transport, .timedOut:
            // Restarting, so its socket is briefly gone.
            true
        default:
            false
        }
    }

    /// One pass over the stream. Separated so the no-session retry can rerun it.
    private func runStream(
        client: DaemonClient,
        capture: SelectionCapturer.Capture,
        preset: Preset
    ) async throws {
        let started = ContinuousClock.now
        var firstToken: Duration?

        for try await event in client.rewrite(text: capture.text, preset: preset.id) {
            if Task.isCancelled { return }
            switch event {
            case let .delta(text):
                if firstToken == nil { firstToken = started.duration(to: .now) }
                overlay.append(text)
            case let .done(full, usage):
                overlay.finish(full: full)
                logTiming(started: started, firstToken: firstToken, usage: usage, preset: preset.id)
            case let .failed(code, message):
                Log.capture.error("rewrite failed: \(code, privacy: .public)")
                overlay.showError(message)
            }
        }
    }

    private func establishSession(client: DaemonClient) async -> Bool {
        do {
            _ = try await client.startSession(SessionRequest(
                provider: preferences.provider.rawValue,
                model: preferences.model,
                baseURL: preferences.baseURL,
                apiKey: apiKey(for: preferences.provider)
            ))
            sessionReady = true
            await refreshPresets(client: client)
            return true
        } catch {
            overlay.showError(readableMessage(for: error))
            return false
        }
    }

    private func applyReplacement(_ text: String, for capture: SelectionCapturer.Capture) {
        Task { @MainActor in
            switch await replacer.replace(text, for: capture) {
            case let .success(method):
                menuBar.flashTrigger("Replaced via \(method.rawValue) in \(capture.appName)")
            case let .failure(error):
                // Never fail silently, and never lose the rewrite. The overlay
                // has already closed by this point, so the clipboard is the
                // only place left to put it — announced rather than silently.
                let pasteboard = NSPasteboard.general
                pasteboard.clearContents()
                let saved = pasteboard.setString(text, forType: .string)

                menuBar.flashTrigger(
                    saved
                        ? "\(error.localizedDescription) The rewrite is on your clipboard."
                        : error.localizedDescription
                )
            }
        }
    }

    /// Pulls the active preset list from the daemon.
    ///
    /// Failure is deliberately quiet: the cached list still works, and a
    /// preset list that could not be refreshed is not worth interrupting a
    /// rewrite the user is waiting on.
    /// Reads the preset list from the daemon whenever one is reachable.
    ///
    /// Separate from establishSession because `/v1/presets` needs no session
    /// and no API key. Hanging the only refresh off the session meant Settings
    /// showed "Waiting for the helper…" — with Edit and Show in Finder both
    /// disabled — until the user had run a rewrite, which is backwards: the
    /// window exists to be opened before the first rewrite, not after.
    private func refreshPresetsFromDaemon() {
        guard let client = daemon?.client else { return }
        Task { @MainActor [weak self] in
            await self?.refreshPresets(client: client)
        }
    }

    private func refreshPresets(client: DaemonClient) async {
        guard let set = try? await client.presets() else { return }

        presets = set.presets
        presetSetPath = set.path
        presetProblem = set.problem

        if let problem = set.problem {
            Log.app.error("presets.json could not be used: \(problem, privacy: .public)")
        }
        // A preset that no longer exists — the file was edited, or an id was
        // renamed — must not leave Tab cycling from nowhere.
        if !presets.contains(where: { $0.id == preferences.presetID }), let first = presets.first {
            preferences.presetID = first.id
            store.save(preferences)
        }
        refreshMenu()
        settingsWindow?.refreshPresetStatus()
    }

    /// The key for a provider, read from the Keychain at most once per launch.
    ///
    /// keychainAccount, not rawValue: Settings writes under the former, and
    /// reading the wrong account silently finds no key and looks to the user
    /// like their saved key was ignored.
    private func apiKey(for provider: ProviderID) -> String {
        let account = provider.keychainAccount
        if let cached = keyCache[account] { return cached }

        let key = ((try? keychain.get(account: account)) ?? nil) ?? ""
        keyCache[account] = key
        return key
    }

    /// Drops the daemon's session so the next rewrite re-sends credentials.
    ///
    /// Deliberately does not clear the key cache. This fires on the common,
    /// invisible path — a daemon restart — and re-reading the Keychain there is
    /// what produced a password prompt per rewrite.
    func invalidateSession() {
        sessionReady = false
    }

    /// Drops both the session and the cached key.
    ///
    /// For user-initiated credential changes only, where one Keychain read is
    /// expected and correct.
    private func invalidateCredentials() {
        keyCache.removeAll()
        sessionReady = false
    }

    private func cyclePreset(for capture: SelectionCapturer.Capture) {
        preferences.presetID = Presets.next(after: preferences.presetID, in: presets)
        store.save(preferences)
        refreshMenu()
        beginRewrite(with: capture)
    }

    private func logTiming(
        started: ContinuousClock.Instant,
        firstToken: Duration?,
        usage: RewriteUsage?,
        preset: String
    ) {
        let total = Double(started.duration(to: .now).components.attoseconds) / 1e15
        let first = firstToken.map { Double($0.components.attoseconds) / 1e15 } ?? -1
        Log.capture.info(
            """
            rewrite done preset=\(preset, privacy: .public) \
            first_token_ms=\(first, format: .fixed(precision: 0)) \
            total_ms=\(total, format: .fixed(precision: 0)) \
            tokens_out=\(usage?.outputTokens ?? -1)
            """
        )
    }

    private func readableMessage(for error: Error) -> String {
        if let daemonError = error as? DaemonError {
            switch daemonError {
            case let .api(_, _, message):
                // The daemon already phrased this for a human.
                return message
            case .unauthorized:
                return "The helper rejected the handshake. Try Restart Helper."
            case .transport, .timedOut:
                // Never the POSIX text: "Network is down" for a Unix socket
                // sends people to check their wifi.
                return "The helper isn't responding. Try Restart Helper from the menu."
            default:
                return daemonError.localizedDescription
            }
        }
        return error.localizedDescription
    }

    private func reportCaptureFailure(_ error: SelectionCapturer.Failure) {
        // Never fail silently: if neither strategy worked the user needs to
        // know that, not wonder whether the shortcut fired at all.
        menuBar.flashTrigger(error.localizedDescription)

        if case .notTrusted = error {
            showOnboarding()
        }
    }

    // MARK: Windows

    private func showSettings() {
        if settingsWindow == nil {
            let controller = SettingsWindowController(
                preferences: preferences, store: store, keychain: keychain
            )
            controller.onPreferencesChanged = { [weak self] updated in
                self?.applyPreferences(updated)
            }
            controller.onShowOnboarding = { [weak self] in self?.showOnboarding() }
            controller.presetSet = { [weak self] in
                (path: self?.presetSetPath, count: self?.presets.count ?? 0, problem: self?.presetProblem)
            }
            settingsWindow = controller
        }
        // Re-read on every open. Someone who has just fixed their JSON expects
        // the warning to be gone when they come back to look, and the daemon
        // re-parses on the next read, so this is the whole of that story.
        refreshPresetsFromDaemon()
        settingsWindow?.show()
    }

    private func showOnboarding() {
        if onboardingWindow == nil {
            let controller = OnboardingWindowController()
            controller.onFinished = { [weak self] in
                guard let self else { return }
                if !self.preferences.hasCompletedOnboarding {
                    self.preferences.hasCompletedOnboarding = true
                    self.store.save(self.preferences)
                }
                self.refreshMenu()

                guard self.showingFirstRunGuide else { return }
                self.showingFirstRunGuide = false
                // Deferred by a turn: this runs from windowWillClose, and
                // opening a window while another is mid-close leaves the new
                // one ordered behind it.
                Task { @MainActor [weak self] in
                    self?.showSettings()
                }
            }
            onboardingWindow = controller
        }
        onboardingWindow?.show()
    }

    private func applyPreferences(_ updated: Preferences) {
        let hotKeyChanged = updated.hotKey != preferences.hotKey
        let debugChanged = updated.debugLogging != preferences.debugLogging
        preferences = updated

        if hotKeyChanged {
            registerHotKey()
        }
        if debugChanged {
            // The daemon reads its log level from the environment at spawn.
            restartDaemon()
        }
        // Any settings change may have altered the credentials, including a
        // new key saved under the same provider — Settings routes key saves and
        // removals through here for exactly that reason. This is the one path
        // where re-reading the Keychain is warranted.
        invalidateCredentials()
        refreshMenu()
    }

    private func refreshMenu() {
        menuBar.update(
            daemon: daemon?.status ?? .stopped,
            preferences: preferences,
            accessibilityTrusted: Accessibility.isTrusted,
            servicesState: ServicesMenu.state()
        )
    }
}

// MARK: - Entry point

@main
@MainActor
struct StarchApp {
    /// NSApplication.delegate is a weak reference, so something has to own the
    /// delegate for the lifetime of the process.
    private static let delegate = AppDelegate()

    static func main() {
        let app = NSApplication.shared
        app.delegate = delegate
        // Menu bar utility: no Dock icon. Info.plist sets LSUIElement as well;
        // this covers running the binary directly during development.
        app.setActivationPolicy(.accessory)
        app.run()
    }
}
