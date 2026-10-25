package manifest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

var fuzzManifestSeeds = []string{
	"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  replicas: 2\n",
	"apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: a}\n",
	"---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n  namespace: other\n---\n",
	"apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata: {key: dmFsdWU=}\n",
	"a: &x 1\nb: *x\n",
	"{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"json\"}}",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: {generateName: x-}\n",
	"apiVersion: ''\n",
	"key: .nan\n",
}

// Rendered output is untrusted: parsing any of it must not panic, and every
// rejection is a classified failure, never an untyped error.
func FuzzParseManifests(f *testing.F) {
	for _, seed := range fuzzManifestSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data string) {
		resources, err := Parse([]string{data}, "in-cluster", "demo", Limits{Bytes: 1 << 20, Documents: 64}, nil)
		if err != nil {
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code == domain.CodeInternalError {
				t.Fatalf("unclassified parse error: %T %v", err, err)
			}
			return
		}
		for key, resource := range resources {
			if key == "" || resource.ID.Kind == "" || resource.ID.Name == "" {
				t.Fatalf("resource without identity: %q %+v", key, resource.ID)
			}
		}
		again, err := Parse([]string{data}, "in-cluster", "demo", Limits{Bytes: 1 << 20, Documents: 64}, nil)
		if err != nil || !reflect.DeepEqual(resources, again) {
			t.Fatalf("parsing is not deterministic: %v", err)
		}
		if changes, _ := Compare(resources, again, security.Redactor{}); hasChanges(changes) {
			t.Fatalf("identical targets compare as changed: %+v", changes)
		}
	})
}

// hasChanges reports a diff of identical targets that claims a change, or
// an unknown without a stated reason. A shape that cannot be compared, such
// as a non-map Secret payload, may stay unknown with its reason.
func hasChanges(diffs []domain.ResourceDiff) bool {
	for _, diff := range diffs {
		switch diff.State {
		case domain.StateUnchanged:
		case domain.StateUnknown:
			if diff.Comparison != domain.ComparisonIncomplete || diff.IncompleteReason == "" {
				return true
			}
		default:
			return true
		}
	}
	return false
}

var fuzzValuesSeeds = []string{
	"image:\n  repository: example.invalid/api\n  tag: v1\nreplicas: 2\n",
	"list:\n- name: a\n  value: 1\n- name: b\n",
	"multi: |\n  line one\n  line two\n",
	"empty: {}\nnull: ~\nbool: true\n",
	"a: &anchor {x: 1}\nb: *anchor\n",
	"? [complex]\n: key\n",
	"",
	"- not a mapping\n",
	"secret:\n  password: fictional\n",
}

// Value files come from pull requests: parsing must not panic, a parsed
// file compares unchanged with itself, and its YAML view and line hunks
// stay bounded and consistent.
func FuzzParseValues(f *testing.F) {
	for _, seed := range fuzzValuesSeeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		values, err := ParseValues(data)
		if err != nil {
			return
		}
		if changes := CompareValues("", values, values, security.Redactor{}); len(changes) != 0 {
			t.Fatalf("a values file differs from itself: %+v", changes)
		}
		if hunks := lineHunks(values, values, nil); len(hunks) != 0 {
			t.Fatalf("identical values have line hunks: %+v", hunks)
		}
	})
}

// Line hunks of any two value trees must not panic, stay within their
// bounds, and contain only context, removed or added lines.
func FuzzLineHunks(f *testing.F) {
	for i, seed := range fuzzValuesSeeds {
		f.Add([]byte(seed), []byte(fuzzValuesSeeds[(i+1)%len(fuzzValuesSeeds)]))
	}
	f.Fuzz(func(t *testing.T, baseData, headData []byte) {
		base, err := ParseValues(baseData)
		if err != nil {
			return
		}
		head, err := ParseValues(headData)
		if err != nil {
			return
		}
		hunks := lineHunks(base, head, nil)
		if len(hunks) > hunkMaxHunks {
			t.Fatalf("%d hunks exceed the bound", len(hunks))
		}
		for _, hunk := range hunks {
			if hunk.BaseStart < 0 || hunk.HeadStart < 0 || len(hunk.Lines) == 0 {
				t.Fatalf("malformed hunk: %+v", hunk)
			}
			for _, line := range hunk.Lines {
				if line == "" || !strings.ContainsRune(" -+", rune(line[0])) {
					t.Fatalf("hunk line %q has no mark", line)
				}
			}
		}
	})
}
