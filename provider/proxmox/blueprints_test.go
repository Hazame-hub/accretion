package proxmox

import "testing"

// Blueprints() must hand out its own copy: a caller that edits the slice
// must not change what the next caller plans with.
func TestAccretionProxmoxBlueprintsAreAFreshCopy(t *testing.T) {
	a := Blueprints()
	a[0].Inputs[0].Name = "changed"
	if Blueprints()[0].Inputs[0].Name == "changed" {
		t.Error("Blueprints() returned shared state")
	}
}

// The planner looks up a blueprint's reconfigure, destroy, power, firewall
// and adopt blueprints by id: every one named must be there.
func TestAccretionProxmoxBlueprintsNameOnlyBlueprintsTheyHave(t *testing.T) {
	bps := Blueprints()
	ids := map[string]bool{}
	for _, b := range bps {
		ids[b.ID] = true
	}
	for _, b := range bps {
		for _, ref := range []string{b.ReconfiguredBy, b.DestroyedBy, b.PoweredOnBy, b.PoweredOffBy, b.FirewalledBy, b.AdoptedBy} {
			if ref != "" && !ids[ref] {
				t.Errorf("%s names %s, which Blueprints() does not have", b.ID, ref)
			}
		}
	}
}
