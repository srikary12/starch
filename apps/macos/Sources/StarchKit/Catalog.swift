import Foundation

// The provider and model catalog, as served by GET /v1/models.
//
// The daemon owns this table so that a Windows or Linux shell does not have to
// carry its own copy and drift. Nothing here decides what is in it; this is a
// decoder plus the two lookups a settings window needs.
//
// It is curated, not live, which has one consequence the UI must honour: a
// model released after this build has to remain reachable by typing its name.
// The list is a convenience, never a whitelist.

public struct ThinkingOptions: Sendable, Equatable, Decodable {
    /// The levels this model accepts, cheapest first.
    public let levels: [String]
    /// What to preselect — `low` nearly everywhere, because a rewrite is not a
    /// reasoning task and most current models think at medium or high unless
    /// told otherwise.
    public let defaultLevel: String

    public init(levels: [String], defaultLevel: String) {
        self.levels = levels
        self.defaultLevel = defaultLevel
    }

    // `default` is a Swift keyword, so the wire name cannot be the property
    // name here the way it is everywhere else.
    private enum CodingKeys: String, CodingKey {
        case levels
        case defaultLevel = "default"
    }
}

public struct CatalogModel: Sendable, Equatable, Decodable, Identifiable {
    public let id: String
    public let name: String
    /// A short qualifier for the picker, where the daemon supplied one.
    public let note: String?
    /// Absent when the model has no reasoning controls. A shell shows no
    /// effort control at all in that case, rather than a disabled one.
    public let thinking: ThinkingOptions?

    public init(id: String, name: String, note: String? = nil, thinking: ThinkingOptions? = nil) {
        self.id = id
        self.name = name
        self.note = note
        self.thinking = thinking
    }

    /// What a picker row reads as: "Claude Haiku 4.5 — Fastest."
    public var label: String {
        guard let note, !note.isEmpty else { return name }
        return "\(name) — \(note)"
    }
}

public struct CatalogEndpoint: Sendable, Equatable, Decodable {
    public let url: String
    public let name: String
    /// Preselected when this endpoint is chosen. Absent where the models
    /// cannot be known ahead of time, such as a local Ollama.
    public let defaultModel: String?
    public let models: [CatalogModel]
    /// Explains an empty model list, or a rolling alias.
    public let note: String?

    public init(
        url: String,
        name: String,
        defaultModel: String? = nil,
        models: [CatalogModel] = [],
        note: String? = nil
    ) {
        self.url = url
        self.name = name
        self.defaultModel = defaultModel
        self.models = models
        self.note = note
    }

    // Declaring init(from:) by hand stops the compiler synthesising these, so
    // they have to be spelled out. With .convertFromSnakeCase the wire's
    // "default_model" arrives already camel-cased, and matches.
    private enum CodingKeys: String, CodingKey {
        case url, name, defaultModel, models, note
    }

    /// Decoded by hand for one reason: `models` must tolerate a null.
    ///
    /// Go marshals a nil slice as `null` rather than `[]`, so an endpoint whose
    /// models cannot be known ahead of time — a local Ollama — arrives as
    /// `"models": null`. A strict decode threw on it, and because the catalog
    /// decodes as a single unit that took down the *whole* table rather than
    /// the one endpoint: every picker in the window went empty, for every
    /// provider. The daemon now emits `[]`, and this tolerates both, because
    /// one side being careful is not the same as the pair being safe.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        url = try container.decode(String.self, forKey: .url)
        name = try container.decode(String.self, forKey: .name)
        defaultModel = try container.decodeIfPresent(String.self, forKey: .defaultModel)
        note = try container.decodeIfPresent(String.self, forKey: .note)
        // decodeIfPresent returns nil for an explicit null as well as for a
        // missing key, which is exactly what is wanted here.
        models = try container.decodeIfPresent([CatalogModel].self, forKey: .models) ?? []
    }

    public func model(id: String) -> CatalogModel? {
        models.first { $0.id == id }
    }
}

public struct CatalogProvider: Sendable, Equatable, Decodable {
    public let id: String
    public let name: String
    public let requiresKey: Bool
    public let endpoints: [CatalogEndpoint]

    public init(id: String, name: String, requiresKey: Bool, endpoints: [CatalogEndpoint]) {
        self.id = id
        self.name = name
        self.requiresKey = requiresKey
        self.endpoints = endpoints
    }

    /// The entry for a base URL, ignoring a trailing slash and case.
    ///
    /// Tolerant on purpose: the stored preference is whatever was typed or
    /// pasted, and an endpoint that fails to match here silently costs the
    /// user their model list.
    public func endpoint(url: String) -> CatalogEndpoint? {
        let wanted = Self.normalized(url)
        return endpoints.first { Self.normalized($0.url) == wanted }
    }

    static func normalized(_ url: String) -> String {
        var s = url.trimmingCharacters(in: .whitespaces).lowercased()
        while s.hasSuffix("/") { s.removeLast() }
        return s
    }
}

public struct ModelCatalog: Sendable, Equatable, Decodable {
    /// `builtin` today. Present so a future live-listing mode is an added
    /// value rather than a contract change.
    public let source: String
    public let providers: [CatalogProvider]

    public init(source: String, providers: [CatalogProvider]) {
        self.source = source
        self.providers = providers
    }

    /// What a shell has before the daemon has answered. Every picker treats an
    /// empty catalog as "no suggestions", not as an error.
    public static let empty = ModelCatalog(source: "", providers: [])

    public var isEmpty: Bool { providers.isEmpty }

    public func provider(_ id: ProviderID) -> CatalogProvider? {
        providers.first { $0.id == id.rawValue }
    }

    /// The models to offer for a provider and endpoint.
    ///
    /// Empty for an endpoint the catalog does not know — a private gateway, a
    /// local server — which is the honest answer rather than offering another
    /// endpoint's models.
    public func models(provider: ProviderID, baseURL: String) -> [CatalogModel] {
        self.provider(provider)?.endpoint(url: baseURL)?.models ?? []
    }

    /// The thinking controls for one selection, or nil if there are none.
    ///
    /// Falls back to any endpoint of the same provider that serves the model,
    /// so pointing at a private Anthropic gateway does not lose the effort
    /// control for a model the catalog plainly knows.
    public func thinking(provider: ProviderID, baseURL: String, model: String) -> ThinkingOptions? {
        guard let provider = self.provider(provider) else { return nil }
        if let exact = provider.endpoint(url: baseURL)?.model(id: model) {
            return exact.thinking
        }
        for endpoint in provider.endpoints {
            if let found = endpoint.model(id: model) { return found.thinking }
        }
        return nil
    }
}

extension DaemonClient {
    /// Fetches the provider and model catalog.
    ///
    /// Needs no session and no API key: the settings window is most often open
    /// precisely because there is no key yet.
    public func models(timeout: TimeInterval = 3.0) async throws -> ModelCatalog {
        let request = HTTPRequest(
            method: "GET",
            path: "/\(Self.apiVersion)/models",
            headers: [HTTPHeader("Authorization", "Bearer \(token)")]
        )
        let (head, body) = try await UnixHTTPClient(socketPath: socketPath)
            .perform(request, timeout: timeout)

        guard head.statusCode == 200 else {
            throw Self.daemonError(from: head, body: body)
        }
        do {
            let decoder = JSONDecoder()
            decoder.keyDecodingStrategy = .convertFromSnakeCase
            return try decoder.decode(ModelCatalog.self, from: body)
        } catch {
            throw DaemonError.decoding(error.localizedDescription)
        }
    }
}
