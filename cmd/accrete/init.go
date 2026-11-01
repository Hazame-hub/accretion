package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/all"
)

const initUsage = `accrete init --from-cluster writes a manifest and a state for the machines
a provider already runs, so they are declared and adopted without being
rebuilt. It sends GET requests only, then runs accrete plan on the result.

Usage:
  accrete init --from-cluster --provider TYPE --endpoint URL (--token-env VAR | --token-file F)
               [--name ENV] [--select REGEX] [--template VMID] [--verify-tls=false]
               [-f env.yaml] [--state FILE]

Machines named alike (web-1, web-2, web-3) with the same size and settings
become one resource with a count. A machine that carries no identity, or
whose name the blueprint cannot take, is left out and the reason printed.
An input the machine does not show (the template a full clone came from) is
never guessed: --template sets it for all of them, or the manifest leaves it
as a variable to fill in. init never overwrites a file.
`

func runInit(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("accrete init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromCluster := fs.Bool("from-cluster", false, "declare the machines the provider runs now")
	ptype := fs.String("provider", "", "the provider's type")
	endpoint := fs.String("endpoint", "", "the provider's API, an https URL")
	tokenEnv := fs.String("token-env", "", "the environment variable holding the API token")
	tokenFile := fs.String("token-file", "", "the file holding the API token, relative to the manifest")
	verifyTLS := fs.Bool("verify-tls", true, "check the endpoint's certificate")
	name := fs.String("name", "imported", "the environment's name")
	sel := fs.String("select", "", "declare only machines whose name matches this regular expression")
	template := fs.String("template", "", "the template the machines came from, when the provider does not say")
	file := fs.String("f", "env.yaml", "the manifest to write")
	statePath := fs.String("state", "", "the state to write (default: accrete.state.json beside the manifest)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long the whole command may take")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*fromCluster || *ptype == "" || *endpoint == "" || fs.NArg() > 0 {
		fmt.Fprint(stderr, initUsage)
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "accrete: "+format+"\n", a...)
		return 2
	}
	var match *regexp.Regexp
	if *sel != "" {
		re, err := regexp.Compile(*sel)
		if err != nil {
			return fail("--select: %v", err)
		}
		match = re
	}
	if *statePath == "" {
		*statePath = filepath.Join(filepath.Dir(*file), "accrete.state.json")
	}
	for _, p := range []string{*file, *statePath} {
		if _, err := os.Stat(p); err == nil {
			return fail("%s exists; init never overwrites a file", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail("%v", err)
		}
	}
	driver, ok := all.Lookup(*ptype)
	if !ok {
		types := all.Types()
		sort.Strings(types)
		return fail("accrete knows no provider of type %q; it knows %s", *ptype, strings.Join(types, ", "))
	}
	ref := &envmanifest.ProviderRef{Type: *ptype, Endpoint: *endpoint, VerifyTLS: *verifyTLS, TokenEnv: *tokenEnv, TokenFile: *tokenFile}
	if err := ref.Validate(); err != nil {
		return fail("%v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	session, err := connect(ctx, ref, filepath.Dir(*file), driver, getenv, stderr)
	if err != nil {
		return fail("%v", err)
	}
	defer session.Close()
	declarer, ok := session.(provider.Declarer)
	if !ok {
		return fail("provider %q cannot describe its machines as a manifest", *ptype)
	}
	found, err := declarer.Declare(ctx)
	if err != nil && len(found) == 0 {
		return fail("the provider could not be read: %s", explainErr(driver, err))
	}
	if err != nil {
		fmt.Fprintf(stdout, "note: the provider's listing is incomplete (%s); machines it did not list are not declared\n", explainErr(driver, err))
	}

	var machines []provider.Declared
	for _, d := range found {
		switch {
		case match != nil && !match.MatchString(d.Name):
			continue
		case d.Skip != "":
			fmt.Fprintf(stdout, "left out %s (%s): %s\n", d.Name, d.Ref.ID, d.Skip)
			continue
		}
		for k := range d.Unread {
			if k == "template_vmid" && *template != "" {
				d.Inputs[k] = *template
				delete(d.Unread, k)
			}
		}
		machines = append(machines, d)
	}
	if len(machines) == 0 {
		return fail("no machine to declare")
	}

	env, members, unread := groupMachines(*name, ref, machines)
	if err := env.Validate(); err != nil {
		return fail("the manifest init built is not valid: %v", err)
	}
	src, err := envmanifest.Marshal(env)
	if err != nil {
		return fail("%v", err)
	}
	header := "# Written by accrete init --from-cluster on " + time.Now().UTC().Format(time.RFC3339) + ".\n"
	for _, u := range unread {
		header += "# " + u + "\n"
	}
	state, err := adoptState(env, members, driver)
	if err != nil {
		return fail("%v", err)
	}
	if err := os.WriteFile(*file, append([]byte(header), src...), 0o644); err != nil {
		return fail("%v", err)
	}
	if err := saveState(*statePath, state); err != nil {
		return fail("state: %v", err)
	}
	fmt.Fprintf(stdout, "wrote %s (%d resources, %d machines) and %s\n", *file, len(env.Spec.Resources), len(machines), *statePath)
	for _, u := range unread {
		fmt.Fprintf(stdout, "to do: %s\n", u)
	}
	for _, d := range machines {
		for _, n := range d.Notes {
			fmt.Fprintf(stdout, "note: %s: %s\n", d.Name, n)
		}
	}
	fmt.Fprintln(stdout, "\nThe plan of what was written (machines that match it plan as unchanged):")
	return run(ctx, []string{"plan", "-f", *file, "--state", *statePath}, stdin, stdout, stderr, getenv)
}

func explainErr(driver all.Driver, err error) string {
	msg := err.Error()
	if driver.Explain != nil {
		if x := driver.Explain(err); x != "" {
			msg += " (" + x + ")"
		}
	}
	return msg
}

// numbered splits "web-12" into "web-" and 12. A number with a leading zero
// does not split: web-01 to web-09 could not be named by {{instance.number}}.
var numbered = regexp.MustCompile(`^(.*[^0-9])([1-9][0-9]*)$`)

// resourceName makes a resource name out of a machine's name.
var notNameChar = regexp.MustCompile(`[^a-z0-9-]+`)

func resourceName(s string) string {
	n := strings.Trim(notNameChar.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if n == "" {
		n = "machine"
	}
	return n
}

// groupMachines turns machines into resources. Machines whose names are one
// prefix numbered 1 to n, and whose inputs are otherwise identical, become one
// resource of count n; every other machine is a resource of its own. members
// maps resource name to its machines, by instance index.
func groupMachines(name string, ref *envmanifest.ProviderRef, machines []provider.Declared) (envmanifest.Environment, map[string][]provider.Declared, []string) {
	type group struct {
		prefix string
		key    string
		byNum  map[int]provider.Declared
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range machines {
		m := numbered.FindStringSubmatch(d.Name)
		if m == nil {
			continue
		}
		num, _ := strconv.Atoi(m[2])
		key := m[1] + "|" + d.Blueprint + "|" + powerOf(d) + "|" + inputsKey(d.Inputs)
		g, ok := groups[key]
		if !ok {
			g = &group{prefix: m[1], key: key, byNum: map[int]provider.Declared{}}
			groups[key] = g
			order = append(order, key)
		}
		g.byNum[num] = d
	}
	grouped := map[string]bool{}
	var resources []envmanifest.EnvironmentResource
	members := map[string][]provider.Declared{}
	used := map[string]int{}
	unique := func(n string) string {
		used[n]++
		if used[n] == 1 {
			return n
		}
		return fmt.Sprintf("%s-%d", n, used[n])
	}
	var unread []string
	unreadSeen := map[string]bool{}
	vars := map[string]any{}
	addResource := func(rname string, ds []provider.Declared, hostname string) {
		first := ds[0]
		inputs := map[string]any{}
		for k, v := range first.Inputs {
			inputs[k] = v
		}
		inputs["hostname"] = hostname
		// One variable per resource and input: machines of two resources
		// may well come from two templates.
		for input, why := range first.Unread {
			v := strings.TrimSuffix(input, "_vmid") + "_" + strings.ReplaceAll(rname, "-", "_")
			inputs[input] = "{{var." + v + "}}"
			vars[v] = ""
			msg := fmt.Sprintf("set variables.%s to the %s resource %s's machines came from (%s)", v, input, rname, why)
			if input == "template_vmid" {
				msg += ", or run init with --template"
			}
			if !unreadSeen[msg] {
				unreadSeen[msg] = true
				unread = append(unread, msg)
			}
		}
		r := envmanifest.EnvironmentResource{Name: rname, Count: len(ds), Blueprint: first.Blueprint, Inputs: inputs}
		if first.State == "stopped" {
			r.Power = "off"
		}
		resources = append(resources, r)
		members[rname] = ds
	}
	for _, key := range order {
		g := groups[key]
		n := len(g.byNum)
		if n < 2 {
			continue
		}
		consecutive := true
		for i := 1; i <= n; i++ {
			if _, ok := g.byNum[i]; !ok {
				consecutive = false
			}
		}
		if !consecutive {
			continue
		}
		ds := make([]provider.Declared, n)
		for i := 1; i <= n; i++ {
			ds[i-1] = g.byNum[i]
			grouped[g.byNum[i].Ref.ID] = true
		}
		addResource(unique(resourceName(g.prefix)), ds, g.prefix+"{{instance.number}}")
	}
	for _, d := range machines {
		if grouped[d.Ref.ID] {
			continue
		}
		addResource(unique(resourceName(d.Name)), []provider.Declared{d}, d.Name)
	}
	env := envmanifest.Environment{Name: name}
	env.Spec.Provider = ref
	env.Spec.Resources = resources
	if len(vars) > 0 {
		env.Spec.Variables = vars
	}
	return env, members, unread
}

func powerOf(d provider.Declared) string {
	if d.State == "stopped" {
		return "off"
	}
	return "on"
}

func inputsKey(in map[string]any) string {
	cp := map[string]any{}
	for k, v := range in {
		if k != "hostname" {
			cp[k] = v
		}
	}
	b, _ := json.Marshal(cp) // map keys are sorted
	return string(b)
}

// adoptState records each machine as adopted for its instance: tied to the
// identity it carries now, built from the inputs the manifest declares, and
// accepted when its power is as declared. A machine whose state is neither
// running nor stopped gets no verdict, and plans as unknown.
func adoptState(env envmanifest.Environment, members map[string][]provider.Declared, driver all.Driver) (State, error) {
	// The inputs a machine is recorded as built from are the ones a plan of
	// the manifest would launch it with, connector and defaults included,
	// so the next plan compares like with like.
	world := &fileWorld{ctx: context.Background(), state: State{Version: stateVersion}, env: env, blueprints: driver.Blueprints(),
		explain: driver.Explain, now: time.Now, notAsked: "init"}
	p := plan.Build(env, world)
	state := State{Version: stateVersion, Verdicts: map[uuid.UUID]string{}}
	integration := ProviderID(*env.Spec.Provider)
	for _, e := range p.Entries {
		ds := members[e.Resource]
		if e.Index >= len(ds) {
			return State{}, fmt.Errorf("init: no machine for %s/%d", e.Resource, e.Index)
		}
		d := ds[e.Index]
		asset := uuid.New()
		state.Realizations = append(state.Realizations, plan.Realization{
			ID: uuid.New(), IntegrationID: integration, AssetID: &asset,
			ResourceRefID: d.Ref.ID, ResourceKind: string(d.Ref.Kind), Managed: true,
			Intent:       &plan.RealizationIntent{Kind: plan.IntentKind, Name: env.Name, ResourceName: e.Resource, InstanceIndex: e.Index},
			IntentInputs: e.Inputs, MintedUUID: d.SMBIOSUUID, AdoptedAs: d.Blueprint,
		})
		if d.State == "running" || d.State == "stopped" {
			state.Verdicts[asset] = plan.AcceptanceAccepted
		}
	}
	return state, nil
}
