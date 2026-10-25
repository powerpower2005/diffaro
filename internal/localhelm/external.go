package localhelm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"diffaro/internal/determinism"
	"diffaro/internal/domain"
	"diffaro/internal/procio"
	"go.yaml.in/yaml/v3"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
)

// External is an operator-selected, frozen executable, never selected by Git
// input. Its subprocess has no inherited API credentials or Helm plugins.
// This environment isolation is not an OS sandbox for arbitrary native code.
type External struct {
	Version, SHA256 string
	// Environment holds operator-configured NAME=value entries added to every
	// render. Values can read services outside Git and may be secrets.
	Environment       []string
	directory, binary string

	// renders holds verified renders by input (renderVerified).
	renders renderMemo[string, verifiedRender]
}

// verifiedRender is the output of inputs that already passed the
// determinism check, with the template failures recovered on the way.
type verifiedRender struct {
	output  string
	reasons []string
}

// Base and head of an unaffected Application usually render identical inputs.
// Their output is checked for determinism once and then reused, up to this
// many bytes per executable.
const maxVerifiedRenderBytes = 64 << 20

// inputKey identifies one render by its environment, the files of its
// isolated directory and its arguments with that directory replaced, so
// identical inputs match across renders.
func inputKey(environment []string, dir string, limit int64, args []string) (string, error) {
	hash := sha256.New()
	fmt.Fprintf(hash, "%d\x00%d\x00%d\x00", limit, len(environment), len(args))
	for _, entry := range environment {
		fmt.Fprintf(hash, "%s\x00", entry)
	}
	for _, arg := range args {
		fmt.Fprintf(hash, "%s\x00", strings.ReplaceAll(arg, dir, "\x00dir"))
	}
	err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, name)
		fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		hash.Write(data)
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil)), err
}

// result returns the output of a verified render, with its recovered
// template failures as a partial render.
func (r verifiedRender) result() (string, error) {
	if len(r.reasons) > 0 {
		return r.output, partialFailure(r.reasons)
	}
	return r.output, nil
}

// EnvironmentNames lists configured variable names; values are never reported.
func (e *External) EnvironmentNames() []string {
	names := make([]string, 0, len(e.Environment))
	for _, entry := range e.Environment {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Upstream Helm prints a leading "v"; some builds set the version without it,
// such as 3.12.0-acme1+gabc1234.
var externalVersion = regexp.MustCompile(`^v?[34]\.[0-9]+\.[0-9]+[a-zA-Z0-9.+_-]*$`)
var templateAction = regexp.MustCompile(`(?s){{.*?}}`)

// unsafeFunction matches a name in nondeterministicFunctions, which omits
// lookup: helm template, like Argo CD's repo-server, runs without a cluster,
// where lookup returns an empty result deterministically.
var unsafeFunction = regexp.MustCompile(`\b(` + strings.Join(nondeterministicFunctions, "|") + `)\b`)

// stableFunction is the part of unsafeFunction whose result can be equal in
// two renders a moment apart: a date, a DNS answer or an integer from a small
// range. The other functions return random output, so the double render of
// every external input catches a call to them.
var stableFunction = regexp.MustCompile(`\b(now|ago|randInt|getHostByName)\b`)

func externalFailure(reason string) error {
	return domain.Fail(domain.CodeHelmExecutionFailed, "render", reason, "provide a trusted Helm 3/4 CLI-compatible executable and run doctor --render; no bundled fallback is used", nil)
}

// versionProbeTimeout bounds the version check of NewExternal; it is a
// variable only so tests can expire it.
var versionProbeTimeout = 30 * time.Second

// NewExternal copies and hashes a bounded regular file once, before either
// revision renders. Replacing the original path cannot change the comparison.
func NewExternal(ctx context.Context, filename string) (*External, error) {
	e, err := snapshotExecutable(filename)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	version, err := e.command(probeCtx, e.directory, nil, 1024, "version", "--short")
	if failure := (*domain.Failure)(nil); errors.As(err, &failure) && failure.Issue.Code == domain.CodeLimitExceeded {
		// The probe's output bound is not render.maxResponseBytes.
		e.Close()
		return nil, externalFailure("Helm executable must report a Helm 3/4 CLI-compatible version")
	}
	if err != nil {
		e.Close()
		// A stopped probe is the version check, not a render, and its own
		// limit is not render.timeout.
		if ctx.Err() != nil {
			return nil, domain.Fail(stoppedCode(ctx.Err()), "render", "external Helm version check did not finish", "rerun the preview", ctx.Err())
		}
		if probeCtx.Err() != nil {
			return nil, externalFailure("Helm executable did not answer version --short within " + versionProbeTimeout.String())
		}
		return nil, err
	}
	e.Version = strings.TrimSpace(version)
	if len(e.Version) > 128 || !externalVersion.MatchString(e.Version) {
		e.Close()
		return nil, externalFailure("Helm executable must report a Helm 3/4 CLI-compatible version")
	}
	return e, nil
}

func snapshotExecutable(filename string) (*External, error) {
	if !filepath.IsAbs(filename) {
		return nil, externalFailure("Helm executable requires an absolute resolved path")
	}
	// Stat follows operator-managed tool symlinks without enumerating ancestors
	// (which may be inaccessible in restricted Windows runners).
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 128<<20 {
		return nil, externalFailure("Helm executable is missing, not a regular file or exceeds 128 MiB")
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, externalFailure("Helm executable cannot be read")
	}
	defer f.Close()
	header := make([]byte, 4)
	if _, err = io.ReadFull(f, header); err != nil || !nativeHeader(header) {
		return nil, externalFailure("native Helm executable required; shell and wrapper scripts are unsupported")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, externalFailure("Helm executable cannot be read")
	}
	dir, err := os.MkdirTemp("", "diffaro-helm-executable-")
	if err != nil {
		return nil, externalFailure("isolated Helm executable directory cannot be created")
	}
	e := &External{directory: dir, binary: filepath.Join(dir, "helm"+filepath.Ext(filename))}
	complete := false
	defer func() {
		if !complete {
			e.Close()
		}
	}()
	out, err := os.OpenFile(e.binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return nil, externalFailure("Helm executable snapshot cannot be created")
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(f, (128<<20)+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n != info.Size() || n > 128<<20 {
		return nil, externalFailure("Helm executable snapshot cannot be completed")
	}
	e.SHA256 = hex.EncodeToString(hash.Sum(nil))
	complete = true
	return e, nil
}

func nativeHeader(header []byte) bool {
	if len(header) != 4 {
		return false
	}
	if header[0] == 'M' && header[1] == 'Z' {
		return true
	}
	for _, magic := range [][]byte{{0x7f, 'E', 'L', 'F'}, {0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, {0xbe, 0xba, 0xfe, 0xca}, {0xca, 0xfe, 0xba, 0xbf}, {0xbf, 0xba, 0xfe, 0xca}} {
		if bytes.Equal(header, magic) {
			return true
		}
	}
	return false
}

func (e *External) Close() {
	if e != nil && e.directory != "" {
		_ = os.RemoveAll(e.directory)
	}
}

// command runs the executable; ch, when set, is the chart a template failure
// may quote a literal message from (chartMessage).
func (e *External) command(ctx context.Context, dir string, ch *chart.Chart, limit int64, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "USERPROFILE=" + dir, "TMPDIR=" + dir, "TMP=" + dir, "TEMP=" + dir, "HELM_CACHE_HOME=" + filepath.Join(dir, "cache"), "HELM_CONFIG_HOME=" + filepath.Join(dir, "config"), "HELM_DATA_HOME=" + filepath.Join(dir, "data"), "HELM_PLUGINS=" + filepath.Join(dir, "no-plugins"), "KUBECONFIG=" + filepath.Join(dir, "no-kubeconfig"), "GOMEMLIMIT=256MiB"}
	for _, name := range []string{"SystemRoot", "WINDIR"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, e.Environment...)
	// Raw output holds indented YAML, "# Source:" comments and hooks, which
	// the targets limit (targetSet) does not count, so it is bounded loosely
	// at four times the limit plus 1 MiB. Diagnostics are only parsed for a
	// template location; past 64 KiB they are truncated, not a failure, so
	// verbose warnings do not stop a render.
	output := &procio.Bounded{Limit: 4*limit + (1 << 20)}
	diagnostics := &procio.Bounded{Limit: 64 << 10, Truncate: true}
	cmd.Stdout, cmd.Stderr = output, diagnostics
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// Name the stopped step; the issue reports it as a timeout or
			// cancellation.
			return "", domain.Fail(stoppedCode(ctx.Err()), "render", "external Helm render did not finish", "rerun the preview, or raise render.timeout if this render needs longer", ctx.Err())
		}
		if output.Exceeded {
			return "", domain.Fail(domain.CodeLimitExceeded, "render", "external Helm output exceeds four times render.maxResponseBytes plus 1 MiB", "reduce the target or adjust render.maxResponseBytes", nil)
		}
		raw := diagnostics.String()
		if m := missingFunction.FindStringSubmatch(raw); len(m) == 4 {
			return "", &helmParseFailure{diagnostic: m[0], cause: templateFailure(ch, m[0])}
		}
		if failure, ok := templateLocationFailure(ch, raw); ok {
			return "", failure
		}
		return "", externalFailure("Helm execution failed or exceeded its output limit; raw diagnostics are suppressed to protect values")
	}
	return output.String(), nil
}

func (e *External) Render(ctx context.Context, req Request) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return render(req, func(ch *chart.Chart, values map[string]any, helm map[string]any, req Request, inputs helmCLIInputs, probe func() error) (string, error) {
		return e.renderChart(ctx, ch, values, helm, req, inputs, probe)
	})
}

// renderChart renders one source's chart in an isolated directory with the
// executable after screening its time, DNS and randomness functions.
func (e *External) renderChart(ctx context.Context, ch *chart.Chart, values map[string]any, helm map[string]any, req Request, inputs helmCLIInputs, probe func() error) (string, error) {
	if hasRemoteSchema(ch) {
		return "", skip("remote JSON schema references require external access")
	}
	if domain.String(helm, "version") == "v4" && strings.HasPrefix(strings.TrimPrefix(e.Version, "v"), "3.") {
		return "", skip("Application requests Helm v4 but the selected executable is Helm v3")
	}
	data, err := yaml.Marshal(values)
	if err != nil {
		return "", renderError()
	}
	random, err := screenFunctions(ch, data, probe)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(e.directory, "render-")
	if err != nil {
		return "", externalFailure("isolated Helm render directory cannot be created")
	}
	defer os.RemoveAll(dir)
	if err = chartutil.SaveDir(ch, dir); err != nil {
		return "", renderError()
	}
	args, err := helmArgs(dir, ch, helm, req, inputs)
	if err != nil {
		return "", err
	}
	return e.renderVerified(ctx, dir, ch, args, req.Limit, random)
}

// screenFunctions reports whether the chart or its values name a
// random-output function. The external process cannot report which functions
// it calls. A chart that names a time, DNS or small-range function is
// rendered only when the bundled engine renders the same chart and values
// completely without calling one (probe). Random-output functions need no
// probe: a call makes the double render in renderVerified differ.
func screenFunctions(ch *chart.Chart, values []byte, probe func() error) (random bool, err error) {
	var named screening
	named.chart(ch)
	named.actions(values)
	random = named.random
	if named.stable {
		var skipped *domain.RenderSkipped
		var failure *domain.Failure
		if err := probe(); errors.Is(err, errNondeterministic) && errors.As(err, &skipped) {
			return false, skip("external Helm " + strings.TrimPrefix(skipped.Reason, "Local Helm analysis skipped: "))
		} else if err != nil {
			reason := "external Helm templates reference randomness, time or live-dependent functions, and the bundled engine could not render the chart to rule out a call"
			if errors.As(err, &failure) {
				reason += ": " + failure.Issue.Reason
			}
			return false, skip(reason)
		}
	}
	return random, nil
}

// helmArgs returns the helm template arguments for the chart saved in dir and
// writes the value files and file parameters they name next to it.
func helmArgs(dir string, ch *chart.Chart, helm map[string]any, req Request, inputs helmCLIInputs) ([]string, error) {
	release := releaseContext(helm, req)
	if chartutil.ValidateReleaseName(release.name) != nil {
		return nil, renderError()
	}
	if release.kubeVersion == "" {
		return nil, skip("Kubernetes version is not available for local rendering")
	}
	args := []string{"template", release.name, filepath.Join(dir, ch.Metadata.Name), "--namespace", release.namespace, "--kube-version", release.kubeVersion}
	// Preserve file bytes and upstream CLI parsing (including numeric types)
	// instead of feeding a premerged values map to a different Helm engine.
	for i, data := range inputs.valueFiles {
		filename := fmt.Sprintf("input-values-%d.yaml", i)
		if err := os.WriteFile(filepath.Join(dir, filename), data, 0600); err != nil {
			return nil, renderError()
		}
		args = append(args, "--values", filename)
	}
	for _, parameter := range inputs.parameters {
		args = append(args, "--set", parameter)
	}
	for _, parameter := range inputs.stringParameters {
		args = append(args, "--set-string", parameter)
	}
	for i, parameter := range inputs.fileParameters {
		filename := fmt.Sprintf("input-parameter-%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, filename), parameter.data, 0600); err != nil {
			return nil, renderError()
		}
		args = append(args, "--set-file", parameter.name+"="+filename)
	}
	if release.invalidAPIs {
		return nil, renderError()
	}
	for _, api := range release.apis {
		args = append(args, "--api-versions", api)
	}
	if helm["skipCrds"] != true {
		args = append(args, "--include-crds")
	}
	if helm["skipSchemaValidation"] == true {
		args = append(args, "--skip-schema-validation")
	}
	// Tests are filtered by shared tracking semantics after rendering, rather
	// than depending on version-specific --skip-tests CLI behavior.
	return args, nil
}

// renderVerified runs helm template in dir twice and returns the output only
// when both runs produce the same targets, or, for charts that name a random
// function (random), the targets with the differing fields marked. Failing templates are removed and recovered as partial render
// reasons. Inputs that already passed are answered from the verified cache,
// and a render of the same inputs that is still running is waited for, such
// as the base and head of an unaffected Application rendered concurrently.
func (e *External) renderVerified(ctx context.Context, dir string, ch *chart.Chart, args []string, limit int64, random bool) (string, error) {
	key, err := inputKey(e.Environment, dir, limit, args)
	if err != nil {
		return "", renderError()
	}
	entry, owner := e.renders.claim(key)
	if !owner {
		kept, ok, err := entry.wait(ctx)
		if err != nil {
			return "", err
		}
		if ok {
			return kept.result()
		}
		// The first render of these inputs failed, was cancelled or was too
		// large to keep: a failed determinism check is not reused, so these
		// inputs are rendered here.
		render, err := e.verify(ctx, dir, ch, args, limit, random)
		if err != nil {
			return "", err
		}
		return render.result()
	}
	var render verifiedRender
	verified := false
	// Deferred so that waiters are released even if the render panics. A
	// verified render is kept within maxVerifiedRenderBytes; a failure,
	// cancellation or a render too large to keep is rendered again by
	// waiters and later callers.
	defer func() {
		e.renders.settle(key, entry, verified, int64(len(render.output)), maxVerifiedRenderBytes, func() verifiedRender { return render })
	}()
	render, err = e.verify(ctx, dir, ch, args, limit, random)
	if err != nil {
		return "", err
	}
	verified = true
	return render.result()
}

// verify is the double render of renderVerified, without the cache.
func (e *External) verify(ctx context.Context, dir string, ch *chart.Chart, args []string, limit int64, random bool) (verifiedRender, error) {
	first, err := e.command(ctx, dir, ch, limit, args...)
	reasons := []string{}
	var index templateIndex
	for err != nil {
		var parseFailure *helmParseFailure
		if !errors.As(err, &parseFailure) || len(reasons) >= 16 {
			return verifiedRender{}, err
		}
		name, ok := recoverTemplate(ch, parseFailure.diagnostic, &index)
		if !ok {
			return verifiedRender{}, err
		}
		if os.Remove(filepath.Join(dir, ch.Metadata.Name, filepath.FromSlash(name))) != nil {
			return verifiedRender{}, renderError()
		}
		reasons = append(reasons, err.Error())
		first, err = e.command(ctx, dir, ch, limit, args...)
	}
	second, err := e.command(ctx, dir, ch, limit, args...)
	if err != nil {
		return verifiedRender{}, err
	}
	if marked, same, ok := determinism.Compare(first, second); !same {
		if !random {
			return verifiedRender{}, domain.Fail(domain.CodeRenderNondeterministic, "render", "external Helm produced different targets for identical inputs", "use a deterministic Helm executable and chart", nil)
		}
		// Fields that differ, such as a generated password, are marked and
		// not compared; the rest of the targets are.
		if !ok {
			return verifiedRender{}, nondeterministicSkip("external Helm templates call randomness functions: two renders of identical inputs produced different resources")
		}
		first = marked
	}
	return verifiedRender{output: first, reasons: reasons}, nil
}

// screening records whether template actions name an unsafeFunction
// (random) or a stableFunction (stable), so one pass answers both.
type screening struct{ random, stable bool }

func (s *screening) done() bool { return s.random && s.stable }

func (s *screening) actions(data []byte) {
	if s.done() {
		return
	}
	for _, action := range templateAction.FindAll(data, -1) {
		s.random = s.random || unsafeFunction.Match(action)
		s.stable = s.stable || stableFunction.Match(action)
		if s.done() {
			return
		}
	}
}

// chart screens the templates, files and values of ch and its subcharts.
// Values that cannot be encoded count as naming both kinds of function.
func (s *screening) chart(ch *chart.Chart) {
	for _, f := range ch.Templates {
		s.actions(f.Data)
	}
	for _, f := range ch.Files {
		s.actions(f.Data)
	}
	if s.done() {
		return
	}
	data, err := yaml.Marshal(ch.Values)
	if err != nil {
		s.random, s.stable = true, true
		return
	}
	s.actions(data)
	for _, child := range ch.Dependencies() {
		if s.done() {
			return
		}
		s.chart(child)
	}
}
