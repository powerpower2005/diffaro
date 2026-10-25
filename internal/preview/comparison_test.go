package preview

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// A second comparison may finish while the first renders. Each must attach
// only its own chart dependencies, while reusing its own head metadata reads.
func TestOverlappingComparisonsKeepTheirOwnInputEvidence(t *testing.T) {
	type runKey struct{}
	started, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	defer unblock()
	runner := graphRunner(graphBackend{render: func(ctx context.Context, _ domain.Application, rev string) ([]string, error) {
		if ctx.Value(runKey{}) == "a" {
			entered.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []string{configMap(rev)}, nil
	}})
	runner.Config.Render.Timeout = "10s"
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	files := []string{"charts/a/templates/config.yaml", "charts/b/templates/config.yaml"}
	present := map[string]bool{"charts/api/Chart.yaml": true, "charts/a/Chart.yaml": true, "charts/b/Chart.yaml": true, files[0]: true, files[1]: true}
	reads := map[string]int{}
	var mu sync.Mutex
	read := func(ctx context.Context, rev, file string) ([]byte, bool, error) {
		name := ctx.Value(runKey{}).(string)
		mu.Lock()
		reads[name+":"+rev+":"+file]++
		mu.Unlock()
		if file == "charts/api/Chart.yaml" {
			return []byte(fmt.Sprintf("apiVersion: v2\nname: api\nversion: 0.1.0\ndependencies:\n- name: %s\n  repository: file://../%s\n", name, name)), true, nil
		}
		if file == "charts/"+name+"/Chart.yaml" {
			return []byte("apiVersion: v2\nname: lib\nversion: 0.1.0\n"), true, nil
		}
		return nil, false, nil
	}
	runner.ChangedPaths = &ChangedPaths{Files: files, Listed: true, Present: present, Read: func(ctx context.Context, file string) ([]byte, bool, error) {
		return read(ctx, graphHead, file)
	}}
	no := false
	inventories := map[string]domain.ApplicationInventory{"one": &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("api"), ManagesApplications: &no}}}}
	pair := graphPair()
	pair.Mode = domain.RevisionModePR
	run := func(name string) domain.Report {
		ctx := context.WithValue(context.Background(), runKey{}, name)
		return runner.Compare(ctx, pair, false, inventories, ComparisonEvidence{Files: &InputFiles{Files: files, Read: read}})
	}
	first := make(chan domain.Report, 1)
	go func() { first <- run("a") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first comparison did not reach rendering")
	}
	second := run("b")
	unblock()
	for name, report := range map[string]domain.Report{"a": <-first, "b": second} {
		app := findApp(t, report, "api")
		if app.Execution != domain.ExecutionComplete || len(app.InputChanges) != 1 || app.InputChanges[0].Path != "charts/"+name+"/templates/config.yaml" {
			t.Fatalf("%s comparison mixed input evidence: %+v", name, app)
		}
		if reads[name+":"+graphHead+":charts/api/Chart.yaml"] != 1 {
			t.Fatalf("%s comparison did not reuse its own head chart read: %v", name, reads)
		}
	}
}
