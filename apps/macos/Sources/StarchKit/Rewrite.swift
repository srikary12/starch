import Foundation
import Network

// The streaming half of the daemon client: hand over credentials, then consume
// a rewrite as it arrives.
//
// The existing exchange buffers a whole response before returning, which is
// right for /healthz and wrong for this — the entire product claim is that you
// see the first token in under half a second.

// MARK: - Server-sent events

/// Incremental decoder for the daemon's event stream.
///
/// The framing is documented in api/README.md §6. It is deliberately a small
/// subset of SSE, because every shell that binds to this contract has to
/// implement it: one JSON object per `data:` line, blank line between events.
///
/// Feed it whatever arrives in whatever sizes; correctness across arbitrary
/// read boundaries is the job, so the tests drive it one byte at a time.
public struct SSEDecoder: Sendable {
    private var pending = Data()
    private var fields: [String] = []

    public init() {}

    /// Consumes bytes and returns the payloads of any events completed by them.
    public mutating func consume(_ data: Data) -> [String] {
        pending.append(data)
        var payloads: [String] = []

        while let newline = pending.firstIndex(of: UInt8(ascii: "\n")) {
            var lineBytes = pending[pending.startIndex..<newline]
            // Tolerate CRLF as well as bare LF.
            if lineBytes.last == UInt8(ascii: "\r") {
                lineBytes = lineBytes.dropLast()
            }
            pending = pending[pending.index(after: newline)...]

            let line = String(decoding: lineBytes, as: UTF8.self)

            if line.isEmpty {
                // Blank line dispatches, but only if something was collected;
                // leading blanks are just separators.
                if !fields.isEmpty {
                    payloads.append(fields.joined(separator: "\n"))
                    fields.removeAll(keepingCapacity: true)
                }
            } else if line.hasPrefix(":") {
                // Comment or keep-alive.
            } else if let value = Self.value(ofField: "data", in: line) {
                fields.append(value)
            }
            // Other fields (event, id, retry) are unused by this contract.
        }

        // Compact so a long stream does not keep re-slicing from a growing base.
        if pending.isEmpty { pending = Data() }
        return payloads
    }

    /// Flushes an event the stream ended without a trailing blank line.
    ///
    /// Two things can be outstanding, and missing either loses the terminal
    /// event — which is the one carrying `full` and the token counts. A final
    /// line may still be unparsed because it had no newline after it, and
    /// parsed fields may not have been dispatched because no blank line
    /// followed.
    public mutating func finish() -> String? {
        if !pending.isEmpty {
            var lineBytes = pending[...]
            if lineBytes.last == UInt8(ascii: "\r") { lineBytes = lineBytes.dropLast() }
            pending = Data()

            let line = String(decoding: lineBytes, as: UTF8.self)
            if !line.hasPrefix(":"), let value = Self.value(ofField: "data", in: line) {
                fields.append(value)
            }
        }

        guard !fields.isEmpty else { return nil }
        defer { fields.removeAll() }
        return fields.joined(separator: "\n")
    }

    private static func value(ofField name: String, in line: String) -> String? {
        guard line.hasPrefix(name) else { return nil }
        let rest = line.dropFirst(name.count)
        guard rest.first == ":" else { return nil }
        var value = rest.dropFirst()
        // Exactly one leading space is stripped, per the specification.
        if value.first == " " { value = value.dropFirst() }
        return String(value)
    }
}

// MARK: - Events

public struct RewriteUsage: Sendable, Equatable, Decodable {
    public let inputTokens: Int
    public let outputTokens: Int
}

/// One event from `POST /v1/rewrite`.
public enum RewriteEvent: Sendable, Equatable {
    case delta(String)
    /// The terminal event. `full` is authoritative — prefer it over a local
    /// concatenation of deltas, since the daemon cleans up wrappers the model
    /// added and restores the selection's own whitespace.
    case done(full: String, usage: RewriteUsage?)
    /// An error that arrived after the stream had already returned 200. The
    /// status code cannot carry these, so clients must handle them in-band.
    case failed(code: String, message: String)
}

/// Wire shape of one event. Every field is optional because the three event
/// kinds share one envelope.
private struct RewriteFrame: Decodable {
    struct Failure: Decodable {
        let code: String
        let message: String
    }
    let delta: String?
    let done: Bool?
    let full: String?
    let usage: RewriteUsage?
    let error: Failure?
}

// MARK: - Session

public struct SessionRequest: Sendable, Encodable {
    public let provider: String
    public let model: String
    public let baseURL: String
    public let apiKey: String
    /// Thinking level, in the provider's own vocabulary. Empty leaves the
    /// endpoint's default alone.
    public let thinkingEffort: String

    public init(
        provider: String,
        model: String,
        baseURL: String,
        apiKey: String,
        thinkingEffort: String = ""
    ) {
        self.provider = provider
        self.model = model
        self.baseURL = baseURL
        self.apiKey = apiKey
        self.thinkingEffort = thinkingEffort
    }

    enum CodingKeys: String, CodingKey {
        case provider, model
        case baseURL = "base_url"
        case apiKey = "api_key"
        case thinkingEffort = "thinking_effort"
    }
}

public struct SessionResponse: Sendable, Equatable, Decodable {
    public let ok: Bool
    public let provider: String
    public let model: String
    public let endpoint: String
}

/// The daemon's view of the preset file. See api/README.md.
public struct PresetSet: Sendable, Equatable, Decodable {
    public let presets: [Preset]
    /// Where presets.json lives, so the shell can offer to open it rather than
    /// making people hunt for a path.
    public let path: String
    /// "file" or "defaults".
    public let source: String
    /// Why the file was not used, when it exists but could not be read.
    public let problem: String?

    public var usingDefaults: Bool { source == "defaults" }
}

extension Preset: Decodable {
    // Only id and name are decoded. The instruction is the daemon's business:
    // a shell that carried prompt text would be a second place for it to drift.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            id: try container.decode(String.self, forKey: .id),
            name: try container.decode(String.self, forKey: .name)
        )
    }

    private enum CodingKeys: String, CodingKey { case id, name }
}

extension DaemonClient {
    /// Fetches the active presets.
    ///
    /// Fetched rather than hard-coded so that editing presets.json shows up in
    /// the overlay and in Tab cycling without the app being rebuilt.
    public func presets(timeout: TimeInterval = 3.0) async throws -> PresetSet {
        let request = HTTPRequest(
            method: "GET",
            path: "/\(Self.apiVersion)/presets",
            headers: [HTTPHeader("Authorization", "Bearer \(token)")]
        )
        let (head, body) = try await UnixHTTPClient(socketPath: socketPath)
            .perform(request, timeout: timeout)

        guard head.statusCode == 200 else {
            throw Self.daemonError(from: head, body: body)
        }
        do {
            return try JSONDecoder().decode(PresetSet.self, from: body)
        } catch {
            throw DaemonError.decoding(error.localizedDescription)
        }
    }

    /// Hands the provider configuration and API key to the daemon.
    ///
    /// This is the only place the key crosses a process boundary. It goes over
    /// the 0600 socket to a daemon that holds it in memory and never persists
    /// it; the shell reads it from the Keychain each time rather than caching
    /// it in a property.
    public func startSession(_ request: SessionRequest, timeout: TimeInterval = 5.0) async throws -> SessionResponse {
        let body = try JSONEncoder().encode(request)
        let httpRequest = HTTPRequest(
            method: "POST",
            path: "/\(Self.apiVersion)/session",
            headers: [
                HTTPHeader("Authorization", "Bearer \(token)"),
                HTTPHeader("Content-Type", "application/json"),
            ],
            body: body
        )

        let (head, responseBody) = try await UnixHTTPClient(socketPath: socketPath)
            .perform(httpRequest, timeout: timeout)

        guard head.statusCode == 200 else {
            throw Self.daemonError(from: head, body: responseBody)
        }
        do {
            return try JSONDecoder().decode(SessionResponse.self, from: responseBody)
        } catch {
            throw DaemonError.decoding(error.localizedDescription)
        }
    }

    /// Streams a rewrite.
    ///
    /// Cancelling the consuming task tears down the socket, which the daemon
    /// sees as a disconnect and turns into cancellation of the upstream model
    /// call. That chain is what makes Escape stop the generation being billed
    /// rather than merely stop it being displayed.
    public func rewrite(
        text: String,
        preset: String,
        hint: String? = nil
    ) -> AsyncThrowingStream<RewriteEvent, Error> {
        AsyncThrowingStream { continuation in
            var payload: [String: String] = ["text": text, "preset": preset]
            if let hint, !hint.isEmpty { payload["hint"] = hint }

            let body: Data
            do {
                body = try JSONSerialization.data(withJSONObject: payload)
            } catch {
                continuation.finish(throwing: DaemonError.decoding("Could not encode the request."))
                return
            }

            let request = HTTPRequest(
                method: "POST",
                path: "/\(Self.apiVersion)/rewrite",
                headers: [
                    HTTPHeader("Authorization", "Bearer \(token)"),
                    HTTPHeader("Content-Type", "application/json"),
                    HTTPHeader("Accept", "text/event-stream"),
                ],
                body: body
            )

            let exchange = StreamingExchange(
                socketPath: socketPath,
                request: request,
                continuation: continuation
            )
            continuation.onTermination = { _ in exchange.cancel() }
            exchange.start()
        }
    }

    /// Exposed for the streaming path, which cannot reach the private one.
    static func daemonError(from head: HTTPResponseHead, body: Data) -> DaemonError {
        if head.statusCode == 401 { return .unauthorized }
        if let envelope = try? JSONDecoder().decode(DaemonErrorBody.self, from: body) {
            return .api(status: head.statusCode, code: envelope.error.code, message: envelope.error.message)
        }
        return .unexpectedStatus(head.statusCode, body: String(decoding: body, as: UTF8.self))
    }
}

// MARK: - Streaming transport

/// One streaming HTTP exchange over the Unix socket.
///
/// Mirrors UnixHTTPExchange, but yields body bytes as they arrive instead of
/// buffering to completion. Everything mutable is confined to `queue`, which
/// is what makes the unchecked conformance sound.
private final class StreamingExchange: @unchecked Sendable {
    private let queue = DispatchQueue(label: "\(Brand.bundleIdentifier).uds.stream")
    private let connection: NWConnection
    private let request: HTTPRequest
    private let continuation: AsyncThrowingStream<RewriteEvent, Error>.Continuation

    private var parser = HTTPResponseParser()
    private var decoder = SSEDecoder()
    private var head: HTTPResponseHead?
    /// Only accumulated for a non-2xx reply, whose body is a small error
    /// envelope. A successful stream is never buffered.
    private var errorBody = Data()
    private var finished = false
    /// Set once the request is sent and a reply is being read. From then on
    /// the receive loop decides how the exchange ends; see socketCloseGrace.
    private var receiving = false

    init(
        socketPath: String,
        request: HTTPRequest,
        continuation: AsyncThrowingStream<RewriteEvent, Error>.Continuation
    ) {
        self.connection = NWConnection(to: .unix(path: socketPath), using: .tcp)
        self.request = request
        self.continuation = continuation
    }

    func start() {
        queue.async { self.begin() }
    }

    func cancel() {
        queue.async {
            guard !self.finished else { return }
            self.finished = true
            self.connection.cancel()
            self.continuation.finish()
        }
    }

    private func begin() {
        connection.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready:
                self.send()
            case let .failed(error):
                self.connectionFailed(error)
            case let .waiting(error):
                // A Unix socket does not wait for a route to appear. Without
                // this the connection sits in .waiting indefinitely and, since
                // this exchange has no timeout of its own, the overlay spins
                // forever with no error.
                self.fail(.transport(describeSocketError(error)))
            case .cancelled:
                self.complete()
            default:
                break
            }
        }
        connection.start(queue: queue)
    }

    private func send() {
        let bytes: Data
        do {
            bytes = try request.serialized()
        } catch {
            fail(.transport(error.localizedDescription))
            return
        }
        connection.send(content: bytes, completion: .contentProcessed { [weak self] error in
            guard let self else { return }
            if let error {
                self.fail(.transport(describeSocketError(error)))
                return
            }
            self.receiving = true
            self.receive()
        })
    }

    /// The connection reported `.failed`.
    ///
    /// Before a reply is being read, the helper genuinely could not be reached.
    /// After, this is almost always the daemon closing the socket as it does
    /// after every rewrite, reported early: Network.framework surfaces that
    /// ordinary close as `.failed(ENETDOWN)`, sometimes before the receive
    /// carrying the last events has run. Failing here is what turned finished
    /// rewrites into "the helper is not listening". So the receive loop is left
    /// to finish the job, and this is only the bound on waiting for it.
    private func connectionFailed(_ error: NWError) {
        guard !finished else { return }
        guard receiving else {
            fail(.transport(describeSocketError(error)))
            return
        }
        queue.asyncAfter(deadline: .now() + socketCloseGrace) { [weak self] in
            guard let self, !self.finished else { return }
            if self.head == nil {
                self.fail(.transport(describeSocketError(error)))
            } else {
                self.finishStream()
            }
        }
    }

    private func receive() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) {
            [weak self] data, _, isComplete, error in
            guard let self else { return }

            // Data before the error. Network.framework can deliver the last
            // bytes of a response and the error that ended the connection in
            // the same callback, and checking the error first throws away the
            // very bytes that might have carried the terminal event.
            if let data, !data.isEmpty {
                self.ingest(data)
            }
            // A terminal event ends the exchange by itself, and whatever the
            // connection reports after that is not about this rewrite.
            if self.finished { return }

            if let error {
                // Network.framework reports a peer's ordinary close on a Unix
                // socket as an error, so what this means depends on how far the
                // response got. Before any reply, the helper was not there to
                // answer. After one began, it plainly was, and the response has
                // simply ended — which finishStream turns into the daemon's own
                // error or an incomplete rewrite, rather than "not listening",
                // which was false and sent people looking for a dead helper.
                if self.head == nil {
                    self.fail(.transport(describeSocketError(error)))
                } else {
                    self.finishStream()
                }
                return
            }
            if isComplete {
                self.drainParser()
                self.finishStream()
                return
            }
            guard !self.finished else { return }
            self.receive()
        }
    }

    private func ingest(_ data: Data) {
        guard !finished else { return }

        let events: [HTTPResponseEvent]
        do {
            events = try parser.append(data)
        } catch let error as HTTPFramingError {
            fail(.framing(error))
            return
        } catch {
            fail(.transport(error.localizedDescription))
            return
        }

        for event in events {
            // A terminal event part-way through this batch has already ended
            // the exchange; the rest of the batch belongs to nothing.
            if finished { return }
            switch event {
            case let .head(h):
                head = h
                // A non-2xx means the daemon rejected this before streaming —
                // a missing session, a bad key. Its body is the JSON envelope.
                if h.statusCode != 200 { continue }

            case let .body(bytes):
                if let head, head.statusCode != 200 {
                    errorBody.append(bytes)
                    continue
                }
                for payload in decoder.consume(bytes) {
                    emit(payload)
                }

            case .end:
                finishStream()
                return
            }
        }
    }

    /// Flushes any response the connection closed without terminating.
    private func drainParser() {
        guard !finished, let events = try? parser.finish() else { return }
        for case let .body(bytes) in events {
            if let head, head.statusCode != 200 {
                errorBody.append(bytes)
            } else {
                for payload in decoder.consume(bytes) { emit(payload) }
            }
        }
    }

    private func emit(_ payload: String) {
        guard !finished, let data = payload.data(using: .utf8) else { return }

        let decoderJSON = JSONDecoder()
        decoderJSON.keyDecodingStrategy = .convertFromSnakeCase
        guard let frame = try? decoderJSON.decode(RewriteFrame.self, from: data) else {
            // A frame we cannot read costs that frame, not the rewrite.
            return
        }

        // Both terminal events end the exchange here, not when the connection
        // closes. The contract says so — exactly one terminal event, and an
        // in-band error "ends the stream" — and waiting for the close instead
        // made the outcome of a finished rewrite depend on how the socket
        // happened to be torn down. When the close surfaced as an error, a
        // rewrite the user had watched complete was reported as a dead helper,
        // and the app re-ran the whole thing.
        if let failure = frame.error {
            continuation.yield(.failed(code: failure.code, message: failure.message))
            complete()
            return
        }
        if frame.done == true {
            continuation.yield(.done(full: frame.full ?? "", usage: frame.usage))
            complete()
            return
        }
        if let delta = frame.delta, !delta.isEmpty {
            continuation.yield(.delta(delta))
        }
    }

    /// The response has ended, one way or another.
    ///
    /// Reaching here with the exchange still open means no terminal event
    /// arrived, and that is never a success. It used to be treated as one: a
    /// daemon that died part-way through ended the stream quietly, and the
    /// overlay was left showing half a sentence as though it were still
    /// arriving, with nothing to say why.
    private func finishStream() {
        guard !finished else { return }

        guard let head else {
            // Hung up without answering at all — most likely a daemon that was
            // restarting. A transport error, so the app's retry can cover it:
            // nothing has been shown yet, so nothing would be repeated.
            fail(.transport("the helper closed the connection without answering"))
            return
        }
        if head.statusCode != 200 {
            finished = true
            connection.cancel()
            continuation.finish(throwing: DaemonClient.daemonError(from: head, body: errorBody))
            return
        }
        if let trailing = decoder.finish() {
            emit(trailing)
        }
        // A trailing terminal event would have completed the exchange.
        fail(.incompleteStream)
    }

    private func complete() {
        guard !finished else { return }
        finished = true
        connection.cancel()
        continuation.finish()
    }

    private func fail(_ error: DaemonError) {
        guard !finished else { return }
        finished = true
        connection.cancel()
        continuation.finish(throwing: error)
    }
}
