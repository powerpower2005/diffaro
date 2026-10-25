package debugbundle

import "diffaro/internal/domain"

// summary is bundle.json. Every field is structured: codes, states, counts,
// diagnostics and pseudonymized names. Free text (reasons, actions, skip
// reasons, limitations), values and digests are left out, because they can
// hold names, paths or content.
type summary struct {
	Schema         string            `json:"schema"`
	Level          string            `json:"level"`
	Generator      *domain.Generator `json:"generator,omitempty"`
	Flags          []string          `json:"flags"`
	Status         domain.Status     `json:"status"`
	ExitCode       int               `json:"exitCode"`
	Coverage       string            `json:"coverage"`
	ChangesFound   bool              `json:"changesFound"`
	Revisions      revisions         `json:"revisions"`
	Counts         counts            `json:"counts"`
	RenderBackends []backend         `json:"renderBackends"`
	Discovery      []discovery       `json:"discovery,omitempty"`
	Applications   []application     `json:"applications"`
	Issues         []issue           `json:"issues"`
	Trace          *trace            `json:"trace,omitempty"`
}

type revisions struct {
	Repository string `json:"repository,omitempty"`
	Base       string `json:"base,omitempty"`
	Head       string `json:"head,omitempty"`
	Mode       string `json:"mode,omitempty"`
}

type counts struct {
	Applications int            `json:"applications"`
	Execution    map[string]int `json:"execution"`
	State        map[string]int `json:"state"`
	IssueCodes   map[string]int `json:"issueCodes"`
	Resources    int            `json:"resources"`
	Limitations  int            `json:"limitations"`
}

type backend struct {
	Instance      string `json:"instance"`
	Kind          string `json:"kind"`
	Version       string `json:"version,omitempty"`
	Compatibility string `json:"compatibility,omitempty"`
	Verification  string `json:"verification"`
	HelmVersion   string `json:"helmVersion,omitempty"`
}

type discovery struct {
	Instance              string     `json:"instance"`
	Status                string     `json:"status"`
	Unaffected            int        `json:"unaffected,omitempty"`
	UnaffectedDescendants int        `json:"unaffectedDescendants,omitempty"`
	Decisions             []decision `json:"decisions"`
}

type decision struct {
	ID              *domain.ApplicationID `json:"id"`
	Selected        bool                  `json:"selected"`
	Selection       string                `json:"selection,omitempty"`
	InputsUnchanged bool                  `json:"inputsUnchanged,omitempty"`
}

type application struct {
	ID            *domain.ApplicationID `json:"id"`
	State         string                `json:"state"`
	Execution     string                `json:"execution"`
	FailureOrigin string                `json:"failureOrigin,omitempty"`
	Depth         int                   `json:"depth"`
	Resources     int                   `json:"resources"`
	Issues        []issue               `json:"issues,omitempty"`
}

type issue struct {
	Code           string                `json:"code"`
	ReasonCode     string                `json:"reasonCode,omitempty"`
	Stage          string                `json:"stage"`
	Side           string                `json:"side,omitempty"`
	SourcePosition int                   `json:"sourcePosition,omitempty"`
	Application    *domain.ApplicationID `json:"application,omitempty"`
	Repository     string                `json:"repository,omitempty"`
	Revision       string                `json:"revision,omitempty"`
	RequiredAccess *access               `json:"requiredAccess,omitempty"`
	Diagnostic     *domain.Diagnostic    `json:"diagnostic,omitempty"`
}

type access struct {
	System        string `json:"system"`
	Instance      string `json:"instance,omitempty"`
	CredentialEnv string `json:"credentialEnv,omitempty"`
	Resource      string `json:"resource,omitempty"`
	Action        string `json:"action"`
}

type trace struct {
	Dropped int    `json:"dropped"`
	Steps   []step `json:"steps"`
}

type step struct {
	Seq         int                   `json:"seq"`
	Application *domain.ApplicationID `json:"application,omitempty"`
	Side        string                `json:"side,omitempty"`
	Revision    string                `json:"revision,omitempty"`
	Walk        string                `json:"walk,omitempty"`
	Phase       string                `json:"phase"`
	Step        string                `json:"step"`
	Subject     string                `json:"subject,omitempty"`
	Outcome     string                `json:"outcome"`
	Code        string                `json:"code,omitempty"`
	Stage       string                `json:"stage,omitempty"`
	Status      int                   `json:"status,omitempty"`
	Diagnostic  *domain.Diagnostic    `json:"diagnostic,omitempty"`
	StartMS     int64                 `json:"startMs"`
	DurationMS  int64                 `json:"durationMs"`
}

func summarize(in Input, n *namer) summary {
	r := in.Report
	flags := in.Flags
	if flags == nil {
		flags = []string{}
	}
	s := summary{
		Schema: Schema, Level: in.Level, Generator: r.Generator, Flags: flags, Status: r.Status, ExitCode: in.ExitCode, Coverage: r.Coverage, ChangesFound: r.ChangesFound,
		Revisions:      revisions{Repository: n.name("repo", r.Revisions.Repository), Base: n.name("rev", r.Revisions.Base), Head: n.name("rev", r.Revisions.Head), Mode: string(r.Revisions.Mode)},
		Counts:         counts{Applications: len(r.Applications), Execution: map[string]int{}, State: map[string]int{}, IssueCodes: map[string]int{}, Limitations: len(r.Limitations)},
		RenderBackends: []backend{}, Applications: []application{}, Issues: []issue{},
	}
	for _, b := range r.RenderBackends {
		s.RenderBackends = append(s.RenderBackends, backend{Instance: n.name("instance", b.Instance), Kind: b.Kind, Version: b.Version, Compatibility: b.Compatibility, Verification: b.Verification, HelmVersion: b.HelmVersion})
	}
	for _, d := range r.Discovery {
		out := discovery{Instance: n.name("instance", d.Instance), Status: d.Status, Unaffected: d.Unaffected, UnaffectedDescendants: d.UnaffectedDescendants, Decisions: []decision{}}
		for _, decision := range d.Decisions {
			out.Decisions = append(out.Decisions, newDecision(decision, n))
		}
		s.Discovery = append(s.Discovery, out)
	}
	for _, app := range r.Applications {
		out := application{ID: n.id(app.ID), State: string(app.State), Execution: string(app.Execution), FailureOrigin: string(app.FailureOrigin), Depth: len(app.GraphPath), Resources: len(app.Resources)}
		for _, i := range app.Issues {
			out.Issues = append(out.Issues, newIssue(i, n))
		}
		s.Counts.Execution[string(app.Execution)]++
		s.Counts.State[string(app.State)]++
		s.Counts.Resources += len(app.Resources)
		s.Applications = append(s.Applications, out)
	}
	// issueCodes counts every distinct issue once: run-level issues and
	// Application issues that are not mirrored at run level.
	counted := map[string]bool{}
	for _, i := range r.Issues {
		s.Issues = append(s.Issues, newIssue(i, n))
		s.Counts.IssueCodes[i.Code]++
		counted[i.DedupeKey()] = true
	}
	for _, app := range r.Applications {
		for _, i := range app.Issues {
			if key := i.DedupeKey(); !counted[key] {
				counted[key] = true
				s.Counts.IssueCodes[i.Code]++
			}
		}
	}
	if len(in.Steps) > 0 || in.Dropped > 0 {
		s.Trace = &trace{Dropped: in.Dropped, Steps: []step{}}
		for _, t := range in.Steps {
			s.Trace.Steps = append(s.Trace.Steps, newStep(t, n))
		}
	}
	return s
}

func newDecision(d domain.DiscoveryDecision, n *namer) decision {
	return decision{ID: n.id(d.ID), Selected: d.Selected, Selection: d.Selection, InputsUnchanged: d.InputsUnchanged}
}

func newIssue(i domain.Issue, n *namer) issue {
	out := issue{Code: i.Code, ReasonCode: i.ReasonCode, Stage: i.Stage, Side: i.Side, SourcePosition: i.SourcePosition, Repository: n.name("repo", i.Repository), Revision: n.name("rev", i.Revision), Diagnostic: i.Diagnostic}
	if i.Application != nil {
		out.Application = n.id(*i.Application)
	}
	if a := i.RequiredAccess; a != nil {
		out.RequiredAccess = &access{System: a.System, Instance: n.name("instance", a.Instance), CredentialEnv: n.name("env", a.CredentialEnv), Resource: a.Resource, Action: a.Action}
	}
	return out
}

// newStep keeps a trace step's structure. Subjects of HTTP steps are fixed
// route templates and stay; other subjects name repositories, revisions,
// paths, projects or resources and become pseudonyms.
func newStep(t domain.TraceStep, n *namer) step {
	out := step{Seq: t.Seq, Side: t.Side, Revision: n.name("rev", t.Revision), Walk: t.Walk, Phase: t.Phase, Step: t.Step, Subject: t.Subject, Outcome: t.Outcome, Code: t.Code, Stage: t.Stage, Status: t.Status, Diagnostic: t.Diagnostic, StartMS: t.StartMS, DurationMS: t.DurationMS}
	if t.Phase != "http" {
		out.Subject = n.name("subject", t.Subject)
	}
	if t.Application != nil {
		out.Application = n.id(*t.Application)
	}
	return out
}
