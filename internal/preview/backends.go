package preview

import (
	"sort"

	"diffaro/internal/domain"
)

// backend is one instance's renderer with its optional capabilities resolved
// once per run. A nil capability is one the renderer does not implement.
type backend struct {
	domain.Renderer
	project  domain.ProjectChecker
	desired  domain.DesiredRenderer
	multi    domain.MultiSourceRenderer
	resolver domain.SourceRevisionResolver
	info     domain.BackendInfoProvider
}

// capabilities resolves a renderer's optional capabilities; nil stays nil.
func capabilities(renderer domain.Renderer) *backend {
	if renderer == nil {
		return nil
	}
	b := &backend{Renderer: renderer}
	b.project, _ = renderer.(domain.ProjectChecker)
	b.desired, _ = renderer.(domain.DesiredRenderer)
	b.multi, _ = renderer.(domain.MultiSourceRenderer)
	b.resolver, _ = renderer.(domain.SourceRevisionResolver)
	b.info, _ = renderer.(domain.BackendInfoProvider)
	return b
}

func resolveBackends(renderers map[string]domain.Renderer) map[string]*backend {
	out := make(map[string]*backend, len(renderers))
	for instance, renderer := range renderers {
		if b := capabilities(renderer); b != nil {
			out[instance] = b
		}
	}
	return out
}

// backendInfo lists the backends that describe themselves, in instance order.
func backendInfo(backends map[string]*backend) []domain.BackendInfo {
	var info []domain.BackendInfo
	for _, b := range backends {
		if b.info != nil {
			info = append(info, b.info.BackendInfo())
		}
	}
	sort.Slice(info, func(i, j int) bool { return info[i].Instance < info[j].Instance })
	return info
}
