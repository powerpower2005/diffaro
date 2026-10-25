package helmerror

import (
	"strings"
	"testing"
)

// An inner include frame names a call site only when its path is both a safe
// chart-relative path and under templates/. Each condition alone
// rejects the frame, so the call site stops at the reported location.
func TestTemplateCallSiteRejectsEachUnsafeInnerFrame(t *testing.T) {
	const top = `template: app/templates/a.yaml:4:3: executing "app/templates/a.yaml" at <include "x" .>: error calling include: `
	for _, tc := range []struct{ name, frame string }{
		{"safe path outside templates", "app/files/fictional.tpl"},
		{"absolute path under templates", "/abs/app/templates/fictional.yaml"},
		{"unclean path under templates", "app/templates/../templates/fictional.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Error: " + top + "template: " + tc.frame + `:9:8: executing "x" at <deepCopy .Values.spec>: error calling deepCopy: `
			got := callSite(raw, "app/templates/a.yaml", "4")
			if got != " (calling include)" || strings.Contains(got, "fictional") {
				t.Fatalf("got %q, want the reported location calling include", got)
			}
		})
	}
}

// The call site comes only from the frames before any message: a
// fail or tpl message that quotes values in the shape of a frame is not read.
func TestTemplateCallSiteReadsOnlyIncludeFrames(t *testing.T) {
	const top = `template: app/templates/a.yaml:4:3: executing "app/templates/a.yaml" at <include "x" .>: error calling include: `
	for _, tc := range []struct{ name, raw, want string }{
		{"include then function", top + `template: app/templates/_helpers.tpl:9:8: executing "x" at <deepCopy .Values.spec>: error calling deepCopy: reflect: call of reflect.Value.Type on zero Value`, " (at app/templates/_helpers.tpl:9 calling deepCopy)"},
		{"fail message shaped like a frame", `template: app/templates/a.yaml:4:3: executing "app/templates/a.yaml" at <fail .Values.m>: error calling fail: template: app/templates/fictional-secret.yaml:7:1: executing "y" at <z>: error calling fictionalsecret: `, " (calling fail)"},
		{"tpl message", top + `template: app/templates/_helpers.tpl:9:8: executing "x" at <tpl .Values.t .>: error calling tpl: error during tpl function execution for "template: app/templates/fictional-secret.yaml:1:1: executing": x`, " (at app/templates/_helpers.tpl:9 calling tpl)"},
		{"first frame elsewhere", `template: app/templates/b.yaml:1:1: executing "b" at <include "x" .>: error calling include: `, ""},
		{"unsafe inner path", top + `template: ../../etc/passwd:1:1: executing "x" at <y>: error calling z: `, " (calling include)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := callSite("Error: "+tc.raw, "app/templates/a.yaml", "4")
			if got != tc.want || strings.Contains(got, "fictional") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
