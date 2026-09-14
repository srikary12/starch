// Package catalog is the curated list of providers, endpoints and models the
// shells offer in their pickers.
//
// It lives in the daemon rather than in each shell for the same reason the
// prompt text does: a macOS shell, a Windows shell and a Linux shell must not
// each carry their own copy of this table and drift apart. A shell reads it
// from GET /v1/models and renders it; it decides nothing about what is in here.
//
// It is a hand-maintained table, not a live query. Nothing in this package
// touches the network or the disk, so the pickers populate before a key is set,
// with no endpoint reachable, and offline. That is the trade: the list is
// instant and always present, and in exchange it has to be updated when a
// provider ships a model. Every shell must therefore keep its model field
// typeable — the list is a convenience, never a whitelist. A model released
// after someone's build has to remain reachable by typing its name.
//
// The other reason this is curated: thinking. Which models take an effort
// setting, and which levels each one accepts, is not something any provider's
// /models endpoint reports uniformly — Google's says a model thinks but not at
// which levels, and OpenAI's says nothing at all. That metadata only exists
// here, and it is what lets a shell show an effort control that cannot be set
// to something the endpoint will reject.
package catalog

// Source names where a catalog came from. Only "builtin" exists today; the
// field is here so that a future live-listing mode is an added value rather
// than a contract change.
const Source = "builtin"

// Effort is a thinking level. The values are the providers' own vocabulary,
// deliberately unmapped: each provider's field takes the string verbatim, so
// there is no translation table here to fall out of date. Which of them a
// given model actually accepts is what Thinking.Levels records.
type Effort string

const (
	// EffortNone disables reasoning outright. OpenAI-compatible only.
	EffortNone Effort = "none"
	// EffortMinimal is Google's floor, below "low".
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

// Thinking describes a model's reasoning controls.
type Thinking struct {
	// Levels are the values this model accepts, cheapest first.
	Levels []Effort `json:"levels"`
	// Default is what a shell should preselect. It is "low" nearly everywhere
	// on purpose: this is an inline rewriter on a 500ms first-token budget,
	// rewriting a sentence is not a reasoning task, and most current models
	// think at medium or high unless told otherwise. That default is the
	// difference between a rewrite landing in half a second and in three.
	Default Effort `json:"default"`
}

// Model is one entry in a model picker.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Note is a short qualifier for the picker, where one is worth having.
	Note string `json:"note,omitempty"`
	// Thinking is nil when the model has no reasoning controls, which a shell
	// should render as no effort control at all rather than a disabled one.
	Thinking *Thinking `json:"thinking,omitempty"`
}

// Endpoint is one base URL, with the models it serves.
//
// Models hang off the endpoint rather than the provider because that is where
// they actually live: "openai_compatible" pointed at api.openai.com serves
// GPT models, and pointed at localhost:11434 serves whatever the user has
// pulled into Ollama. A single flat list per provider would confidently offer
// an Ollama user a list of OpenAI models, which is worse than offering none.
type Endpoint struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	// DefaultModel is preselected when this endpoint is chosen. Empty where
	// the models cannot be known ahead of time.
	DefaultModel string `json:"default_model,omitempty"`
	// Models may be empty, which is not a gap in the table but the truthful
	// answer: a local Ollama serves whatever that machine has pulled, and a
	// gateway like OpenRouter serves hundreds that change weekly. A shell
	// shows an empty list and lets the user type, which is what its model
	// field has to support anyway.
	Models []Model `json:"models"`
	// Note explains an empty list, or a rolling alias, in the picker.
	Note string `json:"note,omitempty"`
}

// Provider is one wire protocol, as POST /v1/session names it.
type Provider struct {
	// ID is the value that goes in a session request's "provider" field.
	ID   string `json:"id"`
	Name string `json:"name"`
	// RequiresKey is false where a local endpoint is a complete configuration.
	RequiresKey bool       `json:"requires_key"`
	Endpoints   []Endpoint `json:"endpoints"`
}

// Catalog is the whole table, as served by GET /v1/models.
type Catalog struct {
	Source    string     `json:"source"`
	Providers []Provider `json:"providers"`
}

// Builtin returns the curated catalog.
//
// The slices are rebuilt per call rather than shared from a package variable,
// so a caller that sorts or filters the result cannot corrupt what the next
// caller sees. It is a few hundred bytes on a settings-window open.
func Builtin() Catalog {
	providers := []Provider{
		anthropic(),
		gemini(),
		openAI(),
		openAICompatible(),
	}

	// Go marshals a nil slice as null, not [], and a shell decoding `models`
	// into a non-optional array throws on that — taking the whole catalog with
	// it rather than the one endpoint, which empties every picker in the
	// window for every provider. Normalised here rather than at each entry, so
	// that adding an endpoint without models cannot bring it back.
	for i := range providers {
		for j := range providers[i].Endpoints {
			if providers[i].Endpoints[j].Models == nil {
				providers[i].Endpoints[j].Models = []Model{}
			}
		}
	}

	return Catalog{Source: Source, Providers: providers}
}

// thinking is a shorthand for the common shape, keeping the tables below
// readable enough to audit against a provider's documentation.
func thinking(def Effort, levels ...Effort) *Thinking {
	return &Thinking{Levels: levels, Default: def}
}

func anthropic() Provider {
	return Provider{
		ID:          "anthropic",
		Name:        "Anthropic",
		RequiresKey: true,
		Endpoints: []Endpoint{{
			URL:          "https://api.anthropic.com",
			Name:         "Anthropic",
			DefaultModel: "claude-sonnet-5",
			Models: []Model{
				{
					ID:   "claude-haiku-4-5",
					Name: "Claude Haiku 4.5",
					// Deliberately no Thinking: Haiku 4.5 rejects the effort
					// parameter outright, so offering the control would produce
					// a 400 rather than a faster rewrite.
					Note: "Fastest. No thinking controls.",
				},
				{
					ID:       "claude-sonnet-5",
					Name:     "Claude Sonnet 5",
					Thinking: thinking(EffortLow, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
				},
				{
					ID:       "claude-sonnet-4-6",
					Name:     "Claude Sonnet 4.6",
					Thinking: thinking(EffortLow, EffortLow, EffortMedium, EffortHigh, EffortMax),
				},
				{
					ID:       "claude-opus-5",
					Name:     "Claude Opus 5",
					Note:     "Most capable, and the most expensive.",
					Thinking: thinking(EffortLow, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax),
				},
			},
		}},
	}
}

func gemini() Provider {
	return Provider{
		ID:          "gemini",
		Name:        "Google AI Studio",
		RequiresKey: true,
		Endpoints: []Endpoint{{
			URL:          "https://generativelanguage.googleapis.com/v1beta",
			Name:         "Google AI Studio",
			DefaultModel: "gemini-flash-latest",
			Models: []Model{
				{
					ID:   "gemini-flash-latest",
					Name: "Gemini Flash (latest)",
					Note: "Rolling alias — Google moves it to each new Flash.",
					// Only the three levels every Flash behind this alias
					// accepts. "minimal" is offered by some of them and not
					// others, and the alias is hot-swapped, so offering it here
					// would break on a swap the user never made.
					Thinking: thinking(EffortLow, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:       "gemini-3.8-flash",
					Name:     "Gemini 3.8 Flash",
					Thinking: thinking(EffortLow, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:       "gemini-3.6-flash",
					Name:     "Gemini 3.6 Flash",
					Thinking: thinking(EffortLow, EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:       "gemini-3.5-flash",
					Name:     "Gemini 3.5 Flash",
					Thinking: thinking(EffortLow, EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:       "gemini-3.5-flash-lite",
					Name:     "Gemini 3.5 Flash-Lite",
					Note:     "Cheapest.",
					Thinking: thinking(EffortLow, EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
			},
		}},
	}
}

func openAI() Provider {
	return Provider{
		ID:   "openai",
		Name: "OpenAI",
		// Its own provider rather than one endpoint inside the compatible
		// category. The models are why: bundled together, the picker offered a
		// list of GPT models to someone pointing at a local Ollama, and made
		// someone on OpenAI go looking for it under a protocol name.
		RequiresKey: true,
		Endpoints: []Endpoint{{
			URL:          "https://api.openai.com/v1",
			Name:         "OpenAI",
			DefaultModel: "gpt-5.6-luna",
			Models: []Model{
				{
					ID:   "gpt-5.6-luna",
					Name: "GPT-5.6 Luna",
					Note: "Cheapest of the 5.6 family.",
					Thinking: thinking(EffortLow,
						EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:   "gpt-5.6-terra",
					Name: "GPT-5.6 Terra",
					Thinking: thinking(EffortLow,
						EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:   "gpt-5.6-sol",
					Name: "GPT-5.6 Sol",
					Thinking: thinking(EffortLow,
						EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
				{
					ID:   "gpt-6-astra",
					Name: "GPT-6 Astra",
					Note: "Most capable, and the most expensive.",
					// No "none": GPT-6 Astra returns 400 for it.
					Thinking: thinking(EffortLow,
						EffortMinimal, EffortLow, EffortMedium, EffortHigh),
				},
			},
		}},
	}
}

func openAICompatible() Provider {
	return Provider{
		ID:   "openai_compatible",
		Name: "OpenAI-compatible",
		// Everything else that speaks the same protocol, now that OpenAI has
		// its own entry. False because the local endpoints here are a complete
		// configuration with no key at all; the gateways do need one, and say
		// so when they reject the request.
		RequiresKey: false,
		Endpoints: []Endpoint{
			{
				URL:  "http://localhost:11434/v1",
				Name: "Ollama (local)",
				Note: "Type the model you have pulled, e.g. qwen3 or llama3.2.",
			},
			{
				URL:  "http://localhost:1234/v1",
				Name: "LM Studio (local)",
				Note: "Type the model you have loaded.",
			},
			{
				URL:  "https://openrouter.ai/api/v1",
				Name: "OpenRouter",
				Note: "Type any OpenRouter model id, e.g. anthropic/claude-sonnet-5.",
			},
			{
				URL:  "https://api.groq.com/openai/v1",
				Name: "Groq",
				Note: "Type a Groq model id.",
			},
		},
	}
}
