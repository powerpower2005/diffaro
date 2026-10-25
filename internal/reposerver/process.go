package reposerver

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/procio"
	"diffaro/internal/profile"
)

const Method = "repository.RepoServerService/GenerateManifest"

//go:embed repo_server_v3_5.proto
var repoServerProto []byte

// protoSchema holds the embedded wire schema written to a temporary file once
// for a Client; remove deletes it when the Client closes.
type protoSchema struct {
	mu   sync.Mutex
	path string
}

// acquire returns the schema file and a release to call after the RPC. A nil
// schema writes a file for this RPC alone, which release removes.
func (s *protoSchema) acquire() (string, func(), error) {
	if s == nil {
		path, err := writeSchema()
		if err != nil {
			return "", func() {}, err
		}
		return path, func() { os.Remove(path) }, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		path, err := writeSchema()
		if err != nil {
			return "", func() {}, err
		}
		s.path = path
	}
	return s.path, func() {}, nil
}

func (s *protoSchema) remove() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		os.Remove(s.path)
		s.path = ""
	}
}

func writeSchema() (string, error) {
	protoFile, err := os.CreateTemp("", "diffaro-repo-server-*.proto")
	if err != nil {
		return "", domain.Fail(domain.CodeRepoServerFailed, "render", "cannot prepare repo-server wire schema", "check writable temporary storage", err)
	}
	if _, err = protoFile.Write(repoServerProto); err != nil {
		protoFile.Close()
		os.Remove(protoFile.Name())
		return "", domain.Fail(domain.CodeRepoServerFailed, "render", "cannot prepare repo-server wire schema", "check writable temporary storage", err)
	}
	if err = protoFile.Close(); err != nil {
		os.Remove(protoFile.Name())
		return "", domain.Fail(domain.CodeRepoServerFailed, "render", "cannot prepare repo-server wire schema", "check writable temporary storage", err)
	}
	return protoFile.Name(), nil
}

// processMethodInvoker calls method through grpcurl. Argo CD v3.5.1 lists its
// repo-server service through reflection but does not resolve its gogo
// protobuf message descriptors there. grpcurl compiles this pinned
// wire-compatible schema instead of depending on server reflection. Source
// and credentials go through stdin, never shell strings or argv.
func processMethodInvoker(config profile.RepoServer, limit int64, method string, schema *protoSchema) func(context.Context, map[string]any) (Response, error) {
	return func(ctx context.Context, request map[string]any) (Response, error) {
		var result Response
		data, err := json.Marshal(request)
		if err != nil || int64(len(data)) > limit {
			return result, domain.LimitError(domain.CodeLimitExceeded, "render", "repo-server RPC request", "render.maxResponseBytes", int64(len(data)), limit, "bytes", false, domain.MaxRenderResponseBytes)
		}
		protoPath, release, err := schema.acquire()
		if err != nil {
			return result, err
		}
		defer release()
		args := []string{"-max-msg-sz", strconv.FormatInt(limit, 10), "-import-path", filepath.Dir(protoPath), "-proto", filepath.Base(protoPath), "-d", "@"}
		if config.Plaintext {
			args = append(args, "-plaintext")
		} else if config.PortForward != nil {
			// kubectl authenticates the Kubernetes API and requested Pod tunnel.
			// The internal repo-server certificate need not name 127.0.0.1.
			args = append(args, "-insecure")
		} else if config.CAFile != "" {
			args = append(args, "-cacert", config.CAFile)
		}
		args = append(args, config.Address, method)
		command := exec.CommandContext(ctx, config.GRPCURL, args...)
		command.WaitDelay = time.Second
		command.Stdin = bytes.NewReader(data)
		command.Env = []string{}
		for _, key := range []string{"PATH", "SystemRoot", "TMP", "TEMP", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
			if value, ok := os.LookupEnv(key); ok {
				command.Env = append(command.Env, key+"="+value)
			}
		}
		// Do not inherit GitHub/Argo tokens or other instances' credentials.
		output := procio.Bounded{Limit: limit}
		var diagnostic processDiagnostic
		defer diagnostic.buffer.Reset()
		command.Stdout, command.Stderr = &output, &diagnostic
		err = command.Run()
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if output.Exceeded {
			return result, domain.LimitError(domain.CodeLimitExceeded, "render", "repo-server RPC response", "render.maxResponseBytes", output.Observed, limit, "bytes", true, domain.MaxRenderResponseBytes)
		}
		if err != nil {
			return result, processFailure(err, diagnostic.buffer.Bytes(), config.Plaintext)
		}
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		if decoder.Decode(&result) != nil {
			return result, domain.Fail(domain.CodeResponseInvalid, "render", "repo-server returned invalid JSON", "check grpcurl and repo-server compatibility", nil)
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return result, domain.Fail(domain.CodeResponseInvalid, "render", "repo-server returned multiple JSON values", "check grpcurl output contract", nil)
		}
		return result, nil
	}
}
