import Foundation
import Network

// Everything needed to talk to starchd over its Unix domain socket:
// HTTP/1.1 framing, the socket transport, and the typed API on top.
//
// The framing is hand-rolled because URLSession cannot address a Unix socket.
// It is a parser over a byte stream rather than a line reader on purpose: SSE
// `data:` lines carrying a rewrite can be arbitrarily long, and every
// truncating-line-reader bug in this space comes from a fixed cap. There is no
// line length limit on body bytes anywhere below.

// MARK: - Headers

public struct HTTPHeader: Sendable, Equatable {
    public let name: String
    public let value: String

    public init(_ name: String, _ value: String) {
        self.name = name
        self.value = value
    }
}

public struct HTTPHeaders: Sendable, Equatable {
    public private(set) var all: [HTTPHeader]

    public init(_ all: [HTTPHeader] = []) { self.all = all }

    mutating func append(_ header: HTTPHeader) { all.append(header) }

    /// First value for `name`, matched case-insensitively per RFC 9110.
    public func first(_ name: String) -> String? {
        all.first { $0.name.caseInsensitiveCompare(name) == .orderedSame }?.value
    }

    public func values(_ name: String) -> [String] {
        all.filter { $0.name.caseInsensitiveCompare(name) == .orderedSame }.map(\.value)
    }
}

// MARK: - Requests

public struct HTTPRequest: Sendable {
    public var method: String
    public var path: String
    public var headers: [HTTPHeader]
    public var body: Data?

    public init(method: String, path: String, headers: [HTTPHeader] = [], body: Data? = nil) {
        self.method = method
        self.path = path
        self.headers = headers
        self.body = body
    }

    /// Serialises the request wire bytes.
    ///
    /// `Connection: close` is unconditional. One connection per request costs
    /// essentially nothing over a Unix socket — no TLS handshake, no routing —
    /// and it removes a whole class of stale-keep-alive bugs. The keep-alive
    /// that actually matters for latency is the daemon's pooled connection to
    /// the model provider, which is a Go concern.
    public func serialized(host: String = "starchd") throws -> Data {
        var out = Data()
        out.append(Data("\(method) \(path) HTTP/1.1\r\n".utf8))

        var all = [HTTPHeader("Host", host), HTTPHeader("Connection", "close")]
        all.append(contentsOf: headers)
        if let body {
            all.append(HTTPHeader("Content-Length", String(body.count)))
        }

        for header in all {
            // A CR or LF smuggled into a header would let a caller inject a
            // second request. Our values are machine-generated, but validating
            // is one line and the failure mode is severe.
            guard !header.name.contains(where: \.isNewline),
                  !header.value.contains(where: \.isNewline)
            else {
                throw HTTPFramingError.illegalHeaderCharacter(header.name)
            }
            out.append(Data("\(header.name): \(header.value)\r\n".utf8))
        }

        out.append(Data("\r\n".utf8))
        if let body { out.append(body) }
        return out
    }
}

// MARK: - Responses

public struct HTTPResponseHead: Sendable, Equatable {
    public let statusCode: Int
    public let reasonPhrase: String
    public let headers: HTTPHeaders
}

public enum HTTPResponseEvent: Sendable, Equatable {
    case head(HTTPResponseHead)
    case body(Data)
    case end
}

public enum HTTPFramingError: Error, Equatable, LocalizedError {
    case malformedStatusLine(String)
    case malformedHeader(String)
    case malformedChunkSize(String)
    case headerSectionTooLarge(limit: Int)
    case chunkSizeLineTooLong(limit: Int)
    case unsupportedTransferEncoding(String)
    case dataAfterEnd
    case unexpectedEndOfStream
    case illegalHeaderCharacter(String)

    public var errorDescription: String? {
        switch self {
        case let .malformedStatusLine(line): "Malformed HTTP status line: \(line.debugDescription)"
        case let .malformedHeader(line): "Malformed HTTP header: \(line.debugDescription)"
        case let .malformedChunkSize(line): "Malformed chunk size: \(line.debugDescription)"
        case let .headerSectionTooLarge(limit): "HTTP headers exceeded \(limit) bytes"
        case let .chunkSizeLineTooLong(limit): "Chunk size line exceeded \(limit) bytes"
        case let .unsupportedTransferEncoding(v): "Unsupported Transfer-Encoding: \(v)"
        case .dataAfterEnd: "Received data after the response ended"
        case .unexpectedEndOfStream: "Connection closed mid-response"
        case let .illegalHeaderCharacter(name): "Header \(name) contains a newline"
        }
    }
}

/// Incremental HTTP/1.1 response parser.
///
/// Feed it whatever bytes arrive, in whatever sizes they arrive; it emits
/// events as they become complete. Correctness across arbitrary read
/// boundaries is the whole point, so the tests drive it one byte at a time.
public struct HTTPResponseParser: Sendable {
    /// Cap on the status line plus all headers. Bodies are uncapped.
    public static let maxHeaderSectionBytes = 64 * 1024
    /// A chunk size line is a hex number plus an optional extension; anything
    /// near this is malformed or hostile.
    public static let maxChunkSizeLineBytes = 1024

    private enum State: Equatable {
        case head
        case bodyLength(remaining: Int)
        case chunkSize
        case chunkData(remaining: Int)
        case chunkDataTerminator
        case trailers
        case bodyUntilClose
        case done
    }

    private var state: State = .head
    private var buffer: [UInt8] = []
    private var cursor = 0

    public init() {}

    /// True once the response is fully framed.
    public var isComplete: Bool { state == .done }

    /// Consumes `data`, returning any events it completed.
    public mutating func append(_ data: Data) throws -> [HTTPResponseEvent] {
        if state == .done, !data.isEmpty { throw HTTPFramingError.dataAfterEnd }

        buffer.append(contentsOf: data)
        var events: [HTTPResponseEvent] = []

        // Each pass consumes what it can; `false` means the parser needs more
        // bytes before it can make further progress.
        while try step(into: &events) {}

        compact()
        return events
    }

    /// Signals that the peer closed the connection.
    ///
    /// A response delimited by connection close ends here; anything else still
    /// mid-body was truncated.
    public mutating func finish() throws -> [HTTPResponseEvent] {
        switch state {
        case .done:
            return []
        case .bodyUntilClose:
            var events: [HTTPResponseEvent] = []
            if cursor < buffer.count {
                events.append(.body(Data(buffer[cursor...])))
                cursor = buffer.count
            }
            state = .done
            events.append(.end)
            return events
        default:
            throw HTTPFramingError.unexpectedEndOfStream
        }
    }

    private mutating func step(into events: inout [HTTPResponseEvent]) throws -> Bool {
        switch state {
        case .head:
            return try parseHead(into: &events)

        case let .bodyLength(remaining):
            let available = buffer.count - cursor
            guard available > 0 else { return false }
            let take = min(available, remaining)
            events.append(.body(Data(buffer[cursor..<(cursor + take)])))
            cursor += take
            let left = remaining - take
            if left == 0 {
                state = .done
                events.append(.end)
                return false
            }
            state = .bodyLength(remaining: left)
            return true

        case .chunkSize:
            guard let line = try readLine(limit: Self.maxChunkSizeLineBytes,
                                          overflow: { .chunkSizeLineTooLong(limit: $0) })
            else { return false }
            // Strip any chunk extension: "1a;name=value".
            let hex = line.split(separator: ";", maxSplits: 1).first.map(String.init) ?? ""
            guard let size = Int(hex.trimmingCharacters(in: .whitespaces), radix: 16), size >= 0 else {
                throw HTTPFramingError.malformedChunkSize(line)
            }
            state = size == 0 ? .trailers : .chunkData(remaining: size)
            return true

        case let .chunkData(remaining):
            let available = buffer.count - cursor
            guard available > 0 else { return false }
            let take = min(available, remaining)
            events.append(.body(Data(buffer[cursor..<(cursor + take)])))
            cursor += take
            let left = remaining - take
            state = left == 0 ? .chunkDataTerminator : .chunkData(remaining: left)
            return true

        case .chunkDataTerminator:
            // The CRLF that follows every chunk's data.
            guard consumeLine() else { return false }
            state = .chunkSize
            return true

        case .trailers:
            guard let line = try readLine(limit: Self.maxHeaderSectionBytes,
                                          overflow: { .headerSectionTooLarge(limit: $0) })
            else { return false }
            if line.isEmpty {
                state = .done
                events.append(.end)
                return false
            }
            return true // Trailer headers are accepted and discarded.

        case .bodyUntilClose:
            let available = buffer.count - cursor
            guard available > 0 else { return false }
            events.append(.body(Data(buffer[cursor...])))
            cursor = buffer.count
            return false

        case .done:
            return false
        }
    }

    private mutating func parseHead(into events: inout [HTTPResponseEvent]) throws -> Bool {
        // Locate the blank line ending the header section before parsing, so a
        // partially arrived head is left untouched for the next append.
        guard let headEnd = findHeaderSectionEnd() else {
            if buffer.count - cursor > Self.maxHeaderSectionBytes {
                throw HTTPFramingError.headerSectionTooLarge(limit: Self.maxHeaderSectionBytes)
            }
            return false
        }

        let raw = String(decoding: buffer[cursor..<headEnd], as: UTF8.self)
        cursor = headEnd + 4 // past CRLFCRLF

        var lines = raw.components(separatedBy: "\r\n")
        let statusLine = lines.removeFirst()

        // "HTTP/1.1 200 OK" — the reason phrase is optional and may contain spaces.
        let parts = statusLine.split(separator: " ", maxSplits: 2, omittingEmptySubsequences: false)
        guard parts.count >= 2, parts[0].hasPrefix("HTTP/"), let code = Int(parts[1]) else {
            throw HTTPFramingError.malformedStatusLine(statusLine)
        }
        let reason = parts.count == 3 ? String(parts[2]) : ""

        var headers = HTTPHeaders()
        for line in lines where !line.isEmpty {
            guard let colon = line.firstIndex(of: ":") else {
                throw HTTPFramingError.malformedHeader(line)
            }
            let name = String(line[line.startIndex..<colon])
            let value = String(line[line.index(after: colon)...])
                .trimmingCharacters(in: .whitespaces)
            guard !name.isEmpty, !name.contains(" ") else {
                throw HTTPFramingError.malformedHeader(line)
            }
            headers.append(HTTPHeader(name, value))
        }

        events.append(.head(HTTPResponseHead(statusCode: code, reasonPhrase: reason, headers: headers)))
        state = try bodyState(for: code, headers: headers)
        if state == .done { events.append(.end) }
        return true
    }

    private func bodyState(for statusCode: Int, headers: HTTPHeaders) throws -> State {
        // Responses defined never to carry a body, whatever their headers claim.
        if (100..<200).contains(statusCode) || statusCode == 204 || statusCode == 304 {
            return .done
        }

        if let encoding = headers.first("Transfer-Encoding") {
            // Only identity and chunked are plausible from our own daemon;
            // anything else means we are talking to the wrong process.
            let last = encoding.split(separator: ",").last?
                .trimmingCharacters(in: .whitespaces).lowercased() ?? ""
            guard last == "chunked" else {
                throw HTTPFramingError.unsupportedTransferEncoding(encoding)
            }
            return .chunkSize
        }

        if let raw = headers.first("Content-Length") {
            guard let length = Int(raw.trimmingCharacters(in: .whitespaces)), length >= 0 else {
                throw HTTPFramingError.malformedHeader("Content-Length: \(raw)")
            }
            return length == 0 ? .done : .bodyLength(remaining: length)
        }

        return .bodyUntilClose
    }

    /// Index of the CR beginning the CRLFCRLF that ends the header section.
    private func findHeaderSectionEnd() -> Int? {
        guard buffer.count - cursor >= 4 else { return nil }
        var i = cursor
        while i + 3 < buffer.count {
            if buffer[i] == 0x0D, buffer[i + 1] == 0x0A, buffer[i + 2] == 0x0D, buffer[i + 3] == 0x0A {
                return i
            }
            i += 1
        }
        return nil
    }

    /// Reads one CRLF-terminated line, enforcing `limit` on its length.
    private mutating func readLine(
        limit: Int,
        overflow: (Int) -> HTTPFramingError
    ) throws -> String? {
        guard let end = findCRLF(within: limit + 2) else {
            if buffer.count - cursor > limit { throw overflow(limit) }
            return nil
        }
        let line = String(decoding: buffer[cursor..<end], as: UTF8.self)
        cursor = end + 2
        return line
    }

    /// Consumes one CRLF-terminated line, discarding it.
    private mutating func consumeLine() -> Bool {
        guard let end = findCRLF(within: buffer.count - cursor) else { return false }
        cursor = end + 2
        return true
    }

    /// Index of the CR of the first CRLF within `window` bytes of the cursor.
    private func findCRLF(within window: Int) -> Int? {
        let end = min(buffer.count, cursor + max(window, 0))
        guard end > cursor else { return nil }
        var i = cursor
        while i + 1 < end {
            if buffer[i] == 0x0D, buffer[i + 1] == 0x0A { return i }
            i += 1
        }
        return nil
    }

    /// Drops consumed bytes so a long stream does not grow the buffer without
    /// bound. Amortised: pays a copy only once the consumed prefix is sizeable.
    private mutating func compact() {
        guard cursor > 8192 else { return }
        buffer.removeFirst(cursor)
        cursor = 0
    }
}

// MARK: - Unix socket transport

public enum DaemonError: Error, LocalizedError {
    case timedOut
    case unauthorized
    case api(status: Int, code: String, message: String)
    case unexpectedStatus(Int, body: String)
    case transport(String)
    case framing(HTTPFramingError)
    case decoding(String)

    public var errorDescription: String? {
        switch self {
        case .timedOut:
            "The daemon did not respond in time."
        case .unauthorized:
            "The daemon rejected the handshake token."
        case let .api(status, code, message):
            "Daemon error \(status) (\(code)): \(message)"
        case let .unexpectedStatus(status, body):
            "Unexpected daemon response \(status): \(body)"
        case let .transport(detail):
            "Could not reach the daemon: \(detail)"
        case let .framing(error):
            error.errorDescription
        case let .decoding(detail):
            "Could not decode the daemon's response: \(detail)"
        }
    }
}

/// A single HTTP request over a Unix domain socket, using Network.framework.
///
/// One connection per request: see `HTTPRequest.serialized`. Everything
/// mutable is confined to `queue`, which is why the unchecked conformance is
/// safe here.
private final class UnixHTTPExchange: @unchecked Sendable {
    private let queue = DispatchQueue(label: "\(Brand.bundleIdentifier).uds")
    private let connection: NWConnection
    private let request: HTTPRequest
    private let timeout: TimeInterval

    private var parser = HTTPResponseParser()
    private var head: HTTPResponseHead?
    private var body = Data()
    private var continuation: CheckedContinuation<(HTTPResponseHead, Data), Error>?
    private var settled = false
    private var cancelledBeforeStart = false

    init(socketPath: String, request: HTTPRequest, timeout: TimeInterval) {
        self.connection = NWConnection(to: .unix(path: socketPath), using: .tcp)
        self.request = request
        self.timeout = timeout
    }

    func run() async throws -> (HTTPResponseHead, Data) {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                queue.async { self.start(continuation) }
            }
        } onCancel: {
            queue.async { self.cancel() }
        }
    }

    private func start(_ continuation: CheckedContinuation<(HTTPResponseHead, Data), Error>) {
        guard !cancelledBeforeStart else {
            continuation.resume(throwing: CancellationError())
            return
        }
        self.continuation = continuation

        queue.asyncAfter(deadline: .now() + timeout) { [weak self] in
            self?.settle(.failure(DaemonError.timedOut))
        }

        connection.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready:
                self.send()
            case let .failed(error):
                // Network.framework also reports the peer's normal close as a
                // failure on this path; ignore anything after we have settled.
                self.settle(.failure(DaemonError.transport(error.localizedDescription)))
            case let .waiting(error):
                // A Unix socket does not "wait" for a route; this means the
                // socket file is missing or unreadable, so fail fast rather
                // than sitting until the timeout.
                self.settle(.failure(DaemonError.transport(error.localizedDescription)))
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
            settle(.failure(error))
            return
        }
        connection.send(content: bytes, completion: .contentProcessed { [weak self] error in
            guard let self else { return }
            if let error {
                self.settle(.failure(DaemonError.transport(error.localizedDescription)))
                return
            }
            self.receive()
        })
    }

    private func receive() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) {
            [weak self] data, _, isComplete, error in
            guard let self else { return }

            if let error {
                self.settle(.failure(DaemonError.transport(error.localizedDescription)))
                return
            }

            do {
                if let data, !data.isEmpty {
                    try self.consume(self.parser.append(data))
                }
                if self.parser.isComplete {
                    self.finishSuccessfully()
                    return
                }
                if isComplete {
                    try self.consume(self.parser.finish())
                    self.finishSuccessfully()
                    return
                }
            } catch let error as HTTPFramingError {
                self.settle(.failure(DaemonError.framing(error)))
                return
            } catch {
                self.settle(.failure(error))
                return
            }

            self.receive()
        }
    }

    private func consume(_ events: [HTTPResponseEvent]) throws {
        for event in events {
            switch event {
            case let .head(head): self.head = head
            case let .body(chunk): body.append(chunk)
            case .end: break
            }
        }
    }

    private func finishSuccessfully() {
        guard let head else {
            settle(.failure(DaemonError.framing(.unexpectedEndOfStream)))
            return
        }
        settle(.success((head, body)))
    }

    private func cancel() {
        guard continuation != nil else {
            cancelledBeforeStart = true
            return
        }
        settle(.failure(CancellationError()))
    }

    /// Resumes the continuation exactly once and tears the connection down.
    private func settle(_ result: Result<(HTTPResponseHead, Data), Error>) {
        guard !settled else { return }
        settled = true
        connection.stateUpdateHandler = nil
        connection.cancel()
        continuation?.resume(with: result)
        continuation = nil
    }
}

/// Minimal HTTP client bound to one Unix domain socket.
public struct UnixHTTPClient: Sendable {
    public let socketPath: String

    public init(socketPath: String) {
        self.socketPath = socketPath
    }

    public func perform(
        _ request: HTTPRequest,
        timeout: TimeInterval = 2.0
    ) async throws -> (head: HTTPResponseHead, body: Data) {
        let exchange = UnixHTTPExchange(socketPath: socketPath, request: request, timeout: timeout)
        return try await exchange.run()
    }
}

// MARK: - Typed daemon API

/// Body of `GET /healthz`. See api/README.md.
public struct DaemonHealth: Sendable, Equatable, Decodable {
    public let status: String
    public let name: String
    public let version: String
    public let apiVersion: String
    public let pid: Int32
    public let uptimeMs: Int64
}

/// The error envelope every non-2xx daemon response uses.
struct DaemonErrorBody: Decodable {
    struct Payload: Decodable {
        let code: String
        let message: String
    }
    let error: Payload
}

/// Typed client for the `v1` wire contract.
public struct DaemonClient: Sendable {
    /// Contract version this client speaks. A daemon reporting anything else
    /// is a mismatched build and the shell should refuse to drive it.
    public static let apiVersion = "v1"

    public let socketPath: String
    public let token: String

    private let http: UnixHTTPClient

    public init(socketPath: String, token: String) {
        self.socketPath = socketPath
        self.token = token
        self.http = UnixHTTPClient(socketPath: socketPath)
    }

    public func health(timeout: TimeInterval = 2.0) async throws -> DaemonHealth {
        try await get("/healthz", timeout: timeout)
    }

    private func get<T: Decodable>(_ path: String, timeout: TimeInterval) async throws -> T {
        let request = HTTPRequest(
            method: "GET",
            path: path,
            headers: [HTTPHeader("Authorization", "Bearer \(token)")]
        )
        let (head, body) = try await http.perform(request, timeout: timeout)

        guard head.statusCode == 200 else {
            throw Self.error(from: head, body: body)
        }

        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        do {
            return try decoder.decode(T.self, from: body)
        } catch {
            throw DaemonError.decoding(error.localizedDescription)
        }
    }

    private static func error(from head: HTTPResponseHead, body: Data) -> DaemonError {
        if head.statusCode == 401 { return .unauthorized }
        if let envelope = try? JSONDecoder().decode(DaemonErrorBody.self, from: body) {
            return .api(status: head.statusCode, code: envelope.error.code, message: envelope.error.message)
        }
        return .unexpectedStatus(head.statusCode, body: String(decoding: body, as: UTF8.self))
    }
}
