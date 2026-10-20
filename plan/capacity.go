package plan

import (
	"fmt"
	"strconv"
)

// Capacity in a plan.
//
// A plan that says "3 to create" for three 16 GiB machines on a host with
// 27 GiB that is already promised 72 GiB is technically right and practically
// a trap: the API accepts every create, and the machines then swap the host
// into the ground. So the plan states the memory arithmetic whenever it would
// create something. It warns and never refuses — overcommit is a legitimate
// choice, and ballooning makes "promised" an upper bound — but the operator
// approving the runs has seen the numbers.

// Capacity is the connector's cluster as the plan read it.
type Capacity struct {
	MemoryBytes          int64
	AllocatedMemoryBytes int64
	// Known is false when some node's memory could not be read.
	Known bool
	// Nodes are the cluster's nodes one by one, which placement chooses
	// among. Empty when the connector reports only totals.
	Nodes []NodeCapacity
}

// MemoryInput is the input the stock machine blueprints size memory with, in
// MiB. A blueprint that names it differently is simply not counted, and the
// plan says how many machines it could not size.
const MemoryInput = "memory_mb"

// CapacityNotChecked is the limit stated when the cluster's capacity could not
// be read.
const CapacityNotChecked = "cluster memory was not checked against what this plan would create"

func capacityNotes(p *Plan, world Snapshot) {
	var asked int64
	creates, unsized := 0, 0
	for _, e := range p.Entries {
		if e.Action != ActionCreate {
			continue
		}
		creates++
		mem, ok := instanceMemory(world, e.Blueprint, e.Inputs)
		if !ok {
			unsized++
			continue
		}
		asked += mem
	}
	if creates == 0 {
		return
	}
	if world.Capacity == nil && world.CapacityError == "" {
		// Not asked at all (no connector engine to ask). The server always
		// sets CapacityError when it asked and could not tell.
		return
	}
	if world.Capacity == nil || !world.Capacity.Known || world.Capacity.MemoryBytes <= 0 {
		reason := CapacityNotChecked
		if world.CapacityError != "" {
			reason += ": " + world.CapacityError
		} else if world.Capacity != nil {
			reason += ": the memory of some node could not be read"
		}
		p.Limits = append(p.Limits, reason)
		return
	}
	c := *world.Capacity
	after := c.AllocatedMemoryBytes + asked
	ratio := float64(after) / float64(c.MemoryBytes)
	if ratio > 1 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"the %d machine(s) to create ask %s of memory; the cluster has %s and running guests are already promised %s, "+
				"so it would be %.1f× overcommitted after this apply",
			creates-unsized, gib(asked), gib(c.MemoryBytes), gib(c.AllocatedMemoryBytes), ratio))
	}
	if unsized > 0 {
		p.Limits = append(p.Limits, fmt.Sprintf("%d machine(s) to create declare no %s, so their memory is not counted", unsized, MemoryInput))
	}
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func gib(b int64) string {
	return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GiB"
}

// blueprintDefault is the default a blueprint declares for an input, so a
// manifest that leaves memory to the blueprint is counted at what the run
// will actually ask for.
func blueprintDefault(world Snapshot, blueprint, input string) any {
	if bp := blueprintByID(world.Blueprints, blueprint); bp != nil {
		for _, in := range bp.Inputs {
			if in.Name == input {
				return in.Default
			}
		}
	}
	return nil
}
