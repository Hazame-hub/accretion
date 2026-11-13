package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var instanceKey = regexp.MustCompile(`\[[^\]]*\]`)

// withoutKeys is a Terraform address with its instance keys removed:
// module.vms["web"].proxmox_virtual_environment_vm.this becomes
// module.vms.proxmox_virtual_environment_vm.this. A removed block's from
// takes only such an address, which names every instance under it.
func withoutKeys(addr string) string {
	// A key may hold "]" inside quotes; Terraform's own keys for these
	// resources do not, and an address that would is left whole.
	if strings.Contains(addr, `\"`) {
		return addr
	}
	return instanceKey.ReplaceAllString(addr, "")
}

// handoverFromTerraform is what Terraform needs to forget the machines
// accrete adopted, without destroying them. Never a state edit: a removed
// block with destroy = false, which terraform plan shows and terraform apply
// carries out. A removed block names every instance under an address, so it
// is written only when all of them were adopted; for the others, the
// command Terraform offers to forget one instance is printed for the
// operator to run, after reading it.
func handoverFromTerraform(addrs []string, o *terraformOwners) (tf string, stateRm []string) {
	groups := map[string][]string{}
	for _, a := range addrs {
		groups[withoutKeys(a)] = append(groups[withoutKeys(a)], a)
	}
	froms := make([]string, 0, len(groups))
	for f := range groups {
		froms = append(froms, f)
	}
	sort.Strings(froms)
	var b strings.Builder
	b.WriteString("# Written by accrete init --from-terraform. Terraform forgets these machines\n" +
		"# without destroying them: accrete owns them now.\n" +
		"#\n" +
		"# 1. Remove them from your Terraform configuration (the resource, or its\n" +
		"#    for_each/count entries), and put this file beside it.\n" +
		"# 2. terraform plan must say each \"will no longer be managed by Terraform\"\n" +
		"#    and show nothing to destroy. Then terraform apply.\n" +
		"# 3. accrete plan --terraform-state <the new state> must show them unchanged.\n" +
		"#\n" +
		"# Needs Terraform 1.7 or later (or OpenTofu 1.7).\n")
	blocks := 0
	for _, f := range froms {
		if o.instances[f] != len(groups[f]) {
			for _, a := range groups[f] {
				stateRm = append(stateRm, "terraform state rm '"+a+"'")
			}
			continue
		}
		fmt.Fprintf(&b, "\nremoved {\n  from = %s\n\n  lifecycle {\n    destroy = false\n  }\n}\n", f)
		blocks++
	}
	if blocks == 0 {
		return "", stateRm
	}
	return b.String(), stateRm
}

// writeNew writes a file that must not exist yet.
func writeNew(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
