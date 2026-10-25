package reposerver

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type portForward struct {
	command func(context.Context) *exec.Cmd // Test seam for process lifecycle.
	mu      sync.Mutex
	config  profile.RepoServer
	cancel  context.CancelFunc
	done    chan struct{}
	// starting is closed when the startup in progress ends. The starter does
	// not hold mu, so waiters can honor their own context.
	starting chan struct{}
	endpoint string
	closed   bool
}

var forwardLine = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:([0-9]+) -> [0-9]+$`)

func forwardAddress(line string) string {
	match := forwardLine.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	port, err := strconv.Atoi(match[1])
	if err != nil || port < 1 || port > 65535 {
		return ""
	}
	return "127.0.0.1:" + match[1]
}

func forwardFailure() error {
	return domain.Fail(domain.CodeRepoServerFailed, "connection", "automatic repo-server port forwarding could not start or was interrupted", "check the selected Kubernetes context, repo-server resource, pods/get and pods/portforward permissions; retry the command", nil)
}

// address returns the forward's endpoint, starting it on first use. A process
// that exits or misses the startup timeout fails every later call.
// A startup abandoned because its caller was cancelled leaves no state, so the
// next caller with a live context starts a new forward.
func (p *portForward) address(ctx context.Context) (string, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return "", forwardFailure()
		}
		if starting := p.starting; starting != nil {
			p.mu.Unlock()
			select {
			case <-starting:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if p.done != nil {
			endpoint, done := p.endpoint, p.done
			p.mu.Unlock()
			select {
			case <-done:
				return "", forwardFailure()
			default:
				return endpoint, nil
			}
		}
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return "", err
		}
		starting := make(chan struct{})
		p.starting = starting
		p.mu.Unlock()
		endpoint, err := p.start(ctx)
		p.mu.Lock()
		p.starting = nil
		close(starting)
		if err == nil {
			p.endpoint = endpoint
		} else if ctx.Err() != nil && err == ctx.Err() {
			p.cancel, p.done = nil, nil
		}
		p.mu.Unlock()
		return endpoint, err
	}
}

// start runs one forward process and waits for its listener. On failure the
// process has exited and been reaped before start returns.
func (p *portForward) start(ctx context.Context) (string, error) {
	k, target := p.config.Kubernetes, p.config.PortForward
	lifetime, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(lifetime, k.Kubectl, "--kubeconfig", k.Kubeconfig, "--context", k.Context, "--namespace", k.Namespace, "--request-timeout=30s", "port-forward", "--address=127.0.0.1", target.Resource, ":"+strconv.Itoa(target.Port))
	if p.command != nil {
		cmd = p.command(lifetime)
	}
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "SystemRoot", "HOME", "USERPROFILE", "LOCALAPPDATA", "TMP", "TEMP"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", forwardFailure()
	}
	if err = cmd.Start(); err != nil {
		cancel()
		stdout.Close()
		return "", forwardFailure()
	}
	done := make(chan struct{})
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	if p.closed {
		cancel()
	}
	p.mu.Unlock()
	ready := make(chan string, 1)
	go func() {
		defer close(done)
		defer func() {
			cancel()
			_ = cmd.Wait()
		}()
		// A panic ends the forward like an exited command: the caller
		// reports the forward failure.
		defer func() { _ = recover() }()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 4096)
		for scanner.Scan() {
			if address := forwardAddress(scanner.Text()); address != "" {
				select {
				case ready <- address:
				default:
				}
			}
		}
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case endpoint := <-ready:
		return endpoint, nil
	case <-ctx.Done():
		cancel()
		<-done
		return "", ctx.Err()
	case <-timer.C:
		cancel()
		<-done
		return "", forwardFailure()
	case <-done:
		return "", forwardFailure()
	}
}

func (p *portForward) close() {
	p.mu.Lock()
	p.closed = true
	cancel, done, starting := p.cancel, p.done, p.starting
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if starting != nil {
		<-starting
	}
}
