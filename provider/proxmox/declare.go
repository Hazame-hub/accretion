package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
)

// linkedBase is a linked clone's disk volume: base-<template>-disk-N/vm-...
var linkedBase = regexp.MustCompile(`:base-([0-9]+)-disk-[0-9]+/`)

// TemplateNotRecorded is why template_vmid is unread on a full clone.
const TemplateNotRecorded = "Proxmox does not record which template a full clone was made from"

// Declare implements provider.Declarer: every qemu guest that is not a
// template, described as provision-proxmox-vm inputs. The sizes are the ones
// the plan's drift check reads (cores, memory, a single boot disk), so a
// machine declared this way plans as unchanged. It sends GET requests only.
func (s *session) Declare(ctx context.Context) ([]provider.Declared, error) {
	resources, err := s.Discover(ctx)
	if err != nil && len(resources) == 0 {
		return nil, err
	}
	var out []provider.Declared
	for _, r := range resources {
		if r.Ref.Kind != provider.KindVM || !strings.Contains(r.Ref.ID, "/qemu/") {
			continue
		}
		out = append(out, declare(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

func declare(r provider.Resource) provider.Declared {
	d := provider.Declared{Ref: r.Ref, Name: r.Name, State: r.State, SMBIOSUUID: r.Identity.SMBIOSUUID, Blueprint: BuildableBlueprint}
	node, _, _ := strings.Cut(r.Ref.ID, "/")
	switch {
	case d.SMBIOSUUID == "":
		d.Skip = "it carries no SMBIOS UUID, so a record could not tell it from another guest later"
		return d
	case !nodeName.MatchString(r.Name):
		d.Skip = fmt.Sprintf("its name %q is not a DNS label, which the blueprint's hostname must be", r.Name)
		return d
	}
	var raw struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	_ = json.Unmarshal(r.Raw, &raw)
	cfg := raw.Config

	obsRaw, _ := json.Marshal(provider.Observation{Facts: r.Facts, Memory: r.Memory, State: r.State})
	obs, _ := plan.ObservedFromProvider(obsRaw)
	in := map[string]any{"pve_node": node, "hostname": r.Name}
	if v, ok := obs.Values[plan.ObservedCPUCores]; ok {
		in["cores"] = int(math.Round(v))
	}
	if v, ok := obs.Values[plan.ObservedMemoryMiB]; ok {
		in["memory_mb"] = int(math.Round(v))
	}
	if v, ok := obs.Values[plan.ObservedBootDiskGiB]; ok {
		in["disk_gb"] = int(math.Round(v))
	} else if len(r.Facts.Disks) > 1 {
		d.Notes = append(d.Notes, "it has several disks; disk_gb is left out, so their sizes are not compared")
	}
	if ConfigString(cfg, "sockets") != "" && ConfigString(cfg, "sockets") != "1" {
		d.Notes = append(d.Notes, "it has several CPU sockets; cores is their total, and a rebuild would put them on one socket")
	}
	cpu := ConfigString(cfg, "cpu")
	cpu, _, _ = strings.Cut(cpu, ",")
	cpu = strings.TrimPrefix(cpu, "cputype=")
	if cpu == "" {
		cpu = "kvm64" // what Proxmox runs a guest whose config names no CPU with
	}
	in["cpu"] = cpu
	for n := 0; n <= 3; n++ {
		key := fmt.Sprintf("net%d", n)
		if v := ConfigString(cfg, key); v != "" {
			in[key] = withoutMAC(v)
		}
	}
	if _, more := in["net1"]; more {
		d.Notes = append(d.Notes, "it has more than one network card; accrete v0.1 builds machines with one, so it can be kept but not rebuilt")
	}
	if t := linkedTemplate(cfg); t != "" {
		in["template_vmid"] = t
	} else {
		d.Unread = map[string]string{"template_vmid": TemplateNotRecorded}
	}
	d.Inputs = in
	return d
}

// withoutMAC drops a NIC's MAC address, which the clone gives each machine
// afresh: "virtio=BC:24:11:AA:BB:02,bridge=vmbr0,tag=30" is declared as
// "virtio,bridge=vmbr0,tag=30".
func withoutMAC(nic string) string {
	parts := strings.Split(nic, ",")
	if model, _, ok := strings.Cut(parts[0], "="); ok {
		parts[0] = model
	}
	return strings.Join(parts, ",")
}

// linkedTemplate is the template a linked clone's disks are based on, "" for
// a full clone (or several bases, which names no one template).
func linkedTemplate(cfg map[string]json.RawMessage) string {
	found := ""
	for key := range cfg {
		if !IsDiskKey(key) {
			continue
		}
		m := linkedBase.FindStringSubmatch(ConfigString(cfg, key))
		if m == nil {
			continue
		}
		if found != "" && found != m[1] {
			return ""
		}
		found = m[1]
	}
	return found
}
