// Package accretion is a declarative VM engine: a manifest that says which
// machines should exist, a planner that lines that intent up against what was
// built, and a provider contract a hypervisor implements to be planned against.
//
// It began inside Sybil Continuum, which now requires it as a module like
// any other user does. The rule that keeps it standalone is the one
// boundary_test.go enforces: nothing in this module imports Sybil.
//
//   - provider: the discovery contract and the identity model.
//   - provider/providertest: the suite every provider must pass.
//   - provider/fake: an in-memory provider.
package accretion
