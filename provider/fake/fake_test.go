package fake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/fake"
	"github.com/hazame-hub/accretion/provider/providertest"
)

// The Fake is the first provider to pass its own contract. If this fails, the
// contract is wrong before any real provider gets to find out.
func TestFakePassesTheContract(t *testing.T) {
	f := fake.New()
	providertest.Contract(t, f, provider.Config{Endpoint: "fake://estate", Params: map[string]any{"token": "s3cret"}}, "s3cret")
	if f.Connects != 2 {
		t.Errorf("contract connected %d times, want 2", f.Connects)
	}
}

func TestFakeDiscoverCanHalfSucceed(t *testing.T) {
	f := fake.New()
	f.FailDiscover = errors.New("node2 unreachable")
	s, _ := f.Connect(context.Background(), provider.Config{})
	got, err := s.Discover(context.Background())
	if err == nil || len(got) != len(f.Estate) {
		t.Fatalf("want resources AND an error, got %d resources, err=%v", len(got), err)
	}
}

func TestFakeRemoveMakesAResourceAbsent(t *testing.T) {
	f := fake.New()
	before := len(f.Refs())
	f.Remove(provider.Ref{Kind: provider.KindVM, ID: "101"})
	if len(f.Refs()) != before-1 {
		t.Fatalf("Remove did not remove: %v", f.Refs())
	}
	s, _ := f.Connect(context.Background(), provider.Config{})
	if _, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindVM, ID: "101"}); !errors.Is(err, fake.ErrNotFound) {
		t.Errorf("Describe of a removed ref = %v, want ErrNotFound", err)
	}
}
