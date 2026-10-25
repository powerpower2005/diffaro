package reporter

import (
	"fmt"
	"sort"
	"strings"

	"diffaro/internal/domain"
)

// causeTitles name the reason codes a reviewer acts on.
var causeTitles = map[string]string{
	domain.ReasonChartPathMissing:       "Chart path does not exist",
	domain.ReasonChartMetadataMissing:   "Chart.yaml is missing",
	domain.ReasonValueFileMissing:       "Value file does not exist",
	domain.ReasonTemplateFailed:         "Helm template failed",
	domain.ReasonParameterInvalid:       "Helm parameter name is invalid",
	domain.ReasonRenderedOutputInvalid:  "Rendered output is invalid",
	domain.ReasonInputInvalid:           "Helm input is invalid",
	domain.ReasonRevisionMissing:        "Git revision does not exist",
	domain.ReasonServerError:            "Backend server error",
	domain.ReasonUnexpectedStatus:       "Unexpected backend response",
	domain.ReasonCredentialMissing:      "Credential is missing",
	domain.ReasonAuthenticationRejected: "Authentication was rejected",
	domain.ReasonRequestForbidden:       "Request was forbidden",
	domain.ReasonTimeout:                "Timed out",
	domain.ReasonDNSFailed:              "Host name could not be resolved",
	domain.ReasonTLSUntrusted:           "TLS certificate was not accepted",
	domain.ReasonConnectionRefused:      "Connection was refused",
	domain.ReasonConnectionClosed:       "Connection was closed",
	domain.ReasonRedirected:             "Server redirected the request",
}

// originLabels explain what comparing base and head shows about a failure.
var originLabels = map[domain.FailureOrigin]string{
	domain.FailureIntroduced:    "introduced by this PR: base renders, head fails",
	domain.FailurePreexisting:   "already failing before this PR: base fails the same way and this PR does not change the Application's inputs",
	domain.FailureInputsChanged: "already failing, inputs changed by this PR: base fails the same way and this PR changes inputs the Application reads; whether it fixes the failure cannot be seen",
	domain.FailureBothSides:     "fails the same way at base and head; whether this PR changes its inputs cannot be determined",
	domain.FailureResolved:      "fixed by this PR: base fails, head renders",
	domain.FailureTransient:     "may succeed on retry",
}

// FailureOriginLabel explains a failure origin for console diagnostics; it is
// empty for an unknown or unset origin.
func FailureOriginLabel(origin domain.FailureOrigin) string { return originLabels[origin] }

// originOrder lists failures a reviewer must act on first.
var originOrder = map[domain.FailureOrigin]int{domain.FailureIntroduced: 0, domain.FailureTransient: 1, "": 2, domain.FailureInputsChanged: 2, domain.FailureBothSides: 2, domain.FailureResolved: 3, domain.FailurePreexisting: 4}

func causeTitle(issue domain.Issue) string {
	if title := causeTitles[issue.ReasonCode]; title != "" {
		return title
	}
	title := strings.ReplaceAll(issue.Code, "_", " ")
	if title == "" {
		return "Issue"
	}
	return strings.ToUpper(title[:1]) + title[1:]
}

type errorGroup struct {
	origin  domain.FailureOrigin
	issue   domain.Issue
	members []errorMember
}

type errorMember struct {
	app, sides, reason, revision string
	appScoped                    bool
}

// errorGroups merges failures with the same cause, action and origin across
// Applications, so one cause reads once with every Application it affects.
// issues are the report's merged issues (collectIssues).
func errorGroups(report domain.Report, issues []domain.Issue) []*errorGroup {
	origins := map[domain.ApplicationID]domain.FailureOrigin{}
	for _, app := range report.Applications {
		origins[app.ID] = app.FailureOrigin
	}
	var groups []*errorGroup
	index := map[string]*errorGroup{}
	for _, issue := range issues {
		var origin domain.FailureOrigin
		app := ""
		if issue.Application != nil {
			origin, app = origins[*issue.Application], issue.Application.String()
		}
		access := ""
		if issue.RequiredAccess != nil {
			access = issue.RequiredAccess.System + "\x00" + issue.RequiredAccess.Action + "\x00" + issue.RequiredAccess.Target
		}
		key := strings.Join([]string{issue.Code, issue.ReasonCode, issue.Stage, issue.Action, access, string(origin)}, "\x00")
		group := index[key]
		if group == nil {
			group = &errorGroup{origin: origin, issue: issue}
			index[key] = group
			groups = append(groups, group)
		}
		group.members = append(group.members, errorMember{app: app, sides: issue.Side, reason: issue.Reason, revision: issue.Revision, appScoped: issue.Application != nil})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if originOrder[groups[i].origin] != originOrder[groups[j].origin] {
			return originOrder[groups[i].origin] < originOrder[groups[j].origin]
		}
		return groups[i].issue.Code < groups[j].issue.Code
	})
	return groups
}

// failedApps returns the Applications that failure causes name.
func failedApps(groups []*errorGroup) map[string]bool {
	apps := map[string]bool{}
	for _, group := range groups {
		for _, member := range group.members {
			if member.appScoped {
				apps[member.app] = true
			}
		}
	}
	return apps
}

// requestedRevisions maps an Application ID and a target revision resolved
// at run time to the branch, tag or HEAD it was resolved from. A failure at
// such a revision can come from that reference moving after the PR.
func requestedRevisions(report domain.Report) map[string]string {
	out := map[string]string{}
	for _, app := range report.Applications {
		for _, target := range []*domain.TargetSummary{app.BaseTarget, app.HeadTarget} {
			if target != nil && target.RequestedRevision != "" {
				out[app.ID.String()+"\x00"+target.Revision] = target.RequestedRevision
			}
		}
	}
	return out
}

// writeErrors states each failure cause (reportView.errors) with where it
// occurred, why, what to do and what it means for the rest of the report.
// With heading, the section starts with it and its cause and Application
// counts.
func writeErrors(b *strings.Builder, v *reportView, heading bool) {
	report, groups, apps := v.report, v.errors, v.failedApps
	if len(groups) == 0 {
		return
	}
	requested := requestedRevisions(report)
	if heading {
		fmt.Fprintf(b, "## Errors (%d cause(s), %d Application(s))\n\n", len(groups), len(apps))
	}
	for i, group := range groups {
		if i == 12 {
			fmt.Fprintf(b, "%d more cause(s) are in JSON or the full Markdown report.\n\n", len(groups)-i)
			break
		}
		fmt.Fprintf(b, "### %s (`%s`", prText(causeTitle(group.issue), 80), prText(group.issue.Code, 60))
		if group.issue.ReasonCode != "" {
			fmt.Fprintf(b, " · `%s`", prText(group.issue.ReasonCode, 60))
		}
		b.WriteString(")\n\n")
		if label := originLabels[group.origin]; label != "" {
			fmt.Fprintf(b, "- **Origin:** %s\n", prText(label, 240))
		}
		sameReason := true
		for _, member := range group.members {
			sameReason = sameReason && member.reason == group.members[0].reason
		}
		for j, member := range group.members {
			if j == 10 {
				fmt.Fprintf(b, "- %d more Application(s) in JSON.\n", len(group.members)-j)
				break
			}
			where := "report"
			if member.appScoped {
				where = "`" + prText(member.app, 180) + "`"
			}
			fmt.Fprintf(b, "- %s · stage `%s`", where, prText(group.issue.Stage, 40))
			if member.sides != "" {
				fmt.Fprintf(b, " · %s", prText(member.sides, 20))
			}
			// Revisions of the report's base and head are in its header.
			if member.revision != "" && member.revision != report.Revisions.Base && member.revision != report.Revisions.Head && member.revision != report.Revisions.Base+", "+report.Revisions.Head {
				fmt.Fprintf(b, " · revision `%s`", prText(member.revision, 200))
				b.WriteString(resolvedSuffix(requested[member.app+"\x00"+member.revision], func(s string) string { return "`" + prText(s, 120) + "`" }))
			}
			if !sameReason {
				fmt.Fprintf(b, ": %s", prText(member.reason, 240))
			}
			b.WriteString("\n")
		}
		if sameReason {
			fmt.Fprintf(b, "- **Why:** %s\n", prText(group.members[0].reason, 240))
		}
		fmt.Fprintf(b, "- **Action:** %s\n", prText(group.issue.Action, 240))
		if fields := defectFields(group.issue); len(fields) > 0 {
			fmt.Fprintf(b, "- **Diagnostic:** %s\n", strings.Join(mapStrings(fields, func(s string) string { return "`" + prText(s, 240) + "`" }), " · "))
		}
		writePRAccess(b, group.issue.RequiredAccess)
		if len(apps) > 0 && group.members[0].appScoped {
			switch group.origin {
			case domain.FailureResolved:
				b.WriteString("- **Effect:** head targets are available, but base targets are unknown, so their changes cannot be compared.\n")
			default:
				b.WriteString("- **Effect:** resource changes of these Applications are unknown, not unchanged.\n")
			}
		}
		b.WriteString("\n")
	}
	if len(apps) > 0 {
		b.WriteString("Results of other Applications remain valid. Graph discovery does not cover the failed Applications.\n\n")
	}
}

// defectFields lists, for an internal_error, the diagnostic a bug report
// needs: where the failure was created or the innermost panicking frame, and
// the cause chain. Other codes name their cause and need none.
func defectFields(issue domain.Issue) []string {
	d := issue.Diagnostic
	if issue.Code != domain.CodeInternalError || d == nil {
		return nil
	}
	var fields []string
	if d.Site != "" {
		fields = append(fields, "site "+d.Site)
	}
	if len(d.Stack) > 0 {
		fields = append(fields, fmt.Sprintf("panic at %s (%d frame(s) in JSON)", d.Stack[0], len(d.Stack)))
	}
	if len(d.Chain) > 0 {
		fields = append(fields, "cause "+strings.Join(d.Chain, " > "))
	}
	return fields
}

func mapStrings(values []string, fn func(string) string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = fn(value)
	}
	return out
}
