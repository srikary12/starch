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
    private let activityItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")

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

    func update(daemon: DaemonProcess.Status, preferences: Preferences, accessibilityTrusted: Bool) {
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
    }

    /// Transient line in the menu, used in M0 to make the hot key observable
    /// before there is any capture or overlay to show.
    func note(_ text: String) {
        activityItem.title = text
        activityItem.isHidden = false
    }

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
                accessibilityTrusted: Accessibility.isTrusted
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
                accessibilityTrusted: Accessibility.isTrusted
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

    /// M0 stops here: capture, overlay and rewriting arrive in M1 and M2.
    /// Recording the trigger in the menu and the log is what makes the hot key
    /// verifiable by hand today.
    private func hotKeyFired() {
        let app = NSWorkspace.shared.frontmostApplication?.localizedName ?? "unknown app"
        let time = Date().formatted(date: .omitted, time: .standard)
        Log.app.info("hot key fired in \(app, privacy: .public)")
        menuBar.note("Shortcut fired at \(time) in \(app)")
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
        refreshMenu()
    }

    private func refreshMenu() {
        menuBar.update(
            daemon: daemon?.status ?? .stopped,
            preferences: preferences,
            accessibilityTrusted: Accessibility.isTrusted
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
