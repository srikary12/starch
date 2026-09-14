import Foundation
import Testing

@testable import StarchKit

// The catalog is decoded off the wire, so these fix the shape against
// api/README.md. A key that fails to decode does not crash anything — it
// silently empties a picker, which looks like the daemon being down.

@Suite("Model catalog")
struct CatalogTests {
    private static let json = """
    {
      "source": "builtin",
      "providers": [
        {
          "id": "anthropic",
          "name": "Anthropic",
          "requires_key": true,
          "endpoints": [
            {
              "url": "https://api.anthropic.com",
              "name": "Anthropic",
              "default_model": "claude-sonnet-5",
              "models": [
                {"id": "claude-haiku-4-5", "name": "Claude Haiku 4.5", "note": "Fastest."},
                {
                  "id": "claude-sonnet-5",
                  "name": "Claude Sonnet 5",
                  "thinking": {"levels": ["low", "medium", "high"], "default": "low"}
                }
              ]
            }
          ]
        },
        {
          "id": "openai_compatible",
          "name": "OpenAI-compatible",
          "requires_key": false,
          "endpoints": [
            {
              "url": "http://localhost:11434/v1",
              "name": "Ollama (local)",
              "models": [],
              "note": "Type the model you have pulled."
            }
          ]
        }
      ]
    }
    """

    private func catalog() throws -> ModelCatalog {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return try decoder.decode(ModelCatalog.self, from: Data(Self.json.utf8))
    }

    @Test("snake_case wire keys reach their camelCase properties")
    func wireKeys() throws {
        let anthropic = try #require(try catalog().provider(.anthropic))
        #expect(anthropic.requiresKey)
        #expect(anthropic.endpoints.first?.defaultModel == "claude-sonnet-5")
    }

    /// A model with no reasoning controls must decode to nil rather than to an
    /// empty options object: the window renders no effort control at all in
    /// that case, and an empty picker is not the same thing.
    @Test("a model without thinking decodes to nil, not to empty options")
    func noThinking() throws {
        let models = try catalog().models(provider: .anthropic, baseURL: "https://api.anthropic.com")
        let haiku = try #require(models.first { $0.id == "claude-haiku-4-5" })
        #expect(haiku.thinking == nil)
        #expect(haiku.label == "Claude Haiku 4.5 — Fastest.")
    }

    @Test("thinking levels and the default decode")
    func thinkingDecodes() throws {
        let thinking = try #require(
            try catalog().thinking(
                provider: .anthropic,
                baseURL: "https://api.anthropic.com",
                model: "claude-sonnet-5"
            )
        )
        #expect(thinking.levels == ["low", "medium", "high"])
        // `default` is a keyword, so this one is mapped by hand and is exactly
        // the kind of thing that decodes to "" unnoticed.
        #expect(thinking.defaultLevel == "low")
    }

    /// The stored endpoint is whatever was typed or pasted. A match that is
    /// strict about a trailing slash costs the user their whole model list,
    /// with nothing on screen to explain why.
    @Test("endpoint matching ignores a trailing slash and case", arguments: [
        "https://api.anthropic.com",
        "https://api.anthropic.com/",
        "  https://API.anthropic.com/  ",
    ])
    func endpointMatching(url: String) throws {
        #expect(try catalog().models(provider: .anthropic, baseURL: url).count == 2)
    }

    /// A private gateway is not in the table, and offering it another
    /// endpoint's models would be worse than offering none.
    @Test("an unknown endpoint offers no models")
    func unknownEndpoint() throws {
        #expect(try catalog().models(provider: .anthropic, baseURL: "https://gateway.internal").isEmpty)
    }

    /// But a model the catalog plainly knows keeps its effort control, even
    /// behind a gateway URL the catalog has never seen.
    @Test("thinking is found for a known model on an unknown endpoint")
    func thinkingThroughAGateway() throws {
        let thinking = try catalog().thinking(
            provider: .anthropic,
            baseURL: "https://gateway.internal/anthropic",
            model: "claude-sonnet-5"
        )
        #expect(thinking?.defaultLevel == "low")
    }

    @Test("an unknown model has no thinking controls")
    func unknownModel() throws {
        #expect(
            try catalog().thinking(
                provider: .anthropic,
                baseURL: "https://api.anthropic.com",
                model: "something-typed-by-hand"
            ) == nil
        )
    }

    /// An endpoint whose models cannot be known ahead of time is a legitimate
    /// entry, not a decode failure.
    @Test("a local endpoint decodes with no models and a note")
    func localEndpoint() throws {
        let ollama = try #require(
            try catalog().provider(.openAICompatible)?.endpoint(url: "http://localhost:11434/v1")
        )
        #expect(ollama.models.isEmpty)
        #expect(ollama.defaultModel == nil)
        #expect(ollama.note?.isEmpty == false)
    }

    /// Every lookup runs before the daemon has answered, on the empty catalog.
    @Test("the empty catalog answers every lookup without crashing")
    func emptyIsSafe() {
        let empty = ModelCatalog.empty
        #expect(empty.isEmpty)
        #expect(empty.provider(.gemini) == nil)
        #expect(empty.models(provider: .gemini, baseURL: "https://example.com").isEmpty)
        #expect(empty.thinking(provider: .gemini, baseURL: "https://example.com", model: "m") == nil)
    }

    /// The regression that emptied every picker in the window.
    ///
    /// Go marshals a nil slice as null, so an endpoint whose models cannot be
    /// known ahead of time arrived as `"models": null`. Because the catalog
    /// decodes as a single unit, that one null took the entire table down with
    /// it — not just its own endpoint — so Anthropic and Gemini went empty too.
    /// The fixture above could never catch it: it was written with `[]`, the
    /// shape intended rather than the shape the daemon emitted.
    @Test("a null models array does not take the whole catalog with it")
    func nullModelsDoesNotBreakEverything() throws {
        let json = """
        {"source": "builtin", "providers": [
          {"id": "anthropic", "name": "Anthropic", "requires_key": true, "endpoints": [
            {"url": "https://api.anthropic.com", "name": "Anthropic",
             "models": [{"id": "claude-sonnet-5", "name": "Claude Sonnet 5"}]}]},
          {"id": "openai_compatible", "name": "OpenAI-compatible", "requires_key": false, "endpoints": [
            {"url": "http://localhost:11434/v1", "name": "Ollama (local)", "models": null},
            {"url": "http://localhost:1234/v1", "name": "LM Studio (local)"}]}
        ]}
        """
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        let catalog = try decoder.decode(ModelCatalog.self, from: Data(json.utf8))

        // The unrelated provider's models are the real assertion: those are
        // what went missing, and why the symptom was "every model is empty".
        #expect(catalog.models(provider: .anthropic, baseURL: "https://api.anthropic.com").count == 1)

        // An explicit null and an absent key both mean "none", not a failure.
        #expect(catalog.models(provider: .openAICompatible, baseURL: "http://localhost:11434/v1").isEmpty)
        #expect(catalog.models(provider: .openAICompatible, baseURL: "http://localhost:1234/v1").isEmpty)
    }
}
