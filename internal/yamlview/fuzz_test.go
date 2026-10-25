package yamlview

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// The YAML view of any decoded value prints without panicking, and every
// printed line has a path.
func FuzzLines(f *testing.F) {
	for _, seed := range []string{
		"containers:\n- name: api\n  image: example.invalid/api:v1\n- name: sidecar\n",
		"rules:\n- alert: High\n  expr: up == 0\n- record: r\n",
		"text: |\n  one\n  two\n",
		"nested: {a: [1, 2, {b: c}]}\n",
		"'quoted key': \"value\"\n",
		"- 1\n- [2, 3]\n",
	} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, valueText, otherText string) {
		var value, other any
		if yaml.Unmarshal([]byte(valueText), &value) != nil || yaml.Unmarshal([]byte(otherText), &other) != nil {
			return
		}
		lines, paths, ok := Lines(value, other)
		if !ok {
			return
		}
		if len(lines) != len(paths) {
			t.Fatalf("%d lines but %d paths", len(lines), len(paths))
		}
		if _, ok := Text(value); !ok {
			t.Fatal("Lines printed a value that Text cannot")
		}
	})
}
