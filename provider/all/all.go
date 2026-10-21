// Package all is every provider Accretion ships, by the type a manifest's
// spec.provider names. A command wires providers through here, so a
// provider's name stays in its own package.
package all

import (
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/proxmox"
)

// Driver is one provider with what the planner needs from it besides a
// session: the blueprints it ships.
type Driver struct {
	Provider provider.Provider
	// Blueprints returns a fresh copy of the provider's blueprints.
	Blueprints func() []plan.Blueprint
}

var drivers = map[string]Driver{
	"proxmox": {Provider: proxmox.New(), Blueprints: proxmox.Blueprints},
}

// Lookup returns the driver for a provider type.
func Lookup(kind string) (Driver, bool) {
	d, ok := drivers[kind]
	return d, ok
}

// Types lists the provider types Accretion ships, for an error message.
func Types() []string {
	out := make([]string, 0, len(drivers))
	for k := range drivers {
		out = append(out, k)
	}
	return out
}
