package main

import (
	"fmt"
	"io"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/all"
	"github.com/hazame-hub/accretion/terraform"
)

// terraformOwners is what a Terraform state owns, for accrete to keep out of:
// one resource has one owner. Read from the state with --terraform-state.
type terraformOwners struct {
	source string
	// byKey maps a claim key (qemu/201) to the Terraform address owning it.
	byKey map[string]string
	// byName maps a machine's name to the Terraform address owning it.
	byName map[string]string
	key    func(provider.Ref) string
}

// loadOwners reads the machines a Terraform state owns.
func loadOwners(path string, stdin io.Reader, driver all.Driver) (*terraformOwners, error) {
	if driver.Terraform == nil {
		return nil, fmt.Errorf("--terraform-state: this provider maps no Terraform resources")
	}
	st, err := terraform.ReadFile(path, stdin)
	if err != nil {
		return nil, fmt.Errorf("--terraform-state: %w", err)
	}
	o := &terraformOwners{source: st.Source, byKey: map[string]string{}, byName: map[string]string{}, key: driver.Terraform.Key}
	for _, in := range st.Instances {
		c, ok, err := driver.Terraform.Claim(in.Type, in.Attributes, false)
		if !ok || err != nil || (c.Kind != provider.KindVM && c.Kind != provider.KindTemplate) {
			continue
		}
		o.byKey[c.Key] = in.Address
		if c.Name != "" {
			o.byName[c.Name] = in.Address
		}
	}
	return o, nil
}

// owner is the Terraform address owning the machine at ref, "" for none.
func (o *terraformOwners) owner(ref provider.Ref) string {
	if o == nil || ref.ID == "" {
		return ""
	}
	if k := o.key(ref); k != "" {
		return o.byKey[k]
	}
	return ""
}

// check makes every plan line that would act on a machine Terraform owns an
// error, so apply and destroy refuse the plan before sending anything:
//   - a line whose machine Terraform's state records (a record of accrete's
//     pointing at it, or a machine adopted by init before it was in the
//     state);
//   - a create, or a create left unknown, whose hostname is the name of a
//     machine Terraform owns: building it would put a second machine of that
//     name beside Terraform's.
func (o *terraformOwners) check(p *plan.Plan) {
	if o == nil {
		return
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		who := fmt.Sprintf("%s/%d", e.Resource, e.Index)
		if addr := o.owner(provider.Ref{Kind: provider.KindVM, ID: e.ResourceRefID}); addr != "" {
			e.Blocked = "Terraform owns this machine"
			p.Errors = append(p.Errors, fmt.Sprintf("%s (%s): Terraform owns this machine (%s in %s); accrete does not act on a machine another tool owns. Leave it to Terraform, or hand it over to accrete first",
				who, e.ResourceRefID, addr, o.source))
			continue
		}
		if e.Action != plan.ActionCreate && e.Action != plan.ActionUnknown {
			continue
		}
		if name, _ := e.Inputs["hostname"].(string); name != "" {
			if addr := o.byName[name]; addr != "" {
				e.Blocked = "Terraform owns a machine of this name"
				p.Errors = append(p.Errors, fmt.Sprintf("%s: a machine named %q is Terraform's (%s in %s); a second one of that name would have two owners. Choose another hostname",
					who, name, addr, o.source))
			}
		}
	}
}

// summary says, for a note, how many machines the state owns.
func (o *terraformOwners) summary() string {
	if len(o.byKey) == 1 {
		return fmt.Sprintf("1 machine is Terraform's (%s); accrete leaves it to Terraform", o.source)
	}
	return fmt.Sprintf("%d machines are Terraform's (%s); accrete leaves them to Terraform", len(o.byKey), o.source)
}
