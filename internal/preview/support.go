package preview

import "diffaro/internal/domain"

// Supported reports whether an Application uses a supported explicit Git Helm
// source (single or multiple). Unsupported sources fail before rendering.
func Supported(a domain.Application) error {
	return domain.SupportedSource(a)
}

func multipleSources(app domain.Application) bool {
	return len(app.Sources()) > 0
}

func multiSourceUnavailable() error {
	return domain.Fail(domain.CodeUnsupportedSource, "render", "renderer does not implement multi-source target rendering", "configure an Argo CD multi-source render backend", nil)
}
