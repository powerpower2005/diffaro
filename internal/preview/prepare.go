package preview

import (
	"context"

	"diffaro/internal/domain"
)

// lookupApplication reads an Application's stored spec through its instance
// backend.
func lookupApplication(ctx context.Context, b *backend, id domain.ApplicationID) (domain.Application, error) {
	end := domain.TraceRead(ctx, "lookup", "application", id.String())
	app, err := b.Application(ctx, id)
	end(func() string { return domain.DigestJSON(app.Spec) }, err)
	return app, err
}

// preparedTarget is an Application that passed the revision-independent
// checks and can be planned and rendered on each revision.
type preparedTarget struct {
	app domain.Application
	// id routes the target to its backend and qualifies its cluster: the
	// configured identity of an explicit Application, the node identity in
	// a graph.
	id domain.ApplicationID
	// root targets render the stored spec (explicit Applications and graph
	// roots); children render the desired spec from their parent's target.
	root        bool
	multiSource bool
	backend     *backend
	// cluster is the destination server, or its name when server is empty.
	cluster, namespace string
}

// prepareTarget runs the revision-independent checks in their shared order:
// a supported Git Helm source, a destination, then the instance backend.
func (r *run) prepareTarget(id domain.ApplicationID, app domain.Application, root bool) (preparedTarget, error) {
	if err := Supported(app); err != nil {
		return preparedTarget{}, err
	}
	cluster, namespace := destination(app)
	if cluster == "" {
		return preparedTarget{}, domain.Fail(domain.CodeDestinationInvalid, "selection", "Application destination is missing", "configure a destination cluster", nil)
	}
	b := r.backends[id.Instance]
	if b == nil {
		return preparedTarget{}, domain.Fail(domain.CodeRoutingInvalid, "routing", "child instance has no renderer", "configure the routed instance", nil)
	}
	return preparedTarget{app: app, id: id, root: root, multiSource: multipleSources(app), backend: b, cluster: cluster, namespace: namespace}, nil
}

// projectChecked reports whether the AppProject is checked before rendering.
// Stored specs and multi-source targets are checked here; a desired
// single-source child is checked by the backend's RenderDesired.
func (t preparedTarget) projectChecked() bool {
	return t.root || t.multiSource
}

// destination returns the destination cluster (server, or name when server is
// empty) and namespace used to scope resources.
func destination(app domain.Application) (string, string) {
	d := app.Destination()
	cluster := domain.String(d, "server")
	if cluster == "" {
		cluster = domain.String(d, "name")
	}
	return cluster, domain.String(d, "namespace")
}

func inputSummary(s map[string]any) domain.InputSummary {
	i := domain.InputSummary{Path: domain.String(s, "path"), ValueFiles: []string{}, Parameters: []string{}, FileParameters: []string{}, Origin: "stored-application-spec"}
	h, _ := s["helm"].(map[string]any)
	i.ReleaseName = domain.String(h, "releaseName")
	_, i.InlineValues = h["values"]
	_, i.ValuesObject = h["valuesObject"]
	if files, ok := h["valueFiles"].([]any); ok {
		for _, f := range files {
			if v, ok := f.(string); ok {
				i.ValueFiles = append(i.ValueFiles, v)
			}
		}
	}
	for _, key := range []string{"parameters", "fileParameters"} {
		if values, ok := h[key].([]any); ok {
			for _, v := range values {
				if p, ok := v.(map[string]any); ok {
					if key == "parameters" {
						i.Parameters = append(i.Parameters, domain.String(p, "name"))
					} else {
						i.FileParameters = append(i.FileParameters, domain.String(p, "name"))
					}
				}
			}
		}
	}
	return i
}
