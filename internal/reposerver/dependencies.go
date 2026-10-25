package reposerver

import (
	"sort"

	"diffaro/internal/argopolicy"
	"diffaro/internal/domain"
)

// Git Helm sources receive Helm repositories (including enableOCI Helm repos),
// not native OCI source repositories. Filter before handling secret material.
func (k *kubernetesContext) helmDependencies(project map[string]any, projectName string) ([]any, []any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	repos, creds := []any{}, []any{}
	selected := []map[string]string{}
	for _, secret := range k.secrets {
		if secret["type"] != "helm" || (secret["project"] != "" && secret["project"] != projectName) {
			continue
		}
		allowed, err := argopolicy.SourceAllowed(project["sourceRepos"], secret["url"])
		if err != nil {
			return nil, nil, err
		}
		if allowed {
			selected = append(selected, secret)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		return selected[i]["_kind"]+"\x00"+selected[i]["url"] < selected[j]["_kind"]+"\x00"+selected[j]["url"]
	})
	seen := map[string]bool{}
	for _, secret := range selected {
		key := secret["_kind"] + "\x00" + secret["url"]
		if secret["url"] == "" || seen[key] {
			return nil, nil, invalid("missing or ambiguous Helm dependency repository identity; use contextFile")
		}
		seen[key] = true
		if secret["_kind"] == "repository" {
			repo, err := selectRepository(k.secrets, secret["url"], projectName)
			if err != nil {
				return nil, nil, err
			}
			repos = append(repos, repo)
		} else if secret["_kind"] == "repo-creds" {
			if secret["project"] != "" {
				return nil, nil, invalid("project-scoped credential templates require explicit contextFile")
			}
			// Convert without inheriting another template. Repository and RepoCreds
			// share credential field names, but their URL fields differ.
			copy := map[string]string{}
			for key, value := range secret {
				switch key {
				case "name", "insecure", "enableLfs", "project":
					return nil, nil, invalid("unsupported Helm credential template field; use contextFile")
				}
				copy[key] = value
			}
			copy["_kind"] = "repository"
			credential, err := selectRepository([]map[string]string{copy}, secret["url"], projectName)
			if err != nil {
				return nil, nil, err
			}
			credential["url"] = domain.String(credential, "repo")
			delete(credential, "repo")
			creds = append(creds, credential)
		}
	}
	return repos, creds, nil
}
