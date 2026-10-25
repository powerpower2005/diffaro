package domain

// Generator identifies the build and the profile shape that produced a
// report or trace, so a bug report names them without the profile itself. It holds no values, hosts, repositories or names.
type Generator struct {
	// Version is the release tag or "dev", with " (modified)" for a build
	// from modified sources.
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	Command   string `json:"command"`
	// Mode is the profile run mode selected with --mode.
	Mode    string       `json:"mode,omitempty"`
	Profile ProfileShape `json:"profile"`
}

// ProfileShape describes a profile by counts and the names of the settings
// and features it uses, never by their values.
type ProfileShape struct {
	Instances int `json:"instances"`
	// Backends lists the distinct render backend kinds of the instances.
	Backends []string `json:"backends"`
	// Selection is the repository discovery selection, absent without
	// discovery.
	Selection string `json:"selection,omitempty"`
	// Features names the optional settings in use, such as "mirrorHosts".
	Features []string `json:"features,omitempty"`
	// ChangedLimits names the render limits set to a non-default value.
	ChangedLimits []string `json:"changedLimits,omitempty"`
}
