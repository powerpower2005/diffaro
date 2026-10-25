package preview

import (
	"context"
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

func InspectBackendVersions(ctx context.Context, report *domain.Report, readers map[string]domain.BackendVersionReader) {
	local, remote := false, false
	for i := range report.RenderBackends {
		backend := &report.RenderBackends[i]
		if backend.Kind == "local-helm" {
			local = true
		} else {
			remote = true
		}
		reader := readers[backend.Instance]
		if reader == nil {
			continue
		}
		version, err := reader.BackendVersion(ctx)
		if err != nil {
			issue := ContextualIssue(err, domain.ApplicationID{Instance: backend.Instance}, report.Revisions.Repository, "")
			issue.Stage = "capabilities"
			report.Issues = append(report.Issues, issue)
			if report.Status == domain.StatusSuccess {
				report.Status = domain.StatusPartialFailure
			}
			backend.Compatibility = "version-unavailable"
			continue
		}
		backend.Version = version
		if backend.Kind == "local-helm" {
			// The server release cannot certify a separately bundled Helm engine.
			continue
		}
		backend.Compatibility = "version-not-in-tested-matrix"
		// Exact releases only, as listed in docs/reference.md#backend-compatibility;
		// a version match never certifies instance context.
		switch strings.SplitN(version, "+", 2)[0] {
		case "v3.0.23", "v3.1.16", "v3.2.12", "v3.3.14", "v3.4.9", "v3.5.1", "v3.5.3":
			backend.Compatibility = "version-in-tested-matrix"
		}
	}
	if remote || !local {
		report.Limitations = append(report.Limitations, domain.LimitationVersionMatrix)
	}
	if local {
		report.Limitations = append(report.Limitations, domain.LimitationVersionLocalHelm)
	}
}

// RecordBackendVersions reads each instance's Argo CD version once for a
// diff report, so a report names the server version it was
// produced against. It is evidence only: a failed read is a limitation and
// never changes the status, the exit code or compatibility.
func RecordBackendVersions(ctx context.Context, report *domain.Report, readers map[string]domain.BackendVersionReader) {
	for i := range report.RenderBackends {
		backend := &report.RenderBackends[i]
		reader := readers[backend.Instance]
		if reader == nil || backend.Version != "" || ctx.Err() != nil {
			continue
		}
		version, err := reader.BackendVersion(ctx)
		if err != nil {
			report.Limitations = append(report.Limitations, fmt.Sprintf("Argo CD instance %s version could not be read (%s); the comparison is unaffected, but the report does not name the server version.", backend.Instance, issueCode(err)))
			continue
		}
		backend.Version = version
	}
}
