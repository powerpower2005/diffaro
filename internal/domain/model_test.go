package domain

import "testing"

func TestExitCodes(t *testing.T) {
	for _, tt := range []struct {
		status  string
		changes bool
		want    int
	}{{"success", false, 0}, {"success", true, 1}, {"policy_violation", true, 2}, {"partial_failure", true, 3}, {"invalid_configuration", false, 4}, {"denied", false, 5}, {"failed", false, 6}, {"cancelled", false, 7}} {
		r := Report{Status: Status(tt.status), ChangesFound: tt.changes}
		if got := r.ExitCode(); got != tt.want {
			t.Errorf("%s: got %d want %d", tt.status, got, tt.want)
		}
	}
}

func TestSetAggregateStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		start     string
		cancelled bool
		failed    int
		denied    int
		useful    bool
		want      string
	}{
		{"no failures keeps status", "success", false, 0, 0, true, "success"},
		{"no failures keeps earlier status", "invalid_configuration", false, 0, 0, false, "invalid_configuration"},
		{"cancelled wins", "success", true, 2, 2, true, "cancelled"},
		{"useful results make partial failure", "success", false, 1, 1, true, "partial_failure"},
		{"all denied", "success", false, 2, 2, false, "denied"},
		{"mixed failures", "success", false, 2, 1, false, "failed"},
	} {
		r := Report{Status: Status(tc.start)}
		r.SetAggregateStatus(tc.cancelled, tc.failed, tc.denied, tc.useful)
		if string(r.Status) != tc.want {
			t.Errorf("%s: status %q, want %q", tc.name, r.Status, tc.want)
		}
	}
}

func TestIssueDedupeKeyDistinguishesEveryActionableField(t *testing.T) {
	app := ApplicationID{Instance: "lab", Namespace: "argocd", Name: "api"}
	other := ApplicationID{Instance: "lab", Namespace: "argocd", Name: "worker"}
	base := Issue{SourcePosition: 1, Side: "head", Code: "render_failed", ReasonCode: "timeout", Stage: "render", Reason: "render timed out", Action: "retry", Repository: "example/charts", Revision: "abc", Application: &app, RequiredAccess: &AccessRequirement{System: "argocd", Action: "get", Target: "applications"}}
	copied := base
	copied.Application = &ApplicationID{Instance: "lab", Namespace: "argocd", Name: "api"}
	if base.DedupeKey() != copied.DedupeKey() {
		t.Fatal("identical issues must share a key")
	}
	for name, mutate := range map[string]func(*Issue){
		"source position": func(i *Issue) { i.SourcePosition = 2 },
		"side":            func(i *Issue) { i.Side = "base" },
		"code":            func(i *Issue) { i.Code = "revision_invalid" },
		"reason code":     func(i *Issue) { i.ReasonCode = "credential_missing" },
		"stage":           func(i *Issue) { i.Stage = "discovery" },
		"reason":          func(i *Issue) { i.Reason = "other reason" },
		"action":          func(i *Issue) { i.Action = "fix values" },
		"repository":      func(i *Issue) { i.Repository = "example/values" },
		"revision":        func(i *Issue) { i.Revision = "def" },
		"application":     func(i *Issue) { i.Application = &other },
		"required access": func(i *Issue) {
			i.RequiredAccess = &AccessRequirement{System: "argocd", Action: "get", Target: "projects"}
		},
	} {
		changed := base
		mutate(&changed)
		if changed.DedupeKey() == base.DedupeKey() {
			t.Errorf("%s difference was collapsed", name)
		}
	}
}

func TestJoinGraphPath(t *testing.T) {
	path := []ApplicationID{{"lab", "argocd", "root"}, {"lab", "argocd", "child"}}
	if got := JoinGraphPath(path, " → "); got != "lab/argocd/root → lab/argocd/child" {
		t.Fatalf("got %q", got)
	}
	if got := JoinGraphPath(nil, " -> "); got != "" {
		t.Fatalf("empty path rendered %q", got)
	}
}
