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

    private var preferences = Preferences()
    private var daemon: DaemonProcess?
    private var settingsWindow: SettingsWindowController?
    private var onboardingWindow: OnboardingWindowController?

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

        startDaemon()
        registerHotKey()
        registerServices()
        refreshMenu()

        // Keep the menu honest about permission changes made in System
        // Settings while the app is running.
        accessibilityWatcher.start { [weak self] _ in self?.refreshMenu() }

        if !preferences.hasCompletedOnboarding {
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
        daemon.onStatusChange = { [weak self] _ in self?.refreshMenu() }
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

        let preset = Presets.named(preferences.presetID)
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
        } catch let error as DaemonError {
            // The daemon lost its session — it restarted, or the supervisor
            // replaced it after a crash. Re-handshake and try once more rather
            // than making the user trigger again for something invisible.
            if case let .api(_, code, _) = error, code == "no_session" {
                sessionReady = false
                guard await establishSession(client: client) else { return }
                do {
                    try await runStream(client: client, capture: capture, preset: preset)
                } catch {
                    overlay.showError(readableMessage(for: error))
                }
                return
            }
            overlay.showError(readableMessage(for: error))
        } catch is CancellationError {
            overlay.hide()
        } catch {
            overlay.showError(readableMessage(for: error))
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
        // keychainAccount, not rawValue: Settings writes under the former, and
        // reading the wrong account silently finds no key and looks to the user
        // like their saved key was ignored.
        let key = (try? keychain.get(account: preferences.provider.keychainAccount)) ?? nil

        do {
            _ = try await client.startSession(SessionRequest(
                provider: preferences.provider.rawValue,
                model: preferences.model,
                baseURL: preferences.baseURL,
                apiKey: key ?? ""
            ))
            sessionReady = true
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
                // Never fail silently: the rewrite is gone from the overlay by
                // now, so the user has to be told it did not land.
                menuBar.flashTrigger(error.localizedDescription)
            }
        }
    }

    /// Drops the daemon's session so the next rewrite re-sends credentials.
    ///
    /// Without this, changing provider or pasting a new key in Settings would
    /// have no effect until the app restarted — the daemon would keep using
    /// whatever it was handed first.
    func invalidateSession() {
        sessionReady = false
    }

    private func cyclePreset(for capture: SelectionCapturer.Capture) {
        preferences.presetID = Presets.next(after: preferences.presetID)
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
            default:
                return daemonError.localizedDescription ?? "The rewrite failed."
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
            settingsWindow = controller
        }
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
        // new key saved under the same provider. Re-handshaking is cheap and a
        // stale session is invisible until a rewrite fails.
        invalidateSession()
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
