package providertest

// The Fake, re-exported.
//
// It used to live here. It moved to internal/provider/fake when the
// end-to-end flow needed to REGISTER it behind SYBIL_TEST_PROVIDER: the moment
// a production binary can register the Fake, the package holding it is linked
// into that binary — and this package imports `testing` for the contract
// suite. Shipping the testing package inside the server is the kind of thing
// nobody notices until a scanner asks why it is there.
//
// These aliases exist so that did not become a rename across every test that
// already uses the Fake, and so the contract suite still sits beside the first
// provider that has to pass it. `providertest.Fake` and `providertest.New()`
// mean exactly what they meant before.

import "github.com/hazame-hub/accretion/provider/fake"

// Fake is an in-memory estate that implements provider.Provider and nothing
// else. See internal/provider/fake.
type Fake = fake.Fake

// New returns a Fake with a small, plausible estate: one host, two VMs with
// UUIDs and MACs, one container with MACs only, one template.
func New() *Fake { return fake.New() }

// ErrNotFound is what Describe returns for a ref the estate does not hold.
var ErrNotFound = fake.ErrNotFound
