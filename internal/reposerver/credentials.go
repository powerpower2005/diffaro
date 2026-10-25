package reposerver

import (
	"strconv"
	"strings"
)

// selectRepository returns the repo-server credential for repository: its
// repository Secret, completed from the longest matching credential template
// when the Secret carries no credentials.
func selectRepository(secrets []map[string]string, repository, project string) (map[string]any, error) {
	selected, err := repositorySecret(secrets, repository, project)
	if err != nil {
		return nil, err
	}
	template, err := credentialTemplate(secrets, repository, selected)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{}
	for key, value := range selected {
		merged[key] = value
	}
	if template != nil {
		if err = inheritCredentials(merged, template); err != nil {
			return nil, err
		}
	}
	return repositoryRequest(repository, merged)
}

// repositorySecret returns the one repository Secret for repository within
// project, or an empty map when none matches.
func repositorySecret(secrets []map[string]string, repository, project string) (map[string]string, error) {
	selected := map[string]string{}
	for _, s := range secrets {
		if s["_kind"] == "repository" && s["url"] == repository && (s["project"] == "" || s["project"] == project) {
			if len(selected) != 0 {
				return nil, invalid("multiple repository Secrets match; use an explicit contextFile")
			}
			selected = s
		}
	}
	if selected["useAzureWorkloadIdentity"] == "true" {
		return nil, invalid("workload identity requires a verified instance-local credential environment; use contextFile")
	}
	return selected, nil
}

// credentialTemplate returns the longest credential template whose URL
// prefixes repository, or nil when selected carries credentials itself or no
// template matches.
func credentialTemplate(secrets []map[string]string, repository string, selected map[string]string) (map[string]string, error) {
	for _, key := range []string{"username", "password", "sshPrivateKey", "githubAppPrivateKey", "tlsClientCertData", "bearerToken", "azureServicePrincipalClientSecret"} {
		if selected[key] != "" {
			return nil, nil
		}
	}
	var template map[string]string
	longest := 0
	for _, s := range secrets {
		if s["_kind"] != "repo-creds" || s["url"] == "" || !strings.HasPrefix(repository, s["url"]) {
			continue
		}
		if s["project"] != "" {
			return nil, invalid("project-scoped credential templates require explicit contextFile")
		}
		if len(s["url"]) == longest {
			return nil, invalid("ambiguous repository credential templates")
		}
		if len(s["url"]) > longest {
			template, longest = s, len(s["url"])
		}
	}
	return template, nil
}

// inheritCredentials fills merged from template as Argo CD's
// CopyCredentialsFrom does.
func inheritCredentials(merged, template map[string]string) error {
	if template["useAzureWorkloadIdentity"] == "true" {
		return invalid("workload identity requires a verified instance-local credential environment; use contextFile")
	}
	for key, value := range template {
		switch key {
		case "_kind", "url":
		case "username", "password", "bearerToken", "sshPrivateKey", "tlsClientCertData", "tlsClientCertKey", "githubAppPrivateKey", "githubAppEnterpriseBaseUrl", "gcpServiceAccountKey", "proxy", "noProxy", "type":
			if merged[key] == "" {
				merged[key] = value
			}
		case "githubAppID", "githubAppInstallationID":
			if merged[key] == "" || merged[key] == "0" {
				merged[key] = value
			}
		case "enableOCI", "insecureOCIForceHttp", "forceHttpBasicAuth", "useAzureWorkloadIdentity":
		default:
			return invalid("unsupported inherited repository credential setting; use contextFile")
		}
	}
	// Upstream CopyCredentialsFrom overwrites these boolean fields even when
	// the template omits them (the protobuf zero value is false).
	for _, key := range []string{"enableOCI", "insecureOCIForceHttp", "forceHttpBasicAuth", "useAzureWorkloadIdentity"} {
		merged[key] = template[key]
		if merged[key] == "" {
			merged[key] = "false"
		}
	}
	return nil
}

// repositoryRequest encodes the credential fields of merged as the
// repo-server request's typed repository fields.
func repositoryRequest(repository string, merged map[string]string) (map[string]any, error) {
	result := map[string]any{"repo": repository}
	for key, value := range merged {
		if key == "url" || key == "_kind" || value == "" {
			continue
		}
		switch key {
		case "username", "password", "sshPrivateKey", "tlsClientCertData", "tlsClientCertKey", "type", "name", "githubAppPrivateKey", "githubAppEnterpriseBaseUrl", "proxy", "project", "gcpServiceAccountKey", "noProxy", "bearerToken":
			result[key] = value
		case "insecure", "enableLfs", "enableOCI", "insecureOCIForceHttp", "forceHttpBasicAuth", "useAzureWorkloadIdentity":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, invalid("invalid repository boolean setting")
			}
			result[key] = b
		case "githubAppID", "githubAppInstallationID":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, invalid("invalid repository application ID")
			}
			result[key] = n
		default:
			return nil, invalid("unsupported repository credential setting; use contextFile")
		}
	}
	return result, nil
}
