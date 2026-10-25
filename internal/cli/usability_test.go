package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestCommandHelpWithoutConfiguration(t *testing.T) {
	for _, command := range []string{"init", "diff", "doctor", "validate"} {
		for _, help := range []string{"--help", "-h"} {
			t.Run(command+help, func(t *testing.T) {
				var out, diagnostic bytes.Buffer
				code := Run(context.Background(), []string{command, "--config", "absent.yaml", help}, &out, &diagnostic)
				if code != 0 || diagnostic.Len() != 0 || !strings.Contains(out.String(), "Usage: diffaro "+command) || !strings.Contains(out.String(), "-config") {
					t.Fatal("help must work without a profile, endpoint or credentials")
				}
				if command == "init" && !strings.Contains(out.String(), "-repo-server-forward") {
					t.Fatal("init help omitted the actual connection options")
				}
				// Help is for binary users: design record numbers mean
				// nothing to them.
				if strings.Contains(out.String(), "ADR-") {
					t.Fatalf("help names a design record:\n%s", out.String())
				}
			})
		}
	}
	var out, diagnostic bytes.Buffer
	if Run(context.Background(), []string{"--help"}, &out, &diagnostic) != 0 || strings.Contains(out.String(), "ADR-") || !strings.Contains(out.String(), "README.md#quick-start in the Diffaro repository") {
		t.Fatalf("top-level help must name where the guide is:\n%s", out.String())
	}
	out.Reset()
	if Run(context.Background(), []string{"--version"}, &out, &diagnostic) != 0 || !strings.HasPrefix(out.String(), "Diffaro ") || !strings.Contains(out.String(), "revision") || diagnostic.Len() != 0 {
		t.Fatal("installed binary must expose build information offline")
	}
}

// Command help lists only options the command accepts: every option it
// leaves out is rejected by that command, and doctor help offers no
// pr-markdown. Top-level help shows --config as optional and the doctor
// and diff options users need.
func TestHelpListsOnlyAcceptedOptions(t *testing.T) {
	config := configFile(t, "https://git.example.invalid", "https://argocd.example.invalid")
	values := map[string]string{"format": "json", "debug-bundle-level": "private"}
	for command, rejected := range rejectedFlags {
		var help, diagnostic bytes.Buffer
		if code := Run(context.Background(), []string{command, "--help"}, &help, &diagnostic); code != 0 {
			t.Fatalf("%s --help exit %d", command, code)
		}
		for _, name := range rejected {
			if strings.Contains(help.String(), "  -"+name+" ") || strings.Contains(help.String(), "  -"+name+"\n") {
				t.Fatalf("%s help lists rejected -%s:\n%s", command, name, help.String())
			}
			args := []string{command, "--config", config, "--" + name}
			switch name {
			case "render", "recursive", "verbose":
			default:
				value, ok := values[name]
				if !ok {
					value = filepath.Join(t.TempDir(), "value")
				}
				args = append(args, value)
			}
			var out, stderr bytes.Buffer
			if code := Run(context.Background(), args, &out, &stderr); code != domain.ExitInvalidConfiguration {
				t.Fatalf("%s accepted hidden -%s: exit %d %s", command, name, code, stderr.String())
			}
		}
		if command == "doctor" && strings.Contains(help.String(), "pr-markdown") {
			t.Fatalf("doctor help offers pr-markdown:\n%s", help.String())
		}
	}
	var out, diagnostic bytes.Buffer
	Run(context.Background(), []string{"--help"}, &out, &diagnostic)
	for _, want := range []string{"validate|doctor|diff [--config profile.yaml]", "Doctor: [--render [--pr NUMBER_OR_URL", "--mode NAME", "--debug-bundle PATH"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("top-level help lacks %q:\n%s", want, out.String())
		}
	}
}

// Options parsed before --help do not appear as their defaults: help shows
// the declared default, never a supplied value.
func TestHelpShowsDeclaredDefaultsAfterParsedOptions(t *testing.T) {
	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"diff", "--format", "json", "--base", "fictional-branch", "--help"}, &out, &diagnostic); code != 0 {
		t.Fatalf("diff --help exit %d", code)
	}
	if strings.Contains(out.String(), "fictional-branch") || strings.Contains(out.String(), `(default "json")`) || !strings.Contains(out.String(), "-format") {
		t.Fatalf("help shows parsed values as defaults:\n%s", out.String())
	}
}

// doctor without --render cannot check a repository discovery profile; the
// error names the command that can, with a PR.
func TestDiscoveryDoctorNamesRenderWithPR(t *testing.T) {
	config := configFile(t, "https://git.example.invalid", "https://argocd.example.invalid")
	editProfile(t, config, "applications:\n  - instance: instance-0\n    namespace: argocd\n    name: api\n", "discovery:\n  mode: repository\nrouting:\n  defaultInstance: instance-0\n")
	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", config}, &out, &diagnostic); code != domain.ExitInvalidConfiguration || !strings.Contains(diagnostic.String(), "run diffaro doctor --render --pr NUMBER") {
		t.Fatalf("exit %d stderr %q", code, diagnostic.String())
	}
}

func TestArgumentDiagnosticsDoNotEchoValues(t *testing.T) {
	for _, args := range [][]string{
		{"diff", "--recursivee=fictional-sensitive-value"},
		{"init", "--repo-server-port=fictional-sensitive-value"},
		{"doctor", "fictional-sensitive-value"},
	} {
		var out, diagnostic bytes.Buffer
		if Run(context.Background(), args, &out, &diagnostic) != 4 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "diffaro "+args[0]+" --help") {
			t.Fatal("invalid arguments must have command-specific guidance")
		}
		assertNoSecrets(t, "invalid argument guidance", diagnostic.String(), "fictional-sensitive-value")
		if args[0] == "diff" && !strings.Contains(diagnostic.String(), "--recursivee") {
			t.Fatal("unknown option name must be identified")
		}
	}
}

func TestProgressDoesNotChangeMachineReport(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	args := []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "json"}
	var human, diagnostic, quiet, quietDiagnostic bytes.Buffer
	if Run(context.Background(), args, &human, &diagnostic) != 1 || Run(context.Background(), append(args, "--quiet"), &quiet, &quietDiagnostic) != 1 {
		t.Fatal("comparison exit behavior changed")
	}
	var report domain.Report
	if json.Unmarshal(human.Bytes(), &report) != nil || !bytes.Equal(human.Bytes(), quiet.Bytes()) || quietDiagnostic.Len() != 0 {
		t.Fatal("stdout JSON must remain pure and identical with or without human guidance")
	}
	for _, message := range []string{"resolving revisions", "rendering and comparing", "status=success", "exit=1", "Changes found"} {
		if !strings.Contains(diagnostic.String(), message) {
			t.Fatalf("missing user guidance: %s", message)
		}
	}
	assertNoSecrets(t, "progress with a provider token or render input", diagnostic.String(), "fictional-")
}

func TestPartialCompletionDoesNotClaimNoChanges(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{unchanged: true})
	b, _ := argoServer(t, "fictional-argo-1", backendOptions{denied: true})
	cfg := configFile(t, g.URL, a.URL, b.URL)
	var out, diagnostic bytes.Buffer
	if Run(context.Background(), []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "json"}, &out, &diagnostic) != 3 || !strings.Contains(diagnostic.String(), "status=partial_failure") || !strings.Contains(diagnostic.String(), "1 complete, 1 failed/denied") || strings.Contains(diagnostic.String(), "No target changes") {
		t.Fatal("partial completion must surface the successful and failed scope")
	}
}

func TestCreatesReportDirectoriesAndShowsSavedPaths(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	cfg := configFile(t, g.URL, a.URL)
	dir := filepath.Join(t.TempDir(), "new", "nested")
	markdown, machine := filepath.Join(dir, "preview.md"), filepath.Join(dir, "fictional-argo-0.json")
	var out, diagnostic bytes.Buffer
	args := []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "pr-markdown", "--output", markdown, "--json-output", machine}
	if Run(context.Background(), args, &out, &diagnostic) != 1 || out.Len() != 0 {
		t.Fatal("new nested report directories should work")
	}
	for _, path := range []string{markdown, machine} {
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			t.Fatal("report missing", err)
		}
	}
	if !strings.Contains(diagnostic.String(), "Saved --output:") || !strings.Contains(diagnostic.String(), "Saved --json-output:") || !strings.Contains(diagnostic.String(), security.Mask) {
		t.Fatal("saved paths must be shown with credential redaction")
	}
	assertNoSecrets(t, "saved paths", diagnostic.String(), "fictional-argo-0")
	assertNoStagedReports(t, dir)
	// Replacing existing reports works on supported operating systems too.
	if Run(context.Background(), append(args, "--quiet"), &out, &diagnostic) != 1 {
		t.Fatal("existing regular report files could not be replaced")
	}
}

func TestOutputFailurePrecedesEveryProviderCall(t *testing.T) {
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("provider was contacted before output validation")
		w.WriteHeader(500)
	}))
	defer backend.Close()
	cfg := configFile(t, backend.URL, backend.URL)
	dir := t.TempDir()
	primary, invalid := filepath.Join(dir, "preview.md"), filepath.Join(dir, "directory")
	if err := os.WriteFile(primary, []byte("previous report"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(invalid, 0700); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--output", primary, "--json-output", invalid}, &out, &diagnostic)
	old, err := os.ReadFile(primary)
	if code != 4 || requests != 0 || err != nil || string(old) != "previous report" || !strings.Contains(diagnostic.String(), "--json-output") {
		t.Fatal("invalid output must fail before provider calls and preserve prior files")
	}
	assertNoStagedReports(t, dir)
	// Neither destination may be a parent directory of the other.
	diagnostic.Reset()
	if Run(context.Background(), []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--output", filepath.Join(dir, "reports"), "--json-output", filepath.Join(dir, "reports", "preview.json")}, &out, &diagnostic) != 4 || requests != 0 {
		t.Fatal("colliding file/directory paths escaped preflight")
	}
}

func TestStagingFailurePreservesBothPriorReports(t *testing.T) {
	dir := t.TempDir()
	primary, machine := filepath.Join(dir, "preview.md"), filepath.Join(dir, "preview.json")
	for _, path := range []string{primary, machine} {
		if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outputs, err := prepareOutputs(primary, machine, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupOutputs(outputs)
	outputs[0].data, outputs[1].data = []byte("new Markdown"), []byte("new JSON")
	outputs[1].file.Close() // Deterministic write failure before publishing either format.
	var diagnostic bytes.Buffer
	if saveOutputs(outputs, &diagnostic, security.Redactor{}, false) {
		t.Fatal("staging failure was treated as successful publication")
	}
	for _, path := range []string{primary, machine} {
		old, err := os.ReadFile(path)
		if err != nil || string(old) != "previous" {
			t.Fatal("one report was overwritten before both formats were staged")
		}
	}
}

func TestDirectoryAliasesCannotOverwriteTheOtherFormat(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skip("directory symlink creation is unavailable on this OS/account")
	}
	outputs, err := prepareOutputs(filepath.Join(dir, "report"), filepath.Join(alias, "report"), "")
	defer cleanupOutputs(outputs)
	if err == nil || !strings.Contains(err.Error(), "different files") {
		t.Fatal("two new paths naming the same destination escaped preflight")
	}
	assertNoStagedReports(t, dir)
}

func TestEmptySelectionCompletionDoesNotImplySuccessfulComparison(t *testing.T) {
	var diagnostic bytes.Buffer
	printCompletion(&diagnostic, domain.NewReport(), security.Redactor{})
	if strings.Contains(diagnostic.String(), "No target changes") || !strings.Contains(diagnostic.String(), "No Application target comparisons completed") {
		t.Fatal("empty selection was presented as a completed target comparison")
	}
}

// Candidates that changed-path selection proved unaffected are a result,
// not a selection problem; a failure among them keeps the warning.
func TestUnaffectedCandidatesCompletionNamesTheResult(t *testing.T) {
	report := domain.NewReport()
	report.Discovery = []domain.DiscoveryResult{{Instance: "a", Status: "complete", Unaffected: 2}, {Instance: "b", Status: "complete", Unaffected: 1}}
	var diagnostic bytes.Buffer
	printCompletion(&diagnostic, report, security.Redactor{})
	if !strings.Contains(diagnostic.String(), "No target changes: no PR file is under the inputs of the 3 Application(s)") || strings.Contains(diagnostic.String(), "check the selection coverage") {
		t.Fatalf("unaffected candidates were reported as an empty selection:\n%s", diagnostic.String())
	}
	report.Applications = []domain.ApplicationResult{{Execution: domain.ExecutionSkipped}}
	diagnostic.Reset()
	printCompletion(&diagnostic, report, security.Redactor{})
	if strings.Contains(diagnostic.String(), "No target changes") || !strings.Contains(diagnostic.String(), "1 skipped; the report states each reason") {
		t.Fatalf("a skipped Application was hidden behind unaffected candidates:\n%s", diagnostic.String())
	}
}

// doctor without --render compares nothing, so its completion line names the
// access check and the render command instead of a comparison result.
func TestPreflightCompletionStatesNoComparison(t *testing.T) {
	report := domain.NewReport()
	report.Coverage = domain.CoverageConnectionPreflight
	report.Applications = []domain.ApplicationResult{{State: domain.StateUnchanged, Execution: domain.ExecutionComplete}}
	var diagnostic bytes.Buffer
	printCompletion(&diagnostic, report, security.Redactor{})
	if strings.Contains(diagnostic.String(), "No target changes") || !strings.Contains(diagnostic.String(), "Read access checked; targets were not rendered. Run doctor --render --pr NUMBER") {
		t.Fatalf("preflight presented as a comparison:\n%s", diagnostic.String())
	}
}

func assertNoStagedReports(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".diffaro-report-*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary report files were retained")
	}
}

type progressMessages chan string

func (messages progressMessages) Write(data []byte) (int, error) {
	select {
	case messages <- string(data):
	default:
	}
	return len(data), nil
}

func TestLongRunningProgressStopsBeforeReturning(t *testing.T) {
	messages := make(progressMessages, 16)
	p := startProgress(messages, false, time.Millisecond)
	p.stage("rendering and comparing target manifests")
	<-messages
	select {
	case message := <-messages:
		if !strings.Contains(message, "still rendering") || !strings.Contains(message, "elapsed") {
			t.Fatal("long running comparison does not explain that work continues")
		}
	case <-time.After(time.Second):
		t.Fatal("progress heartbeat missing")
	}
	p.close()
	p.close()
	select {
	case <-p.done:
	default:
		t.Fatal("progress writer remained active after CLI completion")
	}
}

// TestValidateRejectsReportAndRevisionFlags: validate writes no report and
// compares no revisions, so report and revision flags fail with exit 4 and
// a message instead of being ignored while the profile is reported valid.
func TestValidateRejectsReportAndRevisionFlags(t *testing.T) {
	config := configFile(t, "https://git.example.invalid", "https://argocd.example.invalid")
	report := filepath.Join(t.TempDir(), "out.md")
	run := func(args ...string) (int, string, string) {
		var out, diagnostic bytes.Buffer
		code := Run(context.Background(), append([]string{"validate", "--config", config}, args...), &out, &diagnostic)
		return code, out.String(), diagnostic.String()
	}
	if code, out, _ := run(); code != 0 || !strings.Contains(out, "Configuration valid") {
		t.Fatalf("valid profile without flags: exit %d", code)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--output", report}, "format and output are supported only by diff and doctor"},
		{[]string{"--format", "json"}, "format and output are supported only by diff and doctor"},
		{[]string{"--format", "markdown"}, "format and output are supported only by diff and doctor"},
		{[]string{"--base", "main"}, "revision arguments are supported only by diff and doctor --render"},
		{[]string{"--head", "feature"}, "revision arguments are supported only by diff and doctor --render"},
		{[]string{"--pr", "12"}, "revision arguments are supported only by diff and doctor --render"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, out, diagnostic := run(tc.args...)
			if code != 4 || out != "" || !strings.Contains(diagnostic, tc.want) {
				t.Fatalf("exit %d stdout %q stderr %q, want exit 4 and %q", code, out, diagnostic, tc.want)
			}
			if _, err := os.Stat(report); !os.IsNotExist(err) {
				t.Fatal("validate created an output file")
			}
		})
	}
}

func TestTraceOptionIsDiffOnlyAndNeverSharesAReportFile(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.md")
	for _, tc := range []struct {
		options commandOptions
		want    string
	}{
		{commandOptions{command: "doctor", format: "markdown", trace: "trace.json"}, "only by diff"},
		{commandOptions{command: "diff", format: "markdown", output: report, trace: report}, "different path"},
		{commandOptions{command: "diff", format: "markdown", jsonOutput: report, trace: filepath.Join(dir, ".", "report.md")}, "different path"},
	} {
		if err := tc.options.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: %v", tc.options, err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skip("directory symlink creation is unavailable on this OS/account")
	}
	outputs, err := prepareOutputs("", filepath.Join(dir, "report.json"), filepath.Join(alias, "report.json"))
	defer cleanupOutputs(outputs)
	if err == nil || !strings.Contains(err.Error(), "json-output and trace must be different files") {
		t.Fatal("a trace aliasing the JSON report escaped preflight", err)
	}
	assertNoStagedReports(t, dir)
}
