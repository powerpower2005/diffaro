package profile

import (
	"fmt"
	"strings"
	"testing"
)

// run.modes is bounded and its names are simple identifiers; the section is
// optional.
func TestRunModesAreBoundedAndNamed(t *testing.T) {
	base := "version: v1alpha1\ngit:\n  apiURL: https://api.github.com\n  repository: example/charts\n  repositoryURL: https://github.com/example/charts.git\ninstances:\n  - id: test\n    endpoint: https://argocd.example.com\napplications:\n  - instance: test\n    namespace: argocd\n    name: api\n"
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("profile without run rejected: %v", err)
	}
	var many strings.Builder
	many.WriteString("run:\n  modes:\n")
	for i := 0; i <= MaxRunModes; i++ {
		fmt.Fprintf(&many, "    mode-%d: {quiet: true}\n", i)
	}
	if _, err := Parse([]byte(base + many.String())); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("too many modes accepted: %v", err)
	}
	if _, err := Parse([]byte(base + "run:\n  modes:\n    ci: {output: preview.md, recursive: true}\n")); err != nil {
		t.Fatalf("valid mode rejected: %v", err)
	}
	if _, err := Parse([]byte(base + "run:\n  modes:\n    ci: {unknown: x}\n")); err == nil {
		t.Fatal("unknown run option accepted")
	}
}
