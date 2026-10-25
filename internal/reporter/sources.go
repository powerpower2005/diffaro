package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

func writeSourceContext(b *strings.Builder, app domain.ApplicationResult, compact bool) {
	text := esc
	limit := 32
	if compact {
		text = func(s string) string { return prText(s, 160) }
		limit = 8
	}
	for _, side := range []struct {
		name     string
		sources  []domain.SourceSummary
		repeated []domain.ResourceID
		target   *domain.TargetSummary
	}{
		{"base", app.BaseSources, app.BaseRepeatedResources, app.BaseTarget},
		{"head", app.HeadSources, app.HeadRepeatedResources, app.HeadTarget},
	} {
		if side.target != nil {
			side.sources, side.repeated = side.target.Sources, side.target.RepeatedResources
		}
		if len(side.sources) == 0 && len(side.repeated) == 0 {
			continue
		}
		if len(side.sources) > 0 {
			fmt.Fprintf(b, "\n%s sources for `%s` (in order):\n\n", side.name, text(app.ID.String()))
		} else {
			fmt.Fprintf(b, "\n%s target for `%s`:\n", side.name, text(app.ID.String()))
		}
		for i, source := range side.sources {
			if i >= limit {
				fmt.Fprintf(b, "- %d more sources; see JSON.\n", len(side.sources)-i)
				break
			}
			fmt.Fprintf(b, "- %d. `%s` @ `%s` · rule `%s`", source.Position, text(source.Repository), text(source.Revision), text(source.Rule))
			if source.Ref != "" {
				fmt.Fprintf(b, " · ref `%s`", text(source.Ref))
			}
			fmt.Fprintf(b, "\n  - Path `%s` · release `%s` · values files `%s`\n", text(source.Inputs.Path), text(source.Inputs.ReleaseName), text(strings.Join(source.Inputs.ValueFiles, ", ")))
			if !compact {
				fmt.Fprintf(b, "  - Origin `%s` · parameters `%s` · file parameters `%s` · inline values `%t` · valuesObject `%t`\n", text(source.Inputs.Origin), text(strings.Join(source.Inputs.Parameters, ", ")), text(strings.Join(source.Inputs.FileParameters, ", ")), source.Inputs.InlineValues, source.Inputs.ValuesObject)
			}
		}
		if len(side.repeated) > 0 {
			fmt.Fprintf(b, "\n**Repeated resources: %d.** The last rendered occurrence wins in source order.\n\n", len(side.repeated))
			for i, id := range side.repeated {
				if i >= limit {
					fmt.Fprintf(b, "- %d more repeated resources; see JSON.\n", len(side.repeated)-i)
					break
				}
				fmt.Fprintf(b, "- `%s`\n", text(id.String()))
			}
		}
		b.WriteString("\n")
	}
}
