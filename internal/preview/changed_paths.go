package preview

import (
	"context"
	"path"
	"strings"
	"sync"

	"diffaro/internal/domain"

	"go.yaml.in/yaml/v3"
)

// ChangedPaths enables opt-in changed-path selection. Base and head
// render with the same stored Application spec and backend, so a candidate
// whose PR-repository inputs are unchanged renders identically on both sides.
// Candidates are skipped only when that independence is proven.
type ChangedPaths struct {
	// Files are PR paths, including previous names of renamed files.
	Files []string
	// Read returns a PR-repository file at the head revision; found is false
	// when the file does not exist.
	Read func(ctx context.Context, file string) (data []byte, found bool, err error)
	// Listed reports that Present and Links describe the complete head tree.
	// Without a listing, symbolic links cannot be excluded and every
	// candidate is rendered.
	Listed bool
	// Present, when set, holds every file path at head; Read is not called
	// for other paths.
	Present map[string]bool
	// Links holds the symbolic links at head.
	Links []string
}

const (
	selectionRendered   = "rendered"
	selectionUnaffected = "unaffected"
	selectionIneligible = "ineligible"
	// selectionSourceMissing: the source path exists at neither revision,
	// so nothing renders and the candidate is not rendered.
	selectionSourceMissing = "source-missing"
	maxChartDepth          = 16
)

// changedPathSelector caches Chart.yaml reads shared by several candidates.
// Graph walks decide descendants concurrently: decisions run in parallel and
// only the chart cache is locked, never during a read. Concurrent readers of
// one directory share a single read (chart).
type changedPathSelector struct {
	paths   ChangedPaths
	matches func(string) bool
	charts  flight[string, chartReading]
	// prior, when set, is a selector reading the same revision whose kept
	// chart readings are reused instead of reading again.
	prior *changedPathSelector
	// dirs holds every directory above a Present file, built once.
	dirsOnce sync.Once
	dirs     map[string]bool
}

type chartReading struct {
	dependencies []string
	reason       string
}

func newChangedPathSelector(paths ChangedPaths, matches func(string) bool) *changedPathSelector {
	return &changedPathSelector{paths: paths, matches: matches}
}

// decide returns the selection and a stable reason. manages is the inventory
// status hint for App-of-Apps parents; nil means unknown.
func (s *changedPathSelector) decide(ctx context.Context, app domain.Application, manages *bool) (string, string) {
	selection, reason := s.decideCandidate(ctx, app, manages, false)
	if selection == selectionIneligible {
		if missing := s.missingSource(app); missing != "" {
			return selectionSourceMissing, missing
		}
	}
	return selection, reason
}

// missingSource proves that a candidate's single PR-repository source path
// has no file at head and that no PR file is under it, so the path existed
// at base neither: both renders fail the same way and produce no target or
// child Application. It returns the reason, or "" without that proof.
func (s *changedPathSelector) missingSource(app domain.Application) string {
	if multipleSources(app) {
		return ""
	}
	source := app.Source()
	if source == nil || !s.matches(domain.String(source, "repoURL")) || !s.paths.Listed || s.paths.Present == nil {
		return ""
	}
	// An invalid or escaping path is not proven missing: the candidate is
	// rendered so the backend reports the failure on both revisions.
	root, ok := relativeSourceDir(domain.String(source, "path"))
	if !ok || root == "" || s.touched(root) {
		return ""
	}
	if s.paths.Present[root] || s.presentDirs()[root] {
		return ""
	}
	return "source path " + root + "/ does not exist at base or head and no PR file is under it; the Application renders nothing on either revision"
}

// presentDirs returns every directory D with a Present file under D + "/".
func (s *changedPathSelector) presentDirs() map[string]bool {
	s.dirsOnce.Do(func() {
		s.dirs = map[string]bool{}
		for file := range s.paths.Present {
			// A known directory already holds every directory above it.
			for i := strings.LastIndexByte(file, '/'); i >= 0 && !s.dirs[file[:i]]; i = strings.LastIndexByte(file[:i], '/') {
				s.dirs[file[:i]] = true
			}
		}
	})
	return s.dirs
}

// decideDescendant decides a child rendered with the same spec on both
// revisions. A child without a PR-repository source then has
// identical inputs on both sides; only its own children could still differ.
func (s *changedPathSelector) decideDescendant(ctx context.Context, app domain.Application, manages *bool) (string, string) {
	if s.referencesPR(app) {
		return s.decideCandidate(ctx, app, manages, false)
	}
	if reason := managesReason(manages); reason != "" {
		return selectionIneligible, reason
	}
	return selectionUnaffected, "no source is the PR repository and the Application manages no child Applications"
}

// inputsUnchanged proves, as an unaffected decision does, that the PR changes
// none of the Application's own inputs, whether or not it manages child
// Applications. A chart directory without Chart.yaml that the PR does not
// touch lacks it on both revisions, so it adds no dependency to either side.
func (s *changedPathSelector) inputsUnchanged(ctx context.Context, app domain.Application) bool {
	none := false
	selection, _ := s.decideCandidate(ctx, app, &none, true)
	return selection == selectionUnaffected
}

func (s *changedPathSelector) referencesPR(app domain.Application) bool {
	if sources := app.Sources(); len(sources) > 0 {
		for _, value := range sources {
			if source, ok := value.(map[string]any); ok && s.matches(domain.String(source, "repoURL")) {
				return true
			}
		}
		return false
	}
	source := app.Source()
	return source == nil || s.matches(domain.String(source, "repoURL"))
}

func managesReason(manages *bool) string {
	if manages == nil {
		return "Argo CD status does not show whether this Application manages child Applications"
	}
	if *manages {
		return "App-of-Apps parent; descendants are discovered by rendering"
	}
	return ""
}

// decideCandidate decides one candidate. absentCharts accepts an untouched chart
// directory without Chart.yaml as having no dependencies; selection keeps
// such a candidate rendered so its failure is reported.
func (s *changedPathSelector) decideCandidate(ctx context.Context, app domain.Application, manages *bool, absentCharts bool) (string, string) {
	if multipleSources(app) {
		return selectionIneligible, "multi-source Application inputs can cross repositories"
	}
	source := app.Source()
	if source == nil || !s.matches(domain.String(source, "repoURL")) {
		return selectionIneligible, "source is not the PR repository"
	}
	if domain.UsesNonHelmSource(source) {
		return selectionIneligible, "only Git Helm sources are evaluated"
	}
	root, ok := relativeSourceDir(domain.String(source, "path"))
	if !ok || root == "" {
		return selectionIneligible, "source path is the repository root or not a relative directory"
	}
	if reason := managesReason(manages); reason != "" {
		return selectionIneligible, reason
	}
	files, reason := localInputs(root, source)
	if reason != "" {
		return selectionIneligible, reason
	}
	if s.touched(root) {
		return selectionRendered, "PR changes files under the source path"
	}
	for _, file := range files {
		if s.changed(file) {
			return selectionRendered, "PR changes a value file or file parameter outside the source path"
		}
	}
	dirs := []string{}
	if reason := s.dependencies(ctx, root, root, 0, &dirs, absentCharts); reason != "" {
		return selectionIneligible, reason
	}
	for _, dir := range dirs {
		if s.touched(dir) {
			return selectionRendered, "PR changes a local chart dependency outside the source path"
		}
	}
	if !s.paths.Listed {
		return selectionIneligible, "the repository tree could not be listed, so symbolic links cannot be excluded"
	}
	// Helm follows links inside a chart, which can read files elsewhere.
	for _, link := range s.paths.Links {
		if s.linked(link, append([]string{root}, dirs...), files) {
			return selectionIneligible, "a symbolic link among the inputs may read files outside them"
		}
	}
	return selectionUnaffected, "no PR file is under the source path " + root + "/ or its value files and local chart dependencies"
}

// relativeSourceDir returns an Application source path as a directory
// relative to the repository root, "" for the root itself ("."). ok is false
// for an empty path and for a path that is not clean, is absolute, leaves the
// repository or contains a backslash or NUL.
func relativeSourceDir(p string) (string, bool) {
	if !domain.SafeRelativePathAllowingColon(p) {
		return "", false
	}
	if p == "." {
		return "", true
	}
	return p, true
}

func (s *changedPathSelector) touched(dir string) bool {
	for _, file := range s.paths.Files {
		if strings.HasPrefix(file, dir+"/") {
			return true
		}
	}
	return false
}

func (s *changedPathSelector) linked(link string, dirs, files []string) bool {
	for _, dir := range dirs {
		if strings.HasPrefix(link, dir+"/") {
			return true
		}
	}
	for _, file := range files {
		if link == file {
			return true
		}
	}
	return false
}

func (s *changedPathSelector) changed(name string) bool {
	for _, file := range s.paths.Files {
		if file == name {
			return true
		}
	}
	return false
}

// localInputs returns the value and file inputs outside root as repository
// paths. Inputs that are not literal repository files fail closed.
func localInputs(root string, source map[string]any) ([]string, string) {
	helm, _ := source["helm"].(map[string]any)
	inputs := []string{}
	if files, ok := helm["valueFiles"].([]any); ok {
		for _, f := range files {
			value, _ := f.(string)
			inputs = append(inputs, value)
		}
	}
	if params, ok := helm["fileParameters"].([]any); ok {
		for _, p := range params {
			item, _ := p.(map[string]any)
			inputs = append(inputs, domain.String(item, "path"))
		}
	}
	outside := []string{}
	for _, input := range inputs {
		name, ok := repositoryFile(root, input)
		if !ok {
			return nil, "a valueFiles or fileParameters entry is not a literal file in the repository"
		}
		if !strings.HasPrefix(name, root+"/") {
			outside = append(outside, name)
		}
	}
	return outside, ""
}

// repositoryFile resolves a literal path relative to root. Variables, globs,
// URLs, absolute paths and paths escaping the repository are rejected.
func repositoryFile(root, relative string) (string, bool) {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.ContainsAny(relative, "$*?[\\\x00") || strings.Contains(relative, "://") {
		return "", false
	}
	name := path.Join(root, relative)
	if name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

// dependencies follows local file:// chart dependencies and collects their
// directories outside root. It fails closed when one escapes the repository
// or cannot be read. Chart metadata is read at head; a change to it is a
// change under root or under a collected directory.
func (s *changedPathSelector) dependencies(ctx context.Context, root, dir string, depth int, outside *[]string, absentCharts bool) string {
	if depth > maxChartDepth {
		return "local chart dependencies exceed the supported depth"
	}
	reading := s.chart(ctx, dir)
	if reading.reason == chartMissing && absentCharts && !s.touched(dir) {
		return ""
	}
	if reading.reason != "" {
		return reading.reason
	}
	for _, dependency := range reading.dependencies {
		target, ok := repositoryFile(dir, strings.TrimPrefix(dependency, "file://"))
		if !ok {
			return "a file:// chart dependency resolves outside the repository"
		}
		if !strings.HasPrefix(target+"/", root+"/") {
			*outside = append(*outside, target)
		}
		if reason := s.dependencies(ctx, root, target, depth+1, outside, absentCharts); reason != "" {
			return reason
		}
	}
	return ""
}

const chartMissing = "Chart.yaml is missing at the source path"

// chart returns dir's cached reading or reads it. A caller that finds a read
// of dir in flight waits for it, within its own ctx, instead of reading
// again. A canceled or expired read is not cached, so the next caller, or a
// waiter, reads again.
func (s *changedPathSelector) chart(ctx context.Context, dir string) chartReading {
	if s.prior != nil {
		if reading, ok := s.prior.charts.kept(dir); ok {
			return reading
		}
	}
	reading, _, err := s.charts.do(ctx, dir, func() chartReading { return s.readChart(ctx, dir) }, func(chartReading) bool { return ctx.Err() == nil })
	if err != nil {
		return chartReading{reason: chartUnreadable}
	}
	return reading
}

const chartUnreadable = "chart metadata could not be read"

func (s *changedPathSelector) readChart(ctx context.Context, dir string) chartReading {
	if s.paths.Read == nil {
		return chartReading{reason: "chart metadata is unavailable"}
	}
	local := []string{}
	for i, name := range []string{"Chart.yaml", "requirements.yaml"} {
		file := path.Join(dir, name)
		if s.paths.Present != nil && !s.paths.Present[file] {
			if i == 0 {
				return chartReading{reason: chartMissing}
			}
			continue
		}
		data, found, err := s.paths.Read(ctx, file)
		if err != nil {
			return chartReading{reason: chartUnreadable}
		}
		if !found {
			if i == 0 {
				return chartReading{reason: chartMissing}
			}
			continue
		}
		var chart struct {
			Dependencies []struct {
				Repository string `yaml:"repository"`
			} `yaml:"dependencies"`
		}
		if yaml.Unmarshal(data, &chart) != nil {
			return chartReading{reason: "chart metadata is not valid YAML"}
		}
		for _, d := range chart.Dependencies {
			if strings.HasPrefix(d.Repository, "file://") {
				local = append(local, d.Repository)
			}
		}
	}
	return chartReading{dependencies: local}
}
