package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

func Load(filename string) (Config, error) {
	b, err := readBounded(filename, "configuration", "configuration")
	if err != nil {
		return Config{}, err
	}
	c, err := Parse(b)
	if err != nil {
		return c, err
	}
	if err := resolveInstancePaths(c.Instances, filepath.Dir(filename)); err != nil {
		return c, err
	}
	return c, nil
}

func resolveInstancePaths(instances []Instance, baseDir string) error {
	for i := range instances {
		if p := instances[i].HelmBinary; p != "" {
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			resolved, err := filepath.Abs(p)
			if err != nil {
				return fmt.Errorf("cannot resolve Helm executable path")
			}
			instances[i].HelmBinary = resolved
		}
		rpc := instances[i].RepoServer
		if rpc == nil {
			continue
		}
		paths := []*string{&rpc.GRPCURL, &rpc.ContextFile, &rpc.CAFile}
		if rpc.Kubernetes != nil {
			paths = append(paths, &rpc.Kubernetes.Kubectl, &rpc.Kubernetes.Kubeconfig)
		}
		for _, p := range paths {
			if *p != "" && !filepath.IsAbs(*p) {
				*p = filepath.Join(baseDir, *p)
			}
			if *p != "" {
				resolved, e := filepath.Abs(*p)
				if e != nil {
					return fmt.Errorf("cannot resolve repo-server configuration path")
				}
				*p = resolved
			}
		}
	}
	return nil
}

func Parse(data []byte) (Config, error) {
	c := Config{Render: DefaultRender()}
	if len(data) > MaxConfigBytes {
		return c, fmt.Errorf("configuration exceeds size limit")
	}
	if err := decodeStrict(data, &c, "configuration"); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// readBounded reads the configuration or connections file up to
// MaxConfigBytes; subject starts the size message. An open failure keeps its
// cause, such as a missing file or denied permission, which names only the
// path.
func readBounded(filename, name, subject string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s file: %w", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil || len(b) > MaxConfigBytes {
		return nil, fmt.Errorf("%s exceeds size limit or cannot be read", subject)
	}
	return b, nil
}

// decodeStrict decodes exactly one YAML document with known fields and
// unique keys into out.
func decodeStrict(data []byte, out any, name string) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(out); err != nil {
		message := name + " must be valid YAML with known fields and unique keys"
		if problems := yamlProblems(err); len(problems) > 0 {
			message += ": " + strings.Join(problems, "; ")
		}
		return errors.New(message)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("%s must contain one document", name)
	}
	return nil
}
