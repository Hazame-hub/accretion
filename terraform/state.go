// Package terraform reads what a Terraform (or OpenTofu) state says exists,
// so that accrete can check it against the provider and stay out of the way
// of the resources Terraform owns. It only reads. Nothing in Accretion writes
// a state file: a handover goes through Terraform's own import and removed
// blocks.
//
// Two inputs are accepted:
//   - the output of `terraform show -json` (or `tofu show -json`), the
//     documented, stable format, and the one to use with a remote backend;
//   - a local state file (terraform.tfstate, format version 4), read as a
//     best effort since that format is Terraform's own internal one.
//
// Values Terraform marks sensitive are removed while reading, so they never
// reach a caller, an output or an Accretion state.
//
// The package imports only the standard library.
package terraform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Format names which of the two inputs a State was read from.
type Format string

const (
	// FormatShowJSON is the output of `terraform show -json`.
	FormatShowJSON Format = "show-json"
	// FormatRawState is a terraform.tfstate file.
	FormatRawState Format = "raw-state"
)

// State is what one Terraform state records.
type State struct {
	// Source is where it was read from (a path, or "-" for standard input).
	Source string `json:"source"`
	Format Format `json:"format"`
	// TerraformVersion is the version that last wrote it.
	TerraformVersion string `json:"terraform_version,omitempty"`
	// Serial and Lineage come from a raw state only; show -json drops them.
	Serial  int64  `json:"serial,omitempty"`
	Lineage string `json:"lineage,omitempty"`
	// Instances are the managed resource instances, sorted by address. Data
	// sources are left out: Terraform reads them and owns nothing there.
	Instances []Instance `json:"instances"`
}

// Instance is one managed resource instance.
type Instance struct {
	// Address is Terraform's own name for it, as `terraform state list`
	// prints it: module.vms["web"].proxmox_virtual_environment_vm.this.
	Address string `json:"address"`
	// Type is the resource type (proxmox_virtual_environment_vm).
	Type string `json:"type"`
	// Provider is the provider's source address
	// (registry.terraform.io/bpg/proxmox), "" when the state does not say.
	Provider string `json:"provider,omitempty"`
	// Attributes are the instance's values with every sensitive value
	// removed. Numbers are json.Number.
	Attributes map[string]any `json:"-"`
}

// ReadFile reads a state from path: a file in either format, a directory
// holding terraform.tfstate, or "-" for standard input (stdin).
func ReadFile(path string, stdin io.Reader) (State, error) {
	var src []byte
	var err error
	if path == "-" {
		src, err = io.ReadAll(stdin)
	} else {
		var info os.FileInfo
		if info, err = os.Stat(path); err == nil && info.IsDir() {
			file := filepath.Join(path, "terraform.tfstate")
			if _, serr := os.Stat(file); serr != nil {
				return State{}, fmt.Errorf("%s holds no terraform.tfstate; with a remote backend, run `terraform show -json > state.json` there and pass that file", path)
			}
			path = file
		}
		if err == nil {
			src, err = os.ReadFile(path)
		}
	}
	if err != nil {
		return State{}, err
	}
	s, err := Parse(src)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	s.Source = path
	return s, nil
}

// Parse reads a state in either format, telling them apart by their keys.
func Parse(src []byte) (State, error) {
	var head struct {
		FormatVersion    string          `json:"format_version"`
		Version          *int            `json:"version"`
		TerraformVersion string          `json:"terraform_version"`
		Serial           int64           `json:"serial"`
		Lineage          string          `json:"lineage"`
		Values           json.RawMessage `json:"values"`
		Resources        json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(src, &head); err != nil {
		return State{}, fmt.Errorf("not a Terraform state: %w", err)
	}
	switch {
	case head.FormatVersion != "":
		if !strings.HasPrefix(head.FormatVersion, "1.") {
			return State{}, fmt.Errorf("terraform show -json format %s is not one accrete reads (it reads 1.x)", head.FormatVersion)
		}
		s := State{Format: FormatShowJSON, TerraformVersion: head.TerraformVersion}
		if len(head.Values) > 0 && string(head.Values) != "null" {
			insts, err := parseShowJSON(head.Values)
			if err != nil {
				return State{}, err
			}
			s.Instances = insts
		}
		sortInstances(s.Instances)
		return s, nil
	case head.Version != nil:
		if *head.Version != 4 {
			return State{}, fmt.Errorf("state format version %d is not one accrete reads (it reads 4); pass the output of `terraform show -json` instead", *head.Version)
		}
		insts, err := parseRawState(head.Resources)
		if err != nil {
			return State{}, err
		}
		s := State{Format: FormatRawState, TerraformVersion: head.TerraformVersion, Serial: head.Serial, Lineage: head.Lineage, Instances: insts}
		sortInstances(s.Instances)
		return s, nil
	}
	return State{}, errors.New("not a Terraform state: it has neither format_version (terraform show -json) nor version (terraform.tfstate)")
}

func sortInstances(insts []Instance) {
	sort.Slice(insts, func(i, j int) bool { return insts[i].Address < insts[j].Address })
}

// decode reads JSON keeping numbers exact.
func decode(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(v)
}

// ── terraform show -json ───────────────────────────────────────────────────

type showModule struct {
	Resources    []showResource `json:"resources"`
	ChildModules []showModule   `json:"child_modules"`
}

type showResource struct {
	Address         string          `json:"address"`
	Mode            string          `json:"mode"`
	Type            string          `json:"type"`
	ProviderName    string          `json:"provider_name"`
	Values          json.RawMessage `json:"values"`
	SensitiveValues json.RawMessage `json:"sensitive_values"`
}

func parseShowJSON(values json.RawMessage) ([]Instance, error) {
	var v struct {
		RootModule showModule `json:"root_module"`
	}
	if err := json.Unmarshal(values, &v); err != nil {
		return nil, fmt.Errorf("terraform show -json: %w", err)
	}
	var out []Instance
	var walk func(m showModule) error
	walk = func(m showModule) error {
		for _, r := range m.Resources {
			if r.Mode != "managed" {
				continue
			}
			attrs := map[string]any{}
			if len(r.Values) > 0 && string(r.Values) != "null" {
				if err := decode(r.Values, &attrs); err != nil {
					return fmt.Errorf("%s: values: %w", r.Address, err)
				}
			}
			if len(r.SensitiveValues) > 0 && string(r.SensitiveValues) != "null" {
				var mask any
				if err := decode(r.SensitiveValues, &mask); err != nil {
					return fmt.Errorf("%s: sensitive_values: %w", r.Address, err)
				}
				stripMask(attrs, mask)
			}
			out = append(out, Instance{Address: r.Address, Type: r.Type, Provider: r.ProviderName, Attributes: attrs})
		}
		for _, c := range m.ChildModules {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return out, walk(v.RootModule)
}

// stripMask removes from v every value that mask marks true. mask mirrors
// v's shape: objects by key, lists by position.
func stripMask(v any, mask any) any {
	if b, ok := mask.(bool); ok && b {
		return nil
	}
	switch m := mask.(type) {
	case map[string]any:
		obj, ok := v.(map[string]any)
		if !ok {
			return v
		}
		for k, sub := range m {
			if _, has := obj[k]; !has {
				continue
			}
			if b, ok := sub.(bool); ok && b {
				delete(obj, k)
				continue
			}
			obj[k] = stripMask(obj[k], sub)
		}
	case []any:
		list, ok := v.([]any)
		if !ok {
			return v
		}
		for i, sub := range m {
			if i < len(list) {
				list[i] = stripMask(list[i], sub)
			}
		}
	}
	return v
}

// ── terraform.tfstate ───────────────────────────────────────────────────────

type rawResource struct {
	Module    string        `json:"module"`
	Mode      string        `json:"mode"`
	Type      string        `json:"type"`
	Name      string        `json:"name"`
	Provider  string        `json:"provider"`
	Instances []rawInstance `json:"instances"`
}

type rawInstance struct {
	IndexKey            any             `json:"index_key"`
	Attributes          json.RawMessage `json:"attributes"`
	SensitiveAttributes json.RawMessage `json:"sensitive_attributes"`
}

func parseRawState(resources json.RawMessage) ([]Instance, error) {
	var rs []rawResource
	if len(resources) > 0 {
		if err := decode(resources, &rs); err != nil {
			return nil, fmt.Errorf("terraform.tfstate: resources: %w", err)
		}
	}
	var out []Instance
	for _, r := range rs {
		if r.Mode != "managed" {
			continue
		}
		base := r.Type + "." + r.Name
		if r.Module != "" {
			base = r.Module + "." + base
		}
		for _, in := range r.Instances {
			addr := base + indexSuffix(in.IndexKey)
			attrs := map[string]any{}
			if len(in.Attributes) > 0 && string(in.Attributes) != "null" {
				if err := decode(in.Attributes, &attrs); err != nil {
					return nil, fmt.Errorf("%s: attributes: %w", addr, err)
				}
			}
			if err := stripPaths(attrs, in.SensitiveAttributes); err != nil {
				return nil, fmt.Errorf("%s: sensitive_attributes: %w", addr, err)
			}
			out = append(out, Instance{Address: addr, Type: r.Type, Provider: providerSource(r.Provider), Attributes: attrs})
		}
	}
	return out, nil
}

// indexSuffix is the [key] part of an instance address.
func indexSuffix(key any) string {
	switch k := key.(type) {
	case nil:
		return ""
	case string:
		return "[" + strconv.Quote(k) + "]"
	case json.Number:
		return "[" + k.String() + "]"
	}
	return fmt.Sprintf("[%v]", key)
}

// providerSource turns provider["registry.terraform.io/bpg/proxmox"] (with
// an optional .alias) into registry.terraform.io/bpg/proxmox.
func providerSource(p string) string {
	if i := strings.Index(p, `provider["`); i >= 0 {
		rest := p[i+len(`provider["`):]
		if j := strings.Index(rest, `"]`); j >= 0 {
			return rest[:j]
		}
	}
	return p
}

// stripPaths removes the values a raw state's sensitive_attributes names.
// Terraform writes each path as a list of steps, {"type":"get_attr",
// "value":"password"} or {"type":"index","value":{"value":0,"type":"number"}}.
// A path accrete cannot read removes the whole attribute it starts at, so an
// unreadable mark never lets a secret through.
func stripPaths(attrs map[string]any, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var paths []json.RawMessage
	if err := decode(raw, &paths); err != nil {
		return err
	}
	for _, p := range paths {
		var steps []struct {
			Type  string `json:"type"`
			Value any    `json:"value"`
		}
		if err := decode(p, &steps); err != nil || len(steps) == 0 {
			return fmt.Errorf("a path accrete cannot read: %s", p)
		}
		first, ok := steps[0].Value.(string)
		if steps[0].Type != "get_attr" || !ok {
			return fmt.Errorf("a path accrete cannot read: %s", p)
		}
		if len(steps) == 1 {
			delete(attrs, first)
			continue
		}
		if !removeAt(attrs[first], steps[1:]) {
			delete(attrs, first)
		}
	}
	return nil
}

// removeAt removes the value at steps under v; false when the path does not
// lead anywhere accrete can follow.
func removeAt(v any, steps []struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}) bool {
	s := steps[0]
	last := len(steps) == 1
	switch s.Type {
	case "get_attr":
		obj, ok := v.(map[string]any)
		name, isStr := s.Value.(string)
		if !ok || !isStr {
			return false
		}
		if _, has := obj[name]; !has {
			return true
		}
		if last {
			delete(obj, name)
			return true
		}
		if !removeAt(obj[name], steps[1:]) {
			delete(obj, name)
		}
		return true
	case "index":
		key := s.Value
		if m, ok := key.(map[string]any); ok {
			key = m["value"]
		}
		switch c := v.(type) {
		case []any:
			n, ok := key.(json.Number)
			if !ok {
				return false
			}
			i, err := strconv.Atoi(n.String())
			if err != nil || i < 0 {
				return false
			}
			if i >= len(c) {
				return true
			}
			if last {
				c[i] = nil
				return true
			}
			if !removeAt(c[i], steps[1:]) {
				c[i] = nil
			}
			return true
		case map[string]any:
			k, ok := key.(string)
			if !ok {
				return false
			}
			if last {
				delete(c, k)
				return true
			}
			if !removeAt(c[k], steps[1:]) {
				delete(c, k)
			}
			return true
		case nil:
			return true
		}
	}
	return false
}
