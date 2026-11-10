package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/all"
)

const exportUsage = `accrete export hands machines accrete owns over to Terraform, through
Terraform's own import blocks. It never writes a Terraform state.

Usage:
  accrete export --terraform -f env.yaml [--state FILE] [--select REGEX] [--out FILE]
  accrete export --retire --terraform-state PATH -f env.yaml [--state FILE]

--terraform writes import blocks (default terraform-import.tf beside the
manifest, never overwritten) for the machines this state records, or those
whose resource/index or name --select matches. Then, in Terraform:

  terraform plan -generate-config-out=generated.tf   # Terraform writes the resources
  terraform apply                                    # it imports them; nothing is created

--retire, once Terraform's state records them and the manifest no longer
declares them, retires accrete's records of those machines, so each has one
owner again. A record whose machine the given state does not record, or whose
resource the manifest still declares, is kept and named.

Exit status: 0 done, 1 something was kept, 2 accrete could not start.
`

func runExport(args []string, stdout, stderr io.Writer, getenv func(string) string, stdin io.Reader) int {
	fs := flag.NewFlagSet("accrete export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	toTF := fs.Bool("terraform", false, "write Terraform import blocks for the machines")
	retire := fs.Bool("retire", false, "retire the records of machines Terraform now owns")
	tfState := fs.String("terraform-state", "", "with --retire: the Terraform state that now records them")
	file := fs.String("f", "", "the Environment manifest")
	statePath := fs.String("state", "", "the state file (default: accrete.state.json beside the manifest)")
	sel := fs.String("select", "", "export only machines whose resource/index or name matches")
	out := fs.String("out", "", "the import blocks file (default: terraform-import.tf beside the manifest)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" || *toTF == *retire || (*retire && *tfState == "") || fs.NArg() > 0 {
		_, _ = fmt.Fprint(stderr, exportUsage)
		return 2
	}
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "accrete: "+format+"\n", a...)
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
	driver, ok := all.Lookup(env.Spec.Provider.Type)
	if !ok || driver.Terraform == nil || driver.Terraform.Import == nil {
		return fail("provider %q maps no Terraform resources", env.Spec.Provider.Type)
	}
	dir := filepath.Dir(*file)
	if *statePath == "" {
		*statePath = filepath.Join(dir, "accrete.state.json")
	}
	var match *regexp.Regexp
	if *sel != "" {
		if match, err = regexp.Compile(*sel); err != nil {
			return fail("--select: %v", err)
		}
	}

	if *retire {
		owners, err := loadOwners(*tfState, stdin, driver)
		if err != nil {
			return fail("%v", err)
		}
		lock, err := lockState(*statePath, func(msg string) { _, _ = fmt.Fprintf(stderr, "accrete: %s\n", msg) })
		if err != nil {
			return fail("%v", err)
		}
		defer lock.release()
		state, err := loadState(*statePath)
		if err != nil {
			return fail("state: %v", err)
		}
		return retireHanded(env, &state, *statePath, owners, match, stdout, stderr)
	}

	state, err := loadState(*statePath)
	if err != nil {
		return fail("state: %v", err)
	}
	if *out == "" {
		*out = filepath.Join(dir, "terraform-import.tf")
	}
	var b strings.Builder
	b.WriteString("# Written by accrete export --terraform on " + time.Now().UTC().Format(time.RFC3339) + ".\n" +
		"# Terraform takes these machines over without creating anything:\n" +
		"#   terraform plan -generate-config-out=generated.tf\n" +
		"#   terraform apply\n" +
		"# Then remove them from the accrete manifest and run\n" +
		"#   accrete export --retire --terraform-state <the new state> -f " + filepath.Base(*file) + "\n" +
		"# Needs Terraform 1.5 or later (or OpenTofu 1.5).\n")
	n := 0
	for _, r := range state.Realizations {
		if !r.Current() || r.Intent == nil || r.Intent.Name != env.Name {
			continue
		}
		who := fmt.Sprintf("%s/%d", r.Intent.ResourceName, r.Intent.InstanceIndex)
		name, _ := r.IntentInputs["hostname"].(string)
		if match != nil && !match.MatchString(who) && !match.MatchString(name) {
			continue
		}
		typ, id, ok := driver.Terraform.Import(provider.Ref{Kind: provider.Kind(r.ResourceKind), ID: r.ResourceRefID})
		if !ok {
			_, _ = fmt.Fprintf(stdout, "left out %s (%s): Terraform cannot import it\n", who, r.ResourceRefID)
			continue
		}
		fmt.Fprintf(&b, "\nimport {\n  to = %s.%s\n  id = %q\n}\n", typ, tfName(r.Intent.ResourceName, r.Intent.InstanceIndex), id)
		n++
	}
	if n == 0 {
		return fail("no machine to export")
	}
	if err := writeNew(*out, b.String()); err != nil {
		return fail("%v", err)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s (%d import blocks). accrete still owns these machines until --retire; do not run accrete apply on them meanwhile.\n", *out, n)
	return 0
}

// tfName is a Terraform resource name for an instance: web, 0 → web_1.
var notTFName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func tfName(resource string, index int) string {
	s := notTFName.ReplaceAllString(resource, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "m_" + s
	}
	return fmt.Sprintf("%s_%d", s, index+1)
}

// retireHanded retires the records of machines Terraform now owns whose
// resource the manifest no longer declares. Anything else is kept.
func retireHanded(env envmanifest.Environment, state *State, path string, owners *terraformOwners, match *regexp.Regexp, stdout, stderr io.Writer) int {
	declared := map[string]bool{}
	for _, r := range env.Spec.Resources {
		declared[r.Name] = true
	}
	now := time.Now().UTC()
	retired, kept := 0, 0
	for i := range state.Realizations {
		r := &state.Realizations[i]
		if !r.Current() || r.Intent == nil || r.Intent.Kind != plan.IntentKind || r.Intent.Name != env.Name {
			continue
		}
		who := fmt.Sprintf("%s/%d", r.Intent.ResourceName, r.Intent.InstanceIndex)
		name, _ := r.IntentInputs["hostname"].(string)
		if match != nil && !match.MatchString(who) && !match.MatchString(name) {
			continue
		}
		addr := owners.owner(provider.Ref{Kind: provider.Kind(r.ResourceKind), ID: r.ResourceRefID})
		switch {
		case addr == "":
			if match != nil {
				_, _ = fmt.Fprintf(stdout, "kept %s (%s): %s does not record it\n", who, r.ResourceRefID, owners.source)
				kept++
			}
		case declared[r.Intent.ResourceName]:
			_, _ = fmt.Fprintf(stdout, "kept %s (%s): Terraform owns it (%s), but the manifest still declares resource %s; remove it there first\n",
				who, r.ResourceRefID, addr, r.Intent.ResourceName)
			kept++
		default:
			r.RetiredAt = &now
			if r.AssetID != nil {
				for g, members := range state.Groups {
					var keep = members[:0]
					for _, m := range members {
						if m != *r.AssetID {
							keep = append(keep, m)
						}
					}
					state.Groups[g] = keep
				}
			}
			_, _ = fmt.Fprintf(stdout, "retired %s (%s): handed to Terraform (%s)\n", who, r.ResourceRefID, addr)
			retired++
		}
	}
	if retired > 0 {
		if err := saveState(path, *state); err != nil {
			_, _ = fmt.Fprintf(stderr, "accrete: state: %v\n", err)
			return 2
		}
	}
	_, _ = fmt.Fprintf(stdout, "%d retired, %d kept\n", retired, kept)
	if kept > 0 {
		return 1
	}
	return 0
}
