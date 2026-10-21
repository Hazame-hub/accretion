// Command accrete plans an accretion/v1alpha1 Environment manifest against a
// local state file and what the provider it names reports now.
//
//	accrete plan -f env.yaml [--state accrete.state.json] [-o text|json]
//
// It reads and never writes: it sends the provider GET requests only, no
// machine is changed, and the state file is not rewritten. The plan comes
// from the planner Sybil Continuum's API uses, so a line that is unknown
// there is unknown here, and a machine the provider does not confirm is
// unknown here even when its record alone would say no-op.
//
// The token comes from the file or environment variable the manifest's
// spec.provider names. It is never printed, logged or written anywhere.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/all"
)

const usage = `accrete plans an Environment manifest; it changes nothing.

Usage:
  accrete plan -f env.yaml [--state FILE] [-o text|json] [--offline]

The manifest is apiVersion accretion/v1alpha1. Its spec.provider names the
provider, its endpoint, and the file (token_file) or environment variable
(token_env) holding the API token. --state defaults to accrete.state.json
beside the manifest; a missing state means nothing is built yet.

Exit status: 0 the plan has no errors, 1 it has errors, 2 accrete could not
plan (bad flags, bad manifest or state, no token).
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] != "plan" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("accrete plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "", "the Environment manifest (YAML)")
	statePath := fs.String("state", "", "the state file (default: accrete.state.json beside the manifest)")
	output := fs.String("o", "text", "output: text or json")
	offline := fs.Bool("offline", false, "do not contact the provider; recorded machines plan as unknown")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long the provider may take to answer, in all")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *file == "" || fs.NArg() > 0 || (*output != "text" && *output != "json") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "accrete: "+format+"\n", a...)
		return 2
	}

	src, err := os.ReadFile(*file)
	if err != nil {
		return fail("%v", err)
	}
	env, err := envmanifest.ParseStandalone(src)
	if err != nil {
		return fail("%s: %v", *file, err)
	}
	ref := env.Spec.Provider
	driver, ok := all.Lookup(ref.Type)
	if !ok {
		types := all.Types()
		sort.Strings(types)
		return fail("%s: accrete knows no provider of type %q; it knows %s", *file, ref.Type, strings.Join(types, ", "))
	}
	dir := filepath.Dir(*file)
	if *statePath == "" {
		*statePath = filepath.Join(dir, "accrete.state.json")
	}
	state, err := loadState(*statePath)
	if err != nil {
		return fail("state: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	world := &fileWorld{ctx: ctx, state: state, env: env, blueprints: driver.Blueprints(), now: time.Now}
	if *offline {
		world.notAsked = "--offline"
	} else {
		token, err := ref.Token(dir, getenv, stderr)
		if err != nil {
			return fail("%s: %v", *file, err)
		}
		session, err := driver.Provider.Connect(ctx, provider.Config{
			Endpoint: ref.Endpoint, Params: map[string]any{"api_token": token}, VerifyTLS: ref.VerifyTLS,
		})
		if err != nil {
			return fail("%s: spec.provider: %v", *file, err)
		}
		defer session.Close()
		world.session = session
	}

	p := plan.Build(env, world)
	world.confirm(&p)
	return emit(stdout, *output, p, world.Notes)
}

// apiPlan is the API's plan answer for one Environment: the plan, its summary
// and whether it is valid.
type apiPlan struct {
	plan.Plan
	Summary string `json:"summary"`
	OK      bool   `json:"ok"`
	// Notes are accrete's own: what it could not read. The API has no such
	// field, and a client that does not know it ignores it.
	Notes []string `json:"notes,omitempty"`
}

func emit(w io.Writer, output string, p plan.Plan, notes []string) int {
	out := apiPlan{Plan: p, Summary: p.Summary(), OK: p.OK(), Notes: notes}
	if output == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return 2
		}
	} else {
		printPlan(w, out)
	}
	if !out.OK {
		return 1
	}
	return 0
}

// printPlan prints one instance per line with its reason, the networks and
// firewalls, then what is wrong, what was not assessed and what to weigh.
// The layout follows `sybil env plan`. Limits are printed every time on
// purpose: a no-op that silently skipped comparing is the line somebody acts
// on.
func printPlan(w io.Writer, p apiPlan) {
	fmt.Fprintf(w, "Environment/%s\n", p.Environment)
	for _, e := range p.Entries {
		line := fmt.Sprintf("  %-8s %s/%d", e.Action, e.Resource, e.Index)
		if e.ResourceRefID != "" {
			line += "  (" + e.ResourceRefID + ")"
		}
		if e.Reason != "" {
			line += "  " + e.Reason
		}
		fmt.Fprintln(w, line)
		for _, d := range e.Diff {
			fmt.Fprintf(w, "             %s: %s -> %s\n", d.Input, show(d.From), show(d.To))
		}
		if e.Apply != "" {
			fmt.Fprintf(w, "             apply: %s (%s)\n", e.Apply, e.LifecycleBlueprint)
		} else if e.Blocked != "" {
			fmt.Fprintf(w, "             apply does nothing: %s\n", e.Blocked)
		}
		if pl := e.Placement; pl != nil {
			fmt.Fprintf(w, "             node: %s  %s\n", pl.Node, pl.Reason)
		}
	}
	for _, n := range p.Networks {
		line := fmt.Sprintf("  %-8s network %s", n.Action, n.Name)
		if n.Reason != "" {
			line += "  " + n.Reason
		}
		fmt.Fprintln(w, line)
	}
	for _, f := range p.Firewalls {
		line := fmt.Sprintf("  %-8s firewall %s/%d", f.Action, f.Resource, f.Index)
		if f.Reason != "" {
			line += "  " + f.Reason
		}
		fmt.Fprintln(w, line)
	}
	for _, msg := range p.Errors {
		fmt.Fprintf(w, "  error: %s\n", msg)
	}
	for _, msg := range p.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", msg)
	}
	for _, msg := range p.Limits {
		fmt.Fprintf(w, "  note: %s\n", msg)
	}
	for _, msg := range p.Notes {
		fmt.Fprintf(w, "  accrete: %s\n", msg)
	}
	fmt.Fprintf(w, "  %s\n", p.Summary)
}

func show(v any) string {
	if v == nil {
		return "(unset)"
	}
	return fmt.Sprint(v)
}
