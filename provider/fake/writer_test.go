package fake_test

import (
	"testing"

	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/fake"
	"github.com/hazame-hub/accretion/provider/providertest"
)

// The Fake passes the write contract before any real provider has to.
func TestFakeWriteContract(t *testing.T) {
	f := fake.New()
	cfg := provider.Config{Endpoint: "fake://estate", Params: map[string]any{"api_token": "fake-secret-token"}}
	providertest.WriteContract(t, f, cfg, "fake-secret-token", providertest.WriteFixture{
		Template:            provider.Ref{Kind: provider.KindTemplate, ID: "9000"},
		SeedStorage:         "local",
		Foreign:             provider.Ref{Kind: provider.KindVM, ID: "101"},
		ForeignUUID:         "9a1d3c2b-4e5f-6a7b-8c9d-0e1f2a3b4c5d",
		InterruptNextClone:  f.InterruptNextClone,
		LoseNextCloneAnswer: f.InterruptNextClone,
	})
}
