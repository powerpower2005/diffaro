package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"diffaro/internal/profile"
	"go.yaml.in/yaml/v3"
)

// stringList collects the values of a repeatable flag.
type stringList []string

func (v *stringList) String() string         { return "" }
func (v *stringList) Set(value string) error { *v = append(*v, value); return nil }

type credentialOptions struct {
	git, argo *string
	instances stringList
}

func bindCredentials(f *flag.FlagSet) *credentialOptions {
	o := &credentialOptions{}
	o.git = f.String("git-token", "", "GitHub token; overrides config and tokenEnv")
	o.argo = f.String("argocd-token", "", "Argo CD token for a single instance; overrides config and tokenEnv")
	f.Var(&o.instances, "instance-token", "repeatable ID=TOKEN; overrides credentials for that Argo CD instance")
	return o
}

// resolve modifies only the in-memory configuration. All candidate
// credentials, including overridden values, remain available for redaction.
func (o *credentialOptions) resolve(f *flag.FlagSet, c *profile.Config) ([]string, error) {
	present := map[string]bool{}
	f.Visit(func(v *flag.Flag) { present[v.Name] = true })
	secrets := []string{c.Git.Token, os.Getenv(c.Git.TokenEnv), *o.git, *o.argo}
	selected := map[string]string{}
	for _, value := range o.instances {
		id, token, ok := strings.Cut(value, "=")
		secrets = append(secrets, token)
		if !ok || id == "" || token == "" || !profile.ValidToken(token) {
			return secrets, fmt.Errorf("instance-token requires ID=TOKEN with a non-empty valid token")
		}
		if _, exists := selected[id]; exists {
			return secrets, fmt.Errorf("duplicate instance-token; provide each instance once")
		}
		selected[id] = token
	}
	if present["git-token"] && (*o.git == "" || !profile.ValidToken(*o.git)) {
		return secrets, fmt.Errorf("git-token must be a non-empty valid token")
	}
	if present["argocd-token"] {
		if len(c.Instances) != 1 {
			return secrets, fmt.Errorf("argocd-token requires exactly one instance; use --instance-token ID=TOKEN")
		}
		if *o.argo == "" || !profile.ValidToken(*o.argo) {
			return secrets, fmt.Errorf("argocd-token must be a non-empty valid token")
		}
		if _, exists := selected[c.Instances[0].ID]; exists {
			return secrets, fmt.Errorf("choose argocd-token or instance-token for the same instance")
		}
		selected[c.Instances[0].ID] = *o.argo
	}
	choose := func(configured, envName, override string, explicit bool) (string, string, error) {
		token := configured
		if explicit {
			token = override
		} else if token == "" {
			token = os.Getenv(envName)
		}
		if !profile.ValidToken(token) {
			return "", "", fmt.Errorf("credential must be a valid bounded bearer token; check flags, config and tokenEnv")
		}
		if explicit || configured != "" {
			envName = ""
		}
		return token, envName, nil
	}
	var err error
	c.Git.Token, c.Git.TokenEnv, err = choose(c.Git.Token, c.Git.TokenEnv, *o.git, present["git-token"])
	if err != nil {
		return secrets, err
	}
	for i := range c.Instances {
		instance := &c.Instances[i]
		secrets = append(secrets, instance.Token, os.Getenv(instance.TokenEnv))
		override, explicit := selected[instance.ID]
		instance.Token, instance.TokenEnv, err = choose(instance.Token, instance.TokenEnv, override, explicit)
		if err != nil {
			return secrets, err
		}
		delete(selected, instance.ID)
	}
	if len(selected) != 0 {
		return secrets, fmt.Errorf("instance-token identifies an unconfigured instance; check the profile instance IDs")
	}
	return secrets, nil
}

// persist retains tokenEnv fallback names while saving effective
// credentials. It never copies Application render values into the profile.
// It validates c and returns the profile file and the saved configuration.
func (o *credentialOptions) persist(f *flag.FlagSet, c profile.Config) ([]byte, profile.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, c, err
	}
	original := c
	c.Instances = append([]profile.Instance(nil), c.Instances...)
	if _, err := o.resolve(f, &c); err != nil {
		return nil, c, err
	}
	c.Git.TokenEnv = original.Git.TokenEnv
	for i := range c.Instances {
		c.Instances[i].TokenEnv = original.Instances[i].TokenEnv
	}
	data, err := yaml.Marshal(c)
	return data, c, err
}
