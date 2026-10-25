package preview

import (
	"slices"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/security"
)

// maskTargetValues returns a copy of value file key changes in which every
// string that redaction masked in a rendered target, or that appears inside
// one, is masked, so a sensitive value reached through an ordinary key name
// is not shown.
func maskTargetValues(changes []domain.Change, masked *security.MaskedValues) []domain.Change {
	out := append([]domain.Change{}, changes...)
	for i := range out {
		var before, after bool
		out[i].Before, before = masked.MaskIn(out[i].Before)
		out[i].After, after = masked.MaskIn(out[i].After)
		out[i].Redacted = out[i].Redacted || before || after
	}
	return out
}

// MaskRepeatedValues masks, after every comparison of the run, each string
// that redaction masked in a rendered target and that reappears in another
// resource's changes or line hunks, or in an Application's spec changes,
// with the rule of maskTargetValues: equal, or from 8 bytes contained in a
// masked string. A value masked anywhere then stays masked everywhere, such
// as a Secret payload copied into a ConfigMap. Only
// credentials count: Secret payloads and configured sensitive paths. Values
// masked by key name, which often name a reference, and Helm inline inputs,
// which hold what a child renders such as its image tag, are left out.
func MaskRepeatedValues(report *domain.Report, masked *security.MaskedValues) {
	masked = masked.Credentials()
	if masked.Empty() {
		return
	}
	for i := range report.Applications {
		app := &report.Applications[i]
		if changes, hit := maskChanges(app.SpecChanges, masked); hit {
			app.SpecChanges = changes
		}
		for j := range app.Resources {
			resource := &app.Resources[j]
			if changes, hit := maskChanges(resource.Changes, masked); hit {
				resource.Changes = changes
			}
			if hunks, hit := manifest.MaskHunkLines(resource.LineHunks, masked); hit {
				resource.LineHunks = hunks
			}
		}
	}
}

// maskChanges returns a copy of changes with the values masked hides masked
// and marked redacted, and whether it masked any; changes itself when not.
func maskChanges(changes []domain.Change, masked *security.MaskedValues) ([]domain.Change, bool) {
	var out []domain.Change
	for i, change := range changes {
		before, b := masked.MaskIn(change.Before)
		after, a := masked.MaskIn(change.After)
		if !b && !a {
			continue
		}
		if out == nil {
			out = slices.Clone(changes)
		}
		out[i].Before, out[i].After, out[i].Redacted = before, after, true
	}
	if out == nil {
		return changes, false
	}
	return out, true
}
