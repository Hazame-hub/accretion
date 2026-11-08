package proxmox

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// The Terraform resource types this provider maps. Each comes from the
// bpg/proxmox Terraform provider, whose state records what Proxmox reported
// at the last refresh.
const (
	tfVM   = "proxmox_virtual_environment_vm"
	tfVnet = "proxmox_sdn_vnet"
)

// TerraformTypes lists the Terraform resource types FromTerraform maps.
func TerraformTypes() []string { return []string{tfVM, tfVnet} }

// FromTerraform maps one Terraform resource instance to the claim it makes.
// ok is false for a type this provider does not map. It reads only the
// attributes it names and copies no other value, so nothing else Terraform
// keeps (a cloud-init password, a private key) can travel through it.
//
// Power is claimed only when withPower: bpg/proxmox records `started` as what
// the configuration asks for, which many configurations list in
// lifecycle.ignore_changes, so by default it says nothing about what runs.
func FromTerraform(typ string, attrs map[string]any, withPower bool) (claim provider.Claim, ok bool, err error) {
	switch typ {
	case tfVM:
		c, err := vmClaim(attrs, withPower)
		return c, true, err
	case tfVnet:
		c, err := vnetClaim(attrs)
		return c, true, err
	}
	return provider.Claim{}, false, nil
}

func vmClaim(a map[string]any, withPower bool) (provider.Claim, error) {
	vmid, ok := tfInt(a["vm_id"])
	if !ok || vmid <= 0 {
		return provider.Claim{}, fmt.Errorf("the state records no vm_id")
	}
	c := provider.Claim{Key: fmt.Sprintf("qemu/%d", vmid), Kind: provider.KindVM, Name: tfString(a["name"])}
	if tfBool(a["template"]) {
		c.Kind = provider.KindTemplate
	}
	add := func(name, value, change string) {
		c.Fields = append(c.Fields, provider.Field{Name: name, Value: value, Change: change})
	}
	if c.Name != "" {
		add("name", c.Name, provider.ChangeConfig)
	}
	if node := tfString(a["node_name"]); node != "" {
		add("node", node, "")
	}
	if sm := tfBlock(a["smbios"]); sm != nil {
		if forms := provider.UUIDForms(tfString(sm["uuid"])); forms != nil {
			c.Fields = append(c.Fields, provider.Field{Name: "smbios_uuid", Value: forms[0], Identity: true})
		}
	}
	if !withPower {
		c.Notes = append(c.Notes, "power is not compared: bpg/proxmox records `started` as the configuration asks, not what runs (--terraform-power compares it)")
	} else if v, ok := a["started"].(bool); ok {
		add("power", map[bool]string{true: "running", false: "stopped"}[v], provider.ChangePower)
	}
	if cpu := tfBlock(a["cpu"]); cpu != nil {
		cores, okC := tfInt(cpu["cores"])
		sockets, okS := tfInt(cpu["sockets"])
		if okC && cores > 0 {
			if !okS || sockets <= 0 {
				sockets = 1
			}
			add("cores", strconv.FormatInt(cores*sockets, 10), provider.ChangeConfig)
		}
	}
	if mem := tfBlock(a["memory"]); mem != nil {
		if mib, ok := tfInt(mem["dedicated"]); ok && mib > 0 {
			add("memory_mib", strconv.FormatInt(mib, 10), provider.ChangeConfig)
		}
	}
	if disks, ok := a["disk"].([]any); ok {
		var slots []string
		sizes := map[string]string{}
		for _, d := range disks {
			m, _ := d.(map[string]any)
			slot := tfString(m["interface"])
			gib, ok := tfInt(m["size"])
			if slot == "" || !ok {
				continue
			}
			slots = append(slots, slot)
			sizes[slot] = strconv.FormatInt(gib, 10)
		}
		sort.Strings(slots)
		for _, s := range slots {
			add(s+".gib", sizes[s], provider.ChangeDisk)
		}
		add("disks", strings.Join(slots, ","), provider.ChangeDisk)
		c.Open = append(c.Open, "disks")
	}
	if nics, ok := a["network_device"].([]any); ok {
		// bpg lists the cards in order, net0 first; a card it records as
		// disabled is not on the machine.
		var members []string
		for i, n := range nics {
			m, _ := n.(map[string]any)
			if v, ok := m["enabled"].(bool); ok && !v {
				continue
			}
			slot := fmt.Sprintf("net%d", i)
			members = append(members, slot)
			if b := tfString(m["bridge"]); b != "" {
				add(slot+".bridge", b, provider.ChangeConfig)
			}
			if mac := strings.ToLower(tfString(m["mac_address"])); mac != "" {
				add(slot+".mac", mac, provider.ChangeConfig)
			}
			if tag, ok := tfInt(m["vlan_id"]); ok {
				add(slot+".vlan", vlanValue(tag), provider.ChangeConfig)
			}
			if v, ok := m["disconnected"].(bool); ok {
				add(slot+".link", map[bool]string{true: "down", false: "up"}[v], provider.ChangeConfig)
			}
		}
		add("nics", strings.Join(members, ","), provider.ChangeConfig)
		c.Open = append(c.Open, "nics")
	}
	return c, nil
}

func vnetClaim(a map[string]any) (provider.Claim, error) {
	id := tfString(a["id"])
	if id == "" {
		return provider.Claim{}, fmt.Errorf("the state records no vnet id")
	}
	c := provider.Claim{Key: "vnet/" + id, Kind: provider.KindNetwork, Name: id}
	if z := tfString(a["zone"]); z != "" {
		c.Fields = append(c.Fields, provider.Field{Name: "zone", Value: z, Change: provider.ChangeConfig})
	}
	return c, nil
}

func vlanValue(tag int64) string {
	if tag <= 0 {
		return "none"
	}
	return strconv.FormatInt(tag, 10)
}

// ClaimFields reads off a discovered resource the fields FromTerraform
// claims, under the same key. ok is false for a resource no claim is about.
func ClaimFields(res provider.Resource) (provider.Observed, bool) {
	switch res.Ref.Kind {
	case provider.KindVM, provider.KindTemplate:
	case provider.KindNetwork:
		var row vnetRow
		_ = json.Unmarshal(res.Raw, &row)
		o := provider.Observed{Key: "vnet/" + res.Ref.ID, Fields: map[string]string{}}
		if row.Zone != "" {
			o.Fields["zone"] = row.Zone
		}
		if res.State == "unknown" {
			o.Notes = append(o.Notes, "the vnet is defined but not applied (SDN changes are pending)")
		}
		return o, true
	default:
		return provider.Observed{}, false
	}
	node, kind, vmid, err := splitRef(res.Ref.ID)
	if err != nil || kind != "qemu" {
		return provider.Observed{}, false
	}
	o := provider.Observed{Key: "qemu/" + vmid, Fields: map[string]string{
		"name": res.Name,
		"node": node,
	}, Members: map[string][]string{}}
	if res.Identity.SMBIOSUUID != "" {
		o.Fields["smbios_uuid"] = res.Identity.SMBIOSUUID
	}
	switch res.State {
	case "running", "stopped":
		o.Fields["power"] = res.State
	}

	var raw struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	_ = json.Unmarshal(res.Raw, &raw)
	if raw.Config == nil {
		// The config did not answer: only what the list row says is known.
		return o, true
	}
	if res.Facts.CPUCores > 0 {
		o.Fields["cores"] = strconv.Itoa(int(res.Facts.CPUCores))
	}
	if res.Facts.RAMBytes > 0 {
		o.Fields["memory_mib"] = strconv.FormatInt(res.Facts.RAMBytes>>20, 10)
	}
	if res.Pending != nil && res.Pending.RAMBytes > 0 {
		o.Notes = append(o.Notes, fmt.Sprintf("memory is set to %d MiB for the next restart", res.Pending.RAMBytes>>20))
	}
	var disks []string
	for _, d := range res.Facts.Disks {
		disks = append(disks, d.Mount)
		o.Fields[d.Mount+".gib"] = gibString(d.Total)
	}
	sort.Strings(disks)
	o.Fields["disks"] = strings.Join(disks, ",")
	o.Members["disks"] = disks
	var nics []string
	for k, v := range raw.Config {
		if !strings.HasPrefix(k, "net") || !isDigits(k[3:]) {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		nics = append(nics, k)
		o.Fields[k+".bridge"] = KVField(s, "bridge")
		for _, part := range strings.Split(s, ",") {
			if kv := strings.SplitN(part, "=", 2); len(kv) == 2 && strings.Count(kv[1], ":") == 5 {
				o.Fields[k+".mac"] = strings.ToLower(kv[1])
				break
			}
		}
		tag, _ := strconv.ParseInt(KVField(s, "tag"), 10, 64)
		o.Fields[k+".vlan"] = vlanValue(tag)
		o.Fields[k+".link"] = map[bool]string{true: "down", false: "up"}[KVField(s, "link_down") == "1"]
	}
	sort.Slice(nics, func(i, j int) bool {
		a, _ := strconv.Atoi(nics[i][3:])
		b, _ := strconv.Atoi(nics[j][3:])
		return a < b
	})
	o.Fields["nics"] = strings.Join(nics, ",")
	o.Members["nics"] = nics
	return o, true
}

// gibString writes a size in GiB, with decimals only when it is not whole.
func gibString(b uint64) string {
	const gib = 1 << 30
	if b%gib == 0 {
		return strconv.FormatUint(b/gib, 10)
	}
	return strconv.FormatFloat(float64(b)/gib, 'f', 2, 64)
}

// tfBlock is the first element of a nested block, which Terraform keeps as a
// list of objects; nil when there is none.
func tfBlock(v any) map[string]any {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	m, _ := list[0].(map[string]any)
	return m
}

func tfString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	}
	return ""
}

func tfInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil && f == float64(int64(f)) {
			return int64(f), true
		}
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case int:
		return int64(n), true
	case int64:
		return n, true
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

func tfBool(v any) bool {
	b, _ := v.(bool)
	return b
}
