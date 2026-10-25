package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"diffaro/internal/localhelm"
	"diffaro/internal/reposerver"
)

// The test binary also stands in for the subprocesses Diffaro starts: the
// bundled Helm worker (the production `__helm-render` argument) and grpcurl
// for the repo-server backend. Fake grpcurl output is a deterministic fixture
// that checks the subprocess/stdin protocol, not an actual Helm result.
//
// Diffaro starts grpcurl with an emptied environment so that it cannot
// inherit API tokens, and the tests assert that. A fake grpcurl therefore
// cannot be configured through the environment: fakeGRPCURL links the test
// binary under a name that states its role, and TestMain selects the role
// from that name. The repo-server address is never dialled.

// Fake grpcurl roles.
const (
	// rpcChart and rpcRecursive serve mockConfig (mock_integration_test.go).
	rpcChart     = "chart"
	rpcRecursive = "recursive"
	// rpcComplex0 and rpcComplex1 serve complexConfig's two instances
	// (complex_backend_test.go); rpcComplex0EastHeadFails also fails the head
	// render of Application east.
	rpcComplex0              = "complex-0"
	rpcComplex1              = "complex-1"
	rpcComplex0EastHeadFails = "complex-0-east-head-fails"
)

var fakeRPCRoles = map[string]func() error{
	rpcChart:                 func() error { return mockRPC(false) },
	rpcRecursive:             func() error { return mockRPC(true) },
	rpcComplex0:              func() error { return complexRPC(0, false) },
	rpcComplex1:              func() error { return complexRPC(1, false) },
	rpcComplex0EastHeadFails: func() error { return complexRPC(0, true) },
}

const fakeGRPCURLPrefix = "fake-grpcurl-"

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "__helm-render" {
		if os.Getenv("DIFFARO_GITHUB_TOKEN") != "" || os.Getenv("DIFFARO_ARGOCD_TOKEN") != "" || os.Getenv("KUBECONFIG") != "" {
			os.Exit(6)
		}
		os.Exit(localhelm.Worker(os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == reposerver.Method {
		os.Exit(runFakeGRPCURL())
	}
	dir, err := os.MkdirTemp("", "diffaro-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeToolDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func runFakeGRPCURL() int {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	role, ok := fakeRPCRoles[strings.TrimPrefix(name, fakeGRPCURLPrefix)]
	if !ok || !strings.HasPrefix(name, fakeGRPCURLPrefix) {
		fmt.Fprintf(os.Stderr, "fake grpcurl started without a known role: %s\n", name)
		return 1
	}
	if err := role(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// fakeToolDir holds the role-named links of the test binary for this run.
var fakeToolDir string
var fakeToolMu sync.Mutex

// fakeGRPCURL returns an executable path that runs the test binary as the
// fake grpcurl with the given role.
func fakeGRPCURL(t *testing.T, role string) string {
	t.Helper()
	if fakeRPCRoles[role] == nil {
		t.Fatalf("unknown fake grpcurl role %q", role)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fakeToolDir, fakeGRPCURLPrefix+role+filepath.Ext(executable))
	fakeToolMu.Lock()
	defer fakeToolMu.Unlock()
	if _, err = os.Stat(path); err == nil {
		return path
	}
	// A hard link is cheap; copy when the temporary directory is on another
	// file system.
	if os.Link(executable, path) == nil {
		return path
	}
	if err = copyExecutable(executable, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func copyExecutable(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
