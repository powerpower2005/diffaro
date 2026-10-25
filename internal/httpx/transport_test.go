package httpx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentRequestsReuseConnections protects connection reuse under the
// parallel blob reads of local Helm: eight parallel requests repeated twenty
// times, from two clients as for two instances on one host, must keep reusing
// the connections of the first round instead of dialing again because the
// idle pool keeps fewer connections than were in use.
func TestConcurrentRequestsReuseConnections(t *testing.T) {
	const parallel, rounds = 8, 20
	var opened atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	clients := []*Client{New(s.URL, "fictional-token-a", 5*time.Second, 1024), New(s.URL, "fictional-token-b", 5*time.Second, 1024)}
	for round := range rounds {
		var wg sync.WaitGroup
		for i := range parallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var out any
				if err := clients[(round+i)%2].Get(context.Background(), "/", nil, &out); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	t.Logf("%d requests opened %d connections", parallel*rounds, opened.Load())
	if opened.Load() > parallel {
		t.Fatalf("%d requests opened %d connections, want at most %d", parallel*rounds, opened.Load(), parallel)
	}
}
