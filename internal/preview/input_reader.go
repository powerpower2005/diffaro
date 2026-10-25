package preview

import (
	"context"
	"errors"
	"strconv"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/security"
)

type inputResult struct {
	state      domain.State
	comparison domain.Comparison
	reason     string
	changes    []domain.Change
	exists     bool
}

// inputReader reads each changed value file once per run.
type inputReader struct {
	files      *InputFiles
	pair       domain.RevisionPair
	redactor   security.Redactor
	repository string
	cache      map[string]inputResult
	reads      int
	// charts caches chart metadata per revision across Applications.
	charts map[string]*changedPathSelector
	// head, when set, is the comparison's changed-path selection: head
	// chart metadata it read is reused, and head files outside its tree
	// listing are not read.
	head       *changedPathSelector
	chartReads int
	timeout    time.Duration
}

// read bounds one provider read by the per-stage timeout.
func (c *inputReader) read(ctx context.Context, revision, file string) ([]byte, bool, error) {
	stage, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.files.Read(stage, revision, file)
}

// dependencies collects the local chart dependency directories of root at
// revision, reusing changed-path selection's resolution.
func (c *inputReader) dependencies(ctx context.Context, revision, root string, dirs *[]string) string {
	selector := c.charts[revision]
	if selector == nil {
		paths := ChangedPaths{Read: func(ctx context.Context, file string) ([]byte, bool, error) {
			if c.chartReads == maxChartReads {
				return nil, false, errors.New("chart metadata read limit reached")
			}
			c.chartReads++
			return c.read(ctx, revision, file)
		}}
		var prior *changedPathSelector
		if c.head != nil && revision == c.pair.Head {
			prior, paths.Present = c.head, c.head.paths.Present
		}
		selector = newChangedPathSelector(paths, nil)
		selector.prior = prior
		c.charts[revision] = selector
	}
	return selector.dependencies(ctx, root, root, 0, dirs, false)
}

// valueFile compares a value file at the pair revisions. ok is false when the
// file exists at neither revision.
func (c *inputReader) valueFile(ctx context.Context, file string) (inputResult, bool) {
	if result, ok := c.cache[file]; ok {
		return result, result.exists
	}
	result := c.compare(ctx, file)
	if ctx.Err() == nil {
		c.cache[file] = result
	}
	return result, result.exists
}

func (c *inputReader) compare(ctx context.Context, file string) inputResult {
	incomplete := func(reason string) inputResult {
		return inputResult{state: domain.StateChanged, comparison: domain.ComparisonIncomplete, reason: reason, exists: true}
	}
	if c.reads+2 > maxInputFileReads {
		return incomplete("input file read limit (" + strconv.Itoa(maxInputFileReads) + " reads per run) reached")
	}
	c.reads += 2
	documents := [2]map[string]any{}
	found := [2]bool{}
	for i, revision := range []string{c.pair.Base, c.pair.Head} {
		data, ok, err := c.read(ctx, revision, file)
		if err != nil {
			return incomplete("the file could not be read at " + revision)
		}
		if !ok {
			documents[i] = map[string]any{}
			continue
		}
		found[i] = true
		if len(data) > maxInputValueBytes {
			return incomplete("the file exceeds " + strconv.Itoa(maxInputValueBytes) + " bytes")
		}
		values, err := manifest.ParseValues(data)
		if err != nil {
			return incomplete(err.Error())
		}
		documents[i] = values
	}
	result := inputResult{state: domain.StateChanged, comparison: domain.ComparisonComplete, exists: found[0] || found[1]}
	if !found[0] {
		result.state = domain.StateAdded
	} else if !found[1] {
		result.state = domain.StateRemoved
	}
	var truncated bool
	if result.changes, truncated = valueChanges("", documents, c.redactor.ForValueFiles()); truncated {
		result.comparison, result.reason = domain.ComparisonIncomplete, "more than "+strconv.Itoa(maxInputKeyChanges)+" key changes; see the PR diff for the rest"
	}
	return result
}
