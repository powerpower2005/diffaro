package domain

// PolicyRule is trusted profile configuration, never taken from repository content.
type PolicyRule struct {
	ID    string         `json:"id" yaml:"id"`
	Rule  string         `json:"rule" yaml:"rule"`
	Match PolicySelector `json:"match" yaml:"match"`
}

// Selectors are exact matches. Group defaults to the core API group; Kind is
// required. Empty optional selectors cover every value in the comparison scope.
type PolicySelector struct {
	Instance  string `json:"instance,omitempty" yaml:"instance,omitempty"`
	Project   string `json:"project,omitempty" yaml:"project,omitempty"`
	Group     string `json:"group" yaml:"group"`
	Kind      string `json:"kind" yaml:"kind"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
}

type PolicySummary struct {
	Evaluation  string            `json:"evaluation"`
	Scope       string            `json:"scope"`
	Rules       []PolicyRule      `json:"rules"`
	Violations  []PolicyViolation `json:"violations"`
	Limitations []string          `json:"limitations"`
}

type PolicyViolation struct {
	PolicyID    string         `json:"policyID"`
	RuleID      string         `json:"ruleID"`
	Application ApplicationID  `json:"application"`
	Resource    ResourceID     `json:"resource"`
	Base        PolicyRevision `json:"base"`
	Head        PolicyRevision `json:"head"`
	Evidence    PolicyEvidence `json:"evidence"`
	Reason      string         `json:"reason"`
	Action      string         `json:"action"`
}

// Sources retain every selected revision for a multi-source target. Absent
// Applications have no target revision; the report still records the PR pair.
type PolicyRevision struct {
	Repository string          `json:"repository,omitempty"`
	Revision   string          `json:"revision,omitempty"`
	Sources    []SourceSummary `json:"sources,omitempty"`
	Absent     bool            `json:"absent,omitempty"`
}

type PolicyEvidence struct {
	Path   string `json:"path"`
	Before bool   `json:"before"`
	After  bool   `json:"after"`
}
