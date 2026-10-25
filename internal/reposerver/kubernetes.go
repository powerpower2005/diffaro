package reposerver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/procio"
	"diffaro/internal/profile"
)

type kubernetesContext struct {
	capabilities   func(context.Context, string, string) (argocd.ClusterCapabilities, error)
	clusters       map[string]argocd.ClusterCapabilities
	clusterBytes   int
	tracking       func(context.Context) (string, error)
	trackingMethod string
	mu             sync.Mutex
	config         profile.KubernetesContext
	read           func(context.Context, ...string) (map[string]any, error)
	settings       map[string]any
	secrets        []map[string]string
	version        string
	apis           []any
	// fetching holds the running fetchOnce reads by key and failed the
	// failures they keep for the run. mu is never held while they read.
	fetching map[string]chan struct{}
	failed   map[string]error
}

func newKubernetesContext(config profile.KubernetesContext) *kubernetesContext {
	k := &kubernetesContext{config: config}
	k.read = func(ctx context.Context, args ...string) (map[string]any, error) {
		argv := []string{"--kubeconfig", config.Kubeconfig, "--context", config.Context, "--request-timeout=30s"}
		command := exec.CommandContext(ctx, config.Kubectl, append(argv, args...)...)
		command.Env = []string{}
		for _, key := range []string{"PATH", "SystemRoot", "HOME", "USERPROFILE", "LOCALAPPDATA", "TMP", "TEMP"} {
			if value, ok := os.LookupEnv(key); ok {
				command.Env = append(command.Env, key+"="+value)
			}
		}
		command.WaitDelay = time.Second
		output := procio.Bounded{Limit: 16 << 20}
		var diagnostic processDiagnostic
		defer diagnostic.buffer.Reset()
		command.Stdout, command.Stderr = &output, &diagnostic
		if err := command.Run(); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, contextReadFailure(diagnostic.buffer.Bytes())
		}
		var result map[string]any
		if json.Unmarshal(output.Bytes(), &result) != nil || result == nil {
			return nil, invalid("Kubernetes context response must be a bounded JSON object")
		}
		return result, nil
	}
	return k
}

// errSettledRead marks a kubectl read that the Kubernetes API answered with
// Forbidden or NotFound: repeating it in the same run only reproduces it.
var errSettledRead = errors.New("kubernetes API answered Forbidden or NotFound")

// contextReadFailure is the failure of a kubectl read. kubectl's diagnostic
// only classifies it and never becomes part of the error.
func contextReadFailure(diagnostic []byte) error {
	var cause error
	if text := string(diagnostic); strings.Contains(text, "Error from server (Forbidden)") || strings.Contains(text, "Error from server (NotFound)") {
		cause = errSettledRead
	}
	return domain.Fail(domain.CodeContextReadFailed, "context", "Kubernetes render context read failed", "check the explicit kubeconfig/context and ConfigMap, repository Secret and discovery read permissions", cause)
}

// collectedContext is a fully read Kubernetes render context.
type collectedContext struct {
	settings map[string]any
	secrets  []map[string]string
	version  string
	apis     []any
}

// load collects the Argo CD settings, repository credentials and in-cluster
// Kubernetes capabilities once, committing them only when all were read. A
// denied or missing read fails every later caller of the run without
// reading again.
func (k *kubernetesContext) load(ctx context.Context) error {
	_, err := fetchOnce(ctx, k, "context", func() (collectedContext, bool) {
		return collectedContext{}, k.settings != nil
	}, func() (collectedContext, error) {
		settings, err := k.readSettings(ctx)
		if err != nil {
			return collectedContext{}, err
		}
		secrets, err := k.readRepositorySecrets(ctx)
		if err != nil {
			return collectedContext{}, err
		}
		version, apis, err := k.readCapabilities(ctx)
		return collectedContext{settings, secrets, version, apis}, err
	}, func(c collectedContext) error {
		// Commit only a fully collected context, never a partially populated cache.
		k.settings, k.secrets, k.version, k.apis = c.settings, c.secrets, c.version, c.apis
		return nil
	})
	return err
}

// readSettings reads the argocd-cm data, whose values must all be strings.
func (k *kubernetesContext) readSettings(ctx context.Context) (map[string]any, error) {
	cm, err := k.read(ctx, "-n", k.config.Namespace, "get", "configmap", "argocd-cm", "-o", "json")
	if err != nil {
		return nil, err
	}
	settings, ok := cm["data"].(map[string]any)
	if !ok {
		return nil, invalid("Argo CD ConfigMap data is missing")
	}
	for _, value := range settings {
		if _, ok := value.(string); !ok {
			return nil, invalid("Argo CD settings must contain string values")
		}
	}
	return settings, nil
}

// readRepositorySecrets reads and decodes only repository credential Secret
// types. Other Argo CD tokens and cluster credentials are not requested by
// this collector.
func (k *kubernetesContext) readRepositorySecrets(ctx context.Context) ([]map[string]string, error) {
	secrets := []map[string]string{}
	for _, kind := range []string{"repository", "repo-creds"} {
		list, err := k.read(ctx, "-n", k.config.Namespace, "get", "secrets", "-l", "argocd.argoproj.io/secret-type="+kind, "-o", "json")
		if err != nil {
			return nil, err
		}
		items, ok := list["items"].([]any)
		if !ok || len(items) > 1000 {
			return nil, invalid("repository Secret inventory is invalid or exceeds limit")
		}
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, invalid("repository Secret is invalid")
			}
			data, ok := object["data"].(map[string]any)
			if !ok {
				return nil, invalid("repository Secret data is missing")
			}
			decoded := map[string]string{"_kind": kind}
			for key, value := range data {
				s, ok := value.(string)
				if !ok {
					return nil, invalid("repository Secret data is invalid")
				}
				raw, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, invalid("repository Secret data is not base64")
				}
				decoded[key] = string(raw)
			}
			secrets = append(secrets, decoded)
		}
	}
	return secrets, nil
}

// readCapabilities reads the Kubernetes version and the sorted API versions
// and group/version/kind capabilities served by the cluster.
func (k *kubernetesContext) readCapabilities(ctx context.Context) (string, []any, error) {
	version, err := k.read(ctx, "get", "--raw", "/version")
	if err != nil {
		return "", nil, err
	}
	if domain.String(version, "gitVersion") == "" {
		return "", nil, invalid("Kubernetes version is missing")
	}
	core, err := k.read(ctx, "get", "--raw", "/api")
	if err != nil {
		return "", nil, err
	}
	groups, err := k.read(ctx, "get", "--raw", "/apis")
	if err != nil {
		return "", nil, err
	}
	paths, err := apiPaths(core, groups)
	if err != nil {
		return "", nil, err
	}
	keys := slices.Sorted(maps.Keys(paths))
	discovered := k.discover(ctx, keys, paths)
	capabilities := map[string]bool{}
	for i, key := range keys {
		resources, err := discovered[i].resources, discovered[i].err
		if err != nil {
			return "", nil, err
		}
		items, ok := resources["resources"].([]any)
		if !ok {
			return "", nil, invalid("API resources are missing")
		}
		capabilities[key] = true
		for _, value := range items {
			resource, ok := value.(map[string]any)
			if !ok || domain.String(resource, "kind") == "" || len(domain.String(resource, "kind")) > 253 {
				return "", nil, invalid("invalid API resource")
			}
			capabilities[key+"/"+domain.String(resource, "kind")] = true
		}
		if len(capabilities) > 20000 {
			return "", nil, invalid("API capability inventory exceeds limit")
		}
	}
	apis := []any{}
	for _, key := range slices.Sorted(maps.Keys(capabilities)) {
		apis = append(apis, key)
	}
	return domain.String(version, "gitVersion"), apis, nil
}

// discoveryConcurrency bounds the kubectl discovery reads run at once.
const discoveryConcurrency = 8

type discovery struct {
	resources map[string]any
	err       error
}

// discover reads the discovery document of every key's path with bounded
// concurrency and returns the results in keys order. The caller checks them
// in that order, so the first failing key decides the error as when they
// were read one after another. After a read fails, keys after it are not
// read: the caller stops at that failure or earlier.
func (k *kubernetesContext) discover(ctx context.Context, keys []string, paths map[string]string) []discovery {
	results := make([]discovery, len(keys))
	var mu sync.Mutex
	next, failed := 0, len(keys)
	var wg sync.WaitGroup
	for range min(discoveryConcurrency, len(keys)) {
		wg.Go(func() {
			for {
				mu.Lock()
				i := next
				next++
				stop := i >= len(keys) || i > failed
				mu.Unlock()
				if stop {
					return
				}
				resources, err := k.read(ctx, "get", "--raw", paths[keys[i]])
				results[i] = discovery{resources, err}
				if err != nil {
					mu.Lock()
					failed = min(failed, i)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	return results
}

// apiPaths maps every served API version to its discovery path from the
// /api and /apis responses.
func apiPaths(core, groups map[string]any) (map[string]string, error) {
	paths := map[string]string{}
	coreVersions, ok := core["versions"].([]any)
	if !ok {
		return nil, invalid("core API versions are missing")
	}
	apiGroups, ok := groups["groups"].([]any)
	if !ok {
		return nil, invalid("API groups are missing")
	}
	for _, v := range coreVersions {
		if s, ok := v.(string); ok && safeAPIPath(s) {
			paths[s] = "/api/" + s
		} else {
			return nil, invalid("invalid core API version")
		}
	}
	for _, g := range apiGroups {
		group, ok := g.(map[string]any)
		if !ok {
			return nil, invalid("invalid API group")
		}
		versions, ok := group["versions"].([]any)
		if !ok {
			return nil, invalid("API group versions are missing")
		}
		for _, v := range versions {
			entry, ok := v.(map[string]any)
			if !ok {
				return nil, invalid("invalid group version")
			}
			s := domain.String(entry, "groupVersion")
			if !safeAPIPath(s) {
				return nil, invalid("invalid group version path")
			}
			paths[s] = "/apis/" + s
		}
	}
	if len(paths) == 0 || len(paths) > 512 {
		return nil, invalid("API version inventory is empty or exceeds limit")
	}
	return paths, nil
}

func safeAPIPath(s string) bool {
	return s != "" && len(s) < 300 && !strings.Contains(s, "..") && !strings.Contains(s, "//") && strings.Count(s, "/") <= 1 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-/") == "" && !strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/")
}

func (k *kubernetesContext) binding(ctx context.Context, app domain.Application) (Binding, error) {
	b := Binding{Project: app.Project(), ApplicationNamespace: app.ID.Namespace, RepositoryURL: domain.String(app.Source(), "repoURL")}
	dest := app.Destination()
	end := domain.TraceStart(ctx, "context", "kubernetes", k.config.Namespace)
	err := k.load(ctx)
	end(err)
	if err != nil {
		return b, err
	}
	version, apis := k.version, k.apis
	if domain.String(dest, "server") != "https://kubernetes.default.svc" || domain.String(dest, "name") != "" {
		endCapabilities := domain.TraceRead(ctx, "context", "capabilities", domain.String(dest, "server")+"|"+domain.String(dest, "name"))
		capabilities, err := k.destinationCapabilities(ctx, dest)
		endCapabilities(func() string { return domain.DigestJSON(capabilities) }, err)
		if err != nil {
			return b, err
		}
		version, apis = capabilities.Version, []any{}
		for _, api := range capabilities.APIVersions {
			apis = append(apis, api)
		}
		if domain.String(dest, "name") != "" {
			b.resolvedServer = capabilities.Server
		}
		b.resolvedName = capabilities.Name
	}
	if v := domain.String(k.settings, "helm.enable"); v != "" && v != "true" {
		return b, invalid("Helm generation is disabled in Argo CD settings")
	}
	repo, err := selectRepository(k.secrets, b.RepositoryURL, b.Project)
	if err != nil {
		return b, err
	}
	label := domain.String(k.settings, "application.instanceLabelKey")
	if label == "" {
		label = "app.kubernetes.io/instance"
	}
	tracking, err := k.effectiveTracking(ctx)
	if err != nil {
		return b, err
	}
	schemes := []any{"https", "http"}
	if value, exists := k.settings["helm.valuesFileSchemes"]; exists {
		schemes = []any{}
		for _, s := range strings.Split(value.(string), ",") {
			if s = strings.TrimSpace(s); s != "" {
				schemes = append(schemes, s)
			}
		}
	}
	b.Destination = domain.CloneSpec(dest)
	b.Request = map[string]any{"repo": repo, "repos": []any{}, "helmRepoCreds": []any{}, "kubeVersion": version, "apiVersions": apis, "appLabelKey": label, "trackingMethod": tracking, "installationID": domain.String(k.settings, "installationID"), "helmOptions": map[string]any{"valuesFileSchemes": schemes}, "enabledSourceTypes": map[string]any{"Helm": true}}
	return b, nil
}

// fetchOnce returns the cached value for key or fetches it. k.mu
// guards the caches and k.fetching but is not held during fetch, so lookups
// of other keys and other context reads do not wait for I/O. Concurrent
// callers of the same key wait for the running fetch, or for their own
// cancellation, instead of repeating it. A denied or missing kubectl read
// (errSettledRead) is cached for the run; any other failure is not: each
// waiter then fetches itself, as when callers ran one after another. commit
// runs under k.mu after a successful fetch and may still reject the value.
func fetchOnce[T any](ctx context.Context, k *kubernetesContext, key string, cached func() (T, bool), fetch func() (T, error), commit func(T) error) (T, error) {
	var zero T
	k.mu.Lock()
	for {
		if value, ok := cached(); ok {
			k.mu.Unlock()
			return value, nil
		}
		if err := k.failed[key]; err != nil {
			k.mu.Unlock()
			return zero, err
		}
		running := k.fetching[key]
		if running == nil {
			break
		}
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-running:
		}
		k.mu.Lock()
	}
	if k.fetching == nil {
		k.fetching = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	k.fetching[key] = done
	k.mu.Unlock()
	value, err := fetch()
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.fetching, key)
	close(done)
	if err == nil {
		err = commit(value)
	}
	if err != nil {
		if errors.Is(err, errSettledRead) && ctx.Err() == nil {
			if k.failed == nil {
				k.failed = map[string]error{}
			}
			k.failed[key] = err
		}
		return zero, err
	}
	return value, nil
}

func (k *kubernetesContext) destinationCapabilities(ctx context.Context, dest map[string]any) (argocd.ClusterCapabilities, error) {
	server, name := domain.String(dest, "server"), domain.String(dest, "name")
	key := server + "\x00" + name
	return fetchOnce(ctx, k, "cluster\x00"+key, func() (argocd.ClusterCapabilities, bool) {
		result, ok := k.clusters[key]
		return result, ok
	}, func() (argocd.ClusterCapabilities, error) {
		if k.capabilities == nil {
			return argocd.ClusterCapabilities{}, domain.Fail(domain.CodeContextDestinationUnavailable, "context", "destination capability reader is unavailable", "configure cluster read access or a trusted contextFile", nil)
		}
		return k.capabilities(ctx, server, name)
	}, func(result argocd.ClusterCapabilities) error {
		bytes := len(result.Server) + len(result.Name) + len(result.Version)
		for _, api := range result.APIVersions {
			bytes += len(api)
		}
		if bytes > (16<<20)-k.clusterBytes {
			return domain.LimitError(domain.CodeLimitExceeded, "context", "destination capability cache", "capability cache byte budget", int64(k.clusterBytes+bytes), 16<<20, "bytes", true, 0)
		}
		if k.clusters == nil {
			k.clusters = map[string]argocd.ClusterCapabilities{}
		}
		k.clusters[key] = result
		k.clusterBytes += bytes
		return nil
	})
}

func (k *kubernetesContext) effectiveTracking(ctx context.Context) (string, error) {
	return fetchOnce(ctx, k, "tracking", func() (string, bool) {
		return k.trackingMethod, k.trackingMethod != ""
	}, func() (string, error) {
		tracking := domain.String(k.settings, "application.resourceTrackingMethod")
		if tracking == "" {
			if k.tracking == nil {
				return "", invalid("effective Argo CD tracking method is unavailable")
			}
			var err error
			tracking, err = k.tracking(ctx)
			if err != nil {
				return "", err
			}
		}
		if tracking != "label" && tracking != "annotation" && tracking != "annotation+label" {
			return "", invalid("unsupported effective Argo CD tracking method")
		}
		return tracking, nil
	}, func(tracking string) error {
		k.trackingMethod = tracking
		return nil
	})
}
