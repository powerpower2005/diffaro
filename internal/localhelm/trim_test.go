package localhelm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

const trimValuesRepo = "https://github.com/example/values"
const trimValuesSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func trimArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for name, text := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(text))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(text))
	}
	tw.Close()
	gz.Close()
	return data.Bytes()
}

// trimFixture is a multi-source request whose chart reads a vendored archive,
// a file:// dependency outside its path with its own file:// dependency, a
// .helmignore, value files inside and outside its path and a $ref value file,
// next to unrelated charts and files in both repositories.
func trimFixture(t *testing.T) Request {
	req := fixtureRequest()
	chartSource := req.Plan[0].Source
	chartSource["helm"] = map[string]any{"valueFiles": []any{"values-extra.yaml", "../env/prod.yaml", "$values/shared/values.yaml"}, "fileParameters": []any{map[string]any{"name": "note", "path": "../env/note.txt"}}}
	valuesSource := map[string]any{"repoURL": trimValuesRepo, "targetRevision": trimValuesSHA, "ref": "values"}
	req.Plan = append(req.Plan, domain.SourceRevision{Position: 2, Repository: trimValuesRepo, Ref: "values", Revision: trimValuesSHA, Source: valuesSource})
	req.Application.Spec["sources"] = []any{chartSource, valuesSource}
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/Chart.yaml"] = []byte("apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: common\n    version: 0.1.0\n    repository: file://../common\n  - name: vendored\n    version: 0.1.0\n")
	files["app/.helmignore"] = []byte("*.md\n")
	files["app/NOTES.md"] = []byte("{{ fail \"ignored file was loaded\" }}")
	files["app/values-extra.yaml"] = []byte("message: extra\n")
	files["app/templates/config.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: config}\ndata:\n  region: {{ .Values.region | quote }}\n  shared: {{ .Values.shared | quote }}\n  note: {{ .Values.note | quote }}\n{{ include \"common.resource\" . }}\n{{ include \"vendored.resource\" . }}\n")
	files["app/charts/vendored-0.1.0.tgz"] = trimArchive(t, map[string]string{
		"vendored/Chart.yaml":              "apiVersion: v2\nname: vendored\nversion: 0.1.0\ntype: library\n",
		"vendored/templates/_resource.tpl": "{{ define \"vendored.resource\" }}  vendored: \"yes\"{{ end }}",
	})
	files["common/Chart.yaml"] = []byte("apiVersion: v2\nname: common\nversion: 0.1.0\ntype: library\ndependencies:\n  - name: base\n    version: 0.1.0\n    repository: file://../lib/base\n")
	files["common/templates/_resource.tpl"] = []byte("{{ define \"common.resource\" }}  common: {{ include \"base.value\" . | quote }}\n{{ end }}")
	files["lib/base/Chart.yaml"] = []byte("apiVersion: v2\nname: base\nversion: 0.1.0\ntype: library\n")
	files["lib/base/templates/_value.tpl"] = []byte("{{ define \"base.value\" }}base{{ end }}")
	files["env/prod.yaml"] = []byte("region: prod\n")
	files["env/note.txt"] = []byte("note\n")
	files["env/staging.yaml"] = []byte("region: staging\n")
	files["other/Chart.yaml"] = []byte("apiVersion: v2\nname: other\nversion: 0.1.0\n")
	files["other/templates/big.yaml"] = bytes.Repeat([]byte("# unrelated\n"), 1<<12)
	files["README.md"] = []byte("unrelated\n")
	req.Repositories[trimValuesRepo+"@"+trimValuesSHA] = map[string][]byte{
		"shared/values.yaml": []byte("shared: ref\n"),
		"shared/other.yaml":  []byte("shared: no\n"),
	}
	return req
}

func renderBoth(t *testing.T, req Request, execute func(Request) ([]string, error)) ([]string, error, []string, error) {
	t.Helper()
	full, fullErr := execute(req)
	trimmed := req
	trimmed.Repositories = trimRepositories(req.Plan, req.Repositories)
	out, err := execute(trimmed)
	return full, fullErr, out, err
}

func TestTrimmedRequestRendersIdentically(t *testing.T) {
	cases := map[string]func(Request){
		"multi-source dependencies and values": func(Request) {},
		"missing optional value file": func(r Request) {
			helm := r.Plan[0].Source["helm"].(map[string]any)
			helm["valueFiles"] = append(helm["valueFiles"].([]any), "../env/missing.yaml")
			helm["ignoreMissingValueFiles"] = true
		},
		"source override at path": func(r Request) {
			r.Repositories[testRepo+"@"+testSHA]["app/.argocd-source-api.yaml"] = []byte("helm: {}\n")
		},
		"source override elsewhere": func(r Request) {
			r.Repositories[testRepo+"@"+testSHA]["other/.argocd-source-api.yaml"] = []byte("helm: {}\n")
		},
		"chart path without files": func(r Request) {
			r.Plan[0].Source["path"] = "missing"
		},
	}
	renderers := map[string]func(Request) ([]string, error){"bundled": Render}
	if binary := os.Getenv("DIFFARO_TEST_HELM_BINARY"); binary != "" {
		e, err := NewExternal(context.Background(), binary)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		renderers["external"] = func(r Request) ([]string, error) { return e.Render(context.Background(), r) }
	}
	for renderer, execute := range renderers {
		for name, mutate := range cases {
			t.Run(renderer+"/"+name, func(t *testing.T) {
				req := trimFixture(t)
				mutate(req)
				full, fullErr, out, err := renderBoth(t, req, execute)
				if fmt.Sprint(fullErr) != fmt.Sprint(err) || !reflect.DeepEqual(full, out) {
					t.Fatalf("trimmed render differs:\nfull %v %q\ntrim %v %q", fullErr, full, err, out)
				}
				if name == "multi-source dependencies and values" && (err != nil || len(out) != 2 || !strings.Contains(strings.Join(out, ""), `"vendored":"yes"`) || !strings.Contains(strings.Join(out, ""), `"common":"base"`) || !strings.Contains(strings.Join(out, ""), `"shared":"ref"`) || !strings.Contains(strings.Join(out, ""), `"region":"prod"`)) {
					t.Fatal("fixture did not exercise every input", out, err)
				}
			})
		}
	}
}

func TestTrimRepositoriesExcludesUnrelatedFiles(t *testing.T) {
	req := trimFixture(t)
	before := len(req.Repositories[testRepo+"@"+testSHA])
	trimmed := trimRepositories(req.Plan, req.Repositories)
	if len(req.Repositories[testRepo+"@"+testSHA]) != before {
		t.Fatal("trimming modified the input map")
	}
	chart, values := trimmed[testRepo+"@"+testSHA], trimmed[trimValuesRepo+"@"+trimValuesSHA]
	for _, name := range []string{"app/Chart.yaml", "app/.helmignore", "app/charts/vendored-0.1.0.tgz", "common/Chart.yaml", "lib/base/templates/_value.tpl", "env/prod.yaml", "env/note.txt"} {
		if chart[name] == nil {
			t.Fatal("needed file was trimmed", name)
		}
	}
	for _, name := range []string{"env/staging.yaml", "other/Chart.yaml", "other/templates/big.yaml", "README.md"} {
		if chart[name] != nil {
			t.Fatal("unrelated file was sent", name)
		}
	}
	if len(values) != 1 || values["shared/values.yaml"] == nil {
		t.Fatal("ref repository was not trimmed to its value file", values)
	}
}

func TestTrimRepositoriesKeepsUncertainReadSets(t *testing.T) {
	cases := map[string]func(Request){
		"build environment value file": func(r Request) {
			r.Plan[0].Source["helm"].(map[string]any)["valueFiles"] = []any{"values-$ARGOCD_APP_NAME.yaml"}
		},
		"absolute value file": func(r Request) {
			r.Plan[0].Source["helm"].(map[string]any)["valueFiles"] = []any{"/env/prod.yaml"}
		},
		"unknown ref": func(r Request) {
			r.Plan[0].Source["helm"].(map[string]any)["valueFiles"] = []any{"$other/values.yaml"}
		},
		"glob value file": func(r Request) {
			r.Plan[0].Source["helm"].(map[string]any)["valueFiles"] = []any{"*.yaml"}
		},
		"repository root chart": func(r Request) { r.Plan[0].Source["path"] = "." },
		"empty chart path":      func(r Request) { r.Plan[0].Source["path"] = "" },
		"dependency on repository root": func(r Request) {
			r.Repositories[testRepo+"@"+testSHA]["common/Chart.yaml"] = []byte("apiVersion: v2\nname: common\nversion: 0.1.0\ndependencies:\n  - name: root\n    repository: file://..\n")
		},
		"unparsable chart metadata": func(r Request) {
			r.Repositories[testRepo+"@"+testSHA]["app/charts/broken/Chart.yaml"] = []byte("dependencies: [")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := trimFixture(t)
			mutate(req)
			if trimmed := trimRepositories(req.Plan, req.Repositories); reflect.ValueOf(trimmed).Pointer() != reflect.ValueOf(req.Repositories).Pointer() {
				t.Fatal("uncertain read set was trimmed")
			}
		})
	}
}
