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
