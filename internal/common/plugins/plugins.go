package plugins

import (
	"fmt"
	"sort"
	"strings"

	"github.com/c5c3/forge/internal/common/types"
)

// RenderPastePipeline generates a PasteDeploy api-paste.ini configuration from a PipelineSpec.
// It renders the [pipeline:name] section with middleware inserted at specified positions,
// followed by [filter:name] blocks for each middleware and filter. (CC-0004, REQ-009)
func RenderPastePipeline(spec types.PipelineSpec) string {
	var sections []string

	// Build the pipeline string by inserting middleware into the base pipeline.
	pipeline := insertMiddleware(spec.BasePipeline, spec.Middleware)

	// Render the pipeline section.
	sections = append(sections, fmt.Sprintf("[pipeline:%s]\npipeline = %s", spec.Name, pipeline))

	// Collect all filter sections (middleware + filters) and sort by name for determinism.
	type filterEntry struct {
		name    string
		factory string
		config  map[string]string
	}

	var filters []filterEntry
	for _, mw := range spec.Middleware {
		filters = append(filters, filterEntry{
			name:    mw.Name,
			factory: mw.FilterFactory,
			config:  mw.Config,
		})
	}
	for _, f := range spec.Filters {
		filters = append(filters, filterEntry{
			name:    f.Name,
			factory: f.Factory,
			config:  f.Config,
		})
	}

	sort.Slice(filters, func(i, j int) bool {
		return filters[i].name < filters[j].name
	})

	for _, f := range filters {
		sections = append(sections, renderFilterSection(f.name, f.factory, f.config))
	}

	return strings.Join(sections, "\n\n") + "\n"
}

// RenderPluginConfig converts a slice of PluginSpec into a config map suitable for
// merging with the main INI configuration via MergeDefaults. Each plugin produces one
// section keyed by its Section field. (CC-0004, REQ-010)
func RenderPluginConfig(plugins []types.PluginSpec) map[string]map[string]string {
	result := make(map[string]map[string]string)

	for _, p := range plugins {
		existing, ok := result[p.Section]
		if !ok {
			existing = make(map[string]string)
		}
		for k, v := range p.Config {
			existing[k] = v
		}
		result[p.Section] = existing
	}

	return result
}

// insertMiddleware inserts middleware names into the base pipeline string at the
// positions specified by each middleware's Position field.
func insertMiddleware(basePipeline string, middleware []types.MiddlewareSpec) string {
	tokens := strings.Fields(basePipeline)

	for _, mw := range middleware {
		tokens = insertToken(tokens, mw.Name, mw.Position)
	}

	return strings.Join(tokens, " ")
}

// insertToken inserts a token into the pipeline at the position specified.
// If both After and Before are set, After takes precedence.
// If the requested anchor token (After/Before) is not found in the existing
// pipeline, the token is appended to the end. This fallback behavior is
// intentional; callers that require strict placement should validate their
// configuration before invoking insertToken.
func insertToken(tokens []string, name string, pos types.PipelinePosition) []string {
	if pos.After != "" {
		for i, t := range tokens {
			if t == pos.After {
				result := make([]string, 0, len(tokens)+1)
				result = append(result, tokens[:i+1]...)
				result = append(result, name)
				result = append(result, tokens[i+1:]...)
				return result
			}
		}
	}

	if pos.Before != "" {
		for i, t := range tokens {
			if t == pos.Before {
				result := make([]string, 0, len(tokens)+1)
				result = append(result, tokens[:i]...)
				result = append(result, name)
				result = append(result, tokens[i:]...)
				return result
			}
		}
	}

	// Default: append to end.
	return append(tokens, name)
}

// renderFilterSection renders a [filter:name] INI section with sorted config keys.
func renderFilterSection(name, factory string, config map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[filter:%s]\npaste.filter_factory = %s", name, factory)

	if len(config) > 0 {
		keys := make([]string, 0, len(config))
		for k := range config {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			fmt.Fprintf(&b, "\n%s = %s", k, config[k])
		}
	}

	return b.String()
}
