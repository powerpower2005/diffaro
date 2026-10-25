package profile

import (
	"net/url"
	"strings"
)

// Matches reports whether a source URL identifies the configured repository
// through repositoryURL or an explicit alias. Hosts are never inferred.
func (g Git) Matches(source string) bool {
	return SameRepositoryAny(source, g.RepositoryURL, g.Aliases())
}

// Aliases returns repositoryAliases plus the selected repository's URL on
// every mirror host.
func (g Git) Aliases() []string {
	aliases := append([]string{}, g.RepositoryAliases...)
	repository, err := url.Parse(g.RepositoryURL)
	if err != nil {
		return aliases
	}
	for _, host := range g.MirrorHosts {
		if mirror, err := url.Parse(host); err == nil {
			aliases = append(aliases, (&url.URL{Scheme: mirror.Scheme, Host: mirror.Host, Path: repository.Path}).String())
		}
	}
	return aliases
}

// Unmirror maps a source URL on one of mirrorHosts to the same path on the
// repositoryURL host. Other sources are returned unchanged.
func Unmirror(source, repositoryURL string, mirrorHosts []string) string {
	u, err := url.Parse(source)
	base, baseErr := url.Parse(repositoryURL)
	if err != nil || baseErr != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return source
	}
	for _, host := range mirrorHosts {
		mirror, err := url.Parse(host)
		if err == nil && strings.EqualFold(mirror.Host, u.Host) {
			return (&url.URL{Scheme: base.Scheme, Host: base.Host, Path: u.Path}).String()
		}
	}
	return source
}

// SameRepositoryAny compares a source with a repository and its explicit aliases.
func SameRepositoryAny(source, repository string, aliases []string) bool {
	if SameRepository(source, repository) {
		return true
	}
	for _, alias := range aliases {
		if SameRepository(source, alias) {
			return true
		}
	}
	return false
}

// SamePathOtherHost returns source as https://host/path when it names the
// repository's path on another HTTPS host, such as a read-through mirror the
// profile does not declare. Paths compare case-insensitively, as on GitHub.
func SamePathOtherHost(source, repository string) (string, bool) {
	s, sok := gitRepositoryIdentity(source)
	r, rok := gitRepositoryIdentity(repository)
	if !sok || !rok || !strings.HasPrefix(source, "https://") || !strings.HasPrefix(repository, "https://") {
		return "", false
	}
	sHost, sPath, _ := strings.Cut(s, "/")
	rHost, rPath, _ := strings.Cut(r, "/")
	if sHost == rHost || !strings.EqualFold(sPath, rPath) {
		return "", false
	}
	return "https://" + sHost + "/" + sPath, true
}

// SameRepository compares source identity, never credentials or network endpoints.
// Standard HTTPS and git-user SSH URLs on the same host/path identify one Git
// repository. Custom ports and host aliases require explicit configuration.
func SameRepository(a, b string) bool {
	canonical := func(s string) string { return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git") }
	if canonical(a) == canonical(b) {
		return true
	}
	left, lok := gitRepositoryIdentity(a)
	right, rok := gitRepositoryIdentity(b)
	return lok && rok && left == right
}

func gitRepositoryIdentity(raw string) (string, bool) {
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		host, path, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if !ok || strings.ContainsAny(host, "/:@?# \\") {
			return "", false
		}
		raw = "ssh://git@" + host + "/" + path
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\\\r\n\t ") {
		return "", false
	}
	switch u.Scheme {
	case "https":
		if u.User != nil || (u.Port() != "" && u.Port() != "443") {
			return "", false
		}
	case "ssh":
		if u.User == nil || u.User.Username() != "git" || (u.Port() != "" && u.Port() != "22") {
			return "", false
		}
		if _, password := u.User.Password(); password {
			return "", false
		}
	default:
		return "", false
	}
	path := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, ":@%") {
			return "", false
		}
	}
	return strings.ToLower(u.Hostname()) + "/" + path, true
}
