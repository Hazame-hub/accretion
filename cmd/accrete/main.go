// Command accrete plans and applies an accretion/v1alpha1 Environment
// manifest against a local state file and what the provider it names reports
// now.
//
//	accrete plan    -f env.yaml [--state accrete.state.json] [-o text|json]
//	accrete apply   -f env.yaml [--state accrete.state.json] [--auto-approve] [--forget-gone]
//	accrete destroy -f env.yaml [--state accrete.state.json] [--auto-approve] [--forget-gone]
//	accrete permissions -f env.yaml [-o text|json] [--offline] [--role NAME]
//	accrete init --from-cluster --provider TYPE --endpoint URL --token-env VAR [...]
//
// plan reads and never writes: it sends the provider GET requests only, no
// machine is changed, and the state file is not rewritten. The plan comes
// from the planner Sybil Continuum's API uses, so a line that is unknown
// there is unknown here, and a machine the provider does not confirm is
// unknown here even when its record alone would say no-op.
//
// apply and destroy print the same plan, ask before doing anything, and act
// on its lines one machine at a time, writing the state after every step so
// an interrupted run resumes where it stopped. See apply.go.
//
// permissions prints the one role the token needs to apply the manifest and
// the paths to grant it on, and checks the token against them. See
// permissions.go.
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

const usage = `accrete plans and applies an Environment manifest.

Usage:
  accrete plan    -f env.yaml [--state FILE] [-o text|json] [--offline]
  accrete apply   -f env.yaml [--state FILE] [--auto-approve] [--forget-gone]
  accrete destroy -f env.yaml [--state FILE] [--auto-approve] [--forget-gone]
  accrete permissions -f env.yaml [-o text|json] [--offline] [--role NAME]
  accrete init --from-cluster --provider TYPE --endpoint URL --token-env VAR [...]

The manifest is apiVersion accretion/v1alpha1. Its spec.provider names the
provider, its endpoint, and the file (token_file) or environment variable
(token_env) holding the API token. --state defaults to accrete.state.json
beside the manifest; a missing state means nothing is built yet.

plan changes nothing. apply builds what the plan says to create, resumes
what an interrupted apply left half-built, removes what the manifest no
longer declares and powers machines as declared. destroy removes every
machine the state records for the environment. Both print the plan and ask
for "yes" first, unless --auto-approve. A recorded machine the provider no
longer lists keeps its record (the token may just not see it) unless
--forget-gone.

permissions prints the role the token needs to apply the manifest, the
paths to grant it on and how, and checks what the token has there now
(GET requests only; --offline checks nothing).

init --from-cluster writes a manifest and a state for the machines the
provider already runs, then plans them (accrete init -h).

Exit status: 0 done (or nothing to do), 1 the plan has errors, the run was
declined, or a machine failed or was not done, 2 accrete could not plan or
start (bad flags, bad manifest or state, no token or a refused one, a state
locked by another run). permissions exits 1 when the token lacks a required
privilege and 3 when some could not be checked.
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) > 0 && args[0] == "init" {
		return runInit(ctx, args[1:], stdin, stdout, stderr, getenv)
	}
	if len(args) == 0 || (args[0] != "plan" && args[0] != "apply" && args[0] != "destroy" && args[0] != "permissions") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd := args[0]
	writes := cmd == "apply" || cmd == "destroy"
	fs := flag.NewFlagSet("accrete "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "", "the Environment manifest (YAML)")
	statePath := fs.String("state", "", "the state file (default: accrete.state.json beside the manifest)")
	output, offline, autoApprove, forgetGone, role := new(string), new(bool), new(bool), new(bool), new(string)
	defaultTimeout := 2 * time.Minute
	if writes {
		autoApprove = fs.Bool("auto-approve", false, "do not ask before acting")
		forgetGone = fs.Bool("forget-gone", false, "retire the records of machines to delete that the provider no longer lists")
		defaultTimeout = time.Hour
	} else {
		output = fs.String("o", "text", "output: text or json")
		offline = fs.Bool("offline", false, "do not contact the provider; recorded machines plan as unknown")
	}
	if cmd == "permissions" {
		role = fs.String("role", "", "the role's name (default: accrete- and the environment's name)")
	}
	timeout := fs.Duration("timeout", defaultTimeout, "how long the whole command may take")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *file == "" || fs.NArg() > 0 || (!writes && *output != "text" && *output != "json") {
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
	if cmd == "permissions" {
		return permissions(ctx, env, driver, dir, *offline, *output, *role, *timeout, stdout, stderr, getenv)
	}
	if writes {
		lock, err := lockState(*statePath, func(msg string) { fmt.Fprintf(stderr, "accrete: %s\n", msg) })
		if err != nil {
			return fail("%v", err)
		}
		defer lock.release()
	}
	state, err := loadState(*statePath)
	if err != nil {
		return fail("state: %v", err)
	}

	// destroy plans the environment as if it declared no machines: every
	// recorded one is then a delete line, under the same guards.
	planned := env
	if cmd == "destroy" {
		planned.Spec.Resources, planned.Spec.Networks = nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	world := &fileWorld{ctx: ctx, state: state, env: planned, blueprints: driver.Blueprints(), explain: driver.Explain, now: time.Now}
	var session provider.Session
	if *offline {
		world.notAsked = "--offline"
	} else {
		var err error
		session, err = connect(ctx, ref, dir, driver, getenv, stderr)
		if err != nil {
			return fail("%s: %v", *file, err)
		}
		defer session.Close()
		world.session = session
	}

	p := plan.Build(planned, world)
	world.confirm(&p)
	world.attribute(&p)
	// A refused credential is a configuration error, not a plan whose
	// machines are all unknown: nothing read after it is evidence.
	if world.Refused != nil {
		return fail("%s: spec.provider: %s", *file, world.why(world.Refused))
	}
	if !writes {
		return emit(stdout, *output, p, world.Notes)
	}
	a := &applier{
		ctx: ctx, in: stdin, out: stdout, cmd: cmd, autoApprove: *autoApprove,
		statePath: *statePath, state: state, env: env, session: session,
		provider: ProviderID(*ref), blueprints: driver.Blueprints(), recipe: driver.Recipe, explain: driver.Explain,
	}
	a.gone, a.forgetGone = world.GoneDeletes, *forgetGone
	return a.run(p, world.Notes)
}

// connect opens a session with the provider the manifest names, with the
// token its spec.provider points at. The token is never printed.
func connect(ctx context.Context, ref *envmanifest.ProviderRef, dir string, driver all.Driver, getenv func(string) string, stderr io.Writer) (provider.Session, error) {
	token, err := ref.Token(dir, getenv, stderr)
	if err != nil {
		return nil, err
	}
	if driver.CheckToken != nil {
		if err := driver.CheckToken(token); err != nil {
			return nil, fmt.Errorf("spec.provider: %w", err)
		}
	}
	session, err := driver.Provider.Connect(ctx, provider.Config{
		Endpoint: ref.Endpoint, Params: map[string]any{"api_token": token}, VerifyTLS: ref.VerifyTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("spec.provider: %w", err)
	}
	return session, nil
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
		if c := e.ChangedBy; c != nil {
			if c.By != "" {
				_, _ = fmt.Fprintf(w, "             changed by %s at %s (%s)\n", c.By, c.At.Format(time.RFC3339), c.What)
			} else {
				_, _ = fmt.Fprintf(w, "             changed by: unknown, %s\n", c.Unknown)
			}
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
