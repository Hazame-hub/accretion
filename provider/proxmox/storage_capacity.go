package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// StorageCapacity is what each node's disks hold and are promised: the
// node's own root filesystem, every storage it sees, the volumes on the
// storages that hold VM disks, and the storage's recent history.
//
// Like Capacity it reports and does not judge. What counts as "nearly full"
// or "too overcommitted" is the caller's decision; this only has to be
// honest about what it could and could not read. A part that was refused
// carries its error beside it rather than failing the whole reading, because
// a token that may not list one storage's content can still see that the
// root filesystem is 96 % full, and that is the reading that matters.
//
// GET requests only.
type StorageCapacity struct {
	Nodes []NodeStorage
}

// NodeStorage is one node. Offline nodes are listed with nothing read.
type NodeStorage struct {
	Node    string
	Status  string
	Offline bool
	// RootFS is the node's own system filesystem (/nodes/<n>/status rootfs).
	// Nil with RootFSErr set when it could not be read; a 403 there is
	// Sys.Audit on /nodes/<n>, which MissingPrivilege names.
	RootFS    *FSUsage
	RootFSErr error
	// Storages is every storage the node lists. StoragesErr is set when the
	// listing itself failed, and Storages is then empty.
	Storages    []StorageUsage
	StoragesErr error
}

// FSUsage is a filesystem's size as statfs reports it, in bytes. Used is
// total minus free, Avail what an unprivileged writer may still use, so
// Used+Avail can be less than Total (reserved blocks).
type FSUsage struct {
	Total int64
	Used  int64
	Avail int64
}

// StorageUsage is one storage as one node sees it.
type StorageUsage struct {
	Storage string
	Type    string // dir, lvmthin, zfspool, nfs, …
	Content []string
	Active  bool
	Enabled bool
	Shared  bool
	FSUsage
	// SeenFrom lists the nodes that see this storage. A shared storage is
	// read once, from the first node, and the others are recorded here
	// instead of being counted again.
	SeenFrom []string

	// Volumes are read only for a storage with "images" content.
	// VolumesRead says the listing was asked and answered; false with
	// VolumesErr nil means it was not asked (no images content, inactive).
	Volumes     []Volume
	VolumesRead bool
	VolumesErr  error

	// History is the storage's used/total over the last week, oldest
	// first, from Proxmox's own RRD. HistoryErr set when it was refused.
	History    []UsagePoint
	HistoryErr error
}

// HoldsImages says whether VM disks may live on this storage.
func (s StorageUsage) HoldsImages() bool {
	for _, c := range s.Content {
		if c == "images" {
			return true
		}
	}
	return false
}

// Volume is one disk image. Size is what the guest was promised, Used what
// the storage has actually allocated to it so far; for a thin or sparse
// volume Used grows towards Size as the guest writes. Used is -1 when the
// storage did not say.
type Volume struct {
	VolID  string
	VMID   int
	Format string
	Size   int64
	Used   int64
}

// UsagePoint is one RRD step.
type UsagePoint struct {
	Time  time.Time
	Used  float64
	Total float64
}

// StorageCapacity reads every online node's root filesystem and storages.
// Only the node list is required: any later refusal is kept on the part it
// belongs to.
func (c *Client) StorageCapacity(ctx context.Context) (StorageCapacity, error) {
	var nodes []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := c.Get(ctx, "/nodes", &nodes); err != nil {
		return StorageCapacity{}, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })

	out := StorageCapacity{Nodes: make([]NodeStorage, 0, len(nodes))}
	// shared storage name -> index into the owning node's Storages
	type ref struct{ node, idx int }
	shared := map[string]ref{}
	for _, n := range nodes {
		ns := NodeStorage{Node: n.Node, Status: n.Status}
		if n.Status != "" && n.Status != "online" {
			ns.Offline = true
			out.Nodes = append(out.Nodes, ns)
			continue
		}
		ns.RootFS, ns.RootFSErr = c.nodeRootFS(ctx, n.Node)

		rows, err := c.nodeStorages(ctx, n.Node)
		if err != nil {
			ns.StoragesErr = err
			out.Nodes = append(out.Nodes, ns)
			continue
		}
		for _, s := range rows {
			if s.Shared {
				if r, seen := shared[s.Storage]; seen {
					owner := &out.Nodes[r.node].Storages[r.idx]
					owner.SeenFrom = append(owner.SeenFrom, n.Node)
					continue
				}
			}
			s.SeenFrom = []string{n.Node}
			if s.Active && s.HoldsImages() {
				s.Volumes, s.VolumesErr = c.storageVolumes(ctx, n.Node, s.Storage)
				s.VolumesRead = s.VolumesErr == nil
			}
			if s.Active {
				s.History, s.HistoryErr = c.storageHistory(ctx, n.Node, s.Storage)
			}
			ns.Storages = append(ns.Storages, s)
			if s.Shared {
				shared[s.Storage] = ref{node: len(out.Nodes), idx: len(ns.Storages) - 1}
			}
		}
		out.Nodes = append(out.Nodes, ns)
	}
	return out, nil
}

func (c *Client) nodeRootFS(ctx context.Context, node string) (*FSUsage, error) {
	var st struct {
		RootFS map[string]json.RawMessage `json:"rootfs"`
	}
	if err := c.Get(ctx, "/nodes/"+url.PathEscape(node)+"/status", &st); err != nil {
		return nil, err
	}
	total, okT := rrdNumber(st.RootFS["total"])
	used, okU := rrdNumber(st.RootFS["used"])
	avail, okA := rrdNumber(st.RootFS["avail"])
	if !okT || !okU || !okA || total <= 0 {
		return nil, fmt.Errorf("proxmox: node %s status has no rootfs sizes", node)
	}
	return &FSUsage{Total: int64(total), Used: int64(used), Avail: int64(avail)}, nil
}

func (c *Client) nodeStorages(ctx context.Context, node string) ([]StorageUsage, error) {
	var rows []map[string]json.RawMessage
	if err := c.Get(ctx, "/nodes/"+url.PathEscape(node)+"/storage", &rows); err != nil {
		return nil, err
	}
	out := make([]StorageUsage, 0, len(rows))
	for _, r := range rows {
		var name, typ, content string
		_ = json.Unmarshal(r["storage"], &name)
		_ = json.Unmarshal(r["type"], &typ)
		_ = json.Unmarshal(r["content"], &content)
		if name == "" {
			continue
		}
		s := StorageUsage{Storage: name, Type: typ, Active: flag(r["active"]), Shared: flag(r["shared"])}
		// "enabled" is absent on older releases, where a listed storage is enabled.
		s.Enabled = r["enabled"] == nil || flag(r["enabled"])
		for _, part := range strings.Split(content, ",") {
			if part = strings.TrimSpace(part); part != "" {
				s.Content = append(s.Content, part)
			}
		}
		sort.Strings(s.Content)
		t, _ := rrdNumber(r["total"])
		u, _ := rrdNumber(r["used"])
		a, _ := rrdNumber(r["avail"])
		s.FSUsage = FSUsage{Total: int64(t), Used: int64(u), Avail: int64(a)}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Storage < out[j].Storage })
	return out, nil
}

func (c *Client) storageVolumes(ctx context.Context, node, storage string) ([]Volume, error) {
	var rows []map[string]json.RawMessage
	path := fmt.Sprintf("/nodes/%s/storage/%s/content?content=images", url.PathEscape(node), url.PathEscape(storage))
	if err := c.Get(ctx, path, &rows); err != nil {
		return nil, err
	}
	out := make([]Volume, 0, len(rows))
	for _, r := range rows {
		var v Volume
		_ = json.Unmarshal(r["volid"], &v.VolID)
		_ = json.Unmarshal(r["format"], &v.Format)
		if id, ok := rrdNumber(r["vmid"]); ok {
			v.VMID = int(id)
		}
		size, _ := rrdNumber(r["size"])
		v.Size = int64(size)
		v.Used = -1
		if u, ok := rrdNumber(r["used"]); ok {
			v.Used = int64(u)
		}
		if v.VolID == "" {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VolID < out[j].VolID })
	return out, nil
}

func (c *Client) storageHistory(ctx context.Context, node, storage string) ([]UsagePoint, error) {
	q := url.Values{"timeframe": {string(TimeframeWeek)}, "cf": {string(ConsolidationAverage)}}
	path := fmt.Sprintf("/nodes/%s/storage/%s/rrddata?%s", url.PathEscape(node), url.PathEscape(storage), q.Encode())
	var rows []map[string]json.RawMessage
	if err := c.Get(ctx, path, &rows); err != nil {
		return nil, err
	}
	out := make([]UsagePoint, 0, len(rows))
	for _, r := range rows {
		ts, ok := rrdNumber(r["time"])
		if !ok {
			continue
		}
		used, okU := rrdNumber(r["used"])
		total, okT := rrdNumber(r["total"])
		if !okU || !okT {
			continue // a step with no value is not a zero
		}
		out = append(out, UsagePoint{Time: time.Unix(int64(ts), 0).UTC(), Used: used, Total: total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// flag reads a Proxmox boolean, which arrives as 0/1, sometimes as a string.
func flag(raw json.RawMessage) bool {
	v, ok := rrdNumber(raw)
	return ok && v != 0
}
