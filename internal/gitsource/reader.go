package gitsource

import (
	"context"

	"diffaro/internal/github"
)

// Reader reads immutable source files without following symbolic links.
type Reader interface {
	Archive(context.Context, string, string) (map[string][]byte, []string, error)
	Resolve(context.Context, string, string) (string, error)
}

// TreeReader is an optional Reader capability for file-by-file reads.
type TreeReader interface {
	Tree(context.Context, string, string) ([]github.TreeEntry, error)
	Blob(context.Context, string, string) ([]byte, error)
}

// MaxArchiveEntries leaves room for directories within the archive reader's bound.
const MaxArchiveEntries = github.MaxArchiveEntries / 2
