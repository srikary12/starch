import Foundation
import Security

/// Owns the lifetime of the `starchd` child process.
///
/// The daemon is spawned at app launch rather than on first use: cold-starting
/// a process while the user waits on an overlay spends latency the product
/// does not have.
@MainActor
public final class DaemonProcess {
    public enum Status: Sendable, Equatable {
        case stopped
        case starting
        case running(DaemonHealth)
        /// Reachable but not the daemon we expect — a mismatched contract
        /// version, or someone else's process on our socket.
        case mismatch(String)
        case failed(String)

        public var isHealthy: Bool {
            if case .running = self { return true }
            return false
        }
    }

    /// How often the shell pings `/healthz`. This doubles as the heartbeat
    /// that resets the daemon's idle-exit timer, so it must stay well inside
    /// `idleTimeout`.
    public nonisolated static let heartbeatInterval: Duration = .seconds(5)

    /// How long the daemon survives without hearing from us.
    public nonisolated static let idleTimeout: Duration = .seconds(30)

    /// Restart backoff, capped. Reset once a heartbeat succeeds.
    private nonisolated static let backoffSchedule: [Duration] = [
        .milliseconds(250), .milliseconds(500), .seconds(1), .seconds(2), .seconds(5), .seconds(15),
    ]

    public private(set) var status: Status = .stopped {
        didSet {
            guard status != oldValue else { return }
            onStatusChange?(status)
        }
    }

    /// Called on the main actor whenever `status` changes.
    public var onStatusChange: ((Status) -> Void)?

    public let socketPath: String
    private let executableURL: URL
    private let debug: Bool

    private var process: Process?
    private var token: String?
    private var heartbeat: Task<Void, Never>?
    private var restartAttempt = 0
    private var stopping = false

    public init(executableURL: URL, socketPath: String, debug: Bool = false) {
        self.executableURL = executableURL
        self.socketPath = socketPath
        self.debug = debug
    }

    /// A client bound to the running daemon, or nil before a successful spawn.
    public var client: DaemonClient? {
        guard let token else { return nil }
        return DaemonClient(socketPath: socketPath, token: token)
    }

    // MARK: Lifecycle

    public func start() {
        guard process == nil else { return }
        stopping = false
        status = .starting

        let token = Self.generateToken()
        self.token = token

        let process = Process()
        process.executableURL = executableURL
        // A deliberately small environment: the daemon needs nothing from the
        // user's shell, and a narrow env is one less way for a stray variable
        // to change its behaviour.
        var environment = [
            "PATH": "/usr/bin:/bin",
            Brand.environmentPrefix + "TOKEN": token,
            Brand.environmentPrefix + "SOCKET": socketPath,
            Brand.environmentPrefix + "IDLE_TIMEOUT": Self.durationString(Self.idleTimeout),
        ]
        if let home = ProcessInfo.processInfo.environment["HOME"] {
            environment["HOME"] = home
        }
        if debug {
            environment[Brand.environmentPrefix + "DEBUG"] = "1"
        }
        process.environment = environment

        // The child inherits our process group on purpose. Killing the app's
        // group from a terminal should take the daemon with it, which is the
        // behaviour inheritance already gives; putting the child in its own
        // group would break that. starchd spawns no children of its own, so
        // there is nothing else to reap.
        let stderrPipe = Pipe()
        process.standardError = stderrPipe
        process.standardOutput = FileHandle.nullDevice
        process.standardInput = FileHandle.nullDevice
        forwardDaemonLog(from: stderrPipe)

        process.terminationHandler = { [weak self] finished in
            let reason = finished.terminationReason
            let code = finished.terminationStatus
            Task { @MainActor [weak self] in
                self?.handleTermination(reason: reason, code: code)
            }
        }

        do {
            try process.run()
        } catch {
            self.process = nil
            status = .failed("Could not launch \(Brand.daemonExecutable): \(error.localizedDescription)")
            scheduleRestart()
            return
        }

        self.process = process
        Log.app.info("spawned \(Brand.daemonExecutable, privacy: .public) pid \(process.processIdentifier)")
        startHeartbeat()
    }

    /// Terminates the daemon and waits briefly for it to unlink its socket.
    ///
    /// Called from `applicationWillTerminate`, so it blocks rather than being
    /// async — an orphaned daemon holding an API key is worse than a few
    /// milliseconds at quit. In practice starchd exits in about ten.
    public func stop() {
        stopping = true
        heartbeat?.cancel()
        heartbeat = nil

        guard let process, process.isRunning else {
            self.process = nil
            token = nil
            status = .stopped
            return
        }

        process.terminate() // SIGTERM; the daemon unlinks its socket on the way out.

        let deadline = Date().addingTimeInterval(2)
        while process.isRunning, Date() < deadline {
            usleep(10_000)
        }
        if process.isRunning {
            Log.app.error("daemon ignored SIGTERM, sending SIGKILL")
            kill(process.processIdentifier, SIGKILL)
        }

        self.process = nil
        token = nil
        status = .stopped
    }

    private func handleTermination(reason: Process.TerminationReason, code: Int32) {
        process = nil
        token = nil
        heartbeat?.cancel()
        heartbeat = nil

        guard !stopping else { return }

        let detail = reason == .uncaughtSignal
            ? "daemon killed by signal \(code)"
            : "daemon exited with status \(code)"
        Log.app.error("\(detail, privacy: .public)")
        status = .failed(detail)
        scheduleRestart()
    }

    private func scheduleRestart() {
        let delay = Self.backoffSchedule[min(restartAttempt, Self.backoffSchedule.count - 1)]
        restartAttempt += 1

        Task { @MainActor [weak self] in
            try? await Task.sleep(for: delay)
            guard let self, !self.stopping, self.process == nil else { return }
            Log.app.info("restarting daemon (attempt \(self.restartAttempt))")
            self.start()
        }
    }

    // MARK: Heartbeat

    private func startHeartbeat() {
        heartbeat?.cancel()
        heartbeat = Task { @MainActor [weak self] in
            // Poll fast at first so the menu reaches "connected" promptly
            // instead of sitting on "starting" for a whole interval.
            await self?.pollUntilReady()

            while !Task.isCancelled {
                try? await Task.sleep(for: Self.heartbeatInterval)
                if Task.isCancelled { return }
                await self?.checkHealth()
            }
        }
    }

    private func pollUntilReady() async {
        let deadline = Date().addingTimeInterval(3)
        while !Task.isCancelled, Date() < deadline {
            if await checkHealth(quiet: true) { return }
            try? await Task.sleep(for: .milliseconds(50))
        }
        // Nothing answered in three seconds; report it and let the heartbeat
        // keep trying.
        await checkHealth()
    }

    @discardableResult
    private func checkHealth(quiet: Bool = false) async -> Bool {
        guard let client, let process, process.isRunning else { return false }

        do {
            let health = try await client.health(timeout: 2.0)

            // The socket is a filesystem rendezvous; confirm the process
            // answering is the one we spawned before trusting it with a key.
            guard health.pid == process.processIdentifier else {
                status = .mismatch(
                    "Another \(Brand.daemonExecutable) (pid \(health.pid)) is using the socket."
                )
                return false
            }
            guard health.apiVersion == DaemonClient.apiVersion else {
                status = .mismatch(
                    "Daemon speaks API \(health.apiVersion), this app speaks \(DaemonClient.apiVersion)."
                )
                return false
            }

            restartAttempt = 0
            status = .running(health)
            return true
        } catch {
            if !quiet {
                let message = error.localizedDescription
                Log.app.error("health check failed: \(message, privacy: .public)")
                status = .failed(message)
            }
            return false
        }
    }

    // MARK: Helpers

    /// Forwards the daemon's stderr into unified logging.
    ///
    /// The daemon never logs user text or key material, which is what makes it
    /// safe to relay verbatim. Keep it that way on the Go side.
    private nonisolated func forwardDaemonLog(from pipe: Pipe) {
        pipe.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            guard !data.isEmpty else {
                handle.readabilityHandler = nil
                return
            }
            for line in String(decoding: data, as: UTF8.self)
                .split(separator: "\n", omittingEmptySubsequences: true)
            {
                Log.daemon.info("\(String(line), privacy: .public)")
            }
        }
    }

    /// 256 bits from the system CSPRNG, base64url without padding.
    nonisolated static func generateToken() -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        let result = SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes)
        precondition(result == errSecSuccess, "SecRandomCopyBytes failed: \(result)")

        return Data(bytes).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    /// Formats a Duration the way Go's time.ParseDuration expects.
    nonisolated static func durationString(_ duration: Duration) -> String {
        "\(duration.components.seconds)s"
    }
}
