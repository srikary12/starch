package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/srikary12/starch/internal/catalog"
)

func getCatalog(t *testing.T, c *http.Client) catalog.Catalog {
	t.Helper()
	resp := do(t, c, http.MethodGet, "/v1/models", testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got catalog.Catalog
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return got
}

// The settings window is normally open because there is no key yet, so this
// must answer without a session having been established.
func TestGetModelsNeedsNoSession(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})

	got := getCatalog(t, c)
	if got.Source != catalog.Source {
		t.Errorf("source = %q, want %q", got.Source, catalog.Source)
	}
	if len(got.Providers) == 0 {
		t.Fatal("no providers — every picker in every shell would be empty")
	}
}

// The whole point of serving this from the daemon is that a shell can take a
// provider id straight from the catalog and put it in a session request. If
// these two lists drift, that stops being true and every shell breaks in the
// same way.
func TestEveryCatalogProviderIsOneSessionAccepts(t *testing.T) {
	accepted := map[ProviderID]bool{
		ProviderAnthropic:        true,
		ProviderOpenAI:           true,
		ProviderGemini:           true,
		ProviderOpenAICompatible: true,
	}

	c, _, _, _ := startDaemon(t, Options{})
	for _, p := range getCatalog(t, c).Providers {
		if !accepted[ProviderID(p.ID)] {
			t.Errorf("catalog offers provider %q, which POST /v1/session would reject", p.ID)
		}
		delete(accepted, ProviderID(p.ID))
	}
	for missing := range accepted {
		t.Errorf("session accepts provider %q, but the catalog does not offer it", missing)
	}
}

// A shell reading the catalog has to be able to configure a session from it
// without inventing anything: provider, endpoint and model all come from here.
func TestTheCatalogIsEnoughToConfigureASession(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})

	for _, p := range getCatalog(t, c).Providers {
		for _, e := range p.Endpoints {
			if e.URL == "" {
				t.Errorf("%s: an endpoint with no url cannot be sent as base_url", p.ID)
			}
		}
	}
}

func TestModelsRejectsOtherMethods(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})
	resp := do(t, c, http.MethodPost, "/v1/models", testToken)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
}
