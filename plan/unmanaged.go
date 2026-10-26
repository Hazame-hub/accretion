package plan

// NamesNotChecked is the limit stated when the provider's guests could not be
// listed: a machine this plan creates was not checked against a guest of the
// same name that nothing accounts for.
const NamesNotChecked = "the provider's guests were not checked for one already carrying a name this plan would create"

// unmanagedNotes says when the guests were asked for and could not be read,
// and the plan creates something.
func unmanagedNotes(p *Plan, world Snapshot) {
	if world.Unmanaged != nil || world.UnmanagedError == "" {
		return
	}
	for _, e := range p.Entries {
		if e.Action == ActionCreate {
			p.Limits = append(p.Limits, NamesNotChecked+": "+world.UnmanagedError)
			return
		}
	}
}
