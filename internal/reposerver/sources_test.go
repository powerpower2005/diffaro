package reposerver

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/revision"
)

const valuesRepository = "https://github.com/example/values.git"

func multiFixture(t *testing.T) (Snapshot, domain.Application, map[string]any, []domain.SourceRevision) {
	t.Helper()
	snapshot, app, project := fixture()
	chart := app.Source()
	chart["targetRevision"] = commit
	chart["helm"].(map[string]any)["valueFiles"] = []any{"$values/prod.yaml"}
	app.Spec["sources"] = []any{chart, map[string]any{"repoURL": valuesRepository, "ref": "values", "targetRevision": "main"}}
	delete(app.Spec, "source")
	valuesBinding := snapshot.Bindings[0]
	valuesBinding.RepositoryURL = valuesRepository
	valuesBinding.Request = domain.CloneSpec(valuesBinding.Request)
	valuesBinding.Request["repo"] = map[string]any{"repo": valuesRepository, "password": "fictional-values-password"}
	snapshot.Bindings = append(snapshot.Bindings, valuesBinding)
	project["sourceRepos"] = []any{repository, valuesRepository}
	plan, err := revision.PlanSources(context.Background(), app, valuesRepository, strings.Repeat("b", 40), nil)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, app, project, plan
}

func TestDesiredMultiSourcePreservesReferencesAndContext(t *testing.T) {
	snapshot, app, project, plan := multiFixture(t)
	original := domain.CloneSpec(app.Spec)
	client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot}
	var order []int
	client.invoke = func(_ context.Context, request map[string]any) (Response, error) {
		invokeMu.Lock()
		defer invokeMu.Unlock()
		i := planIndex(t, plan, request)
		order = append(order, i)
		if request["hasMultipleSources"] != true {
			t.Fatal("multi-source context lost")
		}
		if request["appName"] != app.ID.Name || request["namespace"] != "demo" || request["kubeVersion"] != "1.32.0" {
			t.Fatal("render context changed")
		}
		refs := request["refSources"].(map[string]any)
		ref := refs["$values"].(map[string]any)
		if ref["targetRevision"] != plan[1].Revision || ref["repo"].(map[string]any)["password"] != "fictional-values-password" {
			t.Fatal("external values context lost")
		}
		source := request["applicationSource"].(map[string]any)
		if i == 0 {
			helm := source["helm"].(map[string]any)
			if !reflect.DeepEqual(helm["valueFiles"], []any{"$values/prod.yaml"}) || helm["releaseName"] != "changed-release" {
				t.Fatal("Helm inputs lost")
			}
			raw, err := base64.StdEncoding.DecodeString(helm["valuesObject"].(map[string]any)["raw"].(string))
			if err != nil || !strings.Contains(string(raw), "9007199254740993") {
				t.Fatal("valuesObject wire encoding lost precision")
			}
			if request["repo"].(map[string]any)["password"] != "fictional-repo-password" {
				t.Fatal("values credentials leaked into chart repository")
			}
			return Response{Revision: plan[i].Revision, SourceType: "Helm", Manifests: []string{"chart-manifest"}}, nil
		}
		if source["helm"] != nil || source["ref"] != "values" {
			t.Fatal("values-only source became a renderer")
		}
		return Response{Revision: plan[i].Revision}, nil
	}
	got, err := client.RenderDesiredSources(context.Background(), app, plan)
	// Sources render in plan order; the chart renders twice and
	// the values-only source once.
	if err != nil || !reflect.DeepEqual(order, []int{0, 0, 1}) || !reflect.DeepEqual(got, []string{"chart-manifest"}) {
		t.Fatalf("render result: %v, order %v, %v", got, order, err)
	}
	if !reflect.DeepEqual(app.Spec, original) {
		t.Fatal("desired spec was mutated")
	}
}

func TestDesiredMultiSourceFailsBeforeRenderingUnauthorizedOrInvalidInputs(t *testing.T) {
	for _, scenario := range []string{"unbound-values", "denied-values", "missing-ref", "changed-source", "duplicate-ref", "wrong-instance"} {
		t.Run(scenario, func(t *testing.T) {
			snapshot, app, project, plan := multiFixture(t)
			switch scenario {
			case "unbound-values":
				snapshot.Bindings = snapshot.Bindings[:1]
			case "denied-values":
				project["sourceRepos"] = []any{repository}
			case "missing-ref":
				app.Spec["sources"].([]any)[0].(map[string]any)["helm"].(map[string]any)["valueFiles"] = []any{"$missing/prod.yaml"}
				plan[0].Source["helm"].(map[string]any)["valueFiles"] = []any{"$missing/prod.yaml"}
			case "changed-source":
				plan[0].Source["path"] = "unrequested-path"
			case "duplicate-ref":
				app.Spec["sources"].([]any)[0].(map[string]any)["ref"] = "values"
				plan[0].Ref, plan[0].Source["ref"] = "values", "values"
			case "wrong-instance":
				app.ID.Instance = "other"
			}
			client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot}
			client.invoke = func(context.Context, map[string]any) (Response, error) {
				t.Fatal("invalid source reached rendering")
				return Response{}, nil
			}
			got, err := client.RenderDesiredSources(context.Background(), app, plan)
			if err == nil || got != nil {
				t.Fatal("invalid input reported success")
			}
			if scenario == "denied-values" || scenario == "unbound-values" {
				failure := err.(*domain.Failure)
				if failure.Issue.Repository != valuesRepository || failure.Issue.Revision != plan[1].Revision || failure.Issue.SourcePosition != 2 {
					t.Fatal("failure lost external source context")
				}
			}
		})
	}
}

func TestDesiredMultiSourceRenderingOrderAndIncompleteResult(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		snapshot, app, project, plan := multiFixture(t)
		second := app.Spec["sources"].([]any)[1].(map[string]any)
		second["path"], second["helm"] = "overlay", map[string]any{}
		plan[1].Source["path"], plan[1].Source["helm"] = "overlay", map[string]any{}
		client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot}
		client.invoke = func(_ context.Context, request map[string]any) (Response, error) {
			i := planIndex(t, plan, request)
			if i == 1 && failSecond {
				return Response{}, context.DeadlineExceeded
			}
			return Response{Revision: plan[i].Revision, SourceType: "Helm", Manifests: []string{plan[i].Repository}}, nil
		}
		got, err := client.RenderDesiredSources(context.Background(), app, plan)
		if failSecond {
			if err == nil || got != nil {
				t.Fatal("incomplete source set returned success")
			}
		} else if err != nil || !reflect.DeepEqual(got, []string{repository, valuesRepository}) {
			t.Fatal("source output order lost")
		}
	}
}

// Every source is authorized against one AppProject read, and a project
// change while any source renders is drift: the AppProject is read again
// after the last source.
func TestDesiredMultiSourceProjectReadOnceAndDriftCoversEverySource(t *testing.T) {
	for _, changeAt := range []int{-1, 0, 1} {
		snapshot, app, project, plan := multiFixture(t)
		changed, reads := false, 0
		client := &Client{API: fakeAPI(t, func(int) map[string]any {
			reads++
			if changed {
				copy := domain.CloneSpec(project)
				copy["description"] = "changed"
				return copy
			}
			return project
		}), snapshot: snapshot}
		calls := 0
		client.invoke = func(_ context.Context, request map[string]any) (Response, error) {
			invokeMu.Lock()
			defer invokeMu.Unlock()
			i := planIndex(t, plan, request)
			calls++
			changed = changed || i == changeAt
			if plan[i].Ref != "" && domain.String(plan[i].Source, "path") == "" {
				return Response{Revision: plan[i].Revision}, nil
			}
			return Response{Revision: plan[i].Revision, SourceType: "Helm", Manifests: []string{"manifest"}}, nil
		}
		got, err := client.RenderDesiredSources(context.Background(), app, plan)
		// The chart source renders twice, the values-only source once.
		if reads != 2 || calls != 3 {
			t.Fatalf("change at %d: project reads=%d renders=%d", changeAt, reads, calls)
		}
		if changeAt < 0 {
			if err != nil || len(got) != 1 {
				t.Fatalf("stable project failed: %v", err)
			}
		} else if issueCode(err) != domain.CodeRenderContextChanged || got != nil {
			t.Fatalf("project change while source %d rendered was not drift: %v", changeAt+1, err)
		}
	}
}

// planIndex is the position in plan of the source a request renders; each
// source of the fixtures has its own revision.
func planIndex(t *testing.T, plan []domain.SourceRevision, request map[string]any) int {
	t.Helper()
	for i, source := range plan {
		if request["revision"] == source.Revision {
			return i
		}
	}
	t.Fatalf("request for an unplanned revision %v", request["revision"])
	return -1
}
