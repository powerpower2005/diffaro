package profile

import (
	"fmt"
	"strings"
)

// HelmEnvVar sets one variable from a literal value or, for secrets, from a
// variable of the Diffaro process. Exactly one source is allowed.
type HelmEnvVar struct {
	Name         string `yaml:"name"`
	Value        string `yaml:"value,omitempty"`
	ValueFromEnv string `yaml:"valueFromEnv,omitempty"`
}

const MaxHelmEnvironment, MaxHelmEnvValueBytes = 16, 4096

// Diffaro owns these names (isolation, temporary directories, memory limit)
// or they change how the executable loads code, plugins or credentials.
var reservedHelmEnv = map[string]bool{"HOME": true, "USERPROFILE": true, "TMPDIR": true, "TMP": true, "TEMP": true, "PATH": true, "KUBECONFIG": true, "GOMEMLIMIT": true, "SYSTEMROOT": true, "WINDIR": true}
var reservedHelmEnvPrefixes = []string{"HELM_", "DIFFARO_", "LD_", "DYLD_"}

func validHelmEnvironment(vars []HelmEnvVar) bool {
	if len(vars) > MaxHelmEnvironment {
		return false
	}
	seen := map[string]bool{}
	for _, v := range vars {
		upper := strings.ToUpper(v.Name)
		if !envName.MatchString(v.Name) || reservedHelmEnv[upper] || seen[upper] {
			return false
		}
		for _, prefix := range reservedHelmEnvPrefixes {
			if strings.HasPrefix(upper, prefix) {
				return false
			}
		}
		seen[upper] = true
		if (v.Value == "") == (v.ValueFromEnv == "") || len(v.Value) > MaxHelmEnvValueBytes || strings.ContainsAny(v.Value, "\x00\r\n") || (v.ValueFromEnv != "" && !envName.MatchString(v.ValueFromEnv)) {
			return false
		}
	}
	return true
}

// ResolveHelmEnvironment returns NAME=value entries and the values read from
// the process environment, which callers must treat as secrets.
func (i Instance) ResolveHelmEnvironment(getenv func(string) string) (env, secrets []string, err error) {
	for _, v := range i.HelmEnvironment {
		value := v.Value
		if v.ValueFromEnv != "" {
			value = getenv(v.ValueFromEnv)
			if value == "" || len(value) > MaxHelmEnvValueBytes || strings.ContainsAny(value, "\x00\r\n") {
				return nil, secrets, fmt.Errorf("instance %s helmEnvironment %s: valueFromEnv %s is unset or invalid", i.ID, v.Name, v.ValueFromEnv)
			}
			secrets = append(secrets, value)
		}
		env = append(env, v.Name+"="+value)
	}
	return env, secrets, nil
}
