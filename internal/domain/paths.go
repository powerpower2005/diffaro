package domain

import (
	"path"
	"strings"
)

// SafeRelativePath reports whether p is a clean slash-separated path that
// stays inside its repository or chart root: already cleaned (so not empty),
// not absolute, not leaving through "..", and without a backslash, ':' or
// NUL. "." names the root itself. Source tree listings, local repository
// listings and chart paths use it.
func SafeRelativePath(p string) bool {
	return SafeRelativePathAllowingColon(p) && !strings.Contains(p, ":")
}

// SafeRelativePathAllowingColon is SafeRelativePath that accepts ':'.
// Single repository file reads and Application source paths use it.
func SafeRelativePathAllowingColon(p string) bool {
	return p == path.Clean(p) && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "/") && !strings.ContainsAny(p, "\\\x00")
}
