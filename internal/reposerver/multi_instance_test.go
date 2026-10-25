package reposerver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

func TestMultipleInstancesAndClusterRenderContexts(t *testing.T) {
	clients := map[string]*Client{}
	expectedClusters := map[string]string{}
	for index, instance := range []string{"dev", "prod"} {
		snapshot, _, project := fixture()
		originalBinding := snapshot.Bindings[0]
		snapshot.Instance, snapshot.ContextID, snapshot.Bindings = instance, instance+"-snapshot", nil
		project["destinations"] = []any{
			map[string]any{"name": "east", "namespace": "demo"},
			map[string]any{"name": "west", "namespace": "demo"},
		}
		for clusterIndex, cluster := range []string{"east", "west"} {
			binding := originalBinding
			binding.Destination = map[string]any{"name": cluster, "namespace": "demo"}
			binding.Request = domain.CloneSpec(originalBinding.Request)
			binding.Request["repo"].(map[string]any)["password"] = "fictional-" + instance + "-" + cluster + "-credential"
			binding.Request["kubeVersion"] = fmt.Sprintf("1.%d.0", 30+index*2+clusterIndex)
			binding.Request["apiVersions"] = []any{"v1", instance + ".example.com/v1/" + cluster}
			snapshot.Bindings = append(snapshot.Bindings, binding)
		}
		if err := snapshot.Validate(instance); err != nil {
			t.Fatal(err)
		}
		token := "fictional-" + instance + "-api-token"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/api/v1/projects/demo" || r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("detailed render crossed instance credentials or performed a mutation")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": project})
		}))
		t.Cleanup(server.Close)
		client := &Client{API: &argocd.Client{Instance: instance, HTTP: httpx.New(server.URL, token, time.Second, 1<<20)}, snapshot: snapshot}
		client.invoke = func(_ context.Context, request map[string]any) (Response, error) {
			if request["appName"] != "new-child" || request["namespace"] != "demo" || request["noCache"] != true || request["noRevisionCache"] != true {
				t.Fatal("same-name child lost scoped render identity or cache isolation")
			}
			matched := false
			for _, binding := range snapshot.Bindings {
				if binding.Destination["name"] == expectedClusters[instance] {
					for key, value := range binding.Request {
						if !reflect.DeepEqual(request[key], value) {
							t.Fatalf("instance/cluster context crossed boundary: %s", key)
						}
					}
					matched = true
				}
			}
			if !matched {
				t.Fatal("request did not use an instance-local cluster binding")
			}
			// A backend must not mutate credentials/capabilities retained for the next render.
			request["repo"].(map[string]any)["password"] = "fictional-mutated"
			request["apiVersions"].([]any)[0] = "mutated"
			return Response{Revision: request["revision"].(string), SourceType: "Helm", Manifests: []string{instance}}, nil
		}
		clients[instance] = client
	}
	for _, instance := range []string{"dev", "prod"} {
		for _, cluster := range []string{"east", "west"} {
			for _, revision := range []string{commit, strings.Repeat("b", 40)} {
				t.Run(instance+"/"+cluster+"/"+revision[:1], func(t *testing.T) {
					_, app, _ := fixture()
					app.ID.Instance = instance
					app.Spec["destination"] = map[string]any{"name": cluster, "namespace": "demo"}
					delete(app.Source()["helm"].(map[string]any), "kubeVersion")
					delete(app.Source()["helm"].(map[string]any), "apiVersions")
					client := clients[instance]
					expectedClusters[instance] = cluster
					before, _ := json.Marshal(client.snapshot)
					output, err := client.RenderDesired(context.Background(), app, revision)
					after, _ := json.Marshal(client.snapshot)
					if err != nil || len(output) != 1 || output[0] != instance || string(before) != string(after) {
						t.Fatalf("context render failed or mutated frozen inputs: %v", err)
					}
					other := "dev"
					if instance == "dev" {
						other = "prod"
					}
					if _, err := clients[other].RenderDesired(context.Background(), app, revision); issueCode(err) != "routing_invalid" {
						t.Fatal("foreign instance was allowed to borrow render context")
					}
					app.Spec["destination"].(map[string]any)["name"] = "unregistered"
					if _, err := client.RenderDesired(context.Background(), app, revision); issueCode(err) != "access_denied" {
						t.Fatal("unknown cluster borrowed another cluster's context")
					}
				})
			}
		}
	}
}
