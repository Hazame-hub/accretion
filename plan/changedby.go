package plan

import "time"

// ChangeAuthor says who made the change a drifted line reports, from the
// provider's own record of the machine. It is filled by the embedder after
// Build, never by the planner: the planner reads no history.
//
// Exactly one of By and Unknown is set. Unknown says why the author cannot
// be named: the provider keeps no such record, the record was not readable,
// or no recorded change explains what was observed. A change the provider
// did not log is never attributed to whoever made the last one it did.
type ChangeAuthor struct {
	By string     `json:"by,omitempty"`
	At *time.Time `json:"at,omitempty"`
	// What is the provider's name for the recorded change.
	What    string `json:"what,omitempty"`
	Unknown string `json:"unknown,omitempty"`
}
