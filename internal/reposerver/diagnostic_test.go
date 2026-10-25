package reposerver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestProcessDiagnosticsAreActionableAndNeverExposed(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := processMethodInvoker(profile.RepoServer{GRPCURL: executable, Address: "127.0.0.1:8081"}, 1024, Method, nil)
	for _, tc := range []struct{ mode, reason, action string }{
		{"tls", "TLS handshake", "--repo-server-plaintext"},
		{"certificate", "certificate verification", "--repo-server-ca"},
		{"unreachable", "could not be reached", "repoServer.address"},
		{"rpc-denied", "access was denied", "read-only"},
		{"render-error", "Helm template rendering failed in repo-server", "values files"},
		{"diagnostic-overflow", "RPC could not complete", "trusted instance"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			_, err := invoke(context.Background(), map[string]any{"testMode": tc.mode})
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != "repo_server_failed" || !strings.Contains(failure.Issue.Reason, tc.reason) || !strings.Contains(failure.Issue.Action, tc.action) {
				t.Fatalf("safe, specific diagnosis missing: %v", err)
			}
			if strings.Contains(err.Error(), "fictional") || strings.Contains(failure.Issue.Action, "fictional") || errors.Unwrap(err) == nil {
				t.Fatal("raw diagnostic leaked or original process cause was lost")
			}
		})
	}
}

func TestMissingGrpcurlFailsWithExecutableGuidance(t *testing.T) {
	invoke := processMethodInvoker(profile.RepoServer{GRPCURL: t.TempDir() + "/absent-grpcurl", Address: "127.0.0.1:8081"}, 1024, Method, nil)
	_, err := invoke(context.Background(), map[string]any{})
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "repo_server_failed" || !strings.Contains(failure.Issue.Reason, "executable could not be started") || !strings.Contains(failure.Issue.Action, "--grpcurl") {
		t.Fatal("missing grpcurl was misreported as a generic backend failure")
	}
}

func TestDiagnosticCaptureDrainsWithoutRetainingUnboundedText(t *testing.T) {
	d := processDiagnostic{}
	payload := []byte(strings.Repeat("x", diagnosticLimit*4))
	if n, err := d.Write(payload); n != len(payload) || err != nil || d.buffer.Len() != diagnosticLimit {
		t.Fatal("diagnostics must be fully drained with bounded memory")
	}
	if n, err := d.Write([]byte("fictional-tail-secret")); n != 21 || err != nil || strings.Contains(d.buffer.String(), "fictional") {
		t.Fatal("diagnostic overflow retained additional sensitive input")
	}
}

// os/exec drains a pipe through ReadFrom when the writer has one; it must keep
// the same bounded prefix as Write and drain the rest.
func TestDiagnosticReadFromKeepsTheBoundedPrefix(t *testing.T) {
	d := processDiagnostic{}
	payload := strings.Repeat("x", diagnosticLimit*4) + "fictional-tail-secret"
	n, err := d.ReadFrom(strings.NewReader(payload))
	if n != int64(len(payload)) || err != nil || d.buffer.Len() != diagnosticLimit || strings.Contains(d.buffer.String(), "fictional") {
		t.Fatalf("read %d of %d, err %v, kept %d bytes", n, len(payload), err, d.buffer.Len())
	}
}

// A repo-server render failure gets the reason code local Helm would give the
// same cause, read from the shapes of Argo CD v3.1 GenerateManifest errors as
// grpcurl prints them. The message itself, which can quote values, is never
// reported.
func TestRepoServerRenderFailuresNameTheirCause(t *testing.T) {
	const grpc = "ERROR:\n  Code: Unknown\n  Message: "
	const command = "failed to execute helm template command: failed to get command args to log: `helm template . --name-template api --set password=fictional-secret-value` failed exit status 1: "
	for _, tc := range []struct{ name, stderr, code, reason string }{
		{"missing path", grpc + "charts/fictional-secret-value: app path does not exist", domain.ReasonChartPathMissing, "chart path does not exist"},
		{"missing Chart.yaml", grpc + "Manifest generation error (cached): error reading helm chart from /tmp/_argocd-repo/fictional-secret-value/Chart.yaml: open: no such file", domain.ReasonChartMetadataMissing, "no readable Chart.yaml"},
		{"missing revision", grpc + "unable to resolve git revision fictional-secret-value: unable to resolve 'fictional-secret-value' to a commit SHA", domain.ReasonRevisionMissing, "could not resolve the source revision"},
		{"missing value file", grpc + command + "Error: open /tmp/_argocd-repo/values-fictional-secret-value.yaml: no such file or directory", domain.ReasonValueFileMissing, "value file or file parameter does not exist"},
		{"template location", grpc + command + `Error: template: api/templates/deploy.yaml:17:3: executing "api/templates/deploy.yaml" at <include "x" .>: error calling include: template: api/templates/_helpers.tpl:9:8: executing "x" at <deepCopy .Values.spec>: error calling deepCopy: reflect: call of reflect.Value.Type on zero Value fictional-secret-value` + "\n\nUse --debug flag to render out invalid YAML", domain.ReasonTemplateFailed, "template api/templates/deploy.yaml:17 failed: a function received a null value (at api/templates/_helpers.tpl:9 calling deepCopy)"},
		{"invalid YAML", grpc + command + "Error: YAML parse error on api/fictional-secret-value: error converting YAML to JSON", domain.ReasonRenderedOutputInvalid, "not valid YAML"},
		{"unrecognized render failure", grpc + command + "Error: fictional-secret-value", domain.ReasonTemplateFailed, "Helm template rendering failed in repo-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := processFailure(errors.New("exit status 1"), []byte(tc.stderr), false)
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeRepoServerFailed || failure.Issue.ReasonCode != tc.code || failure.Issue.Stage != "render" || !strings.Contains(failure.Issue.Reason, tc.reason) {
				t.Fatalf("issue = %+v, want reason code %s and %q", failure, tc.code, tc.reason)
			}
			if strings.Contains(failure.Issue.Reason+failure.Issue.Action+err.Error(), "fictional-secret-value") {
				t.Fatalf("the repo-server message leaked: %+v", failure.Issue)
			}
		})
	}
	// Transport failures keep their own diagnosis.
	if err := processFailure(errors.New("exit status 1"), []byte("Failed to dial target host: connection refused"), false); !strings.Contains(err.Error(), "could not be reached") {
		t.Fatalf("transport failure misread as a render failure: %v", err)
	}
}
