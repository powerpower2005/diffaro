package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"diffaro/internal/profile"
)

// applyHelmOverrides applies --local-helm and --helm-binary to every instance
// and rejects combinations the profile cannot express.
func applyHelmOverrides(c *profile.Config, o *commandOptions) error {
	var err error
	if o.helmBinary, o.localHelm, err = selectHelmBinary(o.helmBinary, o.localHelm); err != nil {
		return err
	}
	if o.localHelm && !selectLocalHelm(c.Instances, o.helmBinary) {
		return fmt.Errorf("--local-helm cannot be combined with configured repoServer; remove the instance repoServer configuration")
	}
	for _, instance := range c.Instances {
		if len(instance.HelmEnvironment) > 0 && instance.HelmBinary == "" {
			return fmt.Errorf("instance %s helmEnvironment requires an external Helm executable; set helmBinary or --helm-binary", instance.ID)
		}
	}
	return nil
}

// selectHelmBinary resolves a --helm-binary value to an absolute path; a
// selected executable also selects local Helm.
func selectHelmBinary(helmBinary string, localHelm bool) (string, bool, error) {
	if helmBinary == "" {
		return "", localHelm, nil
	}
	resolved, err := resolveHelmBinary(helmBinary)
	if err != nil {
		return helmBinary, localHelm, err
	}
	return resolved, true, nil
}

// resolveHelmBinary turns a --helm-binary value into an absolute path.
func resolveHelmBinary(value string) (string, error) {
	resolved, err := filepath.Abs(value)
	if err != nil || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("invalid Helm executable path")
	}
	return resolved, nil
}

// selectLocalHelm enables local Helm (and an optional external executable) on
// every instance. It returns false, after possibly changing earlier instances,
// when an instance configures repoServer; callers must then stop.
func selectLocalHelm(instances []profile.Instance, helmBinary string) bool {
	for i := range instances {
		if instances[i].RepoServer != nil {
			return false
		}
		instances[i].LocalHelm = true
		if helmBinary != "" {
			instances[i].HelmBinary = helmBinary
		}
	}
	return true
}
