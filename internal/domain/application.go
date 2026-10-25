// Package domain is the shared kernel: Application and resource identity
// (identity.go, application.go), ports implemented by adapters (ports.go,
// sources.go: Renderer, DesiredRenderer, MultiSourceRenderer,
// SourceRevisionResolver, ProjectChecker, BackendInfoProvider,
// BackendVersionReader, GitProvider, ApplicationInventory), the structured
// error model (failure.go: Failure, Issue, RenderPartial; render_skip.go: RenderSkipped) with its issue and
// reason code catalog (codes.go) and limit errors and supported limit ranges
// (limits.go), the versioned Report types encoded by package reporter
// (report.go, change.go) with their typed status, state, execution,
// comparison, operation and failure origin values (states.go), and the
// opt-in diagnostic Trace, which never feeds the Report. It also
// holds helpers shared by manifest and reporter: list item keys
// (listitems.go: ItemKeys), the line edit script (linediff.go: LineEdits) and
// display helpers (display.go: NamedPath, DiffStart), and the repository path
// predicates shared by the source readers, local Helm, helmerror and preview
// (paths.go: SafeRelativePath, SafeRelativePathAllowingColon).
//
// It imports no other internal package. Keep provider-specific types out.
package domain

import (
	"bytes"
	"encoding/json"
)

// Spec stays opaque: adapters must preserve all renderer-specific inputs.
type Application struct {
	ID   ApplicationID
	Spec map[string]any
	// ObservedScopes maps group/kind to clusterScoped as observed in Argo CD's
	// stored status.resources. It is current-state metadata used only to infer
	// resource identity, never compared as target content.
	ObservedScopes map[string]bool
}

func (a Application) Source() map[string]any { s, _ := a.Spec["source"].(map[string]any); return s }
func (a Application) Project() string        { s, _ := a.Spec["project"].(string); return s }

// Sources returns spec.sources, or nil when it is absent or not a list. A
// non-empty list makes the Application multi-source; spec.source is then
// ignored.
func (a Application) Sources() []any { s, _ := a.Spec["sources"].([]any); return s }
func (a Application) Destination() map[string]any {
	d, _ := a.Spec["destination"].(map[string]any)
	return d
}
func String(m map[string]any, key string) string { s, _ := m[key].(string); return s }

type OwnedApplication struct {
	Application Application
	OwnerName   string
	OwnerUID    string
	// ManagesApplications reports whether Argo CD's stored status lists
	// Application or ApplicationSet resources; nil means it is unknown.
	ManagesApplications *bool
}

type ApplicationSelection struct {
	Source       ApplicationID   `json:"source"`
	Mode         string          `json:"mode"`
	OwnerUID     string          `json:"ownerUID,omitempty"`
	Applications []ApplicationID `json:"applications"`
	Boundary     string          `json:"boundary"`
}

func CloneSpec(spec map[string]any) map[string]any {
	b, _ := json.Marshal(spec)
	return DecodeSpec(b)
}

// DecodeSpec decodes JSON produced by marshaling a spec, preserving numbers
// exactly as CloneSpec does.
func DecodeSpec(b []byte) map[string]any {
	var copy map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	_ = d.Decode(&copy)
	return copy
}
