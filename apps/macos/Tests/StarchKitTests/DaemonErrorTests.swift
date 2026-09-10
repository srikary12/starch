import Foundation
import Testing

@testable import StarchKit

// The daemon returns 401 for two unrelated things: our handshake token being
// wrong, and the user's API key being rejected upstream. They need opposite
// responses from the user — restart the helper, versus go and fix your key —
// so the client must tell them apart. It did not, and every rejected API key
// surfaced as "The helper rejected the handshake. Try Restart Helper", which
// sends people to restart a process that was working perfectly.

private func head(_ status: Int) -> HTTPResponseHead {
    HTTPResponseHead(statusCode: status, reasonPhrase: "", headers: HTTPHeaders())
}

private func envelope(code: String, message: String) -> Data {
    Data(#"{"error":{"code":"\#(code)","message":"\#(message)"}}"#.utf8)
}

@Suite("Daemon error classification")
struct DaemonErrorTests {
    @Test("a 401 from the handshake is the helper's problem")
    func handshakeRejection() {
        let error = DaemonClient.error(
            from: head(401),
            body: envelope(code: "unauthorized", message: "Unauthorized.")
        )
        guard case .unauthorized = error else {
            Issue.record("got \(error), want .unauthorized")
            return
        }
    }

    @Test("a 401 from the provider carries the daemon's own wording through")
    func rejectedAPIKey() {
        let error = DaemonClient.error(
            from: head(401),
            body: envelope(
                code: "provider_auth",
                message: "Your API key was rejected. Check it in Settings."
            )
        )
        guard case let .api(status, code, message) = error else {
            Issue.record("got \(error), want .api — a rejected key is not a handshake failure")
            return
        }
        #expect(status == 401)
        #expect(code == "provider_auth")
        // The daemon phrases these for a person, and that phrasing is the
        // whole value: it says what is wrong and where to go and fix it.
        #expect(message == "Your API key was rejected. Check it in Settings.")
    }

    @Test("a 401 with no envelope is still treated as the handshake")
    func bareUnauthorized() {
        // Every 401 the daemon itself produces carries an envelope, so a bare
        // one means something else answered — assume the transport, not a key.
        let error = DaemonClient.error(from: head(401), body: Data())
        guard case .unauthorized = error else {
            Issue.record("got \(error), want .unauthorized")
            return
        }
    }

    @Test("other statuses keep their envelope", arguments: [400, 404, 429, 502])
    func otherStatusesPassThrough(status: Int) {
        let error = DaemonClient.error(
            from: head(status),
            body: envelope(code: "model_not_found", message: "That model was not recognised.")
        )
        guard case let .api(gotStatus, code, _) = error else {
            Issue.record("got \(error), want .api")
            return
        }
        #expect(gotStatus == status)
        #expect(code == "model_not_found")
    }

    @Test("a body that is not an envelope is reported verbatim")
    func unparseableBody() {
        let error = DaemonClient.error(from: head(500), body: Data("upstream exploded".utf8))
        guard case let .unexpectedStatus(status, body) = error else {
            Issue.record("got \(error), want .unexpectedStatus")
            return
        }
        #expect(status == 500)
        #expect(body == "upstream exploded")
    }
}
