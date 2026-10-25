package reposerver

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/helmerror"
)

// Diagnostics can contain credentials and rendered input. Retain only a small
// prefix in memory, drain the remainder, and emit fixed messages, never excerpts.
type processDiagnostic struct{ buffer bytes.Buffer }

const diagnosticLimit = 8 << 10

func (d *processDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	remaining := diagnosticLimit - d.buffer.Len()
	if remaining > 0 {
		d.buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

// ReadFrom drains a process pipe through a small buffer. Without it os/exec
// allocates a 32 KiB copy buffer per command, and a Kubernetes context read
// runs many short kubectl commands.
func (d *processDiagnostic) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 512)
	var total int64
	for {
		n, err := r.Read(buf)
		total += int64(n)
		d.Write(buf[:n])
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func processFailure(err error, diagnostic []byte, plaintext bool) error {
	reason := "repo-server RPC could not complete"
	action := "check repository access and backend logs within the trusted instance; no backend diagnostic text was included"
	text := strings.ToLower(string(diagnostic))
	// A render failure inside repo-server names its cause in Argo CD's
	// message; the message itself is withheld because it can hold values.
	if code, reason, action, ok := helmerror.RenderFailureCause(string(diagnostic), "repo-server"); ok {
		return domain.FailReason(domain.CodeRepoServerFailed, code, "render", reason, action, err)
	}
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, exec.ErrNotFound):
		reason = "grpcurl executable could not be started"
		action = "install grpcurl or correct repoServer.grpcurl; init accepts --grpcurl PATH"
	case errors.Is(err, os.ErrPermission):
		reason = "grpcurl executable access was denied"
		action = "check executable permissions and repoServer.grpcurl; no additional Argo CD permissions are required"
	case strings.Contains(text, "first record does not look like a tls handshake"):
		reason = "repo-server rejected the TLS handshake; the endpoint may use plaintext"
		action = "verify repo-server TLS settings and the selected address; for a trusted loopback plaintext endpoint explicitly set repoServer.plaintext or init --repo-server-plaintext"
	case strings.Contains(text, "x509:"), strings.Contains(text, "certificate verify failed"):
		reason = "repo-server TLS certificate verification failed"
		action = "verify the certificate hostname and validity; configure repoServer.caFile or init --repo-server-ca for the trusted CA"
	case strings.Contains(text, "tls handshake"), strings.Contains(text, "tls: "):
		reason = "repo-server TLS negotiation failed"
		action = "verify client/server TLS configuration and whether the selected endpoint requires a client certificate; do not disable TLS for remote endpoints"
	case strings.Contains(text, "connection refused"), strings.Contains(text, "no such host"), strings.Contains(text, "network is unreachable"):
		reason = "repo-server endpoint could not be reached"
		action = "check repoServer.address, DNS and network access; for Kubernetes forwarding check repoServer.portForward.resource and port"
	case strings.Contains(text, "code: unauthenticated"), strings.Contains(text, "code = unauthenticated"), strings.Contains(text, "code: permissiondenied"), strings.Contains(text, "code = permissiondenied"):
		reason = "repo-server RPC authentication or access was denied"
		action = "check the configured instance's RPC authentication and read-only repository access; do not add sync or update permissions"
	case strings.Contains(text, "code: unimplemented"), strings.Contains(text, "code = unimplemented"), strings.Contains(text, "unknown service"):
		reason = "repo-server RPC is unavailable at the selected endpoint"
		action = "check that the address points to repo-server and that its version supports the configured RPC; consult the tested backend version matrix"
	default:
		if plaintext {
			action = "verify that the selected loopback endpoint actually permits plaintext; otherwise use its TLS configuration, then check repository access and trusted backend logs"
		}
	}
	return domain.Fail(domain.CodeRepoServerFailed, "render", reason, action, err)
}
