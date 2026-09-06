import Foundation
import os

// MARK: - Brand

/// Mirror of the Go `brand` package. The product name lives in exactly two
/// places per language: `Brand.name` here and `brand.Name` in Go, plus the
/// Go module path.
public enum Brand {
    /// The product name. Change this, `brand.Name` and the go.mod module path
    /// to rename the product.
    public static let name = "Starch"

    /// Lowercase form used in identifiers and file names.
    public static let slug = name.lowercased()

    /// Name of the daemon binary shipped inside the app bundle.
    public static let daemonExecutable = slug + "d"

    /// File name of the Unix domain socket.
    public static let socketName = daemonExecutable + ".sock"

    /// Per-user application support directory name.
    public static let supportDirectoryName = name

    /// Prefix on every environment variable the daemon reads.
    public static let environmentPrefix = slug.uppercased() + "_"

    /// The macOS bundle identifier.
    ///
    /// Frozen after first release: macOS keys Accessibility grants and
    /// Keychain ACLs off this, so changing it silently de-authorises every
    /// existing install. It is duplicated in `Resources/Info.plist`; the app
    /// asserts the two agree at launch in debug builds.
    public static let bundleIdentifier = "dev." + slug + "." + name
}

// MARK: - Paths

/// Filesystem locations the shell and daemon agree on.
public enum Paths {
    /// The portable ceiling on `sun_path`: 104 bytes on Darwin, 108 on Linux,
    /// both including the NUL. The daemon enforces the same limit.
    public static let maxSocketPathLength = 103

    public enum PathError: Error, LocalizedError, Equatable {
        case socketPathTooLong(path: String, length: Int)

        public var errorDescription: String? {
            switch self {
            case let .socketPathTooLong(path, length):
                return """
                    The socket path is \(length) bytes, over the \
                    \(Paths.maxSocketPathLength)-byte limit for Unix domain sockets: \(path)
                    """
            }
        }
    }

    /// `~/Library/Application Support/Starch`, created with mode 0700 if absent.
    ///
    /// 0700 matters: the socket lives here, and denying directory traversal is
    /// what closes the umask race on the socket inode the daemon creates.
    public static func supportDirectory() throws -> URL {
        let base = try FileManager.default.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        let dir = base.appendingPathComponent(Brand.supportDirectoryName, isDirectory: true)

        try FileManager.default.createDirectory(
            at: dir,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        // createDirectory does not reapply attributes to a directory that
        // already exists, including one left looser by an older build.
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: dir.path)

        return dir
    }

    /// Path to the daemon's Unix domain socket.
    public static func socketPath() throws -> String {
        let path = try supportDirectory()
            .appendingPathComponent(Brand.socketName, isDirectory: false)
            .path
        try validate(socketPath: path)
        return path
    }

    /// Rejects a socket path the kernel would refuse, so the failure is a
    /// sentence rather than `bind: invalid argument`.
    public static func validate(socketPath path: String) throws {
        let length = path.utf8.count
        guard length <= maxSocketPathLength else {
            throw PathError.socketPathTooLong(path: path, length: length)
        }
    }

    /// The daemon binary inside the running app bundle.
    ///
    /// Nil in tests and under `swift run`, where there is no bundle; callers
    /// fall back to a development path.
    public static func bundledDaemon(in bundle: Bundle = .main) -> URL? {
        bundle.url(forResource: Brand.daemonExecutable, withExtension: nil)
    }
}

// MARK: - Logging

/// Unified-logging channels.
///
/// These write to the local system log only. There is no telemetry in this
/// product: nothing here, and nothing anywhere else, leaves the machine.
/// Never log user text or any part of an API key — the daemon's stderr is
/// forwarded verbatim into `Log.daemon`, so that rule binds the Go side too.
public enum Log {
    public static let app = Logger(subsystem: Brand.bundleIdentifier, category: "app")
    public static let daemon = Logger(subsystem: Brand.bundleIdentifier, category: "daemon")
    public static let permissions = Logger(subsystem: Brand.bundleIdentifier, category: "permissions")

    /// Text capture and replacement.
    ///
    /// Metadata only — which strategy ran, how long it took, how many
    /// characters. The selected text itself is never written here: it is the
    /// most sensitive thing the app touches, and the unified log outlives the
    /// process and is readable by other tooling.
    public static let capture = Logger(subsystem: Brand.bundleIdentifier, category: "capture")
}
