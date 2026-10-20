package plan

import (
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

func sized(count int, memMiB any) envmanifest.EnvironmentResource {
	r := web(count)
	if memMiB != nil {
		r.Inputs["memory_mb"] = memMiB
	}
	return r
}

func TestAPlanThatWouldOvercommitTheClusterWarns(t *testing.T) {
	w := world()
	w.Capacity = &Capacity{MemoryBytes: 27 << 30, AllocatedMemoryBytes: 72 << 30, Known: true}
	p := Build(env(sized(3, 16384)), w)
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "ask 48.0 GiB") ||
		!strings.Contains(p.Warnings[0], "has 27.0 GiB") || !strings.Contains(p.Warnings[0], "4.4× overcommitted") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	if !p.OK() {
		t.Fatal("a warning must never stop an apply")
	}
}

func TestRoomEnoughIsSilent(t *testing.T) {
	w := world()
	w.Capacity = &Capacity{MemoryBytes: 64 << 30, AllocatedMemoryBytes: 8 << 30, Known: true}
	if p := Build(env(sized(2, 4096)), w); len(p.Warnings) != 0 {
		t.Fatalf("warnings = %v", p.Warnings)
	}
}

// A manifest that leaves memory to the blueprint is counted at the blueprint's
// default — what the run will actually ask for.
func TestTheBlueprintDefaultIsCounted(t *testing.T) {
	w := world()
	w.Capacity = &Capacity{MemoryBytes: 3 << 30, AllocatedMemoryBytes: 2 << 30, Known: true}
	p := Build(env(sized(1, nil)), w) // vmBlueprint's default is 2048
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "ask 2.0 GiB") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
}

func TestUnreadCapacityIsALimitNotASilence(t *testing.T) {
	w := world()
	w.CapacityError = "403 Forbidden"
	p := Build(env(sized(1, 2048)), w)
	found := false
	for _, l := range p.Limits {
		found = found || strings.Contains(l, CapacityNotChecked) && strings.Contains(l, "403")
	}
	if !found {
		t.Fatalf("limits = %v", p.Limits)
	}
}

func TestNothingToCreateMeansNothingToSay(t *testing.T) {
	w := world()
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}}))
	_ = w
	for _, l := range p.Limits {
		if strings.Contains(l, CapacityNotChecked) {
			t.Fatalf("limits = %v", p.Limits)
		}
	}
}
