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
	"strings"
	"time"

	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/all"
	"github.com/hazame-hub/accretion/terraform"
)

const verifyUsage = `accrete verify checks what a Terraform state says against what the provider
reports now, resource by resource. It sends GET requests only and writes
nothing: not the Terraform state, not an accrete state.

Usage:
  accrete verify --terraform-state PATH
                 (-f env.yaml | --provider TYPE --endpoint URL (--token-env VAR | --token-file F) [--verify-tls=false])
                 [--select REGEX] [--terraform-power] [-o text|json]

PATH is the output of "terraform show -json" (or "tofu show -json"), a
terraform.tfstate file, a directory holding one, or - for standard input:

  terraform show -json | accrete verify --terraform-state - --provider proxmox ...

Each resource Terraform records is one line: matches (every field it records
is what the provider reports), differs (with each field that does not, and
who changed it when the provider's record says), or unknown (with why).
Machines the provider runs that the state does not record are listed after.
Values Terraform marks sensitive are dropped while reading and never shown.

--select keeps only the addresses that match. --terraform-power also compares
the power state; bpg/proxmox records "started" as the configuration asks,
which is often in lifecycle.ignore_changes, so it is left out by default.

Exit status: 0 everything matches, 1 something differs or is unknown,
2 accrete could not verify (bad flags, an unreadable state, no token or a
refused one).
`

// verifyLine is the verdict on one resource Terraform records.
type verifyLine struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	// Verdict is "matches", "differs" or "unknown".
	Verdict string `json:"verdict"`
	// Ref is the provider's resource the line was checked against.
	Ref     string             `json:"ref,omitempty"`
	Diffs   []fieldDiff        `json:"diffs,omitempty"`
	Unread  []string           `json:"unread,omitempty"`
	Reason  string             `json:"reason,omitempty"`
	Notes   []string           `json:"notes,omitempty"`
	Changed *plan.ChangeAuthor `json:"changed_by,omitempty"`
	// PowerChanged is who changed the power state, when it differs.
	PowerChanged *plan.ChangeAuthor `json:"power_changed_by,omitempty"`
	identity     bool
}

// fieldDiff is one field the state and the provider disagree on. State or
// Provider is "" when that side has no such member (a card the state does
// not record).
type fieldDiff struct {
	Field    string `json:"field"`
	State    string `json:"state"`
	Provider string `json:"provider"`
	Detail   string `json:"detail,omitempty"`
	change   string
}

// stranger is a machine the provider runs that the state does not record.
type stranger struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type verifyReport struct {
	Source           string         `json:"source"`
	Format           string         `json:"format"`
	TerraformVersion string         `json:"terraform_version,omitempty"`
	Serial           int64          `json:"serial,omitempty"`
	Endpoint         string         `json:"endpoint"`
	Lines            []verifyLine   `json:"resources"`
	NotInState       []stranger     `json:"not_in_state"`
	NotVerified      map[string]int `json:"not_verified,omitempty"`
	Notes            []string       `json:"notes,omitempty"`
	Summary          string         `json:"summary"`
	OK               bool           `json:"ok"`
}

func runVerify(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("accrete verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tfState := fs.String("terraform-state", "", "terraform show -json output, a terraform.tfstate, a directory holding one, or -")
	file := fs.String("f", "", "take the provider from this manifest's spec.provider")
	ptype := fs.String("provider", "", "the provider's type")
	endpoint := fs.String("endpoint", "", "the provider's API, an https URL")
	tokenEnv := fs.String("token-env", "", "the environment variable holding the API token")
	tokenFile := fs.String("token-file", "", "the file holding the API token")
	verifyTLS := fs.Bool("verify-tls", true, "check the endpoint's certificate")
	sel := fs.String("select", "", "verify only the addresses matching this regular expression")
	withPower := fs.Bool("terraform-power", false, "also compare the power state the state records")
	output := fs.String("o", "text", "output: text or json")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long the whole command may take")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tfState == "" || fs.NArg() > 0 || (*output != "text" && *output != "json") || (*file == "") == (*ptype == "") {
		_, _ = fmt.Fprint(stderr, verifyUsage)
		return 2
	}
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "accrete: "+format+"\n", a...)
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

	var ref *envmanifest.ProviderRef
	dir := "."
	if *file != "" {
		src, err := os.ReadFile(*file)
		if err != nil {
			return fail("%v", err)
		}
		env, err := envmanifest.ParseStandalone(src)
		if err != nil {
			return fail("%s: %v", *file, err)
		}
		ref, dir = env.Spec.Provider, filepath.Dir(*file)
	} else {
		ref = &envmanifest.ProviderRef{Type: *ptype, Endpoint: *endpoint, VerifyTLS: *verifyTLS, TokenEnv: *tokenEnv, TokenFile: *tokenFile}
		if err := ref.Validate(); err != nil {
			return fail("%v", err)
		}
	}
	driver, ok := all.Lookup(ref.Type)
	if !ok {
		types := all.Types()
		sort.Strings(types)
		return fail("accrete knows no provider of type %q; it knows %s", ref.Type, strings.Join(types, ", "))
	}
	if driver.Terraform == nil {
		return fail("provider %q maps no Terraform resources", ref.Type)
	}
	state, err := terraform.ReadFile(*tfState, stdin)
	if err != nil {
		return fail("--terraform-state: %v", err)
	}

	rep := verifyReport{Source: state.Source, Format: string(state.Format), TerraformVersion: state.TerraformVersion, Serial: state.Serial,
		Endpoint: ref.Endpoint, NotInState: []stranger{}}
	if state.Format == terraform.FormatRawState {
		rep.Notes = append(rep.Notes, "read a terraform.tfstate file, whose format is Terraform's own; the output of `terraform show -json` is the stable one")
	}
	type claimed struct {
		inst  terraform.Instance
		claim provider.Claim
		err   error
	}
	var claims []claimed
	notVerified := map[string]int{}
	for _, inst := range state.Instances {
		if match != nil && !match.MatchString(inst.Address) {
			continue
		}
		c, ok, err := driver.Terraform.Claim(inst.Type, inst.Attributes, *withPower)
		if !ok {
			notVerified[inst.Type]++
			continue
		}
		claims = append(claims, claimed{inst: inst, claim: c, err: err})
	}
	if len(notVerified) > 0 {
		rep.NotVerified = notVerified
	}
	if len(claims) == 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the state records no resource accrete can verify; it maps %s", strings.Join(driver.Terraform.Types(), ", ")))
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	session, err := connect(ctx, ref, dir, driver, getenv, stderr)
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = session.Close() }()
	found, listErr := session.Discover(ctx)
	if listErr != nil && errors.Is(listErr, provider.ErrUnauthorized) {
		return fail("spec.provider: %s", explainErr(driver, listErr))
	}
	if listErr != nil && len(found) == 0 {
		return fail("the provider could not be read: %s", explainErr(driver, listErr))
	}
	if listErr != nil {
		rep.Notes = append(rep.Notes, "the provider's listing is incomplete: "+explainErr(driver, listErr))
	}
	observed := map[string]provider.Observed{}
	refs := map[string]provider.Resource{}
	byName := map[string][]string{}
	for _, r := range found {
		o, ok := driver.Terraform.Observe(r)
		if !ok {
			continue
		}
		observed[o.Key], refs[o.Key] = o, r
		byName[r.Name] = append(byName[r.Name], r.Ref.ID)
	}

	var hist provider.History
	hist, _ = session.(provider.History)
	claimedKeys := map[string]bool{}
	claimNotes := map[string]bool{}
	claimsNetworks := false
	for _, c := range claims {
		line := verifyLine{Address: c.inst.Address, Type: c.inst.Type}
		if c.err != nil {
			line.Verdict, line.Reason = "unknown", "the state's record cannot be read: "+c.err.Error()
			rep.Lines = append(rep.Lines, line)
			continue
		}
		claimedKeys[c.claim.Key] = true
		if c.claim.Kind == provider.KindNetwork {
			claimsNetworks = true
		}
		for _, n := range c.claim.Notes {
			claimNotes[n] = true
		}
		o, ok := observed[c.claim.Key]
		if !ok {
			line.Verdict = "unknown"
			line.Reason = fmt.Sprintf("the provider lists no %s %s", c.claim.Kind, c.claim.Key)
			if listErr != nil {
				line.Reason += ", and its listing is incomplete"
			} else {
				line.Reason += "; it may be gone, or the token may not see it"
			}
			if others := byName[c.claim.Name]; c.claim.Name != "" && len(others) > 0 {
				line.Reason += fmt.Sprintf(" (a resource named %s is at %s)", c.claim.Name, strings.Join(others, ", "))
			}
			rep.Lines = append(rep.Lines, line)
			continue
		}
		res := refs[c.claim.Key]
		line.Ref = res.Ref.ID
		line.Notes = append(line.Notes, o.Notes...)
		compareClaim(&line, c.claim, o)
		if line.Verdict == "differs" && !line.identity && hist != nil && (res.Ref.Kind == provider.KindVM || res.Ref.Kind == provider.KindTemplate) {
			attributeDiffs(ctx, &line, hist, res.Ref, driver)
		}
		rep.Lines = append(rep.Lines, line)
	}

	var cn []string
	for n := range claimNotes {
		cn = append(cn, n)
	}
	sort.Strings(cn)
	rep.Notes = append(rep.Notes, cn...)

	for _, r := range found {
		o, ok := driver.Terraform.Observe(r)
		if !ok || claimedKeys[o.Key] {
			continue
		}
		if r.Ref.Kind == provider.KindVM || (r.Ref.Kind == provider.KindNetwork && claimsNetworks) {
			rep.NotInState = append(rep.NotInState, stranger{Ref: r.Ref.ID, Name: r.Name, Kind: string(r.Ref.Kind)})
		}
	}
	if match != nil && len(rep.NotInState) > 0 {
		// The state was cut down by --select: what it leaves out is not news.
		rep.NotInState = []stranger{}
	}

	counts := map[string]int{}
	for _, l := range rep.Lines {
		counts[l.Verdict]++
	}
	rep.Summary = fmt.Sprintf("%d match, %d differ, %d unknown, %d not in the state", counts["matches"], counts["differs"], counts["unknown"], len(rep.NotInState))
	if n := len(rep.NotVerified); n > 0 {
		total := 0
		for _, k := range rep.NotVerified {
			total += k
		}
		rep.Summary += fmt.Sprintf(", %d not verified", total)
	}
	rep.OK = counts["differs"] == 0 && counts["unknown"] == 0

	if *output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 2
		}
	} else {
		printVerify(stdout, rep)
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// compareClaim sets line's verdict from the fields claim records and o reads.
func compareClaim(line *verifyLine, c provider.Claim, o provider.Observed) {
	what := "resource"
	if c.Kind == provider.KindVM || c.Kind == provider.KindTemplate {
		what = "machine"
	}
	skip := map[string]bool{}
	for _, set := range c.Open {
		skip[set] = true
		var want []string
		for _, f := range c.Fields {
			if f.Name == set && f.Value != "" {
				want = strings.Split(f.Value, ",")
			}
		}
		got, read := o.Members[set]
		if !read {
			line.Unread = append(line.Unread, set)
			continue
		}
		change := ""
		for _, f := range c.Fields {
			if f.Name == set {
				change = f.Change
			}
		}
		has := map[string]bool{}
		for _, m := range got {
			has[m] = true
		}
		wanted := map[string]bool{}
		for _, m := range want {
			wanted[m] = true
			if !has[m] {
				skip[m+"."] = true
				line.Diffs = append(line.Diffs, fieldDiff{Field: m, State: "present", Provider: "", change: change,
					Detail: m + " is in the state, not on the " + what})
			}
		}
		for _, m := range got {
			if !wanted[m] {
				line.Diffs = append(line.Diffs, fieldDiff{Field: m, State: "", Provider: "present", change: change,
					Detail: m + " is on the " + what + ", not in the state" + memberDetail(o, m)})
			}
		}
	}
	for _, f := range c.Fields {
		if skip[f.Name] || skip[prefixOf(f.Name)] {
			continue
		}
		got, read := o.Fields[f.Name]
		if !read {
			line.Unread = append(line.Unread, f.Name)
			continue
		}
		if got == f.Value {
			continue
		}
		d := fieldDiff{Field: f.Name, State: f.Value, Provider: got, change: f.Change}
		if f.Identity {
			line.identity = true
			d.Detail = "the " + what + " there is not the one the state records"
		}
		line.Diffs = append(line.Diffs, d)
	}
	switch {
	case len(line.Diffs) > 0:
		line.Verdict = "differs"
	case len(line.Unread) > 0:
		line.Verdict = "unknown"
		line.Reason = "the provider did not report " + strings.Join(line.Unread, ", ")
	default:
		line.Verdict = "matches"
	}
}

// prefixOf is "net1." for "net1.bridge", "" for a field without one.
func prefixOf(name string) string {
	if i := strings.Index(name, "."); i > 0 {
		return name[:i+1]
	}
	return ""
}

// memberDetail lists what the resource has under an unrecorded member.
func memberDetail(o provider.Observed, m string) string {
	var parts []string
	for k, v := range o.Fields {
		if strings.HasPrefix(k, m+".") && v != "" {
			parts = append(parts, strings.TrimPrefix(k, m+".")+"="+v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return " (" + strings.Join(parts, ", ") + ")"
}

// attributeDiffs says who made the differences, from the provider's record,
// with the same rule plan uses: never a guess.
func attributeDiffs(ctx context.Context, line *verifyLine, hist provider.History, ref provider.Ref, driver all.Driver) {
	changes, err := hist.Changes(ctx, provider.Ref{Kind: provider.KindVM, ID: ref.ID})
	if err != nil {
		line.Changed = &plan.ChangeAuthor{Unknown: "the provider's record of " + ref.ID + " could not be read: " + explainErr(driver, err)}
		return
	}
	seen := map[string]bool{}
	var kinds []string
	power := ""
	for _, d := range line.Diffs {
		switch {
		case d.change == provider.ChangePower:
			power = map[bool]string{true: "on", false: "off"}[d.State == "running"]
		case d.change != "" && !seen[d.change]:
			seen[d.change] = true
			kinds = append(kinds, d.change)
		}
	}
	if len(kinds) > 0 {
		line.Changed = byKind(changes, kinds)
	}
	if power != "" {
		line.PowerChanged = byPower(changes, power)
	}
}

func printVerify(w io.Writer, r verifyReport) {
	head := fmt.Sprintf("Terraform state %s (%s", r.Source, r.Format)
	if r.Serial > 0 {
		head += fmt.Sprintf(", serial %d", r.Serial)
	}
	if r.TerraformVersion != "" {
		head += ", written by Terraform " + r.TerraformVersion
	}
	_, _ = fmt.Fprintf(w, "%s) against %s\n", head, r.Endpoint)
	for _, l := range r.Lines {
		line := fmt.Sprintf("  %-8s %s", l.Verdict, l.Address)
		if l.Ref != "" {
			line += "  (" + l.Ref + ")"
		}
		if l.Reason != "" {
			line += "  " + l.Reason
		}
		_, _ = fmt.Fprintln(w, line)
		for _, d := range l.Diffs {
			switch {
			case d.Detail != "" && (d.State == "" || d.Provider == ""):
				_, _ = fmt.Fprintf(w, "             %s\n", d.Detail)
			case d.Detail != "":
				_, _ = fmt.Fprintf(w, "             %s: the state records %s, the provider reports %s; %s\n", d.Field, d.State, d.Provider, d.Detail)
			default:
				_, _ = fmt.Fprintf(w, "             %s: the state records %s, the provider reports %s\n", d.Field, show(d.State), show(d.Provider))
			}
		}
		for _, a := range []*plan.ChangeAuthor{l.Changed, l.PowerChanged} {
			if a == nil {
				continue
			}
			if a.By != "" {
				_, _ = fmt.Fprintf(w, "             changed by %s at %s (%s)\n", a.By, a.At.Format(time.RFC3339), a.What)
			} else {
				_, _ = fmt.Fprintf(w, "             changed by: unknown, %s\n", a.Unknown)
			}
		}
		if l.Verdict == "differs" && len(l.Unread) > 0 {
			_, _ = fmt.Fprintf(w, "             not reported by the provider: %s\n", strings.Join(l.Unread, ", "))
		}
		for _, n := range l.Notes {
			_, _ = fmt.Fprintf(w, "             note: %s\n", n)
		}
	}
	if len(r.NotInState) > 0 {
		_, _ = fmt.Fprintln(w, "not in the Terraform state:")
		for _, s := range r.NotInState {
			_, _ = fmt.Fprintf(w, "  %-8s %s  (%s)\n", s.Kind, s.Name, s.Ref)
		}
	}
	if len(r.NotVerified) > 0 {
		types := make([]string, 0, len(r.NotVerified))
		for t, n := range r.NotVerified {
			types = append(types, fmt.Sprintf("%s (%d)", t, n))
		}
		sort.Strings(types)
		_, _ = fmt.Fprintf(w, "  not verified (accrete does not map these types): %s\n", strings.Join(types, ", "))
	}
	for _, n := range r.Notes {
		_, _ = fmt.Fprintf(w, "  note: %s\n", n)
	}
	_, _ = fmt.Fprintf(w, "  %s\n", r.Summary)
}
