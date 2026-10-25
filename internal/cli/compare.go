package cli

import (
	"context"
	"slices"

	"diffaro/internal/domain"
	"diffaro/internal/gitsource"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
	"diffaro/internal/revision"
	"diffaro/internal/security"
)

// compare resolves base/head revisions and runs the preview mode selected by
// the profile and flags: repository discovery, recursive or explicit roots.
func compare(ctx context.Context, c profile.Config, b *backends, o *commandOptions, pr int, redactor security.Redactor, progress *progress) domain.Report {
	base, head := o.base, o.head
	if o.command == "doctor" && base == "" && head == "" && pr == 0 {
		base, head = "HEAD", "HEAD"
	}
	progress.stage("resolving revisions")
	endResolve := domain.TraceStart(ctx, "resolve", "revisions", c.Git.Repository)
	pair, err := revision.Resolve(ctx, b.git, c.Git.Repository, base, head, pr)
	endResolve(err)
	if err != nil {
		return preview.RevisionFailureReport(ctx, c, o.recursive, err)
	}
	progress.stage("rendering and comparing target manifests")
	// Strings masked in rendered targets are also masked in value file key
	// changes of this run.
	redactor.Masked = &security.MaskedValues{}
	runner := preview.Runner{Config: c, Renderers: b.renderers, Redactor: redactor}
	var files []string
	var states map[string]domain.State
	complete := false
	var filesErr error
	if pr != 0 && pair.Mode == domain.RevisionModePR {
		if files, states, complete, filesErr = b.git.ChangedFiles(ctx, c.Git.Repository, pr); filesErr != nil {
			complete = false
		}
	}
	evidence := preview.ComparisonEvidence{Notes: func() []string {
		notes := slices.Clone(b.notes)
		if b.local != nil {
			notes = append(notes, b.local.Notes()...)
		}
		return notes
	}}
	if c.Discovery != nil && c.Discovery.Selection == profile.SelectionChangedPaths {
		runner.ChangedPaths, evidence.SelectionNotice = changedPathInput(ctx, b.source, b.sources, c, pair, pr, files, complete)
	}
	if complete {
		evidence.Files = &preview.InputFiles{Files: files, States: states, Read: func(ctx context.Context, revision, file string) ([]byte, bool, error) {
			return b.source.File(ctx, c.Git.Repository, revision, file)
		}}
	}
	if filesErr != nil {
		evidence.FileListFailure = preview.FileListLimitation(filesErr, c.Git.Repository, pr)
	}
	return runner.Compare(ctx, pair, o.recursive, b.inventories, evidence)
}

// changedPathInput reads the PR's changed files for opt-in changed-path
// selection. Without a complete list every candidate is rendered.
func changedPathInput(ctx context.Context, git sourceReader, sources *gitsource.Cache, c profile.Config, pair domain.RevisionPair, pr int, files []string, complete bool) (*preview.ChangedPaths, string) {
	if pr == 0 || pair.Mode != domain.RevisionModePR {
		return nil, "Changed-path selection requires --pr; every repository candidate was rendered."
	}
	if !complete {
		return nil, "Changed-path selection could not read the complete PR file list; every repository candidate was rendered."
	}
	readFile := func(ctx context.Context, file string) ([]byte, bool, error) {
		return git.File(ctx, c.Git.Repository, pair.Head, file)
	}
	paths := &preview.ChangedPaths{Files: files, Read: readFile}
	// One tree listing excludes symbolic links and replaces reads of absent
	// chart metadata. Without it every candidate is rendered. Local Helm
	// renders of the head revision reuse it through the shared cache.
	if entries, err := sources.Tree(ctx, c.Git.Repository, pair.Head); err == nil {
		// Chart metadata comes from one archive read instead of one request
		// per chart when the revision fits the source snapshot limit; local
		// Helm renders of the head revision reuse that archive.
		if fromArchive := sources.ArchiveReader(c.Git.Repository, pair.Head, entries, c.Render.SourceSnapshotBytes()); fromArchive != nil {
			paths.Read = func(ctx context.Context, file string) ([]byte, bool, error) {
				if data, found, err := fromArchive(ctx, file); err == nil {
					return data, found, nil
				}
				return readFile(ctx, file)
			}
		}
		paths.Listed, paths.Present = true, make(map[string]bool, len(entries))
		for _, entry := range entries {
			paths.Present[entry.Path] = true
			if entry.Link() {
				paths.Links = append(paths.Links, entry.Path)
			}
		}
	}
	return paths, ""
}
