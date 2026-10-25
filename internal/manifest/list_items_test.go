package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// podSpec renders a Deployment whose pod template holds the given
// containers, written as YAML flow sequences.
func podSpec(containers string) string {
	return deployment + "  template:\n    spec:\n      containers: " + containers + "\n"
}

func listChanges(t *testing.T, base, head string, redactor security.Redactor) []domain.Change {
	t.Helper()
	diff, _ := Compare(parsed(t, podSpec(base)), parsed(t, podSpec(head)), redactor)
	return diff[0].Changes
}

func describe(changes []domain.Change) string {
	var out []string
	for _, c := range changes {
		before, _ := json.Marshal(c.Before)
		after, _ := json.Marshal(c.After)
		out = append(out, fmt.Sprintf("%s %s %v %s->%s redacted=%t", c.Operation, c.Path, c.ItemNames, before, after, c.Redacted))
	}
	return strings.Join(out, "\n")
}

func TestNamedListItemsAreComparedByName(t *testing.T) {
	const (
		app     = `{name: app, image: app:v1, env: [{name: A, value: "1"}, {name: B, value: "2"}]}`
		sidecar = `{name: sidecar, image: proxy:v1}`
	)
	for _, tc := range []struct {
		name, base, head, want string
	}{
		{
			"appended env item is one add",
			"[" + app + "]",
			`[{name: app, image: app:v1, env: [{name: A, value: "1"}, {name: B, value: "2"}, {name: C, value: "3"}]}]`,
			`add /spec/template/spec/containers/0/env/2 [app C] null->{"name":"C","value":"3"} redacted=false`,
		},
		{
			"image field of a named container",
			"[" + app + ", " + sidecar + "]",
			"[" + app + `, {name: sidecar, image: proxy:v2}]`,
			`replace /spec/template/spec/containers/1/image [sidecar] "proxy:v1"->"proxy:v2" redacted=false`,
		},
		{
			"added item indexes head and removed item indexes base",
			"[" + sidecar + ", " + app + "]",
			"[" + app + `, {name: init, image: init:v1}]`,
			`add /spec/template/spec/containers/1 [init] null->{"image":"init:v1","name":"init"} redacted=false` + "\n" +
				`remove /spec/template/spec/containers/0 [sidecar] {"image":"proxy:v1","name":"sidecar"}->null redacted=false`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describe(listChanges(t, tc.base, tc.head, security.Redactor{})); got != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestListsWithoutUniqueNamesOrWithReorderStayWhole(t *testing.T) {
	for _, tc := range []struct{ name, base, head string }{
		{"reordered shared items", `[{name: a, image: a:v1}, {name: b, image: b:v1}]`, `[{name: b, image: b:v1}, {name: a, image: a:v2}]`},
		{"duplicate name", `[{name: a, image: a:v1}, {name: a, image: a:v0}]`, `[{name: a, image: a:v2}]`},
		{"item without name", `[{image: a:v1}]`, `[{image: a:v2}]`},
		{"scalar items", `[a, b]`, `[a, c]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes := listChanges(t, tc.base, tc.head, security.Redactor{})
			if len(changes) != 1 || changes[0].Path != "/spec/template/spec/containers" || changes[0].Operation != "replace" || changes[0].ItemNames != nil {
				t.Fatalf("list must stay one change:\n%s", describe(changes))
			}
		})
	}
}

func TestSensitiveListItemsDoNotExposeValues(t *testing.T) {
	// A field-level path under an env item would not carry its sensitive
	// name, so the item is reported whole with its value masked.
	changes := listChanges(t,
		`[{name: app, env: [{name: DB_PASSWORD, value: fictional-old}]}]`,
		`[{name: app, env: [{name: DB_PASSWORD, value: fictional-new}]}]`,
		security.Redactor{})
	got := describe(changes)
	if len(changes) != 1 || strings.Contains(got, "fictional") || !changes[0].Redacted || changes[0].Path != "/spec/template/spec/containers/0/env/0" {
		t.Fatalf("sensitive env value exposed or not redacted:\n%s", got)
	}
	// Added and removed items with sensitive names are masked as well.
	changes = listChanges(t, `[{name: app, env: []}]`, `[{name: app, env: [{name: API_TOKEN, value: fictional-token}]}]`, security.Redactor{})
	if got := describe(changes); strings.Contains(got, "fictional") || !changes[0].Redacted {
		t.Fatalf("added sensitive env value exposed:\n%s", got)
	}
	// A configured sensitive list path keeps one masked change.
	changes = listChanges(t, `[{name: a, image: a:v1}]`, `[{name: a, image: a:v2}]`, security.Redactor{Paths: []string{"/spec/template/spec/containers"}})
	if got := describe(changes); len(changes) != 1 || changes[0].Path != "/spec/template/spec/containers" || strings.Contains(got, "a:v2") {
		t.Fatalf("configured sensitive list was split or exposed:\n%s", got)
	}
}

func TestValuesListsAreComparedByName(t *testing.T) {
	base := map[string]any{"jobs": []any{map[string]any{"name": "sync", "schedule": "0 * * * *"}}}
	head := map[string]any{"jobs": []any{map[string]any{"name": "sync", "schedule": "*/5 * * * *"}}}
	changes := CompareValues("", base, head, security.Redactor{})
	if got := describe(changes); got != `replace /jobs/0/schedule [sync] "0 * * * *"->"*/5 * * * *" redacted=false` {
		t.Fatalf("values list not compared by name:\n%s", got)
	}
}

// TestListItemsKeyedByOtherFields verifies that Prometheus rules are
// identified by alert or record and ExternalSecret entries by secretKey.
func TestListItemsKeyedByOtherFields(t *testing.T) {
	rule := func(kind, name, expr string) map[string]any {
		return map[string]any{kind: name, "expr": expr}
	}
	group := func(rules ...any) map[string]any {
		return map[string]any{"spec": map[string]any{"groups": []any{map[string]any{"name": "batch", "rules": rules}}}}
	}
	changes := CompareValues("", group(rule("alert", "Stale", "a > 1"), rule("record", "job:rate", "sum(x)")), group(rule("alert", "Stale", "a > 2"), rule("record", "job:rate", "sum(x)")), security.Redactor{})
	if len(changes) != 1 || changes[0].Operation != "replace" || changes[0].Path != "/spec/groups/0/rules/0/expr" || fmt.Sprint(changes[0].ItemNames) != "[batch Stale]" || changes[0].Before != "a > 1" || changes[0].After != "a > 2" {
		t.Fatalf("alerting rule not compared by alert:\n%s", describe(changes))
	}
	entries := func(keys ...string) map[string]any {
		var items []any
		for _, key := range keys {
			items = append(items, map[string]any{"secretKey": key, "remoteRef": map[string]any{"key": "app", "property": key}})
		}
		return map[string]any{"spec": map[string]any{"data": items}}
	}
	changes = CompareValues("", entries("URL"), entries("URL", "ENDPOINT"), security.Redactor{})
	if len(changes) != 1 || changes[0].Operation != "add" || changes[0].Path != "/spec/data/1" || fmt.Sprint(changes[0].ItemNames) != "[ENDPOINT]" {
		t.Fatalf("ExternalSecret entry not compared by secretKey:\n%s", describe(changes))
	}
	// An item that sets both fields of a group has no single identity.
	ambiguous := group(map[string]any{"alert": "A", "record": "B", "expr": "1"})
	changed := group(map[string]any{"alert": "A", "record": "B", "expr": "2"})
	if changes = CompareValues("", ambiguous, changed, security.Redactor{}); len(changes) != 1 || changes[0].Path != "/spec/groups/0/rules" {
		t.Fatalf("ambiguous rule identity was not kept as one list change:\n%s", describe(changes))
	}
}

// A sensitive env name hides a literal value but not a secretKeyRef, and the
// change is marked redacted only when something was hidden.
func TestSensitiveEnvItemsShowSecretReferences(t *testing.T) {
	changes := listChanges(t, `[{name: app, env: []}]`, `[{name: app, env: [{name: SLACK_TOKEN, valueFrom: {secretKeyRef: {name: app-secrets, key: slack-token}}}]}]`, security.Redactor{})
	got := describe(changes)
	if len(changes) != 1 || changes[0].Redacted || !strings.Contains(got, "app-secrets") || !strings.Contains(got, "slack-token") {
		t.Fatalf("secretKeyRef hidden or change marked redacted:\n%s", got)
	}
	changes = listChanges(t, `[{name: app, env: []}]`, `[{name: app, env: [{name: SLACK_TOKEN, value: fictional-token}]}]`, security.Redactor{})
	if got := describe(changes); len(changes) != 1 || !changes[0].Redacted || strings.Contains(got, "fictional") {
		t.Fatalf("literal value exposed or not marked redacted:\n%s", got)
	}
	// A changed reference key is shown on both sides.
	changes = listChanges(t, `[{name: app, env: [{name: SLACK_TOKEN, valueFrom: {secretKeyRef: {name: app-secrets, key: old-key}}}]}]`, `[{name: app, env: [{name: SLACK_TOKEN, valueFrom: {secretKeyRef: {name: app-secrets, key: new-key}}}]}]`, security.Redactor{})
	if got := describe(changes); len(changes) != 1 || changes[0].Redacted || !strings.Contains(got, "old-key") || !strings.Contains(got, "new-key") {
		t.Fatalf("reference change not shown:\n%s", got)
	}
}
