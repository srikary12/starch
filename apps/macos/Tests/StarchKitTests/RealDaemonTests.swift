import Foundation
import Testing

@testable import StarchKit

// The Swift client against the real Go daemon.
//
// Everything else in this target talks to scripted stand-ins, and for the bug
// this file exists for, that was not good enough: the scripted tests passed
// against the broken client. The real daemon did not. Driven the way the app
// drives it, the unfixed client failed 237 rewrites in 500 — 66 of them after
// the rewrite had already arrived, which is the "helper is not available" users
// saw once a rewrite had finished, and 171 where a complete rewrite was on the
// wire and never delivered at all.
//
// The difference is how the daemon closes the socket. Go's net/http ends a
// `Connection: close` reply with a half-close, and Network.framework reports
// that as `.failed(ENETDOWN)` in a way a scripted peer did not reproduce. So
// this uses the real thing: it spawns starchd and a stub provider and runs a
// few hundred rewrites through them.
//
// It needs a built daemon, which is why it is gated on STARCH_DAEMON rather
// than running everywhere. `make test-swift` sets it; so does CI.

private let daemonPath = ProcessInfo.processInfo.environment["STARCH_DAEMON"]

/// A stub OpenAI-compatible provider, in Python because every macOS image has
/// it and it keeps this file about the client rather than about HTTP servers.
private let stubProvider = #"""
import http.server, json, socketserver
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()
        for piece in ["Thanks ", "for ", "the ", "update."]:
            self.wfile.write(b"data: " + json.dumps(
                {"choices": [{"delta": {"content": piece}}]}).encode() + b"\n\n")
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
srv = socketserver.ThreadingTCPServer(("127.0.0.1", 0), H)
print(srv.server_address[1], flush=True)
srv.serve_forever()
"""#

/// The daemon and its provider, torn down together.
private final class Rig {
    let client: DaemonClient
    let providerPort: Int
    private let daemon: Process
    private let provider: Process
    private let directory: String

    init(daemonPath: String) async throws {
        // The daemon tightens its socket's directory to 0700, so it needs one
        // of its own; and sun_path is 104 bytes, so it has to be short.
        directory = "/tmp/starch-it-\(UUID().uuidString.prefix(6))"
        try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
        let socket = directory + "/d.sock"
        let token = UUID().uuidString

        provider = Process()
        provider.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        provider.arguments = ["python3", "-c", stubProvider]
        let portPipe = Pipe()
        provider.standardOutput = portPipe
        provider.standardError = FileHandle.nullDevice
        try provider.run()

        let line = portPipe.fileHandleForReading.availableData
        guard let port = Int(String(decoding: line, as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)) else {
            provider.terminate()
            throw POSIXError(.EIO)
        }
        providerPort = port

        daemon = Process()
        daemon.executableURL = URL(fileURLWithPath: daemonPath)
        daemon.environment = [
            "STARCH_SOCKET": socket,
            "STARCH_TOKEN": token,
            "STARCH_IDLE_TIMEOUT": "10m",
            "HOME": NSHomeDirectory(),
        ]
        daemon.standardError = FileHandle.nullDevice
        daemon.standardOutput = FileHandle.nullDevice
        try daemon.run()

        client = DaemonClient(socketPath: socket, token: token)

        // Up in a few milliseconds in practice; bounded so a broken build
        // fails here rather than hanging the suite.
        let deadline = Date().addingTimeInterval(5)
        while Date() < deadline {
            if (try? await client.health(timeout: 0.5)) != nil { return }
            try await Task.sleep(for: .milliseconds(20))
        }
        tearDown()
        throw POSIXError(.ETIMEDOUT)
    }

    func tearDown() {
        daemon.terminate()
        provider.terminate()
        daemon.waitUntilExit()
        provider.waitUntilExit()
        try? FileManager.default.removeItem(atPath: directory)
    }
}

@Suite(
    "Against the real daemon",
    .serialized,
    .enabled(if: daemonPath != nil, "set STARCH_DAEMON to a built starchd")
)
struct RealDaemonTests {
    /// Every rewrite arrives, and every one ends as a success.
    ///
    /// A few hundred, because the failure is a race at the end of each
    /// stream. Unfixed, this lost roughly one rewrite in two against a provider
    /// as fast as this stub; against a real one only the tail of each stream is
    /// exposed, which is why users saw it only sometimes.
    @Test("every rewrite is delivered and ends cleanly")
    func everyRewriteEndsCleanly() async throws {
        let rig = try await Rig(daemonPath: try #require(daemonPath))
        defer { rig.tearDown() }

        _ = try await rig.client.startSession(SessionRequest(
            provider: "openai_compatible",
            model: "stub",
            baseURL: "http://127.0.0.1:\(rig.providerPort)/v1",
            apiKey: ""
        ))

        for attempt in 1...300 {
            var events: [RewriteEvent] = []
            do {
                for try await event in rig.client.rewrite(text: "thx for the update", preset: "professional") {
                    events.append(event)
                }
            } catch {
                let when = events.contains { if case .done = $0 { true } else { false } }
                    ? "after the rewrite had arrived" : "before the rewrite arrived"
                Issue.record("attempt \(attempt): threw \(error) \(when)")
                return
            }

            guard case let .done(full, _)? = events.last else {
                Issue.record("attempt \(attempt): ended without a terminal event: \(events)")
                return
            }
            #expect(full == "Thanks for the update.", "attempt \(attempt)")
        }
    }
}

// MARK: - The supervisor, when the shortcut is pressed at a bad moment

/// A socket path in a directory of its own, removed afterwards.
private func scratchSocket() throws -> (socket: String, cleanUp: () -> Void) {
    let directory = "/tmp/starch-sv-\(UUID().uuidString.prefix(6))"
    try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
    return (directory + "/d.sock", { try? FileManager.default.removeItem(atPath: directory) })
}

@MainActor
@Suite(
    "The supervisor, asked for a daemon on demand",
    .serialized,
    .enabled(if: daemonPath != nil, "set STARCH_DAEMON to a built starchd")
)
struct ReadyClientTests {
    /// Pressing the shortcut before anything has started still gets a helper.
    @Test("a stopped supervisor starts the daemon and hands back a client that answers")
    func startsFromStopped() async throws {
        let (socket, cleanUp) = try scratchSocket()
        defer { cleanUp() }
        let supervisor = DaemonProcess(
            executableURL: URL(fileURLWithPath: try #require(daemonPath)),
            socketPath: socket
        )
        defer { supervisor.stop() }

        let client = try #require(await supervisor.readyClient(within: .seconds(3)))
        let health = try await client.health(timeout: 1)
        #expect(health.status == "ok")
    }

    /// The case that used to say "The helper is not running" at once.
    ///
    /// After a crash the supervisor waits out a back-off before respawning.
    /// Someone who has just pressed the shortcut is the best reason there is
    /// to skip it, so asking for a client starts the daemon now — and hands
    /// back one bound to the new process, not the dead one.
    @Test("a killed daemon is replaced promptly when a client is asked for")
    func replacesAKilledDaemon() async throws {
        let (socket, cleanUp) = try scratchSocket()
        defer { cleanUp() }
        let supervisor = DaemonProcess(
            executableURL: URL(fileURLWithPath: try #require(daemonPath)),
            socketPath: socket
        )
        defer { supervisor.stop() }

        let first = try #require(await supervisor.readyClient(within: .seconds(3)))
        let before = try await first.health(timeout: 1).pid
        kill(before, SIGKILL)

        let started = ContinuousClock.now
        let second = try #require(
            await supervisor.readyClient(within: .seconds(3)),
            "no daemon came back within the limit"
        )
        let after = try await second.health(timeout: 1).pid

        #expect(after != before, "handed back the dead daemon's client")
        #expect(started.duration(to: .now) < .seconds(3))
    }
}
