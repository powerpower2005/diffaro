package localhelm

import (
	"path"

	"diffaro/internal/domain"
)

// trimRepositories reduces each repository file map of a render request to
// the files the render can read: the planNeeds chart directories and value or
// file parameter files, plus same-repository file:// dependency directories.
// Source overrides, .helmignore and vendored or downloaded charts lie inside
// those directories. Repository links are checked before rendering and never
// followed, so no link target is read. When the read set is not certain the
// maps are returned unchanged. The input maps are never modified.
func trimRepositories(plan []domain.SourceRevision, repositories map[string]map[string][]byte) map[string]map[string][]byte {
	if !trimmablePlan(plan) {
		return repositories
	}
	needs := planNeeds(Request{Plan: plan})
	result := make(map[string]map[string][]byte, len(repositories))
	for key, files := range repositories {
		need := needs[key]
		if need == nil {
			return repositories
		}
		trimmed, ok := trimFiles(files, need)
		if !ok {
			return repositories
		}
		if len(trimmed) == 0 {
			// The render treats an empty repository as a failure; keep the
			// original so the outcome does not depend on trimming.
			trimmed = files
		}
		result[key] = trimmed
	}
	return result
}

// trimmablePlan reports whether planNeeds names exactly the paths the render
// reads: every chart path is a clean repository subdirectory and every value
// file or file parameter is trimmable (inputFile.trimmable): it resolves
// without build environment expansion or absolute paths, which collectValues
// would resolve differently.
func trimmablePlan(plan []domain.SourceRevision) bool {
	for _, s := range plan {
		root := domain.String(s.Source, "path")
		if root == "" && s.Ref == "" || root != "" && (!domain.SafeRelativePath(root) || root == ".") {
			return false
		}
	}
	for _, in := range resolveInputs(Request{Plan: plan}, planRefs(plan)) {
		if !in.trimmable() {
			return false
		}
	}
	return true
}

// trimFiles keeps the needed files and the files under the needed directories,
// extended by the file:// dependencies of every chart inside them. Including
// charts that the render may never follow only keeps more files.
func trimFiles(files map[string][]byte, need *sourceNeeds) (map[string][]byte, bool) {
	dirs := map[string]bool{}
	for dir := range need.dirs {
		if dir == "." {
			return nil, false
		}
		dirs[dir] = true
	}
	charts := map[string]bool{}
	for name := range files {
		if base := path.Base(name); base == "Chart.yaml" || base == "requirements.yaml" {
			charts[path.Dir(name)] = true
		}
	}
	done := map[string]bool{}
	for added := true; added; {
		added = false
		for dir := range charts {
			if done[dir] || !(dirs[dir] || underAny(dir, dirs)) {
				continue
			}
			done[dir] = true
			// Parsed like the upstream chart loader; a chart file that does
			// not parse leaves the read set uncertain.
			dependencies, ok := fileDependencies(files, dir, true)
			if !ok {
				return nil, false
			}
			for _, dependency := range dependencies {
				if dependency == "." {
					return nil, false
				}
				if domain.SafeRelativePath(dependency) && !dirs[dependency] {
					dirs[dependency] = true
					added = true
				}
			}
		}
	}
	result := map[string][]byte{}
	for name, data := range files {
		if need.files[name] || underAny(name, dirs) {
			result[name] = data
		}
	}
	return result, true
}
