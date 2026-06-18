// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package plugins

import (
	"fmt"
	"strings"

	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/types"
)

// PipelineSpec defines the input for rendering an api-paste.ini pipeline.
type PipelineSpec struct {
	// PipelineName is the INI pipeline section name (e.g., "api_v3").
	PipelineName string
	// AppName is the terminal WSGI application name (e.g., "service_v3").
	// If empty, no terminal application is appended to the pipeline directive.
	AppName string
	// AppFactory is the PasteDeploy "use" directive for the terminal app
	// (e.g., "egg:keystone#service_v3"). When set, an [app:<AppName>] section
	// is generated. Requires AppName to be non-empty.
	AppFactory string
	// BaseFilters contains the default filter names in pipeline order.
	BaseFilters []string
	// BaseFilterFactories maps base filter name to its PasteDeploy "use"
	// directive (e.g., "egg:oslo.middleware#cors"). A [filter:<name>] section
	// is generated for each entry.
	BaseFilterFactories map[string]string
	// BaseFilterConfigs contains extra config keys per base filter, merged
	// into the [filter:<name>] section alongside the "use" directive.
	BaseFilterConfigs map[string]map[string]string
	// CompositeRoutes maps URL paths to pipeline names for a
	// [composite:main] section (e.g., {"/v3": "public_api"}).
	CompositeRoutes map[string]string
	// Middleware contains additional middleware filters to insert.
	Middleware []types.MiddlewareSpec
}

// RenderPastePipeline renders an api-paste.ini snippet as INI sections.
// It builds the pipeline directive from BaseFilters and Middleware, placing
// "before"-positioned middleware before the base filters and "after"-positioned
// middleware after the base filters (but before the terminal app).
// Each middleware also generates a [filter:<name>] section with its
// paste.filter_factory and config entries.
// Returns an error if two middleware entries share the same Name, since the
// second would silently overwrite the first filter section.
// Returns an error if a middleware has an unrecognised Position value.
// Returns a map suitable for use with config.RenderINI.
func RenderPastePipeline(spec PipelineSpec) (map[string]map[string]string, error) {
	if spec.PipelineName == "" {
		return nil, fmt.Errorf("PipelineName must not be empty")
	}

	result := make(map[string]map[string]string)

	var beforeFilters, afterFilters []string
	// All validation (empty Name, empty FilterFactory, duplicate Name,
	// unknown Position) is performed in this single loop to fail fast
	// and keep control flow uniform.
	seen := make(map[string]struct{}, len(spec.Middleware))
	for _, mw := range spec.Middleware {
		if mw.Name == "" {
			return nil, fmt.Errorf("pipeline %q contains a MiddlewareSpec with an empty Name", spec.PipelineName)
		}
		if mw.FilterFactory == "" {
			return nil, fmt.Errorf("middleware %q in pipeline %q has an empty FilterFactory", mw.Name, spec.PipelineName)
		}
		if _, exists := seen[mw.Name]; exists {
			return nil, fmt.Errorf("duplicate middleware Name %q in pipeline %q", mw.Name, spec.PipelineName)
		}
		seen[mw.Name] = struct{}{}
		switch mw.Position {
		case types.PipelinePositionBefore:
			beforeFilters = append(beforeFilters, mw.Name)
		case types.PipelinePositionAfter:
			afterFilters = append(afterFilters, mw.Name)
		default:
			return nil, fmt.Errorf("middleware %q in pipeline %q has unrecognised Position %q", mw.Name, spec.PipelineName, mw.Position)
		}
	}

	// Validate BaseFilters entries — empty strings produce malformed pipeline
	// directives that PasteDeploy may reject at runtime.
	for i, f := range spec.BaseFilters {
		if f == "" {
			return nil, fmt.Errorf("pipeline %q has an empty BaseFilters entry at index %d", spec.PipelineName, i)
		}
	}

	// Build pipeline: [before] [base] [after] [app]
	pipeline := make([]string, 0, len(beforeFilters)+len(spec.BaseFilters)+len(afterFilters)+1)
	pipeline = append(pipeline, beforeFilters...)
	pipeline = append(pipeline, spec.BaseFilters...)
	pipeline = append(pipeline, afterFilters...)
	if spec.AppName != "" {
		pipeline = append(pipeline, spec.AppName)
	}

	pipelineValue := strings.Join(pipeline, " ")
	if pipelineValue == "" {
		return nil, fmt.Errorf("pipeline %q has no filters or application — at least one entry is required", spec.PipelineName)
	}

	pipelineSection := fmt.Sprintf("pipeline:%s", spec.PipelineName)
	result[pipelineSection] = map[string]string{
		"pipeline": pipelineValue,
	}

	// Generate filter sections for each middleware.
	// Duplicate Names are already rejected in the validation loop above.
	for _, mw := range spec.Middleware {
		filterSection := fmt.Sprintf("filter:%s", mw.Name)
		filterConfig := map[string]string{
			"paste.filter_factory": mw.FilterFactory,
		}
		for k, v := range mw.Config {
			if k == "paste.filter_factory" {
				continue // FilterFactory field takes precedence
			}
			filterConfig[k] = v
		}
		result[filterSection] = filterConfig
	}

	// Generate [app:<AppName>] section when AppFactory is set.
	if spec.AppFactory != "" && spec.AppName != "" {
		appSection := fmt.Sprintf("app:%s", spec.AppName)
		result[appSection] = map[string]string{
			"use": spec.AppFactory,
		}
	}

	// Generate [filter:<name>] sections for base filter factories.
	for name, factory := range spec.BaseFilterFactories {
		filterSection := fmt.Sprintf("filter:%s", name)
		filterConfig := map[string]string{
			"use": factory,
		}
		if extra, ok := spec.BaseFilterConfigs[name]; ok {
			for k, v := range extra {
				filterConfig[k] = v
			}
		}
		result[filterSection] = filterConfig
	}

	// Generate [composite:main] section when CompositeRoutes is set.
	if len(spec.CompositeRoutes) > 0 {
		compositeConfig := map[string]string{
			"use": "egg:Paste#urlmap",
		}
		for path, pipelineName := range spec.CompositeRoutes {
			compositeConfig[path] = pipelineName
		}
		result["composite:main"] = compositeConfig
	}

	return result, nil
}

// RenderPluginConfig renders plugin configuration as INI sections.
// Each plugin's ConfigSection becomes a section name with its Config
// key-value pairs. Returns an error if two plugins share the same
// ConfigSection, since the second would silently overwrite the first.
// Returns a map suitable for merging with config.MergeDefaults
// and rendering with config.RenderINI.
func RenderPluginConfig(plugins []types.PluginSpec) (map[string]map[string]string, error) {
	result := make(map[string]map[string]string, len(plugins))
	for i, p := range plugins {
		if p.Name == "" {
			return nil, fmt.Errorf("plugin at index %d has an empty Name", i)
		}
		if p.ConfigSection == "" {
			return nil, fmt.Errorf("plugin %q has an empty ConfigSection", p.Name)
		}
		if _, exists := result[p.ConfigSection]; exists {
			return nil, fmt.Errorf("duplicate ConfigSection %q: plugin %q would overwrite existing config", p.ConfigSection, p.Name)
		}
		section := make(map[string]string, len(p.Config))
		for k, v := range p.Config {
			section[k] = v
		}
		result[p.ConfigSection] = section
	}
	return result, nil
}

// RenderPastePipelineINI renders an api-paste.ini snippet as a formatted INI
// string. This is a convenience wrapper combining RenderPastePipeline with
// deterministic section and key ordering via config.RenderINI.
// Returns an error if RenderPastePipeline returns one — see RenderPastePipeline
// for the full list of validation errors (empty PipelineName, empty middleware
// Name or FilterFactory, unknown Position, empty BaseFilters entries, duplicate
// middleware Names, and an entirely empty pipeline directive).
func RenderPastePipelineINI(spec PipelineSpec) (string, error) {
	sections, err := RenderPastePipeline(spec)
	if err != nil {
		return "", err
	}
	return config.RenderINI(sections), nil
}
