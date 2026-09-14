package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// The table is hand-maintained, so these tests are aimed squarely at the way a
// hand-maintained table goes wrong: a typo'd default that names a model which
// is not in the list, a default effort the model does not accept, a duplicated
// id. Each of those ships a picker that looks fine and preselects something
// broken, and none of them are caught by anything else.

func TestEveryDefaultModelIsInItsOwnList(t *testing.T) {
	for _, p := range Builtin().Providers {
		for _, e := range p.Endpoints {
			if e.DefaultModel == "" {
				continue // endpoints whose models cannot be known ahead of time
			}
			found := false
			for _, m := range e.Models {
				if m.ID == e.DefaultModel {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s / %s: default model %q is not in its own model list",
					p.ID, e.Name, e.DefaultModel)
			}
		}
	}
}

func TestEveryDefaultEffortIsOneTheModelAccepts(t *testing.T) {
	for _, p := range Builtin().Providers {
		for _, e := range p.Endpoints {
			for _, m := range e.Models {
				if m.Thinking == nil {
					continue
				}
				if len(m.Thinking.Levels) == 0 {
					t.Errorf("%s / %s: thinking with no levels — a shell would show an empty picker", p.ID, m.ID)
					continue
				}
				found := false
				for _, level := range m.Thinking.Levels {
					if level == m.Thinking.Default {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s / %s: default effort %q is not among its levels %v",
						p.ID, m.ID, m.Thinking.Default, m.Thinking.Levels)
				}
			}
		}
	}
}

func TestIdentifiersAreUniqueAndPresent(t *testing.T) {
	seenProviders := map[string]bool{}
	for _, p := range Builtin().Providers {
		if p.ID == "" || p.Name == "" {
			t.Errorf("provider %+v is missing an id or a name", p)
		}
		if seenProviders[p.ID] {
			t.Errorf("provider id %q appears twice", p.ID)
		}
		seenProviders[p.ID] = true

		if len(p.Endpoints) == 0 {
			t.Errorf("provider %q has no endpoints, so its picker would be empty", p.ID)
		}

		seenEndpoints := map[string]bool{}
		for _, e := range p.Endpoints {
			if e.URL == "" || e.Name == "" {
				t.Errorf("%s: endpoint %+v is missing a url or a name", p.ID, e)
			}
			if seenEndpoints[e.URL] {
				t.Errorf("%s: endpoint url %q appears twice", p.ID, e.URL)
			}
			seenEndpoints[e.URL] = true

			seenModels := map[string]bool{}
			for _, m := range e.Models {
				if m.ID == "" || m.Name == "" {
					t.Errorf("%s / %s: model %+v is missing an id or a name", p.ID, e.Name, m)
				}
				if seenModels[m.ID] {
					t.Errorf("%s / %s: model id %q appears twice", p.ID, e.Name, m.ID)
				}
				seenModels[m.ID] = true
			}
		}
	}
}

// An endpoint with no models is a legitimate entry — a local Ollama serves
// whatever that machine has pulled — but a shell renders an empty picker, so
// it has to say why rather than looking broken.
func TestAnEmptyModelListExplainsItself(t *testing.T) {
	for _, p := range Builtin().Providers {
		for _, e := range p.Endpoints {
			if len(e.Models) == 0 && e.Note == "" {
				t.Errorf("%s / %s: no models and no note — the picker would be blank with no explanation",
					p.ID, e.Name)
			}
		}
	}
}

// Local endpoints are the answer for anyone who will not send work text to a
// third party, so losing them from the table would be a real regression.
func TestLocalEndpointsAreOffered(t *testing.T) {
	var urls []string
	for _, p := range Builtin().Providers {
		for _, e := range p.Endpoints {
			urls = append(urls, e.URL)
		}
	}
	joined := strings.Join(urls, " ")
	for _, want := range []string{"localhost:11434", "localhost:1234"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no endpoint for %s in %v", want, urls)
		}
	}
}

// The shells decode this straight off the wire, so the JSON shape is the
// contract. Names here must match api/README.md.
func TestJSONShapeMatchesTheContract(t *testing.T) {
	encoded, err := json.Marshal(Builtin())
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	body := string(encoded)

	for _, want := range []string{
		`"source":"builtin"`,
		`"requires_key"`,
		`"default_model"`,
		`"thinking":{"levels":`,
		`"default":"low"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in the encoded catalog", want)
		}
	}

	// Thinking is a pointer so that a model without reasoning controls omits
	// the key entirely rather than sending a zero value a shell would have to
	// tell apart from "no levels".
	if strings.Contains(body, `"thinking":null`) {
		t.Error(`a model encoded "thinking":null; it should be omitted instead`)
	}
}

// The bug this exists for: Go marshals a nil slice as null rather than [], and
// a shell decoding `models` into a non-optional array threw on it — taking the
// entire catalog down rather than the one endpoint, so every picker in the
// settings window went empty for every provider.
//
// A fixture hand-written in the shell's own tests cannot catch this. It tests
// the shape its author intended; this tests the shape this package actually
// emits, which is the only one that reaches a shell.
func TestNoEndpointEncodesNullModels(t *testing.T) {
	encoded, err := json.Marshal(Builtin())
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(encoded), `"models":null`) {
		t.Error(`an endpoint encoded "models":null; every endpoint must encode [] instead`)
	}

	// And again as a decoder sees it, rather than trusting a substring match.
	var round Catalog
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	for _, p := range round.Providers {
		for _, e := range p.Endpoints {
			if e.Models == nil {
				t.Errorf("%s / %s: models decoded as nil", p.ID, e.Name)
			}
		}
	}
}

// OpenAI is its own provider, not an endpoint inside the compatible category.
// Bundled together, the picker offered GPT models to someone pointing at a
// local Ollama — which is worse than offering none.
func TestOpenAIIsSeparateFromTheCompatibleCategory(t *testing.T) {
	var openai, compatible *Provider
	for i, p := range Builtin().Providers {
		switch p.ID {
		case "openai":
			openai = &Builtin().Providers[i]
		case "openai_compatible":
			compatible = &Builtin().Providers[i]
		}
	}

	if openai == nil {
		t.Fatal("no openai provider")
	}
	if !openai.RequiresKey {
		t.Error("OpenAI needs a key, so the shell must be told to ask for one")
	}
	if len(openai.Endpoints) == 0 || len(openai.Endpoints[0].Models) == 0 {
		t.Error("the OpenAI entry carries no models, which is the whole reason it is separate")
	}

	if compatible == nil {
		t.Fatal("no openai_compatible provider")
	}
	if compatible.RequiresKey {
		t.Error("the compatible category must not demand a key: a local Ollama needs none")
	}
	for _, e := range compatible.Endpoints {
		if strings.Contains(e.URL, "api.openai.com") {
			t.Errorf("OpenAI is still listed under the compatible category as %q", e.URL)
		}
		if len(e.Models) != 0 {
			t.Errorf("%s carries models; what a private or local endpoint serves cannot be known here", e.Name)
		}
	}
}
