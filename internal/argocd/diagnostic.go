package argocd

import (
	"errors"
	"reflect"
	"strconv"
	"strings"

	"diffaro/internal/domain"
)

const unreadableChildReason = "Child lookup returned not_found (HTTP 404); no readable stored Application spec is available. Check the child identity and read access; a private Application may be hidden from this token."

// Diagnose only fixed schema paths, never input values or repository-controlled
// map keys. Revision differences have already been excluded by the caller.
func specMismatchReason(desired, stored map[string]any) string {
	paths := []string{}
	var compare func(map[string]any, map[string]any, string)
	compare = func(left, right map[string]any, path string) {
		var keys []string
		switch {
		case path == "":
			keys = []string{"project", "destination", "source", "sources", "syncPolicy", "ignoreDifferences", "info", "revisionHistoryLimit"}
		case strings.HasSuffix(path, "/helm"):
			keys = domain.HelmSourceFields
		default:
			keys = []string{"repoURL", "path", "chart", "ref", "helm", "kustomize", "directory", "plugin"}
		}
		remainingLeft, remainingRight := make(map[string]any), make(map[string]any)
		for key, value := range left {
			remainingLeft[key] = value
		}
		for key, value := range right {
			remainingRight[key] = value
		}
		for _, key := range keys {
			l, lok := left[key]
			r, rok := right[key]
			delete(remainingLeft, key)
			delete(remainingRight, key)
			if lok == rok && reflect.DeepEqual(l, r) {
				continue
			}
			field := path + "/" + key
			if key == "source" || key == "helm" {
				lm, lmap := l.(map[string]any)
				rm, rmap := r.(map[string]any)
				if lmap && rmap {
					compare(lm, rm, field)
					continue
				}
			}
			if key == "sources" {
				ls, la := l.([]any)
				rs, ra := r.([]any)
				if la && ra && len(ls) == len(rs) && len(ls) <= 32 {
					for i := range ls {
						if reflect.DeepEqual(ls[i], rs[i]) {
							continue
						}
						lm, lmap := ls[i].(map[string]any)
						rm, rmap := rs[i].(map[string]any)
						item := field + "/" + strconv.Itoa(i)
						if lmap && rmap {
							compare(lm, rm, item)
						} else {
							paths = append(paths, item)
						}
					}
					continue
				}
			}
			paths = append(paths, field)
		}
		if !reflect.DeepEqual(remainingLeft, remainingRight) {
			if path == "" {
				path = "/spec"
			}
			paths = append(paths, path+" (other fields)")
		}
	}
	compare(desired, stored, "")
	if len(paths) > 12 {
		paths = append(paths[:12], "additional fields omitted; inspect the trusted Application specs")
	}
	return "Desired child spec differs from the stored Application outside targetRevision; changed fields: " + strings.Join(paths, ", ") + ". Input values are withheld."
}

func applicationFailure(err error, id domain.ApplicationID, project string) error {
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		return err
	}
	copy := *failure
	copy.Issue.Application = &id
	// Without a project, Argo CD answers a missing Application with 403 so
	// RBAC does not reveal existence; a mistyped name reads as a denial.
	if project == "" && copy.Issue.ReasonCode == domain.ReasonRequestForbidden {
		copy.Issue.Reason += "; Argo CD also answers 403 when no Application of this name exists in this namespace and instance"
		copy.Issue.Action += "; check the Application name and namespace, such as applications[].name and namespace in the profile; diffaro init --endpoint URL lists the readable Applications"
	}
	return &copy
}
