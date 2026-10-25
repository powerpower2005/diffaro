package localhelm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/procio"
)

// response carries the whole failure issue so Process rebuilds the same
// *domain.Failure as in-process Render, including its reason code.
type response struct {
	Manifests []string
	Reason    string
	Skipped   bool
	Partial   bool
	Issues    []domain.Issue
	Issue     *domain.Issue
}

// workerRequest is the Worker's stdin encoding of a Request.
// SourceRevision.Source is not part of its JSON form, so each plan source's
// spec travels in Sources at the same index.
type workerRequest struct {
	Request
	Sources []map[string]any
}

// workerRender is the render a Worker runs; tests replace it.
var workerRender = Render

// Worker accepts bounded data over stdin. It has no API tokens, kubeconfig,
// inherited Helm plugins, shell invocation or writable repository checkout.
func Worker(input io.Reader, output io.Writer) int {
	var wire workerRequest
	data, err := io.ReadAll(io.LimitReader(input, (128<<20)+1))
	if err != nil || len(data) > 128<<20 {
		return 6
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil {
		return 6
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return 6
	}
	req := wire.Request
	if len(wire.Sources) == len(req.Plan) {
		for i := range req.Plan {
			req.Plan[i].Source = wire.Sources[i]
		}
	}
	// A panic while rendering returns its failure, with the stack, instead of
	// a crash whose stderr Process discards.
	manifests, err := func() (manifests []string, err error) {
		defer domain.RecoverAs(&err, "render")
		return workerRender(req)
	}()
	out := response{Manifests: manifests, Partial: partial(err)}
	if out.Partial {
		out.Issues = domain.RenderIssues(err)
	}
	if err != nil {
		var skipped *domain.RenderSkipped
		var failure *domain.Failure
		if errors.As(err, &skipped) {
			out.Skipped = true
			out.Reason = skipped.Reason
		}
		if errors.As(err, &failure) {
			issue := failure.Issue
			out.Issue = &issue
			if out.Reason == "" {
				out.Reason = failure.Issue.Reason
			}
		}
		if out.Reason == "" {
			out.Reason = "local Helm rendering failed"
		}
	}
	if json.NewEncoder(output).Encode(out) != nil {
		return 6
	}
	return 0
}

// stoppedCode is the issue code of a render its context stopped. It is never
// a settled render failure, so a stopped render is not cached.
func stoppedCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return domain.CodeCancelled
	}
	return domain.CodeTimeout
}

func Process(ctx context.Context, req Request) ([]string, error) {
	wire := workerRequest{Request: req, Sources: make([]map[string]any, len(req.Plan))}
	for i, s := range req.Plan {
		wire.Sources[i] = s.Source
	}
	data, err := json.Marshal(wire)
	if err != nil || len(data) > 128<<20 {
		return nil, renderError()
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, renderError()
	}
	cmd := exec.CommandContext(ctx, binary, "__helm-render")
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = []string{"GOMEMLIMIT=256MiB"}
	for _, key := range []string{"SystemRoot", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	// The worker refuses manifests over req.Limit in total. Each manifest is
	// compact JSON, so encoding it again as a JSON string at most doubles it
	// (only '"' and '\' are escaped); 1 MiB covers the rest of the response.
	output := &procio.Bounded{Limit: 2*req.Limit + (1 << 20)}
	cmd.Stdout = output
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// Name the stopped step; the issue reports it as a timeout or
			// cancellation.
			return nil, domain.Fail(stoppedCode(ctx.Err()), "render", "bundled Helm render did not finish", "rerun the preview, or raise render.timeout if this render needs longer", ctx.Err())
		}
		if output.Exceeded {
			return nil, domain.Fail(domain.CodeLimitExceeded, "render", "local worker output exceeds its byte limit", "reduce the render scope", nil)
		}
		return nil, renderError()
	}
	var out response
	if json.Unmarshal(output.Bytes(), &out) != nil {
		return nil, renderError()
	}
	if out.Skipped {
		err = skip(out.Reason)
	} else if out.Reason != "" {
		issue := domain.Issue{Stage: "render"}
		if out.Issue != nil {
			issue = *out.Issue
		}
		issue.Reason = out.Reason
		err = &domain.Failure{Issue: issue}
		if out.Partial {
			err = domain.PartialRender(out.Issues)
		}
	}
	if err != nil {
		if !out.Partial && out.Issue != nil && out.Issue.SourcePosition > 0 && out.Issue.SourcePosition <= len(req.Plan) {
			err = domain.SourceFailure(err, req.Plan[out.Issue.SourcePosition-1])
		}
		if out.Partial {
			return out.Manifests, err
		}
		return nil, err
	}
	return out.Manifests, nil
}
