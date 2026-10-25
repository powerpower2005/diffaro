package cli

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

const libraryRepo = "https://github.com/example/library.git"
const sharedValuesRepo = "https://github.com/example/shared-values.git"
const sharedValuesSHA = "dddddddddddddddddddddddddddddddddddddddd"

func multiRepositoryApp(name string, instance int, scenario string) map[string]any {
	a := multiInstanceApp(name)
	if !strings.HasPrefix(scenario, "multi-repo") || (name != "east" && name != "west") {
		return a
	}
	spec := a["spec"].(map[string]any)
	chart := spec["source"].(map[string]any)
	chart["repoURL"], chart["targetRevision"] = libraryRepo, ancestorSHA
	chart["helm"].(map[string]any)["valueFiles"] = []any{"values.yaml", "$values/" + name + ".yaml", "$shared/common.yaml"}
	sharedRevision := sharedValuesSHA
	if instance == 0 && name == "east" && scenario == "multi-repo-mutable-source" {
		sharedRevision = "main"
	}
	spec["sources"] = []any{
		chart,
		map[string]any{"repoURL": fakeRepo, "targetRevision": "main", "ref": "values"},
		map[string]any{"repoURL": sharedValuesRepo, "targetRevision": sharedRevision, "ref": "shared"},
	}
	delete(spec, "source")
	return a
}

func TestCLIRecursiveMultipleRepositoriesInstancesAndClusters(t *testing.T) {
	for _, scenario := range []string{"multi-repo", "multi-repo-render-failed", "multi-repo-mutable-source"} {
		t.Run(scenario, func(t *testing.T) {
			git := gitServer(t)
			first, _ := multiInstanceServer(t, 0, scenario)
			second, calls := multiInstanceServer(t, 1, scenario)
			config := multiInstanceConfig(t, git.URL, first.URL, second.URL, scenario)
			report, code := runJSON(t, config, "--base", "main", "--head", "feature", "--recursive")
			want := 1
			if scenario != "multi-repo" {
				want = 3
			}
			if code != want || calls.Load() != 4 || len(report.Applications) != 8 {
				t.Fatalf("unexpected multi-repository result: exit=%d status=%s", code, report.Status)
			}
			for _, instance := range []string{"instance-0", "instance-1"} {
				for _, name := range []string{"east", "west"} {
					app := multiInstanceResult(t, report, instance, name)
					if instance == "instance-0" && name == "east" && scenario != "multi-repo" {
						continue
					}
					if app.Execution != "complete" || len(app.Resources) != 2 {
						t.Fatal("an independent multi-repo Application lost its rendered targets")
					}
					for _, side := range []struct {
						target   *domain.TargetSummary
						revision string
					}{{app.BaseTarget, baseSHA}, {app.HeadTarget, headSHA}} {
						sources := side.target.Sources
						if len(sources) != 3 || sources[0].Repository != libraryRepo || sources[0].Revision != ancestorSHA || sources[1].Repository != fakeRepo || sources[1].Revision != side.revision || sources[1].Rule != "selected-repository-pair" || sources[2].Repository != sharedValuesRepo || sources[2].Revision != sharedValuesSHA || sources[2].Ref != "shared" {
							t.Fatalf("source provenance lost repository or revision mapping: %+v", sources)
						}
						if strings.Join(sources[0].Inputs.ValueFiles, ",") != "values.yaml,$values/"+name+".yaml,$shared/common.yaml" || !sources[0].Inputs.InlineValues || !sources[0].Inputs.ValuesObject || len(sources[0].Inputs.FileParameters) != 1 {
							t.Fatal("external values files or real Helm render context disappeared")
						}
					}
				}
			}
			if scenario == "multi-repo-mutable-source" {
				app := multiInstanceResult(t, report, "instance-0", "east")
				if app.Execution != "failed" || app.Issues[0].Code != "revision_mapping_required" || app.Issues[0].Repository != sharedValuesRepo || app.Issues[0].Revision != "main" || app.Issues[0].SourcePosition != 3 {
					t.Fatalf("mutable source failure blamed the PR repository or omitted its source position: %+v", app.Issues)
				}
			}
			for _, format := range []string{"markdown", "pr-markdown", "json"} {
				output, err := reporter.Encode(report, format, security.Redactor{})
				if err != nil || !strings.Contains(string(output), sharedValuesRepo) {
					t.Fatalf("%s lost multi-repository evidence: %v", format, err)
				}
				assertNoSecrets(t, format, string(output), "fictional-")
			}
		})
	}
}
