package manifest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func hunkDeployment(env string) string {
	return `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"app:v1","env":[` + env + `]}]}}}}`
}

func onlyDiff(t *testing.T, base, head string, r security.Redactor) domain.ResourceDiff {
	t.Helper()
	diff, _ := Compare(parsed(t, base), parsed(t, head), r)
	if len(diff) != 1 {
		t.Fatalf("want one resource, got %+v", diff)
	}
	return diff[0]
}

// An env var added between two others reads with the item before and the
// item after it as context, under the path of the added item.
func TestLineHunkShowsTheNeighbouringItems(t *testing.T) {
	base := hunkDeployment(`{"name":"A","value":"1"},{"name":"C","value":"3"}`)
	head := hunkDeployment(`{"name":"A","value":"1"},{"name":"B","value":"2"},{"name":"C","value":"3"}`)
	d := onlyDiff(t, base, head, security.Redactor{})
	if len(d.LineHunks) != 1 {
		t.Fatalf("want one hunk, got %+v", d.LineHunks)
	}
	h := d.LineHunks[0]
	item := "            - name: "
	want := []string{
		" " + item + "A",
		"               value: \"1\"",
		"+" + item + "B",
		"+              value: \"2\"",
		" " + item + "C",
		"               value: \"3\"",
	}
	if h.Path != "/spec/template/spec/containers[app]/env[B]" || !reflect.DeepEqual(h.Lines, want) {
		t.Fatalf("hunk = %s %q, want the added item between its neighbours", h.Path, h.Lines)
	}
	if h.BaseStart != 12 || h.HeadStart != 12 {
		t.Fatalf("hunk starts at -%d +%d, want the YAML line of the item before", h.BaseStart, h.HeadStart)
	}
}

// An item inserted into a list without item keys starts at its own first
// line, not inside the item before it.
func TestLineHunkAlignsAnInsertedItemToItsFirstLine(t *testing.T) {
	rule := func(resource string) string {
		return `{"apiGroups":[""],"resources":["` + resource + `"],"verbs":["get","list"]}`
	}
	doc := func(rules ...string) string {
		return `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"api"},"rules":[` + strings.Join(rules, ",") + `]}`
	}
	d := onlyDiff(t, doc(rule("pods"), rule("services")), doc(rule("pods"), rule("secrets"), rule("services")), security.Redactor{})
	if len(d.LineHunks) != 1 {
		t.Fatalf("want one hunk, got %+v", d.LineHunks)
	}
	lines := d.LineHunks[0].Lines
	first := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "+") {
			first = i
			break
		}
	}
	if first < 0 || lines[first] != "+  - apiGroups:" || !strings.Contains(strings.Join(lines[first:], "\n"), "+      - secrets") {
		t.Fatalf("inserted item does not start at its first line: %q", lines)
	}
	if last := lines[len(lines)-1]; strings.HasPrefix(last, "+") {
		t.Fatalf("no context after the inserted item: %q", lines)
	}
}

// Context lines are redacted like changed values, and a Secret has no hunks.
func TestLineHunksAreRedactedAndSkipSecrets(t *testing.T) {
	base := hunkDeployment(`{"name":"MODE","value":"a"},{"name":"DB_PASSWORD","value":"fictional-db-password"}`)
	head := hunkDeployment(`{"name":"MODE","value":"b"},{"name":"DB_PASSWORD","value":"fictional-db-password"}`)
	d := onlyDiff(t, base, head, security.Redactor{})
	if len(d.LineHunks) == 0 || strings.Contains(fmt.Sprint(d.LineHunks), "fictional-db-password") || !strings.Contains(fmt.Sprint(d.LineHunks), security.Mask) {
		t.Fatalf("context leaked a secret or lost its mask: %+v", d.LineHunks)
	}
	secret := func(v string) string {
		return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"user":"` + v + `"}}`
	}
	if s := onlyDiff(t, secret("YQ=="), secret("Yg=="), security.Redactor{}); s.State != "changed" || s.LineHunks != nil {
		t.Fatalf("a Secret got line hunks: %+v", s)
	}
}

// Hunks are left out, and reports fall back to field changes, when the
// changed lines pass the bound or a field differs between two renders.
func TestLineHunksAreLeftOutPastTheirBounds(t *testing.T) {
	var many []string
	for i := 0; i < hunkMaxChanged; i++ {
		many = append(many, fmt.Sprintf(`{"name":"V%03d","value":"x"}`, i))
	}
	d := onlyDiff(t, hunkDeployment(""), hunkDeployment(strings.Join(many, ",")), security.Redactor{})
	if d.State != "changed" || d.LineHunks != nil {
		t.Fatalf("%d changed lines kept hunks", 2*hunkMaxChanged)
	}
	d = onlyDiff(t, hunkDeployment(strings.Join(many[:10], ",")), hunkDeployment(strings.Join(many[:11], ",")), security.Redactor{})
	if d.LineHunks == nil {
		t.Fatal("a small change lost its hunks")
	}
	u := `"` + domain.RenderUnstable + `"`
	d = onlyDiff(t, hunkDeployment(`{"name":"A","value":"a"},{"name":"S","value":`+u+`}`), hunkDeployment(`{"name":"A","value":"b"},{"name":"S","value":"fixed"}`), security.Redactor{})
	if d.LineHunks != nil || len(d.UnknownPaths) == 0 {
		t.Fatalf("a resource with uncompared fields kept hunks: %+v", d)
	}
}

// A long changed line is cut from shortly before its first difference.
func TestLineHunkLongLinesKeepTheirDifference(t *testing.T) {
	prefix := strings.Repeat("a", 3000)
	d := onlyDiff(t, hunkDeployment(`{"name":"A","value":"`+prefix+`-old"}`), hunkDeployment(`{"name":"A","value":"`+prefix+`-new"}`), security.Redactor{})
	text := strings.Join(d.LineHunks[0].Lines, "\n")
	if !strings.Contains(text, "-old") || !strings.Contains(text, "-new") || strings.Contains(text, strings.Repeat("a", hunkLineRunes+1)) {
		t.Fatalf("long line lost its difference or was not bounded: %.400q", text)
	}
}

func TestLineHunksAreDeterministic(t *testing.T) {
	base := hunkDeployment(`{"name":"A","value":"1"},{"name":"C","value":"3"}`)
	head := hunkDeployment(`{"name":"A","value":"2"},{"name":"B","value":"2"},{"name":"C","value":"4"}`)
	first := onlyDiff(t, base, head, security.Redactor{})
	for i := 0; i < 20; i++ {
		if again := onlyDiff(t, base, head, security.Redactor{}); !reflect.DeepEqual(first.LineHunks, again.LineHunks) {
			t.Fatal("line hunks differ between runs")
		}
	}
}

// A previous block longer than the context reads by its first lines and an
// elision; a next block cut at the context bound ends with an elision.
func TestLineHunkNamesALongPreviousBlock(t *testing.T) {
	long := func(name string) string {
		return `{"name":"` + name + `","valueFrom":{"secretKeyRef":{"key":"k","name":"s","optional":false}},"extra":{"a":{"b":{"c":{"d":"e"}}}}}`
	}
	base := hunkDeployment(long("A") + `,` + long("C"))
	head := hunkDeployment(long("A") + `,{"name":"B","value":"2"},` + long("C"))
	d := onlyDiff(t, base, head, security.Redactor{})
	if len(d.LineHunks) != 1 {
		t.Fatalf("want one hunk, got %+v", d.LineHunks)
	}
	lines := d.LineHunks[0].Lines
	text := strings.Join(lines, "\n")
	// The name key comes first, so the block's first line names it.
	if lines[0] != "             - name: A" || !strings.Contains(lines[2], "…") {
		t.Fatalf("a long previous block does not start with its first lines and an elision:\n%s", text)
	}
	// Only the two first lines and the elision, not the block's tail.
	if !strings.HasPrefix(lines[3], "+") {
		t.Fatalf("the context before the change is not 2 lines and an elision:\n%s", text)
	}
	if !strings.Contains(text, "+            - name: B") || !strings.HasSuffix(lines[len(lines)-1], "…") {
		t.Fatalf("the added item or the cut after it is not shown:\n%s", text)
	}
	if d.LineHunks[0].BaseStart != 12 {
		t.Fatalf("hunk starts at -%d, want the first line of the previous block", d.LineHunks[0].BaseStart)
	}
}

// Two changes around one long block read as one hunk that names the block
// once.
func TestLineHunksAroundALongBlockMerge(t *testing.T) {
	long := func(name string) string {
		return `{"name":"` + name + `","valueFrom":{"secretKeyRef":{"key":"k","name":"s"}},"extra":{"a":{"b":{"c":{"d":"e"}}}}}`
	}
	// C is longer than the context after B and the context before D together.
	longer := `{"name":"C","extra":{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"j":"k"}}}}}}}}}},"valueFrom":{"secretKeyRef":{"key":"k","name":"s"}}}`
	base := hunkDeployment(long("A") + `,` + longer + `,` + long("E"))
	head := hunkDeployment(long("A") + `,{"name":"B","value":"2"},` + longer + `,{"name":"D","value":"4"},` + long("E"))
	d := onlyDiff(t, base, head, security.Redactor{})
	text := ""
	for _, h := range d.LineHunks {
		text += strings.Join(h.Lines, "\n") + "\n"
	}
	if len(d.LineHunks) != 1 || strings.Count(text, "- name: C") != 1 || strings.Count(text, "…") < 2 || !strings.Contains(text, "+            - name: D") {
		t.Fatalf("changes around one block were split or named it twice (%d hunks):\n%s", len(d.LineHunks), text)
	}
}

// A value that changed while redaction hides it reads as a removed and an
// added masked line noted as hidden, where it is; an unchanged masked value
// stays context.
func TestLineHunkShowsHiddenChangesWhereTheyAre(t *testing.T) {
	base := hunkDeployment(`{"name":"DB_PASSWORD","value":"fictional-old"},{"name":"MODE","value":"a"},{"name":"TOKEN","value":"fictional-same"}`)
	head := hunkDeployment(`{"name":"DB_PASSWORD","value":"fictional-new"},{"name":"MODE","value":"b"},{"name":"TOKEN","value":"fictional-same"}`)
	d := onlyDiff(t, base, head, security.Redactor{})
	text := ""
	for _, h := range d.LineHunks {
		text += strings.Join(h.Lines, "\n") + "\n"
	}
	note := "  # " + domain.HiddenChange
	for _, want := range []string{"             - name: DB_PASSWORD\n-              value: '[REDACTED]'" + note + "\n+              value: '[REDACTED]'" + note + "\n", "-              value: a\n+              value: b\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("hunks lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "fictional-") || strings.Count(text, note) != 2 {
		t.Fatalf("hunks leaked a value or noted an unchanged masked value:\n%s", text)
	}
}

// The lines nested under a changed line stay with it as context, before the
// next block: a renamed item shows its unchanged body.
func TestLineHunkKeepsTheBodyOfAChangedLine(t *testing.T) {
	route := func(name string) string {
		return `{"name":"` + name + `","match":"m","services":[{"name":"api","port":80}]}`
	}
	doc := func(names ...string) string {
		items := []string{}
		for _, n := range names {
			items = append(items, route(n))
		}
		return `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"api"},"rules":[{"apiGroups":[""],"resources":["pods"],"verbs":["get"]}],"spec":{"routes":[` + strings.Join(items, ",") + `]}}`
	}
	d := onlyDiff(t, doc("a", "b", "c"), doc("a", "renamed", "c"), security.Redactor{})
	text := ""
	for _, h := range d.LineHunks {
		text += strings.Join(h.Lines, "\n") + "\n"
	}
	body := strings.Index(text, "+    - name: renamed\n")
	next := strings.Index(text, "     - name: c\n")
	if body < 0 || next < 0 || !strings.Contains(text[body:next], "       match: m\n") {
		t.Fatalf("the unchanged body of the renamed item is not kept before the next item:\n%s", text)
	}
}

// Two changes whose contexts meet read as one hunk without repeated lines.
func TestNearbyChangesShareOneHunk(t *testing.T) {
	base := hunkDeployment(`{"name":"A","value":"1"},{"name":"B","value":"2"},{"name":"C","value":"3"}`)
	head := hunkDeployment(`{"name":"A","value":"10"},{"name":"B","value":"2"},{"name":"C","value":"30"}`)
	d := onlyDiff(t, base, head, security.Redactor{})
	if len(d.LineHunks) != 1 {
		t.Fatalf("want one hunk, got %d: %+v", len(d.LineHunks), d.LineHunks)
	}
	text := strings.Join(d.LineHunks[0].Lines, "\n")
	if strings.Count(text, "- name: B") != 1 || !strings.Contains(text, "+              value: \"10\"") || !strings.Contains(text, "+              value: \"30\"") {
		t.Fatalf("nearby changes were split or repeated a line:\n%s", text)
	}
}

// Rules whose name repeats but whose alert is unique are named by alert in
// change paths, and line hunks name them the same way: a hunk path equals
// the named change path, and a hidden change inside such a rule keeps its
// hunk instead of losing it to a path the change does not have.
func TestLineHunksNameListItemsAsChangePaths(t *testing.T) {
	doc := func(expr, token string) string {
		return `{"apiVersion":"monitoring.coreos.com/v1","kind":"PrometheusRule","metadata":{"name":"api","namespace":"demo"},"spec":{"groups":[{"name":"g","rules":[` +
			`{"name":"dup","alert":"A","expr":"` + expr + `","annotations":{"token":"` + token + `"}},` +
			`{"name":"dup","alert":"B","expr":"b > 1"}]}]}}`
	}
	d := onlyDiff(t, doc("a > 1", "fictional-same"), doc("a > 2", "fictional-same"), security.Redactor{})
	if len(d.Changes) != 1 || len(d.LineHunks) != 1 {
		t.Fatalf("want one change and one hunk, got %+v %+v", d.Changes, d.LineHunks)
	}
	if want := domain.NamedPath(d.Changes[0].Path, d.Changes[0].ItemNames); want != "/spec/groups[g]/rules[A]/expr" || d.LineHunks[0].Path != want {
		t.Fatalf("hunk path %q, change path %q, want both /spec/groups[g]/rules[A]/expr", d.LineHunks[0].Path, want)
	}
	d = onlyDiff(t, doc("a > 1", "fictional-old"), doc("a > 1", "fictional-new"), security.Redactor{})
	if len(d.Changes) != 1 || !d.Changes[0].Hidden() {
		t.Fatalf("want one hidden change, got %+v", d.Changes)
	}
	text := ""
	for _, h := range d.LineHunks {
		text += h.Path + "\n" + strings.Join(h.Lines, "\n") + "\n"
	}
	if strings.Count(text, domain.HiddenChange) != 2 || !strings.Contains(text, "/spec/groups[g]/rules[A]/annotations/token") || strings.Contains(text, "fictional-") {
		t.Fatalf("hidden change inside an alert-named rule lost its hunk or leaked:\n%s", text)
	}
}
