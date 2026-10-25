package yamlview

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func decode(t testing.TB, s string) any {
	t.Helper()
	var v any
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Numbers print as written, neither rounded nor quoted, and strings that
// would read as another type stay quoted.
func TestTextKeepsNumbersAndTypes(t *testing.T) {
	text, ok := Text(decode(t, `{"big":12345678901234567890,"ratio":1.50,"port":"8080","mode":"off","yes":"yes","time":"1:20","empty":"","on":true,"none":null,"script":"a\nb\n"}`))
	want := "big: 12345678901234567890\nempty: \"\"\nmode: \"off\"\nnone: null\n\"on\": true\nport: \"8080\"\nratio: 1.50\nscript: |\n  a\n  b\ntime: \"1:20\"\n\"yes\": \"yes\"\n"
	if !ok || text != want {
		t.Fatalf("Text = %q, want %q", text, want)
	}
}

// The field that names a list item comes first in the item, and only there:
// other objects, such as metadata, keep sorted keys.
func TestTextPutsTheNameOfAListItemFirst(t *testing.T) {
	value := decode(t, `{"metadata":{"name":"api","annotations":{"a":"b"}},"containers":[{"name":"app","args":["x"]}],"rules":[{"alert":"Down","expr":"up == 0"}]}`)
	text, _ := Text(value)
	for _, want := range []string{"metadata:\n  annotations:\n    a: b\n  name: api\n", "  - name: app\n    args:\n", "  - alert: Down\n    expr: up == 0\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Text lacks %q:\n%s", want, text)
		}
	}
	item, _ := ItemText(decode(t, `{"value":"1","name":"A"}`), "A")
	if item != "name: A\nvalue: \"1\"\n" {
		t.Fatalf("ItemText = %q", item)
	}
	// A rule named by alert in its change path starts with the alert, and
	// without a path name with its first matching field.
	rule := decode(t, `{"name":"dup","alert":"A","expr":"1"}`)
	if item, _ := ItemText(rule, "A"); !strings.HasPrefix(item, "alert: A\n") {
		t.Fatalf("ItemText named A = %q, want the alert first", item)
	}
	if item, _ := ItemText(rule, ""); !strings.HasPrefix(item, "name: dup\n") {
		t.Fatalf("ItemText without a name = %q, want the name first", item)
	}
}

func TestLinesNameEachLinesPath(t *testing.T) {
	value := decode(t, `{"metadata":{"annotations":{"example.test/key":"v"}},"env":[{"name":"A","value":"x"}],"args":["one"],"script":"a\nb\n"}`)
	lines, paths, ok := Lines(value, nil)
	if !ok || len(lines) != len(paths) {
		t.Fatalf("Lines failed: %q %q", lines, paths)
	}
	got := map[string]string{}
	for i, line := range lines {
		got[strings.TrimSpace(line)] = strings.Join(paths[i], "")
	}
	for line, want := range map[string]string{
		"example.test/key: v": "/metadata/annotations/example.test~1key",
		"- name: A":           "/env[A]/name",
		"value: x":            "/env[A]/value",
		"- one":               "/args/0",
		"b":                   "/script",
	} {
		if got[line] != want {
			t.Fatalf("path of %q = %q, want %q (all: %v)", line, got[line], want, got)
		}
	}
}

// A list's items are named by the key group both revisions' lists share,
// as change paths name them: base rules with unique names are named by
// alert when the head rules repeat a name, also inside a nested list, and
// each item's first line is that alert.
func TestLinesNameItemsByTheGroupOfBothRevisions(t *testing.T) {
	base := decode(t, `{"rules":[{"name":"x","alert":"A","env":[{"name":"E","alert":"F"}]},{"name":"q","alert":"B"}]}`)
	head := decode(t, `{"rules":[{"name":"x","alert":"A","env":[{"name":"E","alert":"F"},{"name":"E","alert":"G"}]},{"name":"x","alert":"B"}]}`)
	lines, paths, ok := Lines(base, head)
	if !ok {
		t.Fatal("Lines failed")
	}
	got := map[string]string{}
	for i, line := range lines {
		got[strings.TrimSpace(line)] = strings.Join(paths[i], "")
	}
	for line, want := range map[string]string{
		"- alert: A": "/rules[A]/alert",
		"- alert: B": "/rules[B]/alert",
		"- alert: F": "/rules[A]/env[F]/alert",
	} {
		if got[line] != want {
			t.Fatalf("path of %q = %q, want %q (all: %v)", line, got[line], want, got)
		}
	}
	if lines, paths, _ := Lines(base, nil); strings.TrimSpace(lines[1]) != "- name: x" || strings.Join(paths[1], "") != "/rules[x]/name" {
		t.Fatalf("without the other revision, line %q has path %q, want the base list's own group", lines[1], strings.Join(paths[1], ""))
	}
}

func TestLinesAreDeterministic(t *testing.T) {
	value := decode(t, `{"b":{"y":1,"x":2},"a":[{"name":"n","z":true}]}`)
	l1, p1, _ := Lines(value, nil)
	for i := 0; i < 20; i++ {
		if l2, p2, _ := Lines(value, nil); !reflect.DeepEqual(l1, l2) || !reflect.DeepEqual(p1, p2) {
			t.Fatal("Lines differ between runs")
		}
	}
}

// envValue returns an object with n named list items, about 3n YAML lines.
func envValue(tb testing.TB, n int) any {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"name":"V%04d","value":"x","port":%d}`, i, i)
	}
	return decode(tb, `{"env":[`+strings.Join(items, ",")+`]}`)
}

// Printing allocates a bounded number of times per line: 21 when this was
// written. Building each scalar with its own encoder, which once made a
// large resource's comparison seven times slower, allocates 47 per line and
// fails the bound, which leaves room for library changes.
func TestLinesAllocationsPerLine(t *testing.T) {
	value := envValue(t, 300)
	lines, _, _ := Lines(value, nil)
	allocs := testing.AllocsPerRun(5, func() { Lines(value, nil) })
	if perLine := allocs / float64(len(lines)); perLine > 32 {
		t.Fatalf("Lines allocates %.1f times per line (%d lines), want at most 32", perLine, len(lines))
	}
}
