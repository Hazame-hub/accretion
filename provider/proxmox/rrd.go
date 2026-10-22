package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Timeframe is how far back a guest's history reaches. Proxmox keeps one
// round-robin archive per timeframe, each at its own resolution: a minute for
// the last hour, then coarser steps as the window grows.
type Timeframe string

const (
	TimeframeHour  Timeframe = "hour"
	TimeframeDay   Timeframe = "day"
	TimeframeWeek  Timeframe = "week"
	TimeframeMonth Timeframe = "month"
	TimeframeYear  Timeframe = "year"
)

// Valid reports whether t is one of the timeframes Proxmox keeps.
func (t Timeframe) Valid() bool {
	switch t {
	case TimeframeHour, TimeframeDay, TimeframeWeek, TimeframeMonth, TimeframeYear:
		return true
	}
	return false
}

// Consolidation is how Proxmox folds the samples of one step into one value.
type Consolidation string

const (
	// ConsolidationAverage is the mean over each step.
	ConsolidationAverage Consolidation = "AVERAGE"
	// ConsolidationMax is the highest sample in each step. What a question
	// about memory wants: a 30-minute average hides the minute a guest ran
	// out.
	ConsolidationMax Consolidation = "MAX"
)

// Valid reports whether c is a consolidation Proxmox accepts.
func (c Consolidation) Valid() bool {
	return c == ConsolidationAverage || c == ConsolidationMax
}

// RRDPoint is one step of a guest's history as the hypervisor recorded it.
// A nil field is a step Proxmox has no value for (the guest was stopped, or
// did not exist yet), which is not the same as zero.
type RRDPoint struct {
	Time time.Time
	// CPU is the share of the guest's vCPUs in use, 0 to 1.
	CPU    *float64
	MaxCPU *float64
	// Memory in bytes, as the hypervisor sees it.
	Mem    *float64
	MaxMem *float64
	// Rates in bytes per second.
	NetIn     *float64
	NetOut    *float64
	DiskRead  *float64
	DiskWrite *float64
}

// GuestRRD reads a guest's history from the hypervisor: one point per step
// of the timeframe's archive, oldest first.
//
// kind is "qemu" or "lxc". The token needs VM.Audit on the guest; without it
// the error is an APIError that MissingPrivilege names.
func (c *Client) GuestRRD(ctx context.Context, node, kind string, vmid int, tf Timeframe, cf Consolidation) ([]RRDPoint, error) {
	if node == "" {
		return nil, fmt.Errorf("proxmox: rrd: node is required")
	}
	if kind != "qemu" && kind != "lxc" {
		return nil, fmt.Errorf("proxmox: rrd: kind must be qemu or lxc, got %q", kind)
	}
	if vmid <= 0 {
		return nil, fmt.Errorf("proxmox: rrd: vmid must be positive, got %d", vmid)
	}
	if !tf.Valid() {
		return nil, fmt.Errorf("proxmox: rrd: unknown timeframe %q", tf)
	}
	if !cf.Valid() {
		return nil, fmt.Errorf("proxmox: rrd: unknown consolidation %q", cf)
	}

	q := url.Values{"timeframe": {string(tf)}, "cf": {string(cf)}}
	path := fmt.Sprintf("/nodes/%s/%s/%d/rrddata?%s", url.PathEscape(node), kind, vmid, q.Encode())
	var rows []map[string]json.RawMessage
	if err := c.Get(ctx, path, &rows); err != nil {
		return nil, err
	}

	out := make([]RRDPoint, 0, len(rows))
	for _, r := range rows {
		ts, ok := rrdNumber(r["time"])
		if !ok {
			continue // a row with no time places nothing on a chart
		}
		cpu, _ := rrdField(r, "cpu")
		maxCPU, _ := rrdField(r, "maxcpu")
		mem, _ := rrdField(r, "mem")
		maxMem, _ := rrdField(r, "maxmem")
		netIn, _ := rrdField(r, "netin")
		netOut, _ := rrdField(r, "netout")
		diskRead, _ := rrdField(r, "diskread")
		diskWrite, _ := rrdField(r, "diskwrite")
		out = append(out, RRDPoint{
			Time: time.Unix(int64(ts), 0).UTC(),
			CPU:  cpu, MaxCPU: maxCPU, Mem: mem, MaxMem: maxMem,
			NetIn: netIn, NetOut: netOut, DiskRead: diskRead, DiskWrite: diskWrite,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func rrdField(r map[string]json.RawMessage, key string) (*float64, bool) {
	v, ok := rrdNumber(r[key])
	if !ok {
		return nil, false
	}
	return &v, true
}

// rrdNumber reads one RRD value. Proxmox leaves a key out for a step it has
// no value for; older releases have also sent null, a string, or NaN for the
// same thing. All of them are "no value", never zero.
func rrdNumber(raw json.RawMessage) (float64, bool) {
	// null unmarshals into a float64 without error and leaves it zero.
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if g, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(g) && !math.IsInf(g, 0) {
			return g, true
		}
	}
	return 0, false
}
