package meshapi

// UpdateBuiltin names the binary a node's image or install provides — the
// floor its release state falls back to.
const UpdateBuiltin = "builtin"

// UpdateStatus is GET PathUpdate: one node's release state.
type UpdateStatus struct {
	Enabled bool `json:"enabled"`
	// Running is the version of the process answering. Current names what
	// the node starts (UpdateBuiltin or a staged version) and LastGood what
	// it returns to.
	Running  string         `json:"running"`
	Current  string         `json:"current"`
	LastGood string         `json:"last_good"`
	Pending  *UpdatePending `json:"pending,omitempty"`
	Staged   []string       `json:"staged"`
	// Engines is the installed version of each engine the node's models use,
	// where it can be read; absent when it cannot say.
	Engines map[string]string `json:"engines,omitempty"`
}

// UpdatePending is an activated release that has not yet confirmed itself.
type UpdatePending struct {
	Version  string `json:"version"`
	Attempts int    `json:"attempts"`
	// Deadline (RFC 3339) is when the node returns to its last good release
	// unless every backend that was healthy at activation is healthy again.
	// Only the node running Version knows it.
	Deadline string `json:"deadline,omitempty"`
}

// UpdateRequest is the body of PathUpdateStage and PathUpdateActivate.
type UpdateRequest struct {
	Version        string `json:"version"`
	AllowDowngrade bool   `json:"allow_downgrade,omitempty"`
}
