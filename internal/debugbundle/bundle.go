// Package debugbundle builds the opt-in debug bundle of diff --debug-bundle
// : one gzip-compressed tar archive with the structure a bug
// report needs. The public level keeps codes, stages, diagnostics, counts
// and trace steps, drops free text, values and digests, and replaces every
// name with a per-bundle keyed pseudonym; the mapping back to names is
// returned separately so it never travels with the bundle. The private level
// adds the redacted report and trace without pseudonyms.
package debugbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"diffaro/internal/domain"
)

// Schema versions the bundle summary and the names file.
const Schema = "diffaro.bundle/v1alpha1"

// Bundle levels: public bundles may be attached to public issues; private
// bundles keep names and the redacted report for a trusted channel.
const (
	LevelPublic  = "public"
	LevelPrivate = "private"
)

// Levels lists the accepted --debug-bundle-level values.
var Levels = []string{LevelPublic, LevelPrivate}

// Input is everything one bundle is built from.
type Input struct {
	Level    string
	Report   domain.Report
	ExitCode int
	// Flags names the command-line options given, never their values.
	Flags []string
	// Steps are the run's trace steps with subjects and reasons already
	// redacted like the trace file; Dropped counts steps past its bound.
	Steps   []domain.TraceStep
	Dropped int
	// ReportJSON and TraceJSON are the redacted report and trace files,
	// added to private bundles only.
	ReportJSON, TraceJSON []byte
	// Key keys the pseudonyms; a random key is drawn when it is empty.
	Key []byte
}

// Output is the bundle archive and, for a public bundle, the names file
// that maps its pseudonyms back to the operator's names.
type Output struct {
	Bundle []byte
	Names  []byte
}

// Build assembles the bundle of in.
func Build(in Input) (Output, error) {
	if in.Level != LevelPublic && in.Level != LevelPrivate {
		return Output{}, errors.New("debug bundle level must be public or private")
	}
	n := &namer{public: in.Level == LevelPublic, key: in.Key, names: map[string]string{}}
	if n.public && len(n.key) == 0 {
		n.key = make([]byte, 32)
		if _, err := rand.Read(n.key); err != nil {
			return Output{}, err
		}
	}
	summary, err := json.MarshalIndent(summarize(in, n), "", "  ")
	if err != nil {
		return Output{}, err
	}
	files := []file{{"README.md", []byte(readme(in.Level))}, {"bundle.json", append(summary, '\n')}}
	if !n.public {
		if len(in.ReportJSON) > 0 {
			files = append(files, file{"report.json", in.ReportJSON})
		}
		if len(in.TraceJSON) > 0 {
			files = append(files, file{"trace.json", in.TraceJSON})
		}
	}
	archive, err := tarball(files)
	if err != nil {
		return Output{}, err
	}
	out := Output{Bundle: archive}
	if n.public {
		names, err := json.MarshalIndent(struct {
			Schema string            `json:"schema"`
			Note   string            `json:"note"`
			Names  map[string]string `json:"names"`
		}{Schema, "Keep this file private. It maps the pseudonyms in the debug bundle to your names, so you can answer maintainers' questions; do not attach it.", n.names}, "", "  ")
		if err != nil {
			return Output{}, err
		}
		out.Names = append(names, '\n')
	}
	return out, nil
}

// namer replaces names with keyed pseudonyms in a public bundle and keeps
// them in a private one.
type namer struct {
	public bool
	key    []byte
	names  map[string]string
}

// name returns the pseudonym of value as a name of kind, such as
// "app-1f3a9c0b2e", the same for the same kind and value within a bundle.
func (n *namer) name(kind, value string) string {
	if value == "" || !n.public {
		return value
	}
	mac := hmac.New(sha256.New, n.key)
	mac.Write([]byte(kind + "\x00" + value))
	pseudonym := kind + "-" + hex.EncodeToString(mac.Sum(nil))[:10]
	n.names[pseudonym] = value
	return pseudonym
}

func (n *namer) id(id domain.ApplicationID) *domain.ApplicationID {
	return &domain.ApplicationID{Instance: n.name("instance", id.Instance), Namespace: n.name("namespace", id.Namespace), Name: n.name("app", id.Name)}
}

type file struct {
	name string
	data []byte
}

// tarball writes files into a gzip-compressed tar archive with fixed
// metadata, so equal content gives an equal archive.
func tarball(files []file) ([]byte, error) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		header := &tar.Header{Name: "diffaro-debug-bundle/" + f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
