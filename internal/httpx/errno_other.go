//go:build !windows

package httpx

import "syscall"

// Socket errors of a refused and a reset connection.
var (
	refusedErrnos = []error{syscall.ECONNREFUSED}
	resetErrnos   = []error{syscall.ECONNRESET}
)
