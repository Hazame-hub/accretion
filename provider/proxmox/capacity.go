package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"sort"

	"github.com/hazame-hub/accretion/plan"
)

// Capacity is how much memory and CPU each node has, and how much of it the
// running guests are already promised.
type Capacity struct {
	Nodes                []NodeCapacity
	MemoryBytes          int64
	AllocatedMemoryBytes int64
	CPUs                 int
	AllocatedVCPUs       int
	// Known is false when some node's memory could not be read.
	Known bool
}

// NodeCapacity is one node. Offline is a node whose status is neither empty
// nor "online": nothing else about it is read. Known is false for a node
// whose memory reads as zero, which is one this token cannot audit
// (Sys.Audit on /nodes/<node>), not a node with no memory.
type NodeCapacity struct {
	Node                 string
	Status               string
	Offline              bool
	Known                bool
	MemoryBytes          int64
	AllocatedMemoryBytes int64
	CPUs                 int
	AllocatedVCPUs       int
}

// Capacity reads each node's memory and CPU and what the running guests are
// promised.
//
// What a plan reads before it offers to build machines: an OKD control plane
// asks 16 GiB a node, and a cluster whose memory is already overcommitted
// several times says yes to the API call and then swaps itself unusable. The
// numbers are reported, not judged — how much overcommit is acceptable is the
// operator's call, and ballooning makes "allocated" an upper bound.
func (c *Client) Capacity(ctx context.Context) (Capacity, error) {
	var nodes []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
		MaxMem int64  `json:"maxmem"`
		MaxCPU int    `json:"maxcpu"`
	}
	if err := c.Get(ctx, "/nodes", &nodes); err != nil {
		return Capacity{}, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })

	out := Capacity{Nodes: make([]NodeCapacity, 0, len(nodes)), Known: true}
	for _, n := range nodes {
		if n.Status != "" && n.Status != "online" {
			out.Nodes = append(out.Nodes, NodeCapacity{Node: n.Node, Status: n.Status, Offline: true})
			continue
		}
		var alloc int64
		vcpus := 0
		for _, kind := range []string{"qemu", "lxc"} {
			var guests []struct {
				Status   string `json:"status"`
				MaxMem   int64  `json:"maxmem"`
				CPUs     int    `json:"cpus"`
				Template int    `json:"template"`
			}
			if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/%s", url.PathEscape(n.Node), kind), &guests); err != nil {
				return Capacity{}, err
			}
			for _, g := range guests {
				if g.Status == "running" && g.Template == 0 {
					alloc += g.MaxMem
					vcpus += g.CPUs
				}
			}
		}
		nodeKnown := n.MaxMem > 0
		out.Known = out.Known && nodeKnown
		out.MemoryBytes += n.MaxMem
		out.AllocatedMemoryBytes += alloc
		out.CPUs += n.MaxCPU
		out.AllocatedVCPUs += vcpus
		out.Nodes = append(out.Nodes, NodeCapacity{
			Node: n.Node, Status: n.Status, Known: nodeKnown,
			MemoryBytes: n.MaxMem, AllocatedMemoryBytes: alloc,
			CPUs: n.MaxCPU, AllocatedVCPUs: vcpus,
		})
	}
	return out, nil
}

// Observed is the capacity as the planner reads it.
func (c Capacity) Observed() plan.Capacity {
	out := plan.Capacity{MemoryBytes: c.MemoryBytes, AllocatedMemoryBytes: c.AllocatedMemoryBytes, Known: c.Known}
	for _, n := range c.Nodes {
		out.Nodes = append(out.Nodes, plan.NodeCapacity{
			Name: n.Node, Online: n.Known && (n.Status == "" || n.Status == "online"),
			MemoryBytes: n.MemoryBytes, AllocatedMemoryBytes: n.AllocatedMemoryBytes,
		})
	}
	return out
}
