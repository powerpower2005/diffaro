package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

type progress struct {
	mu      sync.Mutex
	writer  io.Writer
	phase   string
	started time.Time
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func startProgress(out io.Writer, quiet bool, interval time.Duration) *progress {
	p := &progress{writer: out, started: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	if quiet {
		p.writer = io.Discard
		close(p.done)
		return p
	}
	go func() {
		defer close(p.done)
		// Progress lines are optional; a panic only stops them.
		defer func() { _ = recover() }()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.tick()
			}
		}
	}()
	return p
}

// tick prints the current phase; the lock is released even if writing panics.
func (p *progress) tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase != "" {
		fmt.Fprintf(p.writer, "Diffaro: still %s (%ds elapsed).\n", p.phase, int(time.Since(p.started).Seconds()))
	}
}

func (p *progress) stage(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
	fmt.Fprintf(p.writer, "Diffaro: %s...\n", phase)
}

func (p *progress) close() {
	p.once.Do(func() { close(p.stop) })
	<-p.done
}

func printCompletion(out io.Writer, report domain.Report, redactor security.Redactor) {
	complete, failed, skipped := 0, 0, 0
	for _, app := range report.Applications {
		switch app.Execution {
		case domain.ExecutionComplete:
			complete++
		case domain.ExecutionFailed, domain.ExecutionDenied:
			failed++
		case domain.ExecutionSkipped:
			skipped++
		}
	}
	fmt.Fprintf(out, "Diffaro: status=%s; exit=%d; Applications: %d complete, %d failed/denied, %d skipped.\n", redactor.Text(string(report.Status)), report.ExitCode(), complete, failed, skipped)
	unaffected := 0
	for _, result := range report.Discovery {
		unaffected += result.Unaffected
	}
	if report.Status == domain.StatusSuccess && report.Coverage == domain.CoverageConnectionPreflight {
		// doctor without --render compares nothing.
		fmt.Fprintln(out, "Read access checked; targets were not rendered. Run doctor --render --pr NUMBER or diff to verify rendering.")
	} else if report.Status == domain.StatusSuccess {
		if complete == 0 && unaffected > 0 && len(report.Applications) == 0 {
			// Changed-path selection proved every candidate's targets
			// identical on both revisions; nothing needed rendering.
			fmt.Fprintf(out, "No target changes: no PR file is under the inputs of the %d Application(s) that use the PR repository, so none needed rendering (changed-path selection).\n", unaffected)
		} else if complete == 0 && skipped > 0 {
			fmt.Fprintf(out, "No Application target comparisons completed: %d skipped; the report states each reason.\n", skipped)
		} else if complete == 0 {
			fmt.Fprintln(out, "No Application target comparisons completed; check the selection coverage.")
		} else if report.ChangesFound {
			fmt.Fprintln(out, "Changes found. Exit 1 indicates changes, not a failed comparison.")
		} else {
			fmt.Fprintln(out, "No target changes found in the compared scope.")
		}
	}
}
