import Foundation
import Testing

@testable import StarchKit

// The whole point of the parser is that it does not care how the bytes are
// sliced, so nearly everything here is run at several chunk sizes including
// one byte at a time. `chunkSizes` is the standard sweep.
private let chunkSizes = [1, 2, 3, 7, 64, 8192]

/// Feeds `raw` to a parser in `chunkSize` slices and returns everything it emitted.
private func parse(
    _ raw: String,
    chunkSize: Int,
    closeAtEnd: Bool = false
) throws -> [HTTPResponseEvent] {
    try parse(Data(raw.utf8), chunkSize: chunkSize, closeAtEnd: closeAtEnd)
}

private func parse(
    _ raw: Data,
    chunkSize: Int,
    closeAtEnd: Bool = false
) throws -> [HTTPResponseEvent] {
    var parser = HTTPResponseParser()
    var events: [HTTPResponseEvent] = []

    var offset = 0
    while offset < raw.count {
        let end = min(offset + chunkSize, raw.count)
        events += try parser.append(raw.subdata(in: offset..<end))
        offset = end
        // A parser that has finished must not be fed the next response's bytes.
        if parser.isComplete { break }
    }
    if closeAtEnd {
        events += try parser.finish()
    }
    return events
}

private extension Array where Element == HTTPResponseEvent {
    var head: HTTPResponseHead? {
        for case let .head(head) in self { return head }
        return nil
    }

    /// Body events concatenated. Chunk boundaries are an artefact of how the
    /// bytes arrived, so tests compare the joined body, never the event list.
    var body: Data {
        reduce(into: Data()) { out, event in
            if case let .body(chunk) = event { out.append(chunk) }
        }
    }

    var bodyText: String { String(decoding: body, as: UTF8.self) }

    var endedCleanly: Bool {
        guard case .end = last else { return false }
        return true
    }
}

// MARK: - Requests

@Suite("Request serialization")
struct RequestSerializationTests {
    @Test("GET carries Host, Connection and the caller's headers")
    func getRequest() throws {
        let request = HTTPRequest(
            method: "GET",
            path: "/healthz",
            headers: [HTTPHeader("Authorization", "Bearer abc123")]
        )
        let wire = String(decoding: try request.serialized(), as: UTF8.self)

        #expect(wire.hasPrefix("GET /healthz HTTP/1.1\r\n"))
        #expect(wire.contains("\r\nHost: starchd\r\n"))
        #expect(wire.contains("\r\nConnection: close\r\n"))
        #expect(wire.contains("\r\nAuthorization: Bearer abc123\r\n"))
        #expect(wire.hasSuffix("\r\n\r\n"))
        // No body means no Content-Length at all, not a zero one.
        #expect(!wire.contains("Content-Length"))
    }

    @Test("POST sets Content-Length from the body's byte count")
    func postRequest() throws {
        // Multi-byte characters: the length is bytes, not characters.
        let body = Data(#"{"text":"café ☕"}"#.utf8)
        let request = HTTPRequest(method: "POST", path: "/v1/rewrite", body: body)
        let wire = try request.serialized()
        let text = String(decoding: wire, as: UTF8.self)

        #expect(text.contains("\r\nContent-Length: \(body.count)\r\n"))
        #expect(wire.suffix(body.count) == body)
    }

    @Test("a newline in a header value is refused rather than split the request")
    func headerInjection() throws {
        let request = HTTPRequest(
            method: "GET",
            path: "/healthz",
            headers: [HTTPHeader("Authorization", "Bearer abc\r\nX-Evil: 1")]
        )
        #expect(throws: HTTPFramingError.illegalHeaderCharacter("Authorization")) {
            try request.serialized()
        }
    }

    @Test("a newline in a header name is refused too")
    func headerNameInjection() throws {
        let request = HTTPRequest(
            method: "GET",
            path: "/healthz",
            headers: [HTTPHeader("X-Bad\nName", "value")]
        )
        #expect(throws: (any Error).self) { try request.serialized() }
    }
}

// MARK: - Content-Length responses

@Suite("Content-Length framing")
struct ContentLengthTests {
    private static let response = """
        HTTP/1.1 200 OK\r
        Content-Type: application/json; charset=utf-8\r
        Content-Length: 27\r
        \r
        {"status":"ok","name":"S"}\n
        """

    @Test("parses regardless of how the bytes are sliced", arguments: chunkSizes)
    func splitAnywhere(chunkSize: Int) throws {
        let events = try parse(Self.response, chunkSize: chunkSize)

        let head = try #require(events.head)
        #expect(head.statusCode == 200)
        #expect(head.reasonPhrase == "OK")
        #expect(head.headers.first("content-type") == "application/json; charset=utf-8")
        #expect(events.bodyText == "{\"status\":\"ok\",\"name\":\"S\"}\n")
        #expect(events.endedCleanly)
    }

    @Test("stops at Content-Length and does not read into the next response")
    func doesNotOverread() throws {
        let raw = Self.response + "HTTP/1.1 500 Internal Server Error\r\n\r\n"
        let events = try parse(raw, chunkSize: 8192)

        #expect(events.head?.statusCode == 200)
        #expect(events.bodyText == "{\"status\":\"ok\",\"name\":\"S\"}\n")
        #expect(events.filter { if case .end = $0 { true } else { false } }.count == 1)
    }

    @Test("Content-Length: 0 ends immediately", arguments: chunkSizes)
    func emptyBody(chunkSize: Int) throws {
        let events = try parse("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", chunkSize: chunkSize)

        #expect(events.head?.statusCode == 200)
        #expect(events.body.isEmpty)
        #expect(events.endedCleanly)
    }

    @Test("a body cut short is reported, not silently accepted")
    func truncatedBody() throws {
        var parser = HTTPResponseParser()
        _ = try parser.append(Data("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nonly ten..".utf8))

        #expect(!parser.isComplete)
        #expect(throws: HTTPFramingError.unexpectedEndOfStream) { try parser.finish() }
    }

    @Test("bytes arriving after the response ends are an error")
    func dataAfterEnd() throws {
        var parser = HTTPResponseParser()
        _ = try parser.append(Data("HTTP/1.1 204 No Content\r\n\r\n".utf8))
        #expect(parser.isComplete)
        #expect(throws: HTTPFramingError.dataAfterEnd) {
            try parser.append(Data("HTTP/1.1 200 OK\r\n\r\n".utf8))
        }
    }
}

// MARK: - Chunked responses

@Suite("Chunked framing")
struct ChunkedTests {
    /// Hand-written wire bytes, sizes computed by hand on purpose: this is the
    /// one fixture that would catch the encoder helper below and the parser
    /// being wrong in the same direction. 1a = 26 bytes, 18 = 24 bytes.
    private static let response = """
        HTTP/1.1 200 OK\r
        Content-Type: text/event-stream\r
        Transfer-Encoding: chunked\r
        \r
        1a\r
        data: {"delta":"Thanks "}\n\r
        18\r
        data: {"delta":"again"}\n\r
        0\r
        \r

        """

    /// Encodes `parts` as a chunked body, so tests after this one never have
    /// to compute hex lengths by hand.
    static func chunked(_ parts: [String]) -> String {
        parts.map { String(format: "%x\r\n", $0.utf8.count) + $0 + "\r\n" }.joined() + "0\r\n\r\n"
    }

    @Test("decodes chunks regardless of slicing", arguments: chunkSizes)
    func splitAnywhere(chunkSize: Int) throws {
        let events = try parse(Self.response, chunkSize: chunkSize)

        #expect(events.head?.statusCode == 200)
        #expect(events.bodyText == """
            data: {"delta":"Thanks "}
            data: {"delta":"again"}

            """)
        #expect(events.endedCleanly)
    }

    @Test("many small chunks reassemble in order", arguments: chunkSizes)
    func manyChunks(chunkSize: Int) throws {
        let deltas = (0..<50).map { #"data: {"delta":"tok\#($0) "}"# + "\n" }
        let raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + Self.chunked(deltas)

        let events = try parse(raw, chunkSize: chunkSize)
        #expect(events.bodyText == deltas.joined())
        #expect(events.endedCleanly)
    }

    @Test("chunk extensions are ignored", arguments: chunkSizes)
    func chunkExtensions(chunkSize: Int) throws {
        let raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5;foo=bar\r\nhello\r\n0\r\n\r\n"
        let events = try parse(raw, chunkSize: chunkSize)

        #expect(events.bodyText == "hello")
        #expect(events.endedCleanly)
    }

    @Test("trailer headers after the last chunk are consumed", arguments: chunkSizes)
    func trailers(chunkSize: Int) throws {
        let raw = """
            HTTP/1.1 200 OK\r
            Transfer-Encoding: chunked\r
            \r
            5\r
            hello\r
            0\r
            X-Trailing: yes\r
            \r

            """
        let events = try parse(raw, chunkSize: chunkSize)

        #expect(events.bodyText == "hello")
        #expect(events.endedCleanly)
    }

    @Test("uppercase hex chunk sizes are valid")
    func uppercaseHex() throws {
        let raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nA\r\n0123456789\r\n0\r\n\r\n"
        #expect(try parse(raw, chunkSize: 8192).bodyText == "0123456789")
    }

    @Test("a non-hex chunk size is rejected")
    func badChunkSize() throws {
        let raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nhello\r\n0\r\n\r\n"
        #expect(throws: HTTPFramingError.malformedChunkSize("zz")) {
            try parse(raw, chunkSize: 8192)
        }
    }

    @Test("a transfer coding we do not implement is refused, not guessed at")
    func unsupportedEncoding() throws {
        let raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip\r\n\r\nhello"
        #expect(throws: HTTPFramingError.unsupportedTransferEncoding("gzip")) {
            try parse(raw, chunkSize: 8192)
        }
    }

    @Test("Transfer-Encoding takes precedence over Content-Length")
    func encodingBeatsLength() throws {
        // A request smuggling shape. Whichever we pick must be deliberate.
        let raw = """
            HTTP/1.1 200 OK\r
            Content-Length: 999\r
            Transfer-Encoding: chunked\r
            \r
            5\r
            hello\r
            0\r
            \r

            """
        let events = try parse(raw, chunkSize: 8192)
        #expect(events.bodyText == "hello")
        #expect(events.endedCleanly)
    }
}

// MARK: - The bug the brief calls out

@Suite("Long lines are never truncated")
struct LongLineTests {
    /// Go's bufio.Scanner caps tokens at 64KB and truncates silently past it.
    /// This parser must have no such ceiling on body bytes, because a rewrite
    /// of a long selection arrives as one very long SSE `data:` line.
    @Test("a single 512KB SSE data line survives intact", arguments: [1024, 64 * 1024])
    func hugeSingleLine(chunkSize: Int) throws {
        let payload = String(repeating: "x", count: 512 * 1024)
        let line = #"data: {"delta":"\#(payload)"}"# + "\n"

        var raw = Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n".utf8)
        raw.append(Data(String(format: "%x\r\n", line.utf8.count).utf8))
        raw.append(Data(line.utf8))
        raw.append(Data("\r\n0\r\n\r\n".utf8))

        let events = try parse(raw, chunkSize: chunkSize)

        #expect(events.body.count == line.utf8.count)
        #expect(events.bodyText == line)
        #expect(events.endedCleanly)
    }

    @Test("a 512KB Content-Length body survives intact")
    func hugeContentLength() throws {
        let payload = String(repeating: "y", count: 512 * 1024)
        let raw = "HTTP/1.1 200 OK\r\nContent-Length: \(payload.utf8.count)\r\n\r\n" + payload

        let events = try parse(raw, chunkSize: 4096)
        #expect(events.body.count == payload.utf8.count)
        #expect(events.endedCleanly)
    }
}

// MARK: - Head parsing

@Suite("Header parsing")
struct HeaderTests {
    @Test("header lookup is case-insensitive")
    func caseInsensitive() throws {
        let raw = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 0\r\n\r\n"
        let head = try #require(try parse(raw, chunkSize: 8192).head)

        #expect(head.headers.first("Content-Type") == "text/plain")
        #expect(head.headers.first("content-type") == "text/plain")
        #expect(head.headers.first("CONTENT-TYPE") == "text/plain")
        #expect(head.headers.first("X-Absent") == nil)
    }

    @Test("repeated headers are all preserved")
    func duplicates() throws {
        let raw = "HTTP/1.1 200 OK\r\nSet-Cookie: a=1\r\nSet-Cookie: b=2\r\nContent-Length: 0\r\n\r\n"
        let head = try #require(try parse(raw, chunkSize: 8192).head)

        #expect(head.headers.values("Set-Cookie") == ["a=1", "b=2"])
        #expect(head.headers.first("Set-Cookie") == "a=1")
    }

    @Test("a status line with no reason phrase is valid")
    func noReasonPhrase() throws {
        let head = try #require(try parse("HTTP/1.1 200\r\nContent-Length: 0\r\n\r\n", chunkSize: 8192).head)
        #expect(head.statusCode == 200)
        #expect(head.reasonPhrase == "")
    }

    @Test("a multi-word reason phrase is kept whole")
    func multiWordReason() throws {
        let raw = "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n"
        let head = try #require(try parse(raw, chunkSize: 8192).head)
        #expect(head.statusCode == 405)
        #expect(head.reasonPhrase == "Method Not Allowed")
    }

    @Test("bodyless statuses end without a body", arguments: [204, 304, 100])
    func bodylessStatuses(status: Int) throws {
        // Content-Length is present but must be ignored for these.
        let raw = "HTTP/1.1 \(status) Whatever\r\nContent-Length: 42\r\n\r\n"
        let events = try parse(raw, chunkSize: 8192)

        #expect(events.head?.statusCode == status)
        #expect(events.body.isEmpty)
        #expect(events.endedCleanly)
    }

    @Test("garbage in the status line is rejected", arguments: [
        "NOT-HTTP 200 OK\r\n\r\n",
        "HTTP/1.1 notanumber OK\r\n\r\n",
        "HTTP/1.1\r\n\r\n",
    ])
    func malformedStatusLine(raw: String) throws {
        #expect(throws: (any Error).self) { try parse(raw, chunkSize: 8192) }
    }

    @Test("a header line with no colon is rejected")
    func malformedHeader() throws {
        let raw = "HTTP/1.1 200 OK\r\nthis-is-not-a-header\r\n\r\n"
        #expect(throws: HTTPFramingError.malformedHeader("this-is-not-a-header")) {
            try parse(raw, chunkSize: 8192)
        }
    }

    @Test("an unbounded header section is refused instead of buffered forever")
    func oversizedHeaders() throws {
        var raw = "HTTP/1.1 200 OK\r\n"
        // Never terminated, so the parser would otherwise buffer without limit.
        raw += String(repeating: "X-Pad: \(String(repeating: "p", count: 512))\r\n", count: 200)

        #expect(throws: HTTPFramingError.headerSectionTooLarge(limit: HTTPResponseParser.maxHeaderSectionBytes)) {
            try parse(raw, chunkSize: 8192)
        }
    }
}

// MARK: - Connection-delimited responses

@Suite("Close-delimited framing")
struct CloseDelimitedTests {
    @Test("with no length and no encoding, the body runs to close", arguments: chunkSizes)
    func untilClose(chunkSize: Int) throws {
        let raw = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nbody runs to EOF"
        let events = try parse(raw, chunkSize: chunkSize, closeAtEnd: true)

        #expect(events.head?.statusCode == 200)
        #expect(events.bodyText == "body runs to EOF")
        #expect(events.endedCleanly)
    }

    @Test("finish on an already-complete response emits nothing extra")
    func finishAfterComplete() throws {
        var parser = HTTPResponseParser()
        _ = try parser.append(Data("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi".utf8))
        #expect(parser.isComplete)
        #expect(try parser.finish().isEmpty)
    }

    @Test("finish mid-head is an error, not an empty success")
    func finishMidHead() throws {
        var parser = HTTPResponseParser()
        _ = try parser.append(Data("HTTP/1.1 200 OK\r\nContent-".utf8))
        #expect(throws: HTTPFramingError.unexpectedEndOfStream) { try parser.finish() }
    }
}
