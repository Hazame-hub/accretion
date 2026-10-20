package plan

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Variables.
//
// spec.variables names values once — a node, a size, a domain — and
// `{{var.name}}` uses them in any string of a resource's inputs, nested ones
// included, and in its image. A string that is nothing but one reference
// takes the variable's own type, so `cores: "{{var.cores}}"` is the number 4,
// not the text "4". Copying an Environment with other variables is how one
// manifest describes staging and production.
//
// An unknown variable is a plan error naming it, never an empty string: a
// machine called "web-" is not what anybody wrote.

var varRef = regexp.MustCompile(`\{\{var\.([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// substituteVars returns v with every {{var.name}} replaced, and the names it
// could not find, sorted.
func substituteVars(v any, vars map[string]any) (any, []string) {
	missing := map[string]bool{}
	out := substitute(v, vars, missing)
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	return out, names
}

func substitute(v any, vars map[string]any, missing map[string]bool) any {
	switch t := v.(type) {
	case string:
		if m := varRef.FindStringSubmatch(t); m != nil && m[0] == t {
			if val, ok := vars[m[1]]; ok {
				return val
			}
			missing[m[1]] = true
			return t
		}
		return varRef.ReplaceAllStringFunc(t, func(ref string) string {
			name := varRef.FindStringSubmatch(ref)[1]
			val, ok := vars[name]
			if !ok {
				missing[name] = true
				return ref
			}
			return fmt.Sprint(val)
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = substitute(x, vars, missing)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = substitute(x, vars, missing)
		}
		return out
	default:
		return v
	}
}

func unknownVarsError(resource string, names []string) string {
	refs := make([]string, len(names))
	for i, n := range names {
		refs[i] = "{{var." + n + "}}"
	}
	return fmt.Sprintf("resource %q uses %s, which spec.variables does not declare", resource, strings.Join(refs, ", "))
}
