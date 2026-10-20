// Package fake is the third provider: an in-memory estate that implements
// provider.Provider in four methods and nothing else.
//
// The Fake proves the acceptance criterion — a third implementation compiles
// against the interface and nothing else in the codebase changes. It cannot
// prove the design (it is written against the interface that already exists),
// which is why the second provider is a real one.
//
// It is also what Sybil Continuum registers behind SYBIL_TEST_PROVIDER, so
// its end-to-end flow needs no hypervisor; the registration half (adapter and
// connection form) lives in Continuum, beside the engine it registers with. **That is why this is
// a package of its own rather than part of providertest**: the moment a
// production binary can register the Fake, the package holding it is linked
// into that binary, and providertest imports `testing` for the contract suite.
// Shipping the testing package inside the server is the kind of thing nobody
// notices until a scanner asks why it is there.
//
// providertest re-exports Fake and New, so every existing test call site is
// unchanged and the contract suite still lives beside the thing it tests.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/hazame-hub/accretion/provider"
)

// Fake is a provider whose estate is whatever the test put in it.
type Fake struct {
	mu sync.Mutex
	// Estate is what Discover returns, in the order given.
	Estate []provider.Resource
	// Caps is what Capabilities returns. Zero value: a provider that knows
	// kinds vm and host and nothing about its guests.
	Caps provider.Capabilities
	// FailConnect, when set, is what Connect returns instead of a session.
	FailConnect error
	// FailDiscover, when set, is returned by Discover alongside whatever
	// Estate holds — the "half-succeeded walk" a bridge must treat as
	// incomplete.
	FailDiscover error
	// Connects counts Connect calls; the contract asserts it is idempotent.
	Connects int
	// Closed counts Close calls.
	Closed int
	// LastConfig is the Config the most recent Connect received, so a test
	// can assert a credential arrived resolved and was not echoed anywhere.
	LastConfig provider.Config
}

// New returns a Fake with a small, plausible estate: one host, two VMs with
// UUIDs and MACs, one container with MACs only, one template.
func New() *Fake {
	return &Fake{
		Caps: provider.Capabilities{
			Kinds:      []provider.Kind{provider.KindHost, provider.KindVM, provider.KindContainer, provider.KindTemplate},
			GuestUUID:  true,
			GuestMACs:  true,
			Hierarchy:  true,
			PowerState: true,
			Templates:  true,
			Memory:     true,
		},
		Estate: []provider.Resource{
			{Ref: provider.Ref{Kind: provider.KindHost, ID: "node1"}, Name: "node1", State: "running",
				Raw: []byte(`{"node":"node1"}`), Tags: []string{}, Identity: provider.Identity{MACs: []string{}, IPs: []string{}}},
			vm("100", "web-01", "node1", "421b7f1e-7b7a-3d6e-2c0e-9c4b5a6f7d8e", "bc:24:11:aa:bb:01", 4, 8<<30),
			vm("101", "db-01", "node1", "9a1d3c2b-4e5f-6a7b-8c9d-0e1f2a3b4c5d", "bc:24:11:aa:bb:02", 2, 4<<30),
			{Ref: provider.Ref{Kind: provider.KindContainer, ID: "200"}, Name: "cache-01", Parent: &provider.Ref{Kind: provider.KindHost, ID: "node1"},
				State: "running", Tags: []string{},
				Identity: provider.Identity{MACs: []string{"bc:24:11:aa:bb:03"}, IPs: []string{}},
				Facts:    provider.Facts{CPUCores: 1, RAMBytes: 1 << 30, Disks: []provider.Disk{}, NetworkInterfaces: []provider.Interface{{Name: "eth0", Addresses: []string{}, HardwareAddr: "bc:24:11:aa:bb:03"}}},
				Memory:   provider.MemoryAllocation{MaxBytes: 1 << 30, CurrentBytes: 1 << 30},
				Raw:      []byte(`{"vmid":200,"type":"lxc"}`)},
			{Ref: provider.Ref{Kind: provider.KindTemplate, ID: "9000"}, Name: "debian-12-cloud", Parent: &provider.Ref{Kind: provider.KindHost, ID: "node1"},
				State: "stopped", Tags: []string{}, Identity: provider.Identity{MACs: []string{}, IPs: []string{}},
				Raw: []byte(`{"vmid":9000,"template":1}`)},
		},
	}
}

func vm(id, name, node, uuid, mac string, cores int32, ram int64) provider.Resource {
	return provider.Resource{
		Ref:    provider.Ref{Kind: provider.KindVM, ID: id},
		Name:   name,
		Parent: &provider.Ref{Kind: provider.KindHost, ID: node},
		State:  "running",
		Tags:   []string{},
		Identity: provider.Identity{
			SMBIOSUUID:        uuid,
			MACs:              []string{mac},
			HypervisorGuestID: id,
			IPs:               []string{},
		},
		Facts: provider.Facts{
			CPUCores: cores, RAMBytes: ram,
			Disks:             []provider.Disk{{Total: 32 << 30}},
			NetworkInterfaces: []provider.Interface{{Name: "net0", Addresses: []string{}, HardwareAddr: mac}},
		},
		Memory: provider.MemoryAllocation{MaxBytes: ram, CurrentBytes: ram},
		Raw:    []byte(fmt.Sprintf(`{"vmid":%s,"name":%q,"smbios1":"uuid=%s"}`, id, name, uuid)),
	}
}

// Connect implements provider.Provider.
func (f *Fake) Connect(_ context.Context, cfg provider.Config) (provider.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Connects++
	f.LastConfig = cfg
	if f.FailConnect != nil {
		return nil, f.FailConnect
	}
	return &session{fake: f}, nil
}

type session struct {
	fake   *Fake
	closed bool
}

func (s *session) Capabilities(context.Context) (provider.Capabilities, error) {
	return s.fake.Caps, nil
}

func (s *session) Discover(context.Context) ([]provider.Resource, error) {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	out := make([]provider.Resource, len(s.fake.Estate))
	copy(out, s.fake.Estate)
	return out, s.fake.FailDiscover
}

// ErrNotFound is what Describe returns for a ref the estate does not hold.
var ErrNotFound = errors.New("fake: no such resource")

func (s *session) Describe(_ context.Context, ref provider.Ref) (provider.Facts, error) {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	for _, r := range s.fake.Estate {
		if r.Ref == ref {
			return r.Facts, nil
		}
	}
	return provider.Facts{}, ErrNotFound
}

func (s *session) Close() error {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	if s.closed {
		return errors.New("fake: session closed twice")
	}
	s.closed = true
	s.fake.Closed++
	return nil
}

// Remove drops a resource from the estate, for tests of absence.
func (f *Fake) Remove(ref provider.Ref) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.Estate[:0]
	for _, r := range f.Estate {
		if r.Ref != ref {
			kept = append(kept, r)
		}
	}
	f.Estate = kept
}

// Refs is the estate's refs, sorted, for assertions.
func (f *Fake) Refs() []provider.Ref {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.Ref, 0, len(f.Estate))
	for _, r := range f.Estate {
		out = append(out, r.Ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}
