import Foundation
import Testing

@testable import StarchKit

// The streaming client against a real Unix socket, with the daemon's side
// scripted per test.
//
// These exist because of a bug that the framing tests could not see. The
// client treated the end of the *connection* as the end of the rewrite, so what
// happened after the terminal event decided whether a finished rewrite was
// reported as a success. When the socket's close surfaced as an error, a rewrite
// the user had already watched complete was reported as a dead helper — and the
// app, reasonably believing that, ran the whole rewrite again. The contract says
// the terminal event ends the stream. These hold the client to it.

// MARK: - A scripted daemon

/// The daemon's end of one connection.
///
/// Every descriptor is closed exactly once, under a lock. That is not tidiness:
/// descriptor numbers are reused the moment they are freed, and Swift Testing
/// runs suites in parallel in one process, so a second close of the same
/// number can land on some other live socket — the client's own included. An
/// earlier version of this harness did exactly that, and produced failures
/// that looked like the bug under test.
private final class Peer: @unchecked Sendable {
    private let lock = NSLock()
    private var fd: Int32?

    init(_ fd: Int32) { self.fd = fd }

    func write(_ text: String) {
        lock.lock()
        defer { lock.unlock() }
        guard let fd else { return }
        let bytes = Array(text.utf8)
        bytes.withUnsafeBufferPointer { buffer in
            _ = Darwin.write(fd, buffer.baseAddress, buffer.count)
        }
    }

    /// Ends the response the way Go's net/http ends a `Connection: close`
    /// reply: shut the write side, give the client a moment to read, then
    /// close. This is what the real daemon does after every rewrite.
    func finishLikeGo() {
        lock.lock()
        if let fd { _ = Darwin.shutdown(fd, SHUT_WR) }
        lock.unlock()
        Thread.sleep(forTimeInterval: 0.05)
        close()
    }

    /// An abrupt close, as a crashed daemon would leave things.
    func close() {
        lock.lock()
        defer { lock.unlock() }
        guard let fd else { return }
        Darwin.close(fd)
        self.fd = nil
    }
}

/// A stand-in daemon listening on a real Unix socket.
///
/// It accepts one connection, reads the request, and hands the connection to
/// the test's script. Network.framework is on the other end, so these exercise
/// the same code path the app does.
private final class ScriptedDaemon: @unchecked Sendable {
    let path: String
    private let listener: Int32
    private let lock = NSLock()
    private var peers: [Peer] = []

    init(script: @escaping @Sendable (Peer) -> Void) throws {
        // Short on purpose: sun_path is 104 bytes, and NSTemporaryDirectory()
        // alone can use half of that.
        path = "/tmp/starch-\(UUID().uuidString.prefix(8)).sock"
        unlink(path)

        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw POSIXError(.EIO) }
        listener = fd

        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        withUnsafeMutableBytes(of: &address.sun_path) { raw in
            raw.copyBytes(from: bytes)
        }
        let bound = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bound == 0, listen(fd, 4) == 0 else {
            Darwin.close(fd)
            throw POSIXError(.EADDRINUSE)
        }

        Thread.detachNewThread { [self] in
            let client = accept(fd, nil, nil)
            guard client >= 0 else { return }
            let peer = Peer(client)
            self.track(peer)

            // The client writes its request in one go, and no script depends
            // on its content, so one read is enough to have consumed it.
            var buffer = [UInt8](repeating: 0, count: 64 * 1024)
            _ = read(client, &buffer, buffer.count)

            script(peer)
        }
    }

    private func track(_ peer: Peer) {
        lock.lock()
        defer { lock.unlock() }
        peers.append(peer)
    }

    func shutdown() {
        lock.lock()
        let open = peers
        peers = []
        lock.unlock()

        for peer in open { peer.close() }
        // Shut the listener down before closing it, so an accept() still
        // blocked on it returns rather than racing a freed descriptor number.
        _ = Darwin.shutdown(listener, SHUT_RDWR)
        Darwin.close(listener)
        unlink(path)
    }
}

/// One HTTP/1.1 chunk, which is how Go frames a streamed response.
private func chunk(_ body: String) -> String {
    String(Array(body.utf8).count, radix: 16) + "\r\n" + body + "\r\n"
}

private func event(_ json: String) -> String { chunk("data: \(json)\n\n") }

private let streamHead =
    "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"

// MARK: - Collecting a stream, with a deadline

private final class EventLog: @unchecked Sendable {
    private let lock = NSLock()
    private var stored: [RewriteEvent] = []

    func append(_ event: RewriteEvent) {
        lock.lock()
        defer { lock.unlock() }
        stored.append(event)
    }

    var events: [RewriteEvent] {
        lock.lock()
        defer { lock.unlock() }
        return stored
    }
}

private enum Outcome {
    case finished([RewriteEvent])
    case threw(Error, after: [RewriteEvent])
    /// Neither finished nor threw before the deadline.
    case hung(after: [RewriteEvent])
}

/// Reads the whole stream, giving up after `limit`.
///
/// The deadline is the point of several of these: the bug being guarded
/// against is a finished rewrite that the client does not *treat* as finished,
/// and the cleanest symptom of that is a stream which never ends.
private func collect(
    _ stream: AsyncThrowingStream<RewriteEvent, Error>,
    within limit: Duration = .seconds(3)
) async -> Outcome {
    let log = EventLog()
    return await withTaskGroup(of: Outcome?.self) { group in
        group.addTask {
            do {
                for try await event in stream { log.append(event) }
                return .finished(log.events)
            } catch {
                return .threw(error, after: log.events)
            }
        }
        group.addTask {
            try? await Task.sleep(for: limit)
            return nil
        }

        let first = await group.next() ?? nil
        group.cancelAll()
        return first ?? .hung(after: log.events)
    }
}

private func rewrite(against daemon: ScriptedDaemon) -> AsyncThrowingStream<RewriteEvent, Error> {
    DaemonClient(socketPath: daemon.path, token: "test-token")
        .rewrite(text: "thx for the update", preset: "professional")
}

// MARK: - Tests

@Suite("Streaming a rewrite over the socket", .serialized)
struct StreamingTests {
    /// The heart of it. The daemon sends the terminal event and then does
    /// nothing at all — no chunk terminator, no close. A client that waits for
    /// the connection to end before believing the rewrite is over never
    /// finishes; one that follows the contract finishes immediately.
    @Test("the terminal event ends the rewrite, whatever the connection does next")
    func terminalEventEndsTheStream() async throws {
        let daemon = try ScriptedDaemon { peer in
            peer.write(streamHead)
            peer.write(event(#"{"delta":"Thanks."}"#))
            peer.write(event(#"{"done":true,"full":"Thanks."}"#))
            // Hold the connection open well past the test's deadline.
            Thread.sleep(forTimeInterval: 10)
        }
        defer { daemon.shutdown() }

        switch await collect(rewrite(against: daemon)) {
        case let .finished(events):
            #expect(events == [.delta("Thanks."), .done(full: "Thanks.", usage: nil)])
        case let .threw(error, after):
            Issue.record("a finished rewrite threw \(error) after \(after)")
        case let .hung(after):
            Issue.record("a finished rewrite never ended; had \(after)")
        }
    }

    /// An in-band error is terminal too. The contract: it "ends the stream".
    @Test("an in-band error ends the rewrite")
    func inBandErrorEndsTheStream() async throws {
        let daemon = try ScriptedDaemon { peer in
            peer.write(streamHead)
            peer.write(event(#"{"error":{"code":"provider_error","message":"The provider refused."}}"#))
            Thread.sleep(forTimeInterval: 10)
        }
        defer { daemon.shutdown() }

        switch await collect(rewrite(against: daemon)) {
        case let .finished(events):
            #expect(events == [.failed(code: "provider_error", message: "The provider refused.")])
        case let .threw(error, after):
            Issue.record("an in-band error was rethrown as \(error) after \(after)")
        case let .hung(after):
            Issue.record("an in-band error did not end the stream; had \(after)")
        }
    }

    /// Nothing after the terminal event reaches the app. Otherwise a trailing
    /// fragment would be appended to a rewrite already marked finished.
    @Test("nothing after the terminal event is delivered")
    func nothingAfterTheTerminalEvent() async throws {
        let daemon = try ScriptedDaemon { peer in
            // Both events in one write, so the client sees them in one read —
            // the case where it cannot rely on stopping between reads.
            peer.write(streamHead
                + event(#"{"done":true,"full":"Thanks."}"#)
                + event(#"{"delta":" and more"}"#)
                + "0\r\n\r\n")
        }
        defer { daemon.shutdown() }

        switch await collect(rewrite(against: daemon)) {
        case let .finished(events):
            #expect(events == [.done(full: "Thanks.", usage: nil)])
        case let .threw(error, after):
            Issue.record("threw \(error) after \(after)")
        case let .hung(after):
            Issue.record("hung after \(after)")
        }
    }

    /// The other half of the same rule. A daemon that dies part-way through —
    /// connection gone, no terminal event — has *not* finished the rewrite,
    /// and the deltas received so far must not be mistaken for one. Before this
    /// was fixed the stream simply ended, the overlay was left showing half a
    /// sentence as though it were still arriving, and nothing said why.
    @Test("a connection that closes before the terminal event is an error")
    func closingMidStreamIsAnError() async throws {
        let daemon = try ScriptedDaemon { peer in
            peer.write(streamHead)
            peer.write(event(#"{"delta":"Thanks for"}"#))
            peer.close()
        }
        defer { daemon.shutdown() }

        switch await collect(rewrite(against: daemon)) {
        case let .finished(events):
            Issue.record("a rewrite cut off part-way ended as though it were complete: \(events)")
        case let .threw(error, after):
            #expect(after == [.delta("Thanks for")])
            guard case .incompleteStream = error as? DaemonError else {
                Issue.record("threw \(error), want DaemonError.incompleteStream")
                return
            }
        case let .hung(after):
            Issue.record("hung after \(after)")
        }
    }

    /// A complete stream followed by an abrupt close, as a daemon that crashed
    /// or was killed right after answering would leave it.
    ///
    /// Network.framework reports a peer's close of a Unix socket as
    /// `.failed(ENETDOWN)` through the state handler, which can run before the
    /// receive carrying the reply's last bytes. The production failure is
    /// covered by RealDaemonTests, against the real daemon — this scripted
    /// version did *not* reproduce it, because the ordinary close is a
    /// half-close and this is not. It stays as the guard for the abrupt case.
    @Test("a complete rewrite followed by an immediate close always succeeds")
    func completeThenCloseAlwaysSucceeds() async throws {
        for attempt in 1...400 {
            let daemon = try ScriptedDaemon { peer in
                peer.write(streamHead
                    + event(#"{"delta":"Thanks."}"#)
                    + event(#"{"done":true,"full":"Thanks."}"#)
                    + "0\r\n\r\n")
                peer.close()
            }
            defer { daemon.shutdown() }

            switch await collect(rewrite(against: daemon)) {
            case let .finished(events):
                #expect(events.last == .done(full: "Thanks.", usage: nil), "attempt \(attempt)")
            case let .threw(error, after):
                Issue.record("attempt \(attempt): a finished rewrite threw \(error) after \(after)")
                return
            case let .hung(after):
                Issue.record("attempt \(attempt): hung after \(after)")
                return
            }
        }
    }

    /// And a daemon that hangs up before saying anything at all.
    @Test("a connection that closes before any response is an error")
    func closingBeforeRespondingIsAnError() async throws {
        let daemon = try ScriptedDaemon { peer in
            peer.close()
        }
        defer { daemon.shutdown() }

        switch await collect(rewrite(against: daemon)) {
        case let .finished(events):
            Issue.record("a connection that never answered ended as a success: \(events)")
        case .threw:
            break
        case let .hung(after):
            Issue.record("hung after \(after)")
        }
    }
}

// MARK: - The buffered requests

/// The same race on the non-streaming path, which carries the health check,
/// the session handshake and the presets.
///
/// With an abrupt close this fails the unfixed client roughly one request in
/// twenty. With the daemon's ordinary half-close it does not, so on its own it
/// is not what users saw; it is what a daemon dying mid-reply would produce —
/// "not listening" for a helper that had in fact just answered.
@Suite("Buffered requests over the socket", .serialized)
struct BufferedRequestTests {
    @Test("a complete reply followed by an immediate close always succeeds")
    func completeReplyThenCloseAlwaysSucceeds() async throws {
        let body = #"{"status":"ok","name":"Starch","version":"0.0.0-test","api_version":"v1","pid":42,"uptime_ms":1}"#
        let reply = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
            + "Content-Length: \(Array(body.utf8).count)\r\n\r\n" + body

        for attempt in 1...400 {
            let daemon = try ScriptedDaemon { peer in
                peer.write(reply)
                peer.close()
            }
            defer { daemon.shutdown() }

            do {
                let health = try await DaemonClient(socketPath: daemon.path, token: "test-token")
                    .health(timeout: 2.0)
                #expect(health.pid == 42, "attempt \(attempt)")
            } catch {
                Issue.record("attempt \(attempt): a complete reply threw \(error)")
                return
            }
        }
    }
}
