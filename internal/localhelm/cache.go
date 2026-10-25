package localhelm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"

	"diffaro/internal/domain"
)

// repositoriesDigest identifies the source files of a render request for the
// phase trace without encoding them: repositories in order, then
// their paths in order, each with its length and content.
func repositoriesDigest(repositories map[string]map[string][]byte) string {
	keys := make([]string, 0, len(repositories))
	for key := range repositories {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		files := repositories[key]
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(h, "repository\x00%s\x00%d\x00", key, len(names))
		for _, name := range names {
			fmt.Fprintf(h, "%s\x00%d\x00", name, len(files[name]))
			h.Write(files[name])
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// bundledProcess renders with the bundled engine in a worker subprocess. It
// is a variable so tests can count worker starts.
var bundledProcess = Process

// maxRenderMemoBytes bounds the target manifests one Client keeps from
// bundled renders for later renders of the same inputs.
const maxRenderMemoBytes = 64 << 20

// memoRender is one kept bundled render. Callers of the same inputs reuse it
// instead of starting another worker.
type memoRender struct {
	manifests []string
	issues    []domain.Issue
}

// memoized renders a trimmed request with the bundled engine once per
// identical inputs (renderKey), such as the base and head of an Application
// whose files did not change. The bundled engine fails a render that calls a
// randomness, time or live-dependent function, so identical inputs render
// identically. Complete and partial results are kept within
// maxRenderMemoBytes; failures, skips and cancellations are not, and a waiter
// then renders itself.
func (c *Client) memoized(ctx context.Context, req Request) ([]string, error) {
	key, ok := renderKey(req)
	if !ok {
		return bundledProcess(ctx, req)
	}
	entry, owner := c.renders.claim(key)
	if !owner {
		kept, ok, err := entry.wait(ctx)
		if err != nil {
			return nil, err
		}
		if ok {
			return kept.result(req.Plan)
		}
		return bundledProcess(ctx, req)
	}
	manifests, err := bundledProcess(ctx, req)
	var size int64
	for _, doc := range manifests {
		size += int64(len(doc))
	}
	c.renders.settle(key, entry, err == nil || partial(err), size, maxRenderMemoBytes, func() memoRender {
		return memoRender{manifests: slices.Clone(manifests), issues: domain.RenderIssues(err)}
	})
	return manifests, err
}

// result returns the kept render for plan. Its issues name the revisions of
// plan, which can differ from the kept render's only where the render did not
// read them (renderKey).
func (m memoRender) result(plan []domain.SourceRevision) ([]string, error) {
	issues := slices.Clone(m.issues)
	for i := range issues {
		for _, s := range plan {
			if s.Position == issues[i].SourcePosition {
				issues[i].Revision = s.Revision
			}
		}
	}
	return slices.Clone(m.manifests), domain.PartialRender(issues)
}

// renderKey digests everything a bundled render reads from req: the limit,
// Application, render settings and capabilities, each plan source without its
// targetRevision, which plan sources share a repository revision, and their
// files. Revisions are part of it only when a source spec contains "$", which
// can expand a build environment variable such as ARGOCD_APP_REVISION or
// name a ref source; otherwise a render reads a revision only to name the
// source of an issue. It reports false when an input cannot be encoded.
func renderKey(req Request) ([32]byte, bool) {
	h := sha256.New()
	encode := func(v any) bool {
		data, err := json.Marshal(v)
		if err == nil {
			fmt.Fprintf(h, "%d\x00", len(data))
			h.Write(data)
		}
		return err == nil
	}
	fmt.Fprintf(h, "%d\x00%d\x00%d\x00", req.Limit, len(req.Plan), len(req.Repositories))
	if !encode(req.Application) || !encode(req.Settings) || !encode(req.Capabilities) {
		return [32]byte{}, false
	}
	revisions := false
	first := map[string]int{}
	for i, s := range req.Plan {
		source := maps.Clone(s.Source)
		delete(source, "targetRevision")
		spec, err := json.Marshal(source)
		if err != nil {
			return [32]byte{}, false
		}
		revisions = revisions || bytes.Contains(spec, []byte("$"))
		fmt.Fprintf(h, "source\x00%d\x00%s\x00%s\x00%s\x00%d\x00", s.Position, s.Repository, s.Ref, s.Rule, len(spec))
		h.Write(spec)
		repoKey := s.Repository + "@" + s.Revision
		if j, ok := first[repoKey]; ok {
			fmt.Fprintf(h, "same\x00%d\x00", j)
			continue
		}
		first[repoKey] = i
		files := req.Repositories[repoKey]
		fmt.Fprintf(h, "files\x00%t\x00%d\x00", files != nil, len(files))
		for _, name := range slices.Sorted(maps.Keys(files)) {
			fmt.Fprintf(h, "%s\x00%d\x00", name, len(files[name]))
			h.Write(files[name])
		}
	}
	if revisions {
		for _, s := range req.Plan {
			fmt.Fprintf(h, "revision\x00%s\x00", s.Revision)
		}
	}
	var key [32]byte
	h.Sum(key[:0])
	return key, true
}
