package manifest

import (
	"encoding/json"
	"io"
	"strings"

	"diffaro/internal/domain"
	"go.yaml.in/yaml/v3"
)

type Resource struct {
	ID     domain.ResourceID
	Object map[string]any
	Secret bool
	// ObservedScope reports that identity used Argo CD's observed scope.
	ObservedScope bool
}
type Limits struct {
	Bytes     int64
	Documents int
}

// Parse infers missing namespaces from rendered CRDs, built-in kinds, then
// observed scopes. Observed scopes apply only to resources without a namespace.
func Parse(manifests []string, cluster, namespace string, limits Limits, observed map[string]bool) (map[string]Resource, error) {
	return parse(manifests, cluster, namespace, limits, observed, nil)
}

// ParseMultiSource applies Argo CD's ordered, last-resource-wins semantics.
// Repeated identities remain explicit evidence rather than silent diff noise.
func ParseMultiSource(manifests []string, cluster, namespace string, limits Limits, observed map[string]bool) (map[string]Resource, []domain.ResourceID, error) {
	repeated := []domain.ResourceID{}
	resources, err := parse(manifests, cluster, namespace, limits, observed, &repeated)
	return resources, repeated, err
}

// parse runs the stages in order: decodeObjects (which flattens each List
// document as it is decoded), crdScopes, then identify and normalize for each
// object in manifest order. The first failing check in that order is
// returned. With repeated nil a repeated identity is an error; otherwise the
// last object wins and the identity is recorded once.
func parse(manifests []string, cluster, namespace string, limits Limits, observed map[string]bool, repeated *[]domain.ResourceID) (map[string]Resource, error) {
	objects, err := decodeObjects(manifests, limits)
	if err != nil {
		return nil, err
	}
	scopes := crdScopes(objects)
	result := map[string]Resource{}
	seenRepeated := map[string]bool{}
	for _, object := range objects {
		id, usedObserved, err := identify(object, cluster, namespace, scopes, observed)
		if err != nil {
			return nil, err
		}
		normalized, err := normalize(object, id.Namespace)
		if err != nil {
			return nil, err
		}
		key := id.String()
		if _, exists := result[key]; exists {
			if repeated == nil {
				return nil, parseError(domain.CodeIdentityConflict, "multiple manifests describe the same resource identity")
			}
			if !seenRepeated[key] {
				*repeated = append(*repeated, id)
				seenRepeated[key] = true
			}
		}
		result[key] = Resource{ID: id, Object: normalized, Secret: id.Group == "" && id.Kind == "Secret", ObservedScope: usedObserved}
	}
	return result, nil
}

// decodeObjects decodes every YAML document into an object and flattens List
// documents. Limits are checked as it goes: total bytes before each manifest
// is decoded, documents (including empty ones) and resources (including List
// items) after each document, so the first exceeded limit or invalid document
// is the one reported.
func decodeObjects(manifests []string, limits Limits) ([]map[string]any, error) {
	objects := []map[string]any{}
	var size int64
	documents := 0
	for _, raw := range manifests {
		size += int64(len(raw))
		if size > limits.Bytes {
			return nil, domain.LimitError(domain.CodeLimitExceeded, "manifest", "target manifests", "render.maxResponseBytes", size, limits.Bytes, "bytes", true, domain.MaxRenderResponseBytes)
		}
		d := yaml.NewDecoder(strings.NewReader(raw))
		for {
			var node yaml.Node
			err := d.Decode(&node)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, parseError(domain.CodeManifestInvalid, "manifest is not valid YAML")
			}
			documents++
			if documents > limits.Documents {
				return nil, domain.LimitError(domain.CodeLimitExceeded, "manifest", "YAML documents (including empty documents)", "render.maxDocuments", int64(documents), int64(limits.Documents), "documents", true, domain.MaxRenderDocuments)
			}
			object, err := decodeDocument(&node)
			if err != nil {
				return nil, err
			}
			if object == nil {
				continue
			}
			items, err := flattenList(object)
			if err != nil {
				return nil, err
			}
			objects = append(objects, items...)
			if len(objects) > limits.Documents {
				return nil, domain.LimitError(domain.CodeLimitExceeded, "manifest", "rendered resources (including List items)", "render.maxDocuments", int64(len(objects)), int64(limits.Documents), "resources", true, domain.MaxRenderDocuments)
			}
		}
	}
	return objects, nil
}

// decodeDocument returns the object of one YAML document, or nil for an empty
// or null document. Aliases, non-string keys and excessive nesting are
// rejected before decoding.
func decodeDocument(node *yaml.Node) (map[string]any, error) {
	if len(node.Content) == 0 {
		return nil, nil
	}
	count := 0
	if !safeNode(node, 0, &count) {
		return nil, parseError(domain.CodeManifestInvalid, "YAML aliases, complex keys or excessive nesting are unsupported")
	}
	var object map[string]any
	if err := node.Decode(&object); err != nil {
		return nil, parseError(domain.CodeManifestInvalid, "manifest must contain unique string keys")
	}
	return object, nil
}

// flattenList returns the items of a kind: List object, or the object itself.
// List items must be objects and must not be Lists.
func flattenList(object map[string]any) ([]map[string]any, error) {
	if domain.String(object, "kind") != "List" {
		return []map[string]any{object}, nil
	}
	items, ok := object["items"].([]any)
	if !ok {
		return nil, parseError(domain.CodeManifestInvalid, "List manifest must contain items")
	}
	objects := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || domain.String(m, "kind") == "List" {
			return nil, parseError(domain.CodeManifestInvalid, "nested or malformed List item")
		}
		objects = append(objects, m)
	}
	return objects, nil
}

// crdScopes returns, by "group/kind", whether each CustomResourceDefinition
// in the rendered target declares a cluster-scoped kind. CRDs whose scope is
// neither Cluster nor Namespaced are ignored.
func crdScopes(objects []map[string]any) map[string]bool {
	scopes := map[string]bool{}
	for _, object := range objects {
		if domain.String(object, "apiVersion") == "apiextensions.k8s.io/v1" && domain.String(object, "kind") == "CustomResourceDefinition" {
			spec, _ := object["spec"].(map[string]any)
			names, _ := spec["names"].(map[string]any)
			scope := domain.String(spec, "scope")
			if scope == "Cluster" || scope == "Namespaced" {
				scopes[domain.String(spec, "group")+"/"+domain.String(names, "kind")] = scope == "Cluster"
			}
		}
	}
	return scopes
}

// identify returns the stable identity of an object and whether its scope
// came from Argo CD's observed scopes. Scope is taken from rendered CRDs, then
// built-in kinds, then, only for an object without a namespace, observed
// scopes. A cluster-scoped object has no namespace; a namespaced object
// without one gets the destination namespace.
func identify(object map[string]any, cluster, namespace string, scopes, observed map[string]bool) (domain.ResourceID, bool, error) {
	api, kind := domain.String(object, "apiVersion"), domain.String(object, "kind")
	meta, _ := object["metadata"].(map[string]any)
	name := domain.String(meta, "name")
	if api == "" || kind == "" || name == "" {
		return domain.ResourceID{}, false, parseError(domain.CodeIdentityUnavailable, "resource requires apiVersion, kind and metadata.name; generateName is not a stable identity")
	}
	group := ""
	if strings.Contains(api, "/") {
		parts := strings.Split(api, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return domain.ResourceID{}, false, parseError(domain.CodeManifestInvalid, "invalid apiVersion")
		}
		group = parts[0]
	}
	ns := domain.String(meta, "namespace")
	clusterScoped, known := scope(group, kind, scopes)
	usedObserved := false
	if !known && ns == "" {
		clusterScoped, known = observed[group+"/"+kind]
		usedObserved = known
	}
	if clusterScoped {
		// Like Argo CD's DeduplicateTargetObjects and the API server, a
		// cluster-scoped target's namespace is ignored, not an error.
		ns = ""
	} else if ns == "" {
		if !known {
			return domain.ResourceID{}, false, domain.Fail(domain.CodeScopeUnavailable, "manifest", "cannot infer the namespace of "+api+" "+kind+" "+name+": its scope is unknown because neither the rendered target nor Argo CD's current status for this Application declares it", "set metadata.namespace in the chart, render the CustomResourceDefinition in the same target, or let Argo CD manage this kind for the Application first; a repo-server backend does not change scope inference", nil)
		}
		if namespace == "" {
			return domain.ResourceID{}, false, domain.Fail(domain.CodeScopeUnavailable, "manifest", "cannot infer the namespace of "+api+" "+kind+" "+name+": the resource is namespaced but the Application has no destination namespace", "set metadata.namespace in the chart or the Application destination namespace", nil)
		}
		ns = namespace
	}
	return domain.ResourceID{Cluster: cluster, Group: group, Kind: kind, Namespace: ns, Name: name}, usedObserved, nil
}

// normalizedMetadataFields are the only fields normalization removes
// (docs/reference.md, "Limits and normalization"). A new normalization rule
// belongs here and in that contract.
var normalizedMetadataFields = []string{"creationTimestamp", "resourceVersion", "uid", "managedFields"}

// normalize returns a JSON-normalized copy of object without
// normalizedMetadataFields and with metadata.namespace set to the identity's
// namespace, or removed when that is empty. The input is not modified.
func normalize(object map[string]any, ns string) (map[string]any, error) {
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, parseError(domain.CodeManifestInvalid, "manifest contains non-JSON values such as NaN or infinity")
	}
	// Equivalent to domain.CloneSpec(object) without marshaling twice.
	copy := domain.DecodeSpec(encoded)
	meta, _ := copy["metadata"].(map[string]any)
	for _, field := range normalizedMetadataFields {
		delete(meta, field)
	}
	if ns != "" {
		meta["namespace"] = ns
	} else {
		delete(meta, "namespace")
	}
	return copy, nil
}

func safeNode(n *yaml.Node, depth int, count *int) bool {
	*count++
	if depth > 64 || *count > 200000 || n.Kind == yaml.AliasNode {
		return false
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Kind != yaml.ScalarNode || n.Content[i].Tag != "!!str" {
				return false
			}
		}
	}
	for _, child := range n.Content {
		if !safeNode(child, depth+1, count) {
			return false
		}
	}
	return true
}
func parseError(code, reason string) error {
	return domain.Fail(code, "manifest", reason, "check manifests, explicit namespaces and configured limits", nil)
}

// builtinScopes records, by "group/kind", whether each built-in kind is
// cluster-scoped. Kinds that are not listed have an unknown scope.
var builtinScopes = scopeTable(
	map[string]string{"": "Namespace Node PersistentVolume ComponentStatus", "rbac.authorization.k8s.io": "ClusterRole ClusterRoleBinding", "apiextensions.k8s.io": "CustomResourceDefinition", "apiregistration.k8s.io": "APIService", "storage.k8s.io": "StorageClass CSIDriver CSINode VolumeAttachment", "admissionregistration.k8s.io": "MutatingWebhookConfiguration ValidatingWebhookConfiguration ValidatingAdmissionPolicy ValidatingAdmissionPolicyBinding", "scheduling.k8s.io": "PriorityClass", "networking.k8s.io": "IngressClass", "argoproj.io": "ClusterWorkflowTemplate"},
	map[string]string{"": "Pod Service ConfigMap Secret ServiceAccount PersistentVolumeClaim ReplicationController ResourceQuota LimitRange Endpoints Event", "apps": "Deployment StatefulSet DaemonSet ReplicaSet ControllerRevision", "batch": "Job CronJob", "networking.k8s.io": "Ingress NetworkPolicy", "rbac.authorization.k8s.io": "Role RoleBinding", "autoscaling": "HorizontalPodAutoscaler", "policy": "PodDisruptionBudget", "coordination.k8s.io": "Lease", "discovery.k8s.io": "EndpointSlice", "argoproj.io": "Application ApplicationSet"},
)

// scopeTable builds a "group/kind" table from space-separated kinds per
// group. A kind listed as both cluster-scoped and namespaced is cluster-scoped.
func scopeTable(cluster, namespaced map[string]string) map[string]bool {
	table := map[string]bool{}
	for group, kinds := range namespaced {
		for _, kind := range strings.Fields(kinds) {
			table[group+"/"+kind] = false
		}
	}
	for group, kinds := range cluster {
		for _, kind := range strings.Fields(kinds) {
			table[group+"/"+kind] = true
		}
	}
	return table
}

// scope reports whether group/kind is cluster-scoped and whether its scope is
// known, preferring custom (rendered CRD) scopes over built-in kinds.
func scope(group, kind string, custom map[string]bool) (bool, bool) {
	if v, ok := custom[group+"/"+kind]; ok {
		return v, true
	}
	v, ok := builtinScopes[group+"/"+kind]
	return v, ok
}
