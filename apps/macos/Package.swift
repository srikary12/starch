// swift-tools-version: 6.0
import PackageDescription

// StarchKit holds everything testable without a running app: HTTP framing over
// the Unix socket, the daemon client and its lifecycle, Keychain access and
// hot-key encoding. The Starch executable is the thin AppKit shell on top.
// Splitting them exists so the framing tests can run under `swift test`
// without an app bundle or a window server.
let package = Package(
    name: "Starch",
    platforms: [.macOS(.v13)],
    targets: [
        .target(
            name: "StarchKit",
            swiftSettings: [.swiftLanguageMode(.v6)]
        ),
        .executableTarget(
            name: "Starch",
            dependencies: ["StarchKit"],
            swiftSettings: [.swiftLanguageMode(.v6)]
        ),
        .testTarget(
            name: "StarchKitTests",
            dependencies: ["StarchKit"],
            swiftSettings: [.swiftLanguageMode(.v6)]
        ),
    ]
)
