// Package accretion is a declarative VM engine: a manifest that says which
// machines should exist, a planner that lines that intent up against what was
// built, and a provider contract a hypervisor implements to be planned against.
//
// It began inside Sybil Continuum and is being extracted from it. The rule
// that makes the extraction real is the one boundary_test.go enforces: nothing
// in this module imports Sybil.
//
//   - provider: the discovery contract and the identity model.
//   - provider/providertest: the suite every provider must pass.
//   - provider/fake: an in-memory provider.
package accretion
