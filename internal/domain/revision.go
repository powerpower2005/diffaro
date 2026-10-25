package domain

import "regexp"

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// IsCommitSHA reports whether s is a full 40-character hexadecimal commit SHA,
// the only revision form treated as immutable. GitHub responses are checked
// separately and more strictly (lowercase) by github.ValidSHA.
func IsCommitSHA(s string) bool { return commitSHA.MatchString(s) }
