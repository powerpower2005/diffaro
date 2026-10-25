package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

type Issue struct {
	SourcePosition int                `json:"sourcePosition,omitempty"`
	Side           string             `json:"side,omitempty"`
	Code           string             `json:"code"`
	ReasonCode     string             `json:"reasonCode,omitempty"`
	Stage          string             `json:"stage"`
	Reason         string             `json:"reason"`
	Action         string             `json:"action"`
	Repository     string             `json:"repository,omitempty"`
	Revision       string             `json:"revision,omitempty"`
	Application    *ApplicationID     `json:"application,omitempty"`
	RequiredAccess *AccessRequirement `json:"requiredAccess,omitempty"`
	// Diagnostic classifies the failure without its raw text: where it was
	// created, its cause chain and provider response, and the
	// stack of a recovered panic.
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
}

// DedupeKey identifies a distinct issue in human-readable report sections.
// Every field participates: location (side, Application, source position,
// repository, revision), classification (code, reason code, stage) and
// remediation (reason, action, required access) each change what a reader
// must fix, so collapsing issues that differ in any of them would hide an
// actionable failure. Fields added later participate automatically.
func (i Issue) DedupeKey() string {
	key, _ := json.Marshal(i)
	return string(key)
}

// GroupIssues merges issues that differ only in side and revision, such as one
// render failure at base and head, for human-readable sections. A merged
// issue lists its sides and revisions comma-separated in first-occurrence
// order. Identical issues collapse into one; JSON keeps every issue.
func GroupIssues(issues []Issue) []Issue {
	var out []Issue
	index := map[string]int{}
	for _, issue := range issues {
		key := issue
		key.Side, key.Revision = "", ""
		dedupe := key.DedupeKey()
		i, ok := index[dedupe]
		if !ok {
			index[dedupe] = len(out)
			out = append(out, issue)
			continue
		}
		out[i].Side = appendListed(out[i].Side, issue.Side)
		out[i].Revision = appendListed(out[i].Revision, issue.Revision)
	}
	return out
}

func appendListed(list, value string) string {
	if value == "" || slices.Contains(strings.Split(list, ", "), value) {
		return list
	}
	if list == "" {
		return value
	}
	return list + ", " + value
}

// AccessRequirement describes the request's required access, not a verified
// missing grant. CredentialEnv identifies a configuration entry, never a value.
// Object is omitted when the exact policy object cannot be resolved safely.
type AccessRequirement struct {
	System        string `json:"system"`
	Instance      string `json:"instance,omitempty"`
	CredentialEnv string `json:"credentialEnv,omitempty"`
	Resource      string `json:"resource,omitempty"`
	Action        string `json:"action"`
	Target        string `json:"target"`
	Object        string `json:"object,omitempty"`
	Scope         string `json:"scope,omitempty"`
}

// RenderPartial accompanies usable targets whose coverage is incomplete.
// Callers must preserve the failure and must not infer absence from missing targets.
type RenderPartial struct {
	Cause  error
	Issues []Issue
}

func (e *RenderPartial) Error() string { return e.Cause.Error() }
func (e *RenderPartial) Unwrap() error { return e.Cause }
func IsRenderPartial(err error) bool   { var p *RenderPartial; return errors.As(err, &p) }

// RenderIssues preserves per-source failures without duplicating wrapper issues.
func RenderIssues(err error) []Issue {
	var p *RenderPartial
	if errors.As(err, &p) && len(p.Issues) > 0 {
		return append([]Issue(nil), p.Issues...)
	}
	var f *Failure
	if errors.As(err, &f) {
		return []Issue{f.Issue}
	}
	return nil
}
func PartialRender(issues []Issue) error {
	if len(issues) == 0 {
		return nil
	}
	return &RenderPartial{Cause: &Failure{Issue: issues[0]}, Issues: append([]Issue(nil), issues...)}
}

type Failure struct {
	Issue Issue
	Cause error
}

func (e *Failure) Error() string { return e.Issue.Code + ": " + e.Issue.Reason }
func (e *Failure) Unwrap() error { return e.Cause }
func Fail(code, stage, reason, action string, cause error) error {
	return &Failure{Issue: Issue{Code: code, Stage: stage, Reason: reason, Action: action, Diagnostic: located(1, cause)}, Cause: cause}
}

// FailReason is Fail with a reason code that names the cause within code,
// such as chart_path_missing within local_render_failed.
func FailReason(code, reasonCode, stage, reason, action string, cause error) error {
	return &Failure{Issue: Issue{Code: code, ReasonCode: reasonCode, Stage: stage, Reason: reason, Action: action, Diagnostic: located(1, cause)}, Cause: cause}
}
