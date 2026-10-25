package localhelm

import (
	"errors"
	"testing"

	"diffaro/internal/domain"
	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// Screening finds a random-output function and a time, DNS or small-range
// function wherever the chart or values name it: templates, files, values,
// any subchart depth, or the supplied values. A stable function also names a
// nondeterministic one, so it reports random and runs the bundled probe.
func TestScreenFunctionsFindsEveryLocation(t *testing.T) {
	clean := func() *chart.Chart {
		ch := &chart.Chart{Metadata: &chart.Metadata{Name: "app"}, Values: map[string]any{"message": "{{ .Values.name }}"}}
		ch.Templates = []*common.File{{Name: "templates/a.yaml", Data: []byte("{{ .Values.message | quote }}")}}
		child := &chart.Chart{Metadata: &chart.Metadata{Name: "child"}, Values: map[string]any{}}
		grandchild := &chart.Chart{Metadata: &chart.Metadata{Name: "grandchild"}, Values: map[string]any{}}
		child.AddDependency(grandchild)
		ch.AddDependency(child)
		return ch
	}
	grandchild := func(ch *chart.Chart) *chart.Chart { return ch.Dependencies()[0].Dependencies()[0] }
	for _, tc := range []struct {
		name           string
		mutate         func(*chart.Chart)
		values         string
		random, stable bool
	}{
		{"none", func(*chart.Chart) {}, "", false, false},
		{"random template", func(ch *chart.Chart) {
			ch.Templates = append(ch.Templates, &common.File{Name: "templates/b.yaml", Data: []byte("{{ randAlphaNum 8 }}")})
		}, "", true, false},
		{"stable file", func(ch *chart.Chart) { ch.Files = []*common.File{{Name: "files/a.tpl", Data: []byte("{{ now }}")}} }, "", true, true},
		{"random grandchild values", func(ch *chart.Chart) { grandchild(ch).Values["token"] = "{{ uuidv4 }}" }, "", true, false},
		{"stable grandchild template", func(ch *chart.Chart) {
			grandchild(ch).Templates = []*common.File{{Name: "templates/c.yaml", Data: []byte("{{ getHostByName \"example.invalid\" }}")}}
		}, "", true, true},
		{"random then stable in later charts", func(ch *chart.Chart) {
			ch.Templates = append(ch.Templates, &common.File{Name: "templates/b.yaml", Data: []byte("{{ randAlpha 4 }}")})
			grandchild(ch).Values["date"] = "{{ ago .Values.time }}"
		}, "", true, true},
		{"stable supplied values", func(*chart.Chart) {}, "date: '{{ now }}'", true, true},
		{"name outside an action", func(ch *chart.Chart) {
			ch.Templates = append(ch.Templates, &common.File{Name: "templates/b.yaml", Data: []byte("now uuidv4")})
		}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := clean()
			tc.mutate(ch)
			probes := 0
			random, err := screenFunctions(ch, []byte(tc.values), func() error { probes++; return nil })
			if err != nil || random != tc.random || (probes == 1) != tc.stable || probes > 1 {
				t.Fatalf("random=%v probes=%d err=%v", random, probes, err)
			}
			// A failed probe skips a chart that names a stable function.
			_, err = screenFunctions(ch, []byte(tc.values), func() error { return renderError() })
			var skipped *domain.RenderSkipped
			if errors.As(err, &skipped) != tc.stable {
				t.Fatal("stable function screening did not decide the probe", err)
			}
		})
	}
}
