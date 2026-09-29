import Foundation
import Testing

@testable import StarchKit

@Suite("Durations as milliseconds")
struct TimingTests {
    /// The case that was wrong. Everything at or over a second lost its whole
    /// seconds, so the app's latency log reported a 6s rewrite as 52ms.
    @Test("whole seconds are counted", arguments: [
        (Duration.milliseconds(6_052), 6_052.0),
        (Duration.milliseconds(2_092), 2_092.0),
        (Duration.seconds(1), 1_000.0),
        (Duration.seconds(90), 90_000.0),
    ])
    func wholeSecondsAreCounted(duration: Duration, want: Double) {
        #expect(abs(duration.milliseconds - want) < 0.001)
    }

    /// And below a second, which the old code happened to get right.
    @Test("fractions of a second are kept", arguments: [
        (Duration.milliseconds(0), 0.0),
        (Duration.milliseconds(52), 52.0),
        (Duration.microseconds(1_500), 1.5),
        (Duration.milliseconds(999), 999.0),
    ])
    func fractionsAreKept(duration: Duration, want: Double) {
        #expect(abs(duration.milliseconds - want) < 0.001)
    }
}
