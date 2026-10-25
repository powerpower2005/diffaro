package argocd

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func TestSpecMismatchDiagnosticsExposePathsWithoutValues(t *testing.T) {
	for _, field := range []string{"valueFiles", "values", "valuesObject", "parameters", "fileParameters", "releaseName", "namespace", "destination", "project", "other"} {
		t.Run(field, func(t *testing.T) {
			stored := map[string]any{"project": "demo", "source": map[string]any{"helm": map[string]any{}}, "destination": map[string]any{"name": "east"}}
			desired := domain.CloneSpec(stored)
			expected := "/source/helm/" + field
			switch field {
			case "project", "destination":
				desired[field] = "fictional-private-input"
				expected = "/" + field
			case "other":
				desired["fictional-private-key"] = "fictional-private-input"
				expected = "/spec (other fields)"
			default:
				desired["source"].(map[string]any)["helm"].(map[string]any)[field] = map[string]any{"fictional-private-key": "fictional-private-input"}
			}
			reason := specMismatchReason(desired, stored)
			if !strings.Contains(reason, expected) || !strings.Contains(reason, "outside targetRevision") || strings.Contains(reason, "fictional-private") {
				t.Fatalf("diagnostic leaked an input or hid the changed field: %s", reason)
			}
			if strings.Contains(specMismatchReason(desired, stored), "fictional-private") {
				t.Fatal("repeated diagnostic leaked input")
			}
			if _, ok := stored["fictional-private-key"]; ok || len(stored["source"].(map[string]any)["helm"].(map[string]any)) != 0 {
				t.Fatal("diagnosis changed stored context")
			}
		})
	}
}

func TestMultiSourceMismatchDiagnosticPreservesPositionAndIgnoresRevision(t *testing.T) {
	stored := map[string]any{"sources": []any{
		map[string]any{"repoURL": "https://example.com/chart.git", "targetRevision": "main", "helm": map[string]any{"valuesObject": map[string]any{"password": "fictional-old"}}},
		map[string]any{"repoURL": "https://example.com/values.git", "targetRevision": "main", "ref": "values"},
	}}
	desired := domain.CloneSpec(stored)
	sources := desired["sources"].([]any)
	sources[0].(map[string]any)["targetRevision"] = "feature"
	sources[0].(map[string]any)["helm"].(map[string]any)["valuesObject"] = map[string]any{"password": "fictional-new"}
	sources[1].(map[string]any)["ref"] = "new-ref"
	reason := specMismatchReason(withoutSourceRevisions(desired), withoutSourceRevisions(stored))
	if !strings.Contains(reason, "/sources/0/helm/valuesObject") || !strings.Contains(reason, "/sources/1/ref") || strings.Contains(reason, "/targetRevision") || strings.Contains(reason, "fictional-") {
		t.Fatal("source positions or safe revision exclusion were lost:", reason)
	}
}
