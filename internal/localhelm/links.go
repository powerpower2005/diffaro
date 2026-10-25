package localhelm

import (
	"path"
	"strings"

	"diffaro/internal/domain"
)

// Argo CD follows symbolic links that stay inside a repository. Local Helm
// keeps only their paths (see gitsource.Reader), so a render that would read
// through a link is skipped instead of silently missing files.

func linkedInput() error {
	return skip("a chart, dependency, value file or file parameter path goes through a repository symbolic link that Argo CD would follow; local rendering does not follow links")
}

// linked reports whether p is a link, lies under a linked directory or
// contains a link.
func linked(links []string, p string) bool {
	p = path.Clean(p)
	for _, link := range links {
		if p == "." || p == link || strings.HasPrefix(p, link+"/") || strings.HasPrefix(link, p+"/") {
			return true
		}
	}
	return false
}

// linkedInputs checks every source's chart path, value files and file
// parameters against the links of the repository they are read from, with
// paths resolved like collectValues (resolveInputs).
func linkedInputs(req Request, links [][]string) error {
	inputs := resolveInputs(req, planRefs(req.Plan))
	for i, source := range req.Plan {
		root := domain.String(source.Source, "path")
		if root != "" && linked(links[i], root) {
			return domain.SourceFailure(linkedInput(), source)
		}
		for _, in := range inputs {
			// The render fails on a malformed or unresolved input before
			// reading any file.
			if in.Source != i || in.Malformed || in.Raw == "" || in.Err != nil {
				continue
			}
			if repoLinks := links[in.ReadFrom]; len(repoLinks) > 0 && linked(repoLinks, in.Path) {
				return domain.SourceFailure(linkedInput(), source)
			}
		}
	}
	return nil
}
