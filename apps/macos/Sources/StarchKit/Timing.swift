import Foundation

extension Duration {
    /// The whole duration, in milliseconds.
    ///
    /// Not `components.attoseconds / 1e15`, which is what this replaced in
    /// three places. `components` splits a Duration into whole *seconds* and
    /// the attoseconds *below* one second, so reading only the second half
    /// silently drops every whole second. A rewrite whose first token took
    /// 6,049ms was logged as 52ms, and every latency line the app has written
    /// claimed the 500ms budget was being met while it was being missed by
    /// several times over.
    public var milliseconds: Double {
        let (seconds, attoseconds) = components
        return Double(seconds) * 1_000 + Double(attoseconds) / 1e15
    }
}
