import Foundation
import Testing

@testable import StarchKit

// The mirror of the Go-side SSE tests. Both ends of this contract parse the
// same framing, and both have to survive arbitrary read boundaries — the bytes
// arrive from a socket, so a chunk can end anywhere, including inside a
// multi-byte character.

/// Feeds a string through the decoder in fixed-size chunks.
private func decode(_ input: String, chunkSize: Int) -> [String] {
    var decoder = SSEDecoder()
    var payloads: [String] = []
    let bytes = Array(input.utf8)

    var index = 0
    while index < bytes.count {
        let end = min(index + chunkSize, bytes.count)
        payloads += decoder.consume(Data(bytes[index..<end]))
        index = end
    }
    if let trailing = decoder.finish() { payloads.append(trailing) }
    return payloads
}

@Suite("SSE decoding")
struct SSEDecoderTests {
    // One byte at a time puts a boundary between every character, including
    // between the CR and the LF and inside multi-byte runes.
    static let chunkSizes = [1, 2, 3, 7, 64, 65536]

    struct Fixture {
        let name: String
        let input: String
        let want: [String]
    }

    static let fixtures: [Fixture] = [
        Fixture(name: "a single event", input: "data: {\"delta\":\"hi\"}\n\n", want: ["{\"delta\":\"hi\"}"]),
        Fixture(
            name: "two events",
            input: "data: one\n\ndata: two\n\n",
            want: ["one", "two"]
        ),
        Fixture(
            name: "multi-line data is newline-joined",
            input: "data: first\ndata: second\n\n",
            want: ["first\nsecond"]
        ),
        Fixture(name: "comments are ignored", input: ": keep-alive\ndata: real\n\n", want: ["real"]),
        Fixture(name: "CRLF endings", input: "data: payload\r\n\r\n", want: ["payload"]),
        Fixture(name: "no space after the colon", input: "data:payload\n\n", want: ["payload"]),
        Fixture(
            name: "only one leading space is stripped",
            input: "data:  indented\n\n",
            want: [" indented"]
        ),
        Fixture(
            name: "a value containing colons survives",
            input: "data: {\"u\":\"https://x.test:443/y\"}\n\n",
            want: ["{\"u\":\"https://x.test:443/y\"}"]
        ),
        Fixture(
            name: "an event without a trailing blank line is still flushed",
            input: "data: last",
            want: ["last"]
        ),
        Fixture(name: "blank lines between events are skipped", input: "\n\ndata: x\n\n\n", want: ["x"]),
        Fixture(name: "an empty stream yields nothing", input: "", want: []),
        Fixture(name: "unicode survives", input: "data: naïve — 日本語 🎉\n\n", want: ["naïve — 日本語 🎉"]),
        Fixture(name: "event and id fields are ignored", input: "event: x\nid: 1\ndata: p\n\n", want: ["p"]),
    ]

    @Test("parses identically at every chunk size", arguments: fixtures, chunkSizes)
    func chunkIndependence(fixture: Fixture, chunkSize: Int) {
        let got = decode(fixture.input, chunkSize: chunkSize)
        #expect(got == fixture.want, "\(fixture.name) at chunk size \(chunkSize)")
    }

    /// A rewrite of a long selection genuinely produces a delta this size, and
    /// the failure mode of getting it wrong is silent truncation of the tail.
    @Test("handles a payload far larger than any buffer", arguments: [64 * 1024, 512 * 1024])
    func hugePayload(size: Int) {
        let payload = String(repeating: "x", count: size)
        let got = decode("data: \(payload)\n\n", chunkSize: 4096)

        #expect(got.count == 1)
        #expect(got.first?.count == size)
    }

    /// Multi-byte characters split across a read boundary must not be mangled.
    @Test("a rune split across chunks is reassembled")
    func splitRune() {
        let text = "café 日本語 🎉"
        let got = decode("data: \(text)\n\n", chunkSize: 1)
        #expect(got == [text])
    }

    @Test("finish returns nothing when the stream ended cleanly")
    func finishAfterCleanEnd() {
        var decoder = SSEDecoder()
        _ = decoder.consume(Data("data: x\n\n".utf8))
        #expect(decoder.finish() == nil)
    }
}
