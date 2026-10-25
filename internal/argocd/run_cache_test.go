package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

// Settings and destination capabilities are read once per run for every
// caller of the instance's client, whichever lookup reads them. A failed read
// is not kept: the next caller reads again and its failure names its own
// lookup.
func TestSettingsAndCapabilitiesReadOncePerRun(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var settings, clusters atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/settings":
				settings.Add(1)
				if fail {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"trackingMethod": "annotation", "appLabelKey": "app.kubernetes.io/instance", "controllerNamespace": "argocd"})
			case strings.HasPrefix(r.URL.Path, "/api/v1/clusters/"):
				clusters.Add(1)
				if fail {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "remote", "server": "https://remote.example", "info": map[string]any{"serverVersion": "v1.32.0", "apiVersions": []string{"v1"}, "connectionState": map[string]any{"status": "Successful"}}})
			default:
				http.NotFound(w, r)
			}
		}))
		c := &Client{Instance: "one", HTTP: httpx.New(s.URL, "instance-token", time.Second, 1<<20)}
		ctx := context.Background()
		tracking, trackingErr := c.EffectiveTrackingMethod(ctx)
		render, renderErr := c.RenderSettings(ctx)
		_, firstErr := c.ClusterCapabilities(ctx, "", "remote")
		caps, secondErr := c.ClusterCapabilities(ctx, "", "remote")
		s.Close()
		if fail {
			var failure *domain.Failure
			if settings.Load() != 2 || clusters.Load() != 2 || firstErr == nil || secondErr == nil {
				t.Fatalf("failed reads were kept: settings=%d clusters=%d", settings.Load(), clusters.Load())
			}
			if !errors.As(renderErr, &failure) || !strings.Contains(failure.Issue.Reason, "local render settings lookup") || trackingErr == nil || strings.Contains(trackingErr.Error(), "local render") {
				t.Fatalf("a failure did not name its own lookup: %v / %v", trackingErr, renderErr)
			}
			continue
		}
		if settings.Load() != 1 || clusters.Load() != 1 || trackingErr != nil || renderErr != nil || firstErr != nil || secondErr != nil {
			t.Fatalf("reads not shared: settings=%d clusters=%d", settings.Load(), clusters.Load())
		}
		if tracking != "annotation" || render.TrackingMethod != "annotation" || render.ControllerNamespace != "argocd" || caps.Server != "https://remote.example" {
			t.Fatalf("cached values differ: %q %+v %+v", tracking, render, caps)
		}
	}
}
