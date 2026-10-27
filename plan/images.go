package plan

import (
	"fmt"
	"sort"
	"strings"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Images by name.
//
// A manifest said `template_vmid: "103"`: a Proxmox detail, and a number that
// means another template the day the first one is rebuilt. A resource can say
// `image: rocky-9` instead, or `image: rocky-9@sha256:…` to refuse anything but
// the template Sybil built from exactly that image.
//
// The planner stays pure: it says which lookups it needs (ImageQueries), the
// caller runs them against the provider and hands the answers back in
// Snapshot.Images, and Build writes each answer into the blueprint input that
// takes an image (BlueprintInput.TakesImage).

// ImageQuery is one lookup: an image name, through a selector, with the
// selector's dependency values (on Proxmox, the node the template is on).
type ImageQuery struct {
	Name     string
	Selector string
	// Params is the dependency values, as the canonical "k=v&k=v" the
	// selector's action is called with.
	Params string
}

// ImageAnswer is what the provider listed for an ImageQuery.
type ImageAnswer struct {
	// Value is what the selector stores (a template's VMID).
	Value any
	// SHA256 is the digest of the image Sybil built the object from, when
	// Sybil built it and verified the bytes; empty otherwise.
	SHA256 string
	// Err says why the image could not be resolved: none or several of that
	// name, or the provider could not be asked.
	Err string
	// Approval is the status of SHA256's image record, as the caller keeps
	// them: "approved", "planned" or "rejected", and "" when there is no
	// record. Read only when Snapshot.RequireApprovedImages is set.
	Approval string
}

// ImageApproved is the one Approval a plan builds from when approved images
// are required.
const ImageApproved = "approved"

// ImageNotPinned is stated when an image resolved by name only.
const ImageNotPinned = "some images are named without a digest: a template rebuilt under the same name is taken as is; " +
	"pin one with @sha256:… to refuse anything but the template built from that image"

// ParamsKey renders selector parameters canonically.
func ParamsKey(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}

// ParseParamsKey is the inverse of ParamsKey.
func ParseParamsKey(key string) map[string]string {
	out := map[string]string{}
	if key == "" {
		return out
	}
	for _, kv := range strings.Split(key, "&") {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// imageTarget is what resolving one instance's image needs.
func imageTarget(in Instance, image string, world Snapshot) (ImageQuery, string, error) {
	bp := blueprintByID(world.Blueprints, in.Blueprint)
	if bp == nil {
		// checkReferences already says so.
		return ImageQuery{}, "", nil
	}
	var input *BlueprintInput
	for i := range bp.Inputs {
		if bp.Inputs[i].TakesImage {
			input = &bp.Inputs[i]
		}
	}
	if input == nil {
		return ImageQuery{}, "", fmt.Errorf("resource %q: blueprint %q takes no image", in.Resource, in.Blueprint)
	}
	if _, both := in.Inputs[input.Name]; both {
		return ImageQuery{}, "", fmt.Errorf("resource %q sets both image and inputs.%s; image is the name for that input, so pick one",
			in.Resource, input.Name)
	}
	var spec *Selector
	for i := range world.Selectors {
		if world.Selectors[i].Name == input.Selector {
			spec = &world.Selectors[i]
		}
	}
	if spec == nil {
		return ImageQuery{}, "", fmt.Errorf("resource %q: connector %q cannot list images", in.Resource, connectorName(world))
	}
	params := map[string]string{}
	for _, dep := range spec.DependsOn {
		var v any
		found := false
		for _, bi := range bp.Inputs {
			if bi.Selector != dep {
				continue
			}
			if iv, ok := in.Inputs[bi.Name]; ok {
				v, found = iv, true
			} else if bi.Default != nil {
				v, found = bi.Default, true
			}
		}
		if !found || fmt.Sprint(v) == "" {
			return ImageQuery{}, "", fmt.Errorf("resource %q: finding image %q needs a %s", in.Resource, image, dep)
		}
		params[dep] = fmt.Sprint(v)
	}
	name, _ := envmanifest.SplitImage(image)
	return ImageQuery{Name: name, Selector: spec.Name, Params: ParamsKey(params)}, input.Name, nil
}

func connectorName(world Snapshot) string {
	if world.Connector == nil {
		return ""
	}
	return world.Connector.Name
}

// ImageQueries lists the lookups a plan of env needs, each once.
func ImageQueries(env envmanifest.Environment, w World) []ImageQuery {
	world := w.Read()
	images := resourceImages(env)
	if len(images) == 0 {
		return nil
	}
	instances, _ := Expand(env)
	// On the node placement chooses, as Build will, not the blueprint's
	// default one.
	place(env, instances, world)
	seen := map[ImageQuery]bool{}
	var out []ImageQuery
	for _, in := range instances {
		img := images[in.Resource]
		if img == "" || strings.Contains(img, "{{var.") || in.Unplaced {
			continue
		}
		q, _, err := imageTarget(in, img, world)
		if err != nil || q.Name == "" || seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
	}
	return out
}

func resourceImages(env envmanifest.Environment) map[string]string {
	out := map[string]string{}
	for _, r := range env.Spec.Resources {
		if r.Image != "" {
			// Variables first; an unknown one is reported by Expand.
			img, _ := substituteVars(r.Image, env.Spec.Variables)
			out[r.Name], _ = img.(string)
		}
	}
	return out
}

// resolveImages writes each instance's resolved image into its inputs, and
// returns what is wrong, one message per resource and problem, and what the
// operator should weigh.
func resolveImages(env envmanifest.Environment, instances []Instance, world Snapshot) (errs, warnings []string, unpinned bool) {
	images := resourceImages(env)
	if len(images) == 0 {
		return nil, nil, false
	}
	said := map[string]bool{}
	report := func(msg string) {
		if !said[msg] {
			said[msg] = true
			errs = append(errs, msg)
		}
	}
	warn := func(msg string) {
		if !said[msg] {
			said[msg] = true
			warnings = append(warnings, msg)
		}
	}
	built := builtInstances(env, world)
	for i := range instances {
		in := &instances[i]
		image := images[in.Resource]
		if image == "" || strings.Contains(image, "{{var.") || in.Unplaced {
			// Empty, a variable Expand already reported unknown, or no node
			// to look it up on.
			continue
		}
		if !envmanifest.ImagePattern.MatchString(image) {
			report(fmt.Sprintf("resource %q has image %q; want a name, optionally @sha256:<64 hex digits>", in.Resource, image))
			continue
		}
		q, input, err := imageTarget(*in, image, world)
		if err != nil {
			report(err.Error())
			continue
		}
		if input == "" {
			continue
		}
		ans, ok := world.Images[q]
		switch {
		case !ok:
			report(fmt.Sprintf("resource %q: image %q was not looked up", in.Resource, q.Name))
			continue
		case ans.Err != "":
			report(fmt.Sprintf("resource %q: image %q: %s", in.Resource, q.Name, ans.Err))
			continue
		}
		_, digest := envmanifest.SplitImage(image)
		if digest == "" {
			unpinned = true
		} else if ans.SHA256 == "" {
			report(fmt.Sprintf("resource %q: image %q is pinned to sha256:%s, but Sybil did not build that template "+
				"from a verified image, so its content is not known", in.Resource, q.Name, short(digest)))
			continue
		} else if !strings.EqualFold(ans.SHA256, digest) {
			report(fmt.Sprintf("resource %q: image %q is pinned to sha256:%s, but the template of that name was built "+
				"from sha256:%s", in.Resource, q.Name, short(digest), short(ans.SHA256)))
			continue
		}
		// Only for a machine still to be built: one that exists was built
		// already, and refusing its plan would also refuse removing it.
		if world.RequireApprovedImages && !built[instanceKey{in.Resource, in.Index}] {
			switch {
			case ans.SHA256 == "" && env.Spec.Images == envmanifest.ImagesKnownOrWarn:
				warn(fmt.Sprintf("resource %q: image %q: provenance UNKNOWN: Sybil did not build that template from a verified image, "+
					"so it cannot be approved; planned anyway because spec.images is %s", in.Resource, q.Name, envmanifest.ImagesKnownOrWarn))
			case ans.SHA256 == "":
				report(fmt.Sprintf("resource %q: image %q: provenance UNKNOWN: Sybil did not build that template from a verified image, "+
					"so it cannot be approved. Build it through Sybil, or set spec.images: %s to plan it with a warning",
					in.Resource, q.Name, envmanifest.ImagesKnownOrWarn))
				continue
			case ans.Approval != ImageApproved:
				status := ans.Approval
				if status == "" {
					status = "not registered"
				}
				report(fmt.Sprintf("resource %q: image %q (sha256:%s) is %s, not approved", in.Resource, q.Name, short(ans.SHA256), status))
				continue
			}
		}
		inputs := make(map[string]any, len(in.Inputs)+1)
		for k, v := range in.Inputs {
			inputs[k] = v
		}
		inputs[input] = imageValue(ans.Value, world, in.Blueprint, input)
		in.Inputs = inputs
		in.ImageSHA256 = ans.SHA256
	}
	return errs, warnings, unpinned
}

// builtInstances are the instances of env a current realization already
// stands for.
func builtInstances(env envmanifest.Environment, world Snapshot) map[instanceKey]bool {
	out := map[instanceKey]bool{}
	for _, r := range world.Realizations {
		if r.Current() && r.Intent != nil && r.Intent.Kind == IntentKind && r.Intent.Name == env.Name {
			out[instanceKey{r.Intent.ResourceName, r.Intent.InstanceIndex}] = true
		}
	}
	return out
}

// imageValue is the answer in the input's own type: a VMID listed as a number
// fills a string input as its decimal text.
func imageValue(v any, world Snapshot, blueprint, input string) any {
	if bp := blueprintByID(world.Blueprints, blueprint); bp != nil {
		for _, bi := range bp.Inputs {
			if bi.Name == input && bi.Type == "string" {
				switch n := v.(type) {
				case float64:
					return fmt.Sprintf("%.0f", n)
				default:
					return fmt.Sprint(v)
				}
			}
		}
	}
	return v
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12] + "…"
	}
	return digest
}
