// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package plugins

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/forge/internal/common/types"
)

// Feature: CC-0004

func TestRenderPastePipeline(t *testing.T) {
	tests := []struct {
		name string
		spec PipelineSpec
		want map[string]map[string]string
	}{
		{
			name: "empty middleware produces pipeline with base filters and app only",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors", "sizelimit"},
				Middleware:   nil,
			},
			want: map[string]map[string]string{
				"pipeline:api_v3": {"pipeline": "cors sizelimit service_v3"},
			},
		},
		{
			name: "no base filters with app only",
			spec: PipelineSpec{
				PipelineName: "main",
				AppName:      "app",
				BaseFilters:  nil,
				Middleware:   nil,
			},
			want: map[string]map[string]string{
				"pipeline:main": {"pipeline": "app"},
			},
		},
		{
			name: "before-positioned middleware prepended to base filters",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors", "sizelimit"},
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "audit_middleware:filter_factory",
						Position:      types.PipelinePositionBefore,
					},
				},
			},
			want: map[string]map[string]string{
				"pipeline:api_v3": {"pipeline": "audit cors sizelimit service_v3"},
				"filter:audit":    {"paste.filter_factory": "audit_middleware:filter_factory"},
			},
		},
		{
			name: "after-positioned middleware appended after base filters before app",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors", "sizelimit"},
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "custom",
						FilterFactory: "custom_pkg:filter_factory",
						Position:      types.PipelinePositionAfter,
					},
				},
			},
			want: map[string]map[string]string{
				"pipeline:api_v3": {"pipeline": "cors sizelimit custom service_v3"},
				"filter:custom":   {"paste.filter_factory": "custom_pkg:filter_factory"},
			},
		},
		{
			name: "mixed before and after middleware with config",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors", "sizelimit"},
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "audit_middleware:filter_factory",
						Position:      types.PipelinePositionBefore,
						Config:        map[string]string{"audit_map_file": "/etc/audit.yaml"},
					},
					{
						Name:          "healthcheck",
						FilterFactory: "oslo_middleware:healthcheck_filter",
						Position:      types.PipelinePositionAfter,
						Config:        map[string]string{"path": "/healthcheck", "detailed": "true"},
					},
				},
			},
			want: map[string]map[string]string{
				"pipeline:api_v3": {"pipeline": "audit cors sizelimit healthcheck service_v3"},
				"filter:audit": {
					"paste.filter_factory": "audit_middleware:filter_factory",
					"audit_map_file":       "/etc/audit.yaml",
				},
				"filter:healthcheck": {
					"paste.filter_factory": "oslo_middleware:healthcheck_filter",
					"path":                 "/healthcheck",
					"detailed":             "true",
				},
			},
		},
		{
			name: "multiple before-positioned middleware preserves insertion order",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors"},
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "first",
						FilterFactory: "first:filter_factory",
						Position:      types.PipelinePositionBefore,
					},
					{
						Name:          "second",
						FilterFactory: "second:filter_factory",
						Position:      types.PipelinePositionBefore,
					},
				},
			},
			want: map[string]map[string]string{
				"pipeline:api_v3": {"pipeline": "first second cors service_v3"},
				"filter:first":    {"paste.filter_factory": "first:filter_factory"},
				"filter:second":   {"paste.filter_factory": "second:filter_factory"},
			},
		},
		{
			name: "empty AppName omits terminal app from pipeline",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "",
				BaseFilters:  []string{"cors", "sizelimit"},
				Middleware:   nil,
			},
			want: map[string]map[string]string{
				"pipeline:api_v3": {"pipeline": "cors sizelimit"},
			},
		},
		{
			name: "middleware with empty config map",
			spec: PipelineSpec{
				PipelineName: "main",
				AppName:      "app",
				BaseFilters:  nil,
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "simple",
						FilterFactory: "simple:factory",
						Position:      types.PipelinePositionBefore,
						Config:        map[string]string{},
					},
				},
			},
			want: map[string]map[string]string{
				"pipeline:main": {"pipeline": "simple app"},
				"filter:simple": {"paste.filter_factory": "simple:factory"},
			},
		},
		{
			name: "Config with paste.filter_factory key does not overwrite FilterFactory field (CC-0004)",
			spec: PipelineSpec{
				PipelineName: "main",
				AppName:      "app",
				BaseFilters:  nil,
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "correct:factory",
						Position:      types.PipelinePositionBefore,
						Config:        map[string]string{"paste.filter_factory": "wrong:factory", "extra": "val"},
					},
				},
			},
			want: map[string]map[string]string{
				"pipeline:main": {"pipeline": "audit app"},
				"filter:audit":  {"paste.filter_factory": "correct:factory", "extra": "val"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result, err := RenderPastePipeline(tt.spec)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result).To(Equal(tt.want))
		})
	}
}

func TestRenderPastePipeline_emptyPipelineNameReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "",
		AppName:      "app",
		BaseFilters:  []string{"cors"},
		Middleware:   nil,
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("PipelineName must not be empty"))
	g.Expect(result).To(BeNil())
}

func TestRenderPastePipeline_emptyPipelineReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "empty",
		AppName:      "",
		BaseFilters:  nil,
		Middleware:   nil,
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no filters or application"))
	g.Expect(err.Error()).To(ContainSubstring("empty"))
	g.Expect(result).To(BeNil())
}

func TestRenderPastePipeline_duplicateMiddlewareNameReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "api_v3",
		AppName:      "service_v3",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{
				Name:          "audit",
				FilterFactory: "audit_middleware:filter_factory",
				Position:      types.PipelinePositionBefore,
			},
			{
				Name:          "audit",
				FilterFactory: "different_audit:filter_factory",
				Position:      types.PipelinePositionAfter,
			},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("duplicate middleware Name"))
	g.Expect(err.Error()).To(ContainSubstring("audit"))
	g.Expect(err.Error()).To(ContainSubstring("api_v3"))
	g.Expect(result).To(BeNil())
}

func TestRenderPastePipeline_emptyMiddlewareNameReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "api_v3",
		AppName:      "service_v3",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{
				Name:          "",
				FilterFactory: "some:factory",
				Position:      types.PipelinePositionBefore,
			},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty Name"))
	g.Expect(err.Error()).To(ContainSubstring("api_v3"))
	g.Expect(result).To(BeNil())
}

func TestRenderPastePipeline_emptyFilterFactoryReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "api_v3",
		AppName:      "service_v3",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{
				Name:          "audit",
				FilterFactory: "",
				Position:      types.PipelinePositionBefore,
			},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty FilterFactory"))
	g.Expect(err.Error()).To(ContainSubstring("audit"))
	g.Expect(err.Error()).To(ContainSubstring("api_v3"))
	g.Expect(result).To(BeNil())
}

func TestRenderPastePipeline_emptyBaseFiltersEntryReturnsError(t *testing.T) {
	tests := []struct {
		name        string
		baseFilters []string
		wantIndex   string
	}{
		{
			name:        "trailing empty entry",
			baseFilters: []string{"cors", ""},
			wantIndex:   "index 1",
		},
		{
			name:        "leading empty entry",
			baseFilters: []string{"", "cors"},
			wantIndex:   "index 0",
		},
		{
			name:        "single empty entry",
			baseFilters: []string{""},
			wantIndex:   "index 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			spec := PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  tt.baseFilters,
				Middleware:   nil,
			}

			result, err := RenderPastePipeline(spec)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("empty BaseFilters entry"))
			g.Expect(err.Error()).To(ContainSubstring("api_v3"))
			g.Expect(err.Error()).To(ContainSubstring(tt.wantIndex))
			g.Expect(result).To(BeNil())
		})
	}
}

func TestRenderPastePipeline_unknownPositionReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "api_v3",
		AppName:      "service_v3",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{
				Name:          "invalid",
				FilterFactory: "invalid:factory",
				Position:      types.PipelinePosition("unknown"),
			},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unrecognised Position"))
	g.Expect(err.Error()).To(ContainSubstring("invalid"))
	g.Expect(err.Error()).To(ContainSubstring("api_v3"))
	g.Expect(result).To(BeNil())
}

func TestRenderPastePipeline_zeroValuePositionReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "main",
		AppName:      "app",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{
				Name:          "noposition",
				FilterFactory: "noposition:factory",
				// Position is zero-value empty string
			},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("unrecognised Position"))
	g.Expect(err.Error()).To(ContainSubstring("noposition"))
	g.Expect(result).To(BeNil())
}

func TestRenderPluginConfig(t *testing.T) {
	tests := []struct {
		name    string
		plugins []types.PluginSpec
		want    map[string]map[string]string
	}{
		{
			name:    "nil plugins returns empty map",
			plugins: nil,
			want:    map[string]map[string]string{},
		},
		{
			name:    "empty plugins slice returns empty map",
			plugins: []types.PluginSpec{},
			want:    map[string]map[string]string{},
		},
		{
			name: "single plugin renders its config section",
			plugins: []types.PluginSpec{
				{
					Name:          "keystone-keycloak-backend",
					ConfigSection: "keycloak",
					Config:        map[string]string{"server_url": "https://keycloak.example.com", "realm": "openstack"},
				},
			},
			want: map[string]map[string]string{
				"keycloak": {"server_url": "https://keycloak.example.com", "realm": "openstack"},
			},
		},
		{
			name: "multiple plugins render separate sections",
			plugins: []types.PluginSpec{
				{
					Name:          "ldap-backend",
					ConfigSection: "ldap",
					Config:        map[string]string{"url": "ldap://ldap.example.com", "user_tree_dn": "ou=Users,dc=example,dc=com"},
				},
				{
					Name:          "oauth2-driver",
					ConfigSection: "oauth2",
					Config:        map[string]string{"driver": "oauth2", "token_endpoint": "https://idp.example.com/token"},
				},
			},
			want: map[string]map[string]string{
				"ldap":   {"url": "ldap://ldap.example.com", "user_tree_dn": "ou=Users,dc=example,dc=com"},
				"oauth2": {"driver": "oauth2", "token_endpoint": "https://idp.example.com/token"},
			},
		},
		{
			name: "plugin with nil Config produces empty section (CC-0004)",
			plugins: []types.PluginSpec{
				{
					Name:          "nil-config-plugin",
					ConfigSection: "nilsec",
					Config:        nil,
				},
			},
			want: map[string]map[string]string{
				"nilsec": {},
			},
		},
		{
			name: "plugin with empty config map",
			plugins: []types.PluginSpec{
				{
					Name:          "minimal-plugin",
					ConfigSection: "minimal",
					Config:        map[string]string{},
				},
			},
			want: map[string]map[string]string{
				"minimal": {},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result, err := RenderPluginConfig(tt.plugins)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result).To(Equal(tt.want))
		})
	}
}

func TestRenderPluginConfig_duplicateConfigSectionReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	plugins := []types.PluginSpec{
		{
			Name:          "plugin-a",
			ConfigSection: "shared",
			Config:        map[string]string{"key_a": "value_a"},
		},
		{
			Name:          "plugin-b",
			ConfigSection: "shared",
			Config:        map[string]string{"key_b": "value_b"},
		},
	}

	result, err := RenderPluginConfig(plugins)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("duplicate ConfigSection"))
	g.Expect(err.Error()).To(ContainSubstring("shared"))
	g.Expect(err.Error()).To(ContainSubstring("plugin-b"))
	g.Expect(result).To(BeNil())
}

func TestRenderPluginConfig_emptyNameReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	plugins := []types.PluginSpec{
		{
			Name:          "",
			ConfigSection: "section",
			Config:        map[string]string{"key": "value"},
		},
	}

	result, err := RenderPluginConfig(plugins)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty Name"))
	g.Expect(err.Error()).To(ContainSubstring("index 0"))
	g.Expect(result).To(BeNil())
}

func TestRenderPluginConfig_emptyConfigSectionReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	plugins := []types.PluginSpec{
		{
			Name:          "bad-plugin",
			ConfigSection: "",
			Config:        map[string]string{"key": "value"},
		},
	}

	result, err := RenderPluginConfig(plugins)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("empty ConfigSection"))
	g.Expect(err.Error()).To(ContainSubstring("bad-plugin"))
	g.Expect(result).To(BeNil())
}

func TestRenderPluginConfig_doesNotMutateInput(t *testing.T) {
	g := NewGomegaWithT(t)

	plugins := []types.PluginSpec{
		{
			Name:          "test-plugin",
			ConfigSection: "test",
			Config:        map[string]string{"key": "value"},
		},
	}

	result, err := RenderPluginConfig(plugins)
	g.Expect(err).NotTo(HaveOccurred())

	// Mutating the result should not affect the input.
	result["test"]["key"] = "mutated"
	result["test"]["new_key"] = "new_value"

	g.Expect(plugins[0].Config["key"]).To(Equal("value"))
	g.Expect(plugins[0].Config).NotTo(HaveKey("new_key"))
}

func TestRenderPastePipeline_doesNotMutateInputs(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "api_v3",
		AppName:      "service_v3",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{
				Name:          "audit",
				FilterFactory: "audit:factory",
				Position:      types.PipelinePositionBefore,
				Config:        map[string]string{"key": "value"},
			},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).NotTo(HaveOccurred())

	// Mutating the result should not affect spec.Middleware[0].Config.
	result["filter:audit"]["key"] = "mutated"
	result["filter:audit"]["new_key"] = "new_value"

	g.Expect(spec.Middleware[0].Config["key"]).To(Equal("value"))
	g.Expect(spec.Middleware[0].Config).NotTo(HaveKey("new_key"))
}

func TestRenderPastePipelineINI(t *testing.T) {
	tests := []struct {
		name string
		spec PipelineSpec
		want string
	}{
		{
			name: "empty middleware renders pipeline section only",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors", "sizelimit"},
			},
			want: "[pipeline:api_v3]\npipeline = cors sizelimit service_v3\n",
		},
		{
			name: "middleware renders pipeline and filter sections sorted",
			spec: PipelineSpec{
				PipelineName: "api_v3",
				AppName:      "service_v3",
				BaseFilters:  []string{"cors"},
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "audit_middleware:filter_factory",
						Position:      types.PipelinePositionAfter,
					},
				},
			},
			want: "[filter:audit]\npaste.filter_factory = audit_middleware:filter_factory\n\n" +
				"[pipeline:api_v3]\npipeline = cors audit service_v3\n",
		},
		{
			name: "filter config keys sorted alphabetically",
			spec: PipelineSpec{
				PipelineName: "main",
				AppName:      "app",
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "check",
						FilterFactory: "check:factory",
						Position:      types.PipelinePositionBefore,
						Config:        map[string]string{"zebra": "z", "alpha": "a"},
					},
				},
			},
			want: "[filter:check]\nalpha = a\npaste.filter_factory = check:factory\nzebra = z\n\n" +
				"[pipeline:main]\npipeline = check app\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result, err := RenderPastePipelineINI(tt.spec)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result).To(Equal(tt.want))
		})
	}
}

func TestRenderPastePipeline_AppFactory(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "public_api",
		AppName:      "admin_service",
		AppFactory:   "egg:keystone#service_v3",
		BaseFilters:  []string{"cors"},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(HaveKey("app:admin_service"))
	g.Expect(result["app:admin_service"]).To(Equal(map[string]string{
		"use": "egg:keystone#service_v3",
	}))
}

func TestRenderPastePipeline_BaseFilterFactories(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "public_api",
		AppName:      "app",
		BaseFilters:  []string{"cors", "sizelimit"},
		BaseFilterFactories: map[string]string{
			"cors":      "egg:oslo.middleware#cors",
			"sizelimit": "egg:oslo.middleware#sizelimit",
		},
		BaseFilterConfigs: map[string]map[string]string{
			"cors": {"oslo_config_project": "keystone"},
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result["filter:cors"]).To(Equal(map[string]string{
		"use":                 "egg:oslo.middleware#cors",
		"oslo_config_project": "keystone",
	}))
	g.Expect(result["filter:sizelimit"]).To(Equal(map[string]string{
		"use": "egg:oslo.middleware#sizelimit",
	}))
}

func TestRenderPastePipeline_CompositeRoutes(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "public_api",
		AppName:      "app",
		BaseFilters:  []string{"cors"},
		CompositeRoutes: map[string]string{
			"/v3": "public_api",
		},
	}

	result, err := RenderPastePipeline(spec)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(HaveKey("composite:main"))
	g.Expect(result["composite:main"]).To(Equal(map[string]string{
		"use": "egg:Paste#urlmap",
		"/v3": "public_api",
	}))
}

func TestRenderPastePipelineINI_FullKeystoneSpec(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "public_api",
		AppName:      "admin_service",
		AppFactory:   "egg:keystone#service_v3",
		BaseFilters:  []string{"cors", "sizelimit", "http_proxy_to_wsgi", "url_normalize", "request_id"},
		BaseFilterFactories: map[string]string{
			"cors":               "egg:oslo.middleware#cors",
			"sizelimit":          "egg:oslo.middleware#sizelimit",
			"http_proxy_to_wsgi": "egg:oslo.middleware#http_proxy_to_wsgi",
			"url_normalize":      "egg:keystone#url_normalize",
			"request_id":         "egg:oslo.middleware#request_id",
		},
		BaseFilterConfigs: map[string]map[string]string{
			"cors": {"oslo_config_project": "keystone"},
		},
		CompositeRoutes: map[string]string{"/v3": "public_api"},
	}

	result, err := RenderPastePipelineINI(spec)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(ContainSubstring("[app:admin_service]"))
	g.Expect(result).To(ContainSubstring("use = egg:keystone#service_v3"))
	g.Expect(result).To(ContainSubstring("[composite:main]"))
	g.Expect(result).To(ContainSubstring("/v3 = public_api"))
	g.Expect(result).To(ContainSubstring("use = egg:Paste#urlmap"))
	g.Expect(result).To(ContainSubstring("[filter:cors]"))
	g.Expect(result).To(ContainSubstring("oslo_config_project = keystone"))
	g.Expect(result).To(ContainSubstring("[filter:request_id]"))
	g.Expect(result).To(ContainSubstring("[filter:sizelimit]"))
	g.Expect(result).To(ContainSubstring("[filter:url_normalize]"))
	g.Expect(result).To(ContainSubstring("[filter:http_proxy_to_wsgi]"))
	g.Expect(result).To(ContainSubstring("[pipeline:public_api]"))
	g.Expect(result).To(ContainSubstring("pipeline = cors sizelimit http_proxy_to_wsgi url_normalize request_id admin_service"))
}

func TestRenderPastePipelineINI_duplicateMiddlewareNameReturnsError(t *testing.T) {
	g := NewGomegaWithT(t)

	spec := PipelineSpec{
		PipelineName: "api_v3",
		AppName:      "service_v3",
		BaseFilters:  []string{"cors"},
		Middleware: []types.MiddlewareSpec{
			{Name: "audit", FilterFactory: "a:ff", Position: types.PipelinePositionBefore},
			{Name: "audit", FilterFactory: "b:ff", Position: types.PipelinePositionAfter},
		},
	}

	result, err := RenderPastePipelineINI(spec)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("duplicate middleware Name"))
	g.Expect(err.Error()).To(ContainSubstring("audit"))
	g.Expect(result).To(BeEmpty())
}
