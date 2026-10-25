package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

// writeSelectionReasons lists why changed-path selection rendered each
// candidate; unaffected candidates are counted above.
func writeSelectionReasons(b *strings.Builder, scope domain.DiscoveryResult) {
	shown := 0
	for _, decision := range scope.Decisions {
		if !decision.Selected || decision.SelectionReason == "" || decision.Selection == "unaffected" {
			continue
		}
		if shown == 10 {
			b.WriteString("  - More rendered candidates and reasons are in JSON.\n")
			return
		}
		fmt.Fprintf(b, "  - `%s`: %s (%s)\n", prText(decision.ID.String(), 180), prText(decision.Selection, 40), prText(decision.SelectionReason, 240))
		shown++
	}
}

func writeSelections(b *strings.Builder, report domain.Report, heading string) {
	if len(report.Discovery) > 0 {
		b.WriteString(heading + "\n\n")
		for _, scope := range report.Discovery {
			selected := 0
			for _, decision := range scope.Decisions {
				if decision.Selected {
					selected++
				}
			}
			fmt.Fprintf(b, "- %s: %s; %d readable apps, %d repository candidates", prText(scope.Instance, 180), prText(string(scope.Status), 40), len(scope.Decisions), selected)
			missing := 0
			for _, decision := range scope.Decisions {
				if decision.Selection == "source-missing" {
					missing++
				}
			}
			if scope.Unaffected > missing {
				fmt.Fprintf(b, "; %d unaffected and not rendered (changed-path selection)", scope.Unaffected-missing)
			}
			if missing > 0 {
				fmt.Fprintf(b, "; %d not rendered because their source path exists at neither revision", missing)
			}
			if scope.UnaffectedDescendants > 0 {
				fmt.Fprintf(b, "; %d unaffected descendants not rendered", scope.UnaffectedDescendants)
			}
			b.WriteString(".\n")
			writeSelectionReasons(b, scope)
		}
		b.WriteString("\nScope: readable inventory and rendered descendants. Reasons for unaffected and excluded apps are in JSON.\n\n")
	}
	if len(report.Selections) == 0 {
		return
	}
	b.WriteString("**ApplicationSet selection:** current readable generated apps only; base/head membership and template changes are not evaluated.\n\n")
	for i, selection := range report.Selections {
		if i == 10 {
			b.WriteString("More selection sources are in JSON.\n\n")
			break
		}
		fmt.Fprintf(b, "- `%s`: %d selected app(s)\n", prText(selection.Source.String(), 180), len(selection.Applications))
	}
	b.WriteString("\n")
}
