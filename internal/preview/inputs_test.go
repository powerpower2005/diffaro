package preview

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"diffaro/internal/security"
)

const (
	inputRepo = "https://github.com/example/charts.git"
	inputBase = "1111111111111111111111111111111111111111"
	inputHead = "2222222222222222222222222222222222222222"
)

type inputRepository struct {
	files map[string]map[string]string
	reads map[string]int
	fail  string
}

func (f *inputRepository) read(_ context.Context, revision, file string) ([]byte, bool, error) {
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[revision+":"+file]++
	if file == f.fail {
		return nil, false, errors.New("provider failure")
	}
	data, ok := f.files[revision][file]
	return []byte(data), ok, nil
}

func inputRunner() Runner {
	return Runner{Config: profile.Config{Git: profile.Git{Repository: "example/charts", RepositoryURL: inputRepo}}, Redactor: security.Redactor{}}
}

func inputReport(apps ...domain.ApplicationResult) domain.Report {
	report := domain.NewReport()
	report.Revisions = domain.RevisionPair{Repository: "example/charts", Base: inputBase, Head: inputHead, Mode: "pr-merge-base-head"}
	report.Applications = apps
	return report
}

func inputTarget(repository, revision, chart string, valueFiles ...string) *domain.TargetSummary {
	return &domain.TargetSummary{Repository: repository, Revision: revision, Inputs: domain.InputSummary{Path: chart, ValueFiles: valueFiles}}
}

func changesOf(app domain.ApplicationResult) map[string]domain.InputChange {
	out := map[string]domain.InputChange{}
	for _, change := range app.InputChanges {
		out[change.Path] = change
	}
	return out
}

func TestAttachInputChangesListsPRFilesAnApplicationReads(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{
		inputBase: {"charts/api/values.yaml": "replicaCount: 2\nimage:\n  tag: v1\n", "envs/prod.yaml": "db:\n  password: old\n", "charts/api/Chart.yaml": "name: api\n"},
		inputHead: {"charts/api/values.yaml": "replicaCount: 3\nimage:\n  tag: v1\n", "envs/prod.yaml": "db:\n  password: new\n", "charts/api/Chart.yaml": "name: api\n"},
	}}
	files := &InputFiles{Files: []string{"charts/api/values.yaml", "charts/api/templates/deployment.yaml", "envs/prod.yaml", "docs/readme.md", "charts/other/values.yaml"}, Read: repo.read}
	app := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: "api"}, State: "changed",
		BaseTarget: inputTarget(inputRepo, inputBase, "charts/api", "../../envs/prod.yaml"), HeadTarget: inputTarget(inputRepo, inputHead, "charts/api", "../../envs/prod.yaml")}
	report := inputReport(app)
	inputRunner().AttachInputChanges(context.Background(), &report, files)

	got := changesOf(report.Applications[0])
	if len(got) != 3 {
		t.Fatalf("inputs outside the Application's chart and value files must not be listed: %+v", report.Applications[0].InputChanges)
	}
	defaults := got["charts/api/values.yaml"]
	if defaults.Kind != "valueFile" || defaults.Comparison != "complete" || len(defaults.Changes) != 1 || defaults.Changes[0].Path != "/replicaCount" || defaults.Changes[0].Before.(interface{ String() string }).String() != "2" {
		t.Fatalf("chart default values need a key-level diff: %+v", defaults)
	}
	if template := got["charts/api/templates/deployment.yaml"]; template.Kind != "chartFile" || template.Comparison != "path-only" || len(template.Changes) != 0 {
		t.Fatalf("other chart files are listed by path: %+v", template)
	}
	secret := got["envs/prod.yaml"]
	if secret.Kind != "valueFile" || len(secret.Changes) != 1 || secret.Changes[0].Path != "/db/password" || !secret.Changes[0].Redacted || secret.Changes[0].After != security.Mask {
		t.Fatalf("sensitive keys must stay masked: %+v", secret)
	}
	if paths := []string{report.Applications[0].InputChanges[0].Path, report.Applications[0].InputChanges[1].Path, report.Applications[0].InputChanges[2].Path}; !reflect.DeepEqual(paths, []string{"charts/api/templates/deployment.yaml", "charts/api/values.yaml", "envs/prod.yaml"}) {
		t.Fatalf("input order must be stable: %v", paths)
	}
	if report.Applications[0].State != "changed" || report.ChangesFound {
		t.Fatal("input evidence must not change states or change detection")
	}
}

// A chart file the PR adds or removes is listed with that state, not as
// changed; its content is still not read.
func TestAttachInputChangesKeepsAddedAndRemovedChartFiles(t *testing.T) {
	repo := &inputRepository{}
	files := &InputFiles{
		Files:  []string{"charts/api/templates/cron.yaml", "charts/api/templates/job.yaml", "charts/api/templates/service.yaml"},
		States: map[string]domain.State{"charts/api/templates/cron.yaml": domain.StateRemoved, "charts/api/templates/job.yaml": domain.StateAdded},
		Read:   repo.read,
	}
	app := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: "api"}, State: "changed",
		BaseTarget: inputTarget(inputRepo, inputBase, "charts/api"), HeadTarget: inputTarget(inputRepo, inputHead, "charts/api")}
	report := inputReport(app)
	inputRunner().AttachInputChanges(context.Background(), &report, files)

	got := changesOf(report.Applications[0])
	for file, want := range map[string]domain.State{"charts/api/templates/cron.yaml": domain.StateRemoved, "charts/api/templates/job.yaml": domain.StateAdded, "charts/api/templates/service.yaml": domain.StateChanged} {
		if change := got[file]; change.Kind != "chartFile" || change.State != want || change.Comparison != "path-only" {
			t.Fatalf("%s: %+v, want state %s", file, change, want)
		}
	}
	for key := range repo.reads {
		if strings.HasSuffix(key, ".yaml") && strings.Contains(key, "/templates/") {
			t.Fatalf("path-only chart files must not be read: %s", key)
		}
	}
}

func TestAttachInputChangesFollowsOnlyPRRevisions(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{inputBase: {"charts/api/values.yaml": "a: 1\n"}, inputHead: {"charts/api/values.yaml": "a: 2\n"}}}
	files := &InputFiles{Files: []string{"charts/api/values.yaml"}, Read: repo.read}
	pinned := "3333333333333333333333333333333333333333"
	report := inputReport(
		domain.ApplicationResult{ID: domain.ApplicationID{Name: "pinned"}, BaseTarget: inputTarget(inputRepo, pinned, "charts/api"), HeadTarget: inputTarget(inputRepo, pinned, "charts/api")},
		domain.ApplicationResult{ID: domain.ApplicationID{Name: "other-repo"}, BaseTarget: inputTarget("https://github.com/example/other.git", inputBase, "charts/api"), HeadTarget: inputTarget("https://github.com/example/other.git", inputHead, "charts/api")},
	)
	inputRunner().AttachInputChanges(context.Background(), &report, files)
	for _, app := range report.Applications {
		if len(app.InputChanges) != 0 {
			t.Fatalf("%s: a source not rendered at the PR revisions does not change with the PR: %+v", app.ID.Name, app.InputChanges)
		}
	}
	if len(repo.reads) != 0 {
		t.Fatal("unrelated sources must not cause reads")
	}
}

func TestAttachInputChangesSharesReadsAndResolvesReferences(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{
		inputBase: {"values/common.yaml": "a: 1\n", "charts/lib/Chart.yaml": "name: lib\n", "charts/api/Chart.yaml": "dependencies:\n- name: lib\n  repository: file://../lib\n"},
		inputHead: {"values/common.yaml": "a: 2\nb: true\n", "charts/lib/Chart.yaml": "name: lib\n", "charts/api/Chart.yaml": "dependencies:\n- name: lib\n  repository: file://../lib\n"},
	}}
	files := &InputFiles{Files: []string{"values/common.yaml", "charts/lib/templates/_helpers.tpl"}, Read: repo.read}
	sources := func(revision string) []domain.SourceSummary {
		return []domain.SourceSummary{
			{Position: 1, Repository: inputRepo, Revision: revision, Inputs: domain.InputSummary{Path: "charts/api", ValueFiles: []string{"$values/values/common.yaml"}}},
			{Position: 2, Repository: inputRepo, Ref: "values", Revision: revision},
		}
	}
	var apps []domain.ApplicationResult
	for _, name := range []string{"api-a", "api-b"} {
		apps = append(apps, domain.ApplicationResult{ID: domain.ApplicationID{Name: name}, BaseSources: sources(inputBase), HeadSources: sources(inputHead)})
	}
	report := inputReport(apps...)
	inputRunner().AttachInputChanges(context.Background(), &report, files)
	for _, app := range report.Applications {
		got := changesOf(app)
		if values := got["values/common.yaml"]; len(values.Changes) != 2 || values.Changes[1].Path != "/b" || values.Changes[1].Operation != "add" {
			t.Fatalf("%s: $ref value file needs key changes: %+v", app.ID.Name, app.InputChanges)
		}
		if helper := got["charts/lib/templates/_helpers.tpl"]; helper.Kind != "chartFile" {
			t.Fatalf("%s: a change in a local file:// dependency must be listed: %+v", app.ID.Name, app.InputChanges)
		}
	}
	if repo.reads[inputHead+":values/common.yaml"] != 1 || repo.reads[inputHead+":charts/api/Chart.yaml"] != 1 {
		t.Fatalf("a file shared by Applications must be read once: %v", repo.reads)
	}
	if len(report.Limitations) != 0 {
		t.Fatalf("resolved dependencies need no limitation: %v", report.Limitations)
	}
}

func TestAttachInputChangesExplicitRunAndFailures(t *testing.T) {
	repo := &inputRepository{fail: "charts/api/values-prod.yaml", files: map[string]map[string]string{
		inputHead: {"charts/api/values-new.yaml": "a: 1\n", "charts/api/values-bad.yaml": "? [a]\n: b\n"},
		inputBase: {"charts/api/values-bad.yaml": "a: 1\n"},
	}}
	files := &InputFiles{Files: []string{"charts/api/values-prod.yaml", "charts/api/values-new.yaml", "charts/api/values-bad.yaml"}, Read: repo.read}
	explicit := domain.ApplicationResult{ID: domain.ApplicationID{Name: "api"}, Inputs: domain.InputSummary{Path: "charts/api", ValueFiles: []string{"values-prod.yaml", "values-new.yaml", "values-bad.yaml"}}}
	report := inputReport(explicit)
	inputRunner().AttachInputChanges(context.Background(), &report, files)
	got := changesOf(report.Applications[0])
	if failed := got["charts/api/values-prod.yaml"]; failed.Comparison != "incomplete" || !strings.Contains(failed.Reason, "could not be read") {
		t.Fatalf("a read failure is explicit, not an empty diff: %+v", failed)
	}
	if added := got["charts/api/values-new.yaml"]; added.State != "added" || added.Comparison != "complete" || len(added.Changes) != 1 {
		t.Fatalf("a new value file is added: %+v", added)
	}
	if bad := got["charts/api/values-bad.yaml"]; bad.Comparison != "incomplete" || bad.Reason == "" {
		t.Fatalf("unsupported YAML keeps comparison incomplete: %+v", bad)
	}

	without := inputReport(explicit)
	inputRunner().AttachInputChanges(context.Background(), &without, nil)
	if len(without.Applications[0].InputChanges) != 0 || without.Limitations[len(without.Limitations)-1] != inputFilesUnavailable {
		t.Fatal("without a complete PR file list the report states that file inputs are not listed")
	}
}

func TestInlineValueChangesShowKeysWithMaskedValues(t *testing.T) {
	spec := func(values string) domain.Application {
		return domain.Application{Spec: map[string]any{"source": map[string]any{"repoURL": inputRepo, "helm": map[string]any{"values": values}}}}
	}
	changes := inlineValueChanges(spec("image:\n  tag: v1\nreplicas: 2\n"), spec("image:\n  tag: v2\nreplicas: 2\n"), security.Redactor{})
	if len(changes) != 1 || changes[0].Kind != "inlineValues" || changes[0].Path != "/spec/source/helm/values" || len(changes[0].Changes) != 1 {
		t.Fatalf("inline values need key-level changes: %+v", changes)
	}
	key := changes[0].Changes[0]
	if key.Path != "/image/tag" || !key.Redacted || key.Before != security.Mask || key.After != security.Mask {
		t.Fatalf("Helm input values stay masked as in specChanges: %+v", key)
	}
	if same := inlineValueChanges(spec("a: 1\n"), spec("a: 1\n"), security.Redactor{}); len(same) != 0 {
		t.Fatal("unchanged inline values are not listed")
	}
}

func TestAttachInputChangesNeedsKnownPRInputs(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{inputBase: {"charts/api/values.yaml": "a: 1\n"}, inputHead: {"charts/api/values.yaml": "a: 2\n"}}}
	files := &InputFiles{Files: []string{"charts/api/values.yaml", "README.md"}, Read: repo.read}
	report := inputReport(
		// An explicit Application skipped because its source is another repository.
		domain.ApplicationResult{ID: domain.ApplicationID{Name: "other"}, State: "skipped", SkipReason: otherRepositorySkip, Inputs: domain.InputSummary{Path: "charts/api"}},
		// A multi-source plan that failed leaves no known inputs.
		domain.ApplicationResult{ID: domain.ApplicationID{Name: "unknown"}, BaseTarget: inputTarget(inputRepo, inputBase, ""), HeadTarget: inputTarget(inputRepo, inputHead, "")},
	)
	inputRunner().AttachInputChanges(context.Background(), &report, files)
	for _, app := range report.Applications {
		if len(app.InputChanges) != 0 {
			t.Fatalf("%s: inputs must not be attributed without a known PR-repository source path: %+v", app.ID.Name, app.InputChanges)
		}
	}
	root := inputReport(domain.ApplicationResult{ID: domain.ApplicationID{Name: "root-chart"}, BaseTarget: inputTarget(inputRepo, inputBase, "."), HeadTarget: inputTarget(inputRepo, inputHead, ".")})
	inputRunner().AttachInputChanges(context.Background(), &root, files)
	if len(root.Applications[0].InputChanges) != 2 {
		t.Fatalf("a chart at the repository root reads every file: %+v", root.Applications[0].InputChanges)
	}
}

// Value file key changes are redacted for value files: a key that names a
// Secret is shown and a credential stays masked.
func TestAttachInputChangesShowsValueFileSecretNames(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{
		inputBase: {"charts/api/values.yaml": "slack:\n  secretName: old-secrets\n  secretKey: fictional-old\n"},
		inputHead: {"charts/api/values.yaml": "slack:\n  secretName: new-secrets\n  secretKey: fictional-new\n"},
	}}
	files := &InputFiles{Files: []string{"charts/api/values.yaml"}, Read: repo.read}
	report := inputReport(domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: "api"}, State: "changed",
		BaseTarget: inputTarget(inputRepo, inputBase, "charts/api"), HeadTarget: inputTarget(inputRepo, inputHead, "charts/api")})
	inputRunner().AttachInputChanges(context.Background(), &report, files)
	changes := changesOf(report.Applications[0])["charts/api/values.yaml"].Changes
	if len(changes) != 2 || changes[0].Path != "/slack/secretKey" || changes[0].After != security.Mask || changes[1].Path != "/slack/secretName" || changes[1].Before != "old-secrets" || changes[1].After != "new-secrets" || changes[1].Redacted {
		t.Fatalf("value file Secret name hidden or credential shown: %+v", changes)
	}
}

// A value file key with an ordinary name whose value a rendered target held
// under a sensitive path or in a Secret is shown masked, also when it is
// embedded in a longer masked string.
func TestInputChangesMaskValuesMaskedInTargets(t *testing.T) {
	repo := &inputRepository{files: map[string]map[string]string{
		inputBase: {"charts/api/values.yaml": "auth:\n  apiKey: fictional-old-key\ndb:\n  pass: fictional-old-pass\nreplicas: 2\n"},
		inputHead: {"charts/api/values.yaml": "auth:\n  apiKey: fictional-new-key\ndb:\n  pass: fictional-new-pass\nreplicas: 3\n"},
	}}
	files := &InputFiles{Files: []string{"charts/api/values.yaml"}, Read: repo.read}
	app := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a", Namespace: "argocd", Name: "api"}, State: "changed",
		BaseTarget: inputTarget(inputRepo, inputBase, "charts/api"), HeadTarget: inputTarget(inputRepo, inputHead, "charts/api")}
	report := inputReport(app)
	runner := inputRunner()
	runner.Redactor.Masked = &security.MaskedValues{}
	// What comparing the targets masked: Secret payloads and a connection URL.
	runner.Redactor.Masked.Add([]any{"fictional-old-key", "fictional-new-key", "postgres://u:fictional-new-pass@db/app"})
	runner.AttachInputChanges(context.Background(), &report, files)

	got := map[string]domain.Change{}
	for _, change := range report.Applications[0].InputChanges[0].Changes {
		got[change.Path] = change
	}
	if key := got["/auth/apiKey"]; key.Before != security.Mask || key.After != security.Mask || !key.Redacted {
		t.Fatalf("a Secret value reached through an ordinary key is shown: %+v", key)
	}
	if pass := got["/db/pass"]; pass.After != security.Mask || pass.Before != "fictional-old-pass" || !pass.Redacted {
		t.Fatalf("an embedded masked value is shown, or an unmasked one hidden: %+v", pass)
	}
	if replicas := got["/replicas"]; replicas.Redacted || replicas.After == security.Mask {
		t.Fatalf("an ordinary value was masked: %+v", replicas)
	}
}
