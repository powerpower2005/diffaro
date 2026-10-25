package reporter

import (
	"fmt"
	"sort"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

type imageChange struct {
	container, name, before, after string
}

// Project only observed, paired target changes. Array order and all other
// container fields remain in the original diff and JSON machine contract.
func imageChanges(resource domain.ResourceDiff) []imageChange {
	if resource.State != domain.StateChanged || resource.Comparison != domain.ComparisonComplete {
		return nil
	}
	var lists []containerList
	switch resource.ID.Group + "/" + resource.ID.Kind {
	case "apps/Deployment", "apps/StatefulSet", "apps/DaemonSet", "apps/ReplicaSet", "/ReplicationController", "batch/Job":
		lists = templateContainers
	case "batch/CronJob":
		lists = cronJobContainers
	case "/Pod":
		lists = podContainers
	default:
		return nil
	}
	var out []imageChange
	for _, list := range lists {
		field, path := list.field, list.path
		var found []imageChange
		for _, change := range resource.Changes {
			if change.Redacted {
				continue
			}
			// Lists compared by container name report one image field
			//; unnamed or reordered lists stay one list change.
			if name, ok := itemImageChange(change, path); ok {
				before, _ := change.Before.(string)
				after, _ := change.After.(string)
				if before != "" && after != "" && !strings.Contains(before, security.Mask) && !strings.Contains(after, security.Mask) {
					found = append(found, imageChange{field, name, before, after})
				}
				continue
			}
			if change.Path != path {
				continue
			}
			base, baseOK := containerImages(change.Before)
			head, headOK := containerImages(change.After)
			if !baseOK || !headOK {
				continue // Missing, masked or ambiguous inputs are not image changes.
			}
			for name, before := range base {
				if after, exists := head[name]; exists && before != after && !strings.Contains(before, security.Mask) && !strings.Contains(after, security.Mask) {
					found = append(found, imageChange{field, name, before, after})
				}
			}
		}
		sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
		out = append(out, found...)
	}
	return out
}

// containerList is a container list field of a pod spec and its path in a
// workload.
type containerList struct{ field, path string }

func containerLists(podPath string) []containerList {
	var out []containerList
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		out = append(out, containerList{field, podPath + "/" + field})
	}
	return out
}

var (
	templateContainers = containerLists("/spec/template/spec")
	cronJobContainers  = containerLists("/spec/jobTemplate/spec/template/spec")
	podContainers      = containerLists("/spec")
)

// itemImageChange reports the container name of a replaced image field in a
// container list compared by item name.
func itemImageChange(change domain.Change, list string) (string, bool) {
	rest, ok := strings.CutPrefix(change.Path, list)
	if ok {
		rest, ok = strings.CutPrefix(rest, "/")
	}
	if !ok || change.Operation != domain.OperationReplace || len(change.ItemNames) == 0 {
		return "", false
	}
	index, field, ok := strings.Cut(rest, "/")
	if !ok || field != "image" || strings.Trim(index, "0123456789") != "" {
		return "", false
	}
	return change.ItemNames[len(change.ItemNames)-1], true
}

func containerImages(value any) (map[string]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	images := make(map[string]string, len(items))
	for _, item := range items {
		container, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		name, nameOK := container["name"].(string)
		image, imageOK := container["image"].(string)
		if !nameOK || !imageOK || name == "" || image == "" || strings.Contains(name, security.Mask) {
			return nil, false
		}
		if _, duplicate := images[name]; duplicate {
			return nil, false
		}
		images[name] = image
	}
	return images, true
}

// reportImage is an image change of a resource in an Application.
type reportImage struct {
	app      domain.ApplicationID
	resource domain.ResourceID
	imageChange
}

// reportImages lists the image changes of every resource in report order.
func reportImages(report domain.Report) []reportImage {
	var out []reportImage
	for _, app := range report.Applications {
		for _, resource := range app.Resources {
			for _, change := range imageChanges(resource) {
				out = append(out, reportImage{app.ID, resource.ID, change})
			}
		}
	}
	return out
}

func writeImageSummary(b *strings.Builder, images []reportImage) {
	if len(images) == 0 {
		return
	}
	b.WriteString("**Container image changes (observed targets):**\n\n")
	for i, image := range images {
		if i == 12 {
			fmt.Fprintf(b, "- %d more image changes are in the resource evidence and JSON.\n", len(images)-i)
			break
		}
		fmt.Fprintf(b, "- `%s` · `%s/%s/%s` · `%s/%s`: `%s` → `%s`\n", prText(image.app.String(), 180), prText(image.resource.Kind, 60), prText(image.resource.Namespace, 80), prText(image.resource.Name, 100), image.container, prText(image.name, 100), prText(image.before, 220), prText(image.after, 220))
	}
	b.WriteString("\n")
}
