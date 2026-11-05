package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/all"
)

// permissions prints what the token needs to apply env: one role, the paths
// to grant it on, how to do it, and, unless offline, what the token lacks on
// each path now. It sends GET requests only and reads no state: what apply
// needs follows from the machines the manifest declares.
//
// Exit status: 0 the token has every required privilege (or offline: the
// list was printed), 1 it lacks one, 2 accrete could not run, 3 some could not
// be checked. A missing privilege outranks an unchecked one.
func permissions(ctx context.Context, env envmanifest.Environment, driver all.Driver, dir string, offline bool,
	output, role string, timeout time.Duration, stdout, stderr io.Writer, getenv func(string) string) int {
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "accrete: "+format+"\n", a...)
		return 2
	}
	if driver.Permissions == nil {
		return fail("provider %q cannot say which privileges it needs", env.Spec.Provider.Type)
	}
	if role == "" {
		role = "accrete-" + env.Name
	}

	var notes []string
	var recipes []provider.Recipe
	instances, errs := plan.Expand(env)
	notes = append(notes, errs...)
	for _, in := range instances {
		if driver.Recipe == nil {
			break
		}
		rec, err := driver.Recipe(in.Blueprint, in.Inputs)
		if err != nil {
			notes = append(notes, fmt.Sprintf("resource %s: left out, accrete does not build it: %v", in.Resource, err))
			continue
		}
		recipes = append(recipes, rec)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var session provider.Session
	if !offline {
		s, err := connect(ctx, env.Spec.Provider, dir, driver, getenv, stderr)
		if err != nil {
			return fail("%v", err)
		}
		defer func() { _ = s.Close() }()
		session = s
	}
	p := driver.Permissions(ctx, session, role, recipes)
	p.Notes = append(notes, p.Notes...)

	if output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(p); err != nil {
			return 2
		}
	} else {
		printPermissions(stdout, env.Name, len(recipes), offline, p)
	}
	switch {
	case p.Lacking():
		return 1
	case !offline && p.Unchecked():
		return 3
	}
	return 0
}

func printPermissions(w io.Writer, envName string, machines int, offline bool, p provider.Permissions) {
	_, _ = fmt.Fprintf(w, "Environment %s: %d machine(s) to build. Role %s needs:\n\n", envName, machines, p.Role)
	for _, g := range p.Grants {
		status := "the token has them"
		switch {
		case offline:
			status = "not checked"
		case !g.Checked:
			status = "not checked: " + g.NotChecked
		case len(g.Missing) > 0:
			status = "the token lacks " + strings.Join(g.Missing, ", ")
		}
		if g.Optional {
			status += " (optional)"
		}
		_, _ = fmt.Fprintf(w, "  %s: %s\n      %s\n      %s\n", g.Path, g.Why, strings.Join(g.Privileges, " "), status)
	}
	switch {
	case offline:
		_, _ = fmt.Fprintln(w, "\nThe token was not checked (--offline).")
	case p.Lacking():
		_, _ = fmt.Fprintln(w, "\nThe token lacks privileges an apply needs: apply would stop part way. Grant them as below.")
	case p.Unchecked():
		_, _ = fmt.Fprintln(w, "\nSome paths could not be checked, so this does not say the token is enough.")
	default:
		_, _ = fmt.Fprintln(w, "\nThe token has every privilege an apply needs.")
	}
	if len(p.Setup) > 0 {
		_, _ = fmt.Fprintln(w, "\nHow to grant them:")
		for _, s := range p.Setup {
			_, _ = fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if len(p.Notes) > 0 {
		_, _ = fmt.Fprintln(w, "\nNotes:")
		for _, n := range p.Notes {
			_, _ = fmt.Fprintf(w, "  - %s\n", n)
		}
	}
}
