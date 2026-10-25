package preview

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/profile"
	"diffaro/internal/security"
)

// InputFiles supplies the PR's changed files for input-change evidence.
type InputFiles struct {
	// Files are the complete PR paths, including previous names of renamed files.
	Files []string
	// States holds StateAdded or StateRemoved for the paths the PR adds or
	// removes; every other path is changed.
	States map[string]domain.State
	// Read returns a PR-repository file at an immutable revision; found is
	// false when the file does not exist there.
	Read func(ctx context.Context, revision, file string) (data []byte, found bool, err error)
}

const (
	maxInputFileReads  = 128
	maxChartReads      = 256
	maxInputValueBytes = 1 << 20
	maxInputChanges    = 64
	maxInputKeyChanges = 200
)

// inputFilesUnavailable states why PR file input changes are not listed.
const inputFilesUnavailable = "Value and chart file input changes (inputChanges) need --pr and a complete PR file list; none are listed in this report."

// FileListLimitation states why the PR file list could not be read. The list
// is evidence for changed-path selection and inputChanges only, so its
// failure is a limitation that never changes the status or the exit code.
func FileListLimitation(err error, repository string, pr int) string {
	i := ContextualIssue(err, domain.ApplicationID{}, repository, "")
	code := i.Code
	if i.ReasonCode != "" {
		code += " (" + i.ReasonCode + ")"
	}
	text := fmt.Sprintf("The PR file list of %s#%d could not be read: %s at stage %s. %s. Required action: %s.", repository, pr, code, i.Stage, i.Reason, i.Action)
	if access := i.RequiredAccess; access != nil {
		text += fmt.Sprintf(" Required access: %s %s on %s.", access.System, access.Action, access.Target)
	}
	return text
}

// AttachInputChanges lists, for each Application, the PR files among the
// inputs it reads on base or head: value files with key-level changes, and
// other files under its chart directories by path. Only sources rendered at
// the PR's base and head revisions are considered; sources pinned elsewhere do
// not change with the PR. The result is evidence and never changes states or
// the exit code.
func (r Runner) AttachInputChanges(ctx context.Context, report *domain.Report, files *InputFiles) {
	r.attachInputChanges(ctx, report, files, nil)
}

func (r Runner) attachInputChanges(ctx context.Context, report *domain.Report, files *InputFiles, head *changedPathSelector) {
	if len(report.Applications) == 0 {
		return
	}
	if files == nil || report.Revisions.Mode != domain.RevisionModePR {
		report.Limitations = append(report.Limitations, inputFilesUnavailable)
		return
	}
	reader := &inputReader{files: files, pair: report.Revisions, redactor: r.Redactor, repository: r.Config.Git.RepositoryURL, cache: map[string]inputResult{}, charts: map[string]*changedPathSelector{}, timeout: r.Config.Timeout()}
	reader.head = head
	changed := append([]string(nil), files.Files...)
	sort.Strings(changed)
	changed = compactStrings(changed)
	var unresolved, truncated bool
	for i := range report.Applications {
		app := &report.Applications[i]
		valueFiles, chartDirs := map[string]bool{}, map[string]bool{}
		for _, side := range []struct {
			sources  []domain.SourceSummary
			revision string
		}{{inputSources(*app, report.Revisions, reader.repository, true), report.Revisions.Base}, {inputSources(*app, report.Revisions, reader.repository, false), report.Revisions.Head}} {
			if !r.sideInputs(ctx, side.sources, side.revision, changed, reader, valueFiles, chartDirs) {
				unresolved = true
			}
		}
		added := 0
		for _, file := range changed {
			kind := ""
			if valueFiles[file] {
				kind = "valueFile"
			} else if dir, ok := chartDir(chartDirs, file); ok {
				kind = "chartFile"
				if file == path.Join(dir, "values.yaml") {
					kind = "valueFile"
				}
			}
			if kind == "" {
				continue
			}
			if added == maxInputChanges {
				truncated = true
				break
			}
			change := domain.InputChange{Kind: kind, Repository: reader.repository, Path: file, State: domain.StateChanged, Comparison: domain.ComparisonPathOnly, Changes: []domain.Change{}}
			if state, ok := files.States[file]; ok {
				change.State = state
			}
			if kind == "valueFile" {
				result, ok := reader.valueFile(ctx, file)
				if !ok {
					continue
				}
				change.State, change.Comparison, change.Reason, change.Changes = result.state, result.comparison, result.reason, maskTargetValues(result.changes, r.Redactor.Masked)
			}
			app.InputChanges = append(app.InputChanges, change)
			added++
		}
		sort.SliceStable(app.InputChanges, func(a, b int) bool {
			return app.InputChanges[a].Path < app.InputChanges[b].Path
		})
	}
	if unresolved {
		report.Limitations = append(report.Limitations, "Some local file:// chart dependencies could not be followed; PR changes under them may be missing from inputChanges.")
	}
	if truncated {
		report.Limitations = append(report.Limitations, "inputChanges lists at most "+strconv.Itoa(maxInputChanges)+" changed input files per Application; see the PR diff for the rest.")
	}
}

// inputSources returns one side's sources as summaries. Explicit runs render
// a single stored source at the pair revisions and carry no target summary.
func inputSources(app domain.ApplicationResult, pair domain.RevisionPair, repository string, base bool) []domain.SourceSummary {
	target, sources, revision := app.HeadTarget, app.HeadSources, pair.Head
	if base {
		target, sources, revision = app.BaseTarget, app.BaseSources, pair.Base
	}
	if target != nil {
		if len(target.Sources) > 0 {
			return target.Sources
		}
		return []domain.SourceSummary{{Repository: target.Repository, Revision: target.Revision, Inputs: target.Inputs}}
	}
	if len(sources) > 0 {
		return sources
	}
	if app.BaseTarget != nil || app.HeadTarget != nil || app.SkipReason == otherRepositorySkip {
		return nil
	}
	return []domain.SourceSummary{{Repository: repository, Revision: revision, Inputs: app.Inputs}}
}

// sideInputs adds the PR-repository value files and chart directories that
// one side's sources read at revision. Local file:// chart dependencies are
// followed only when a changed file lies outside the chart directory. It
// returns false when a dependency could not be followed.
func (r Runner) sideInputs(ctx context.Context, sources []domain.SourceSummary, revision string, changed []string, reader *inputReader, valueFiles, chartDirs map[string]bool) bool {
	follows := func(s domain.SourceSummary) bool {
		return s.Revision == revision && profile.SameRepositoryAny(s.Repository, r.Config.Git.RepositoryURL, r.Config.Git.Aliases())
	}
	refs := map[string]domain.SourceSummary{}
	for _, s := range sources {
		if s.Ref != "" {
			refs[s.Ref] = s
		}
	}
	resolved := true
	for _, s := range sources {
		if !follows(s) {
			continue
		}
		// An empty path names no chart (a values-only source or unknown
		// inputs); "." is a chart at the repository root.
		root, ok := relativeSourceDir(s.Inputs.Path)
		if s.Inputs.Path != "" && !ok {
			continue
		}
		if s.Inputs.Path != "" {
			chartDirs[root] = true
			if root != "" && outside(changed, root) {
				dirs := []string{}
				if reader.dependencies(ctx, revision, root, &dirs) != "" {
					resolved = false
				}
				for _, dir := range dirs {
					chartDirs[dir] = true
				}
			}
		}
		for _, value := range s.Inputs.ValueFiles {
			if strings.HasPrefix(value, "$") {
				name, rest, _ := strings.Cut(strings.TrimPrefix(value, "$"), "/")
				if ref, ok := refs[name]; ok && follows(ref) {
					if file, ok := repositoryFile("", rest); ok {
						valueFiles[file] = true
					}
				}
				continue
			}
			if file, ok := repositoryFile(root, value); ok {
				valueFiles[file] = true
			}
		}
	}
	return resolved
}

func outside(files []string, dir string) bool {
	for _, file := range files {
		if !strings.HasPrefix(file, dir+"/") {
			return true
		}
	}
	return false
}

func chartDir(dirs map[string]bool, file string) (string, bool) {
	best, found := "", false
	for dir := range dirs {
		if (dir == "" || strings.HasPrefix(file, dir+"/")) && (!found || len(dir) > len(best)) {
			best, found = dir, true
		}
	}
	return best, found
}

func compactStrings(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// valueChanges compares base and head values documents and keeps at most
// maxInputKeyChanges key changes; truncated reports that some were dropped.
func valueChanges(pointer string, documents [2]map[string]any, redactor security.Redactor) (changes []domain.Change, truncated bool) {
	changes = manifest.CompareValues(pointer, documents[0], documents[1], redactor)
	if len(changes) > maxInputKeyChanges {
		return changes[:maxInputKeyChanges], true
	}
	return changes, false
}

// inlineValueChanges lists key-level changes of spec helm.values strings
// between two specs of the same Application. Helm input values stay masked,
// as in specChanges; only changed key paths are shown.
func inlineValueChanges(base, head domain.Application, redactor security.Redactor) []domain.InputChange {
	type located struct {
		pointer string
		helm    map[string]any
	}
	collect := func(app domain.Application) map[string]located {
		out := map[string]located{}
		if sources := app.Sources(); len(sources) > 0 {
			for i, value := range sources {
				source, _ := value.(map[string]any)
				helm, _ := source["helm"].(map[string]any)
				pointer := "/spec/sources/" + strconv.Itoa(i) + "/helm/values"
				out[pointer] = located{pointer, helm}
			}
			return out
		}
		helm, _ := app.Source()["helm"].(map[string]any)
		out["/spec/source/helm/values"] = located{"/spec/source/helm/values", helm}
		return out
	}
	left, right := collect(base), collect(head)
	pointers := map[string]bool{}
	for p := range left {
		pointers[p] = true
	}
	for p := range right {
		pointers[p] = true
	}
	keys := make([]string, 0, len(pointers))
	for p := range pointers {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	var out []domain.InputChange
	for _, pointer := range keys {
		b, bok := left[pointer].helm["values"].(string)
		h, hok := right[pointer].helm["values"].(string)
		if (!bok && !hok) || (bok && hok && b == h) {
			continue
		}
		change := domain.InputChange{Kind: "inlineValues", Path: pointer, State: domain.StateChanged, Comparison: domain.ComparisonComplete, Changes: []domain.Change{}}
		if !bok {
			change.State = domain.StateAdded
		} else if !hok {
			change.State = domain.StateRemoved
		}
		documents := [2]map[string]any{{}, {}}
		for i, text := range []string{b, h} {
			if len(text) > maxInputValueBytes {
				change.Comparison, change.Reason = domain.ComparisonIncomplete, "inline values exceed "+strconv.Itoa(maxInputValueBytes)+" bytes"
				break
			}
			values, err := manifest.ParseValues([]byte(text))
			if err != nil {
				change.Comparison, change.Reason = domain.ComparisonIncomplete, strings.Replace(err.Error(), "values file", "inline values", 1)
				break
			}
			documents[i] = values
		}
		if change.Comparison == domain.ComparisonComplete {
			var truncated bool
			if change.Changes, truncated = valueChanges(pointer, documents, redactor); truncated {
				change.Comparison, change.Reason = domain.ComparisonIncomplete, "more than "+strconv.Itoa(maxInputKeyChanges)+" key changes"
			}
		}
		out = append(out, change)
	}
	return out
}
