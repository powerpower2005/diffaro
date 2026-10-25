package localhelm

import (
	"path"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"sigs.k8s.io/yaml"

	"diffaro/internal/domain"
)

// inputKind names the helm field that declares an input file.
type inputKind string

const (
	valueFileInput     inputKind = "valueFiles"
	fileParameterInput inputKind = "fileParameters"
)

// inputFile is one helm.valueFiles or helm.fileParameters entry of a plan
// source, resolved like Argo CD by resolveInputs. Source planning
// (planNeeds), link checks (linkedInputs), trimming (trimmablePlan) and value
// collection (collectValues) read these instead of the raw spec, so they agree
// on the files a render reads. Each consumer keeps its own choice to skip or
// fail an entry that is malformed or does not resolve.
type inputFile struct {
	// Source is the index in Request.Plan of the source that declares it.
	Source int
	Kind   inputKind
	// Name is the file parameter name; value files have none.
	Name string
	// Raw is the path as written in the Application spec.
	Raw string
	// Malformed marks a value file that is not a string or a file parameter
	// that is not an object. It has no Raw path.
	Malformed bool
	// Ref is "$name" when Raw selects a ref source's repository root, and
	// Relative is Raw without that "$name/" prefix.
	Ref, Relative string
	// ReadFrom is the index in Request.Plan of the source whose repository
	// Path is read from, and RepoKey its Request.Repositories key.
	ReadFrom int
	RepoKey  string
	// Path is the resolved repository-relative path; Err is why Raw does not
	// resolve (a skip or an invalid input) and then Path is empty.
	Path string
	Err  error
}

// planRefs maps "$name" to the index of the plan source declaring ref name.
// A later duplicate wins; the render rejects duplicates before resolving.
func planRefs(plan []domain.SourceRevision) map[string]int {
	refs := map[string]int{}
	for i, s := range plan {
		if s.Ref != "" {
			refs["$"+s.Ref] = i
		}
	}
	return refs
}

// resolveInputs resolves the input files of every plan source in plan order,
// value files before file parameters. refs selects the ref sources a "$name/"
// prefix may name (planRefs, or the render's refs that have files).
func resolveInputs(req Request, refs map[string]int) []inputFile {
	isRef := func(name string) bool { _, ok := refs[name]; return ok }
	var out []inputFile
	for i, s := range req.Plan {
		root := domain.String(s.Source, "path")
		helm, _ := s.Source["helm"].(map[string]any)
		add := func(kind inputKind, name, raw string, malformed bool) {
			in := inputFile{Source: i, Kind: kind, Name: name, Raw: raw, Malformed: malformed, Relative: raw, ReadFrom: i}
			if !malformed {
				in.Ref, in.Relative = splitRef(raw, isRef)
				if in.Ref != "" {
					in.ReadFrom = refs[in.Ref]
				}
				in.Path, in.Err = inputPath(raw, root, isRef, req, s)
			}
			from := req.Plan[in.ReadFrom]
			in.RepoKey = from.Repository + "@" + from.Revision
			out = append(out, in)
		}
		if values, ok := helm["valueFiles"].([]any); ok {
			for _, value := range values {
				file, ok := value.(string)
				add(valueFileInput, "", file, !ok)
			}
		}
		if parameters, ok := helm["fileParameters"].([]any); ok {
			for _, parameter := range parameters {
				p, ok := parameter.(map[string]any)
				add(fileParameterInput, domain.String(p, "name"), domain.String(p, "path"), !ok)
			}
		}
	}
	return out
}

// splitRef returns the "$name" ref that raw selects and the rest of raw, or
// no ref and raw unchanged.
func splitRef(raw string, isRef func(string) bool) (ref, rest string) {
	if strings.HasPrefix(raw, "$") {
		name, rest, ok := strings.Cut(raw, "/")
		if ok && isRef(name) {
			return name, rest
		}
	}
	return "", raw
}

// inputPath resolves a value file or file parameter path like Argo CD. A
// "$ref/" prefix naming a ref source (isRef) selects that repository's root,
// build environment variables are expanded;
// a leading "/" is relative to the repository root instead of the chart root.
func inputPath(raw, root string, isRef func(string) bool, req Request, source domain.SourceRevision) (p string, err error) {
	if strings.ContainsAny(raw, "*?[]") || strings.Contains(raw, "://") {
		return "", skip("remote or glob value file inputs are outside the local context")
	}
	prefix := root
	if name, rest := splitRef(raw, isRef); name != "" {
		prefix, raw = ".", rest
	}
	raw, err = buildEnv(raw, req, source)
	if err != nil {
		return "", err
	}
	p = path.Join(prefix, raw)
	if strings.HasPrefix(raw, "/") {
		p = strings.TrimPrefix(raw, "/")
	}
	if !domain.SafeRelativePath(p) {
		return "", renderError()
	}
	return p, nil
}

// trimmable reports whether the input names its path without the render
// context: it is well formed and not empty, and its path after any ref prefix
// is not absolute and has no build environment variable, undefined ref, glob
// or remote URL, which collectValues would resolve differently or not at all.
func (in inputFile) trimmable() bool {
	f := in.Relative
	return !in.Malformed && f != "" && !strings.HasPrefix(f, "/") && !strings.ContainsAny(f, "$*?[]") && !strings.Contains(f, "://")
}

// fileDependencyDir returns the directory of a file:// chart dependency
// declared by the chart at dir, and false for any other repository.
func fileDependencyDir(dir, repository string) (string, bool) {
	if !strings.HasPrefix(repository, "file://") {
		return "", false
	}
	return path.Join(dir, strings.TrimPrefix(repository, "file://")), true
}

// fileDependencies returns the directories of file:// chart dependencies
// declared by the chart at dir in Chart.yaml or legacy requirements.yaml,
// read independently so the result is a superset. A strict caller parses the
// files like the upstream chart loader and gets false when one does not
// parse; otherwise only the dependency list is read and a file that does not
// parse is skipped.
func fileDependencies(files map[string][]byte, dir string, strict bool) ([]string, bool) {
	var out []string
	for _, name := range []string{"Chart.yaml", "requirements.yaml"} {
		data, ok := files[path.Join(dir, name)]
		if !ok {
			continue
		}
		var repositories []string
		if strict {
			var metadata chart.Metadata
			if yaml.Unmarshal(data, &metadata) != nil {
				return nil, false
			}
			for _, dependency := range metadata.Dependencies {
				if dependency != nil {
					repositories = append(repositories, dependency.Repository)
				}
			}
		} else {
			var metadata struct {
				Dependencies []struct {
					Repository string `yaml:"repository"`
				} `yaml:"dependencies"`
			}
			if yamlv3.Unmarshal(data, &metadata) != nil {
				continue
			}
			for _, dependency := range metadata.Dependencies {
				repositories = append(repositories, dependency.Repository)
			}
		}
		for _, repository := range repositories {
			if dependencyDir, ok := fileDependencyDir(dir, repository); ok {
				out = append(out, dependencyDir)
			}
		}
	}
	return out, true
}

// dependencyLoaded reports whether ch already holds a loaded (vendored or
// added) chart named name.
func dependencyLoaded(ch *chart.Chart, name string) bool {
	present := false
	for _, loaded := range ch.Dependencies() {
		if loaded.Name() == name {
			present = true
		}
	}
	return present
}
