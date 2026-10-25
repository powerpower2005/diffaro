package domain

type DiscoveryDecision struct {
	ID       ApplicationID `json:"id"`
	Project  string        `json:"project"`
	Selected bool          `json:"selected"`
	Reason   string        `json:"reason"`
	// Selection and SelectionReason are set only by opt-in changed-path
	// selection: rendered, unaffected (inputs unchanged; not rendered) or
	// ineligible (independence could not be proven; rendered).
	Selection       string `json:"selection,omitempty"`
	SelectionReason string `json:"selectionReason,omitempty"`
	// InputsUnchanged proves that the PR changes none of the Application's
	// own inputs, even when it was rendered for another reason.
	InputsUnchanged bool `json:"inputsUnchanged,omitempty"`
}

type DiscoveryResult struct {
	Instance  string              `json:"instance"`
	Projects  []string            `json:"projects"`
	Status    string              `json:"status"`
	Decisions []DiscoveryDecision `json:"decisions"`
	// Unaffected counts candidates that changed-path selection did not render.
	Unaffected int `json:"unaffected,omitempty"`
	// UnaffectedDescendants counts rendered children that changed-path
	// selection proved identical on both revisions and did not render.
	UnaffectedDescendants int `json:"unaffectedDescendants,omitempty"`
}
