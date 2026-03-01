package plugins

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/forge/internal/common/types"
)

func TestRenderPastePipeline(t *testing.T) {
	tests := []struct {
		name     string
		spec     types.PipelineSpec
		contains []string
		exact    string // if non-empty, assert exact match instead of contains
	}{
		{
			name: "base pipeline only",
			spec: types.PipelineSpec{
				Name:         "api_v3",
				BasePipeline: "cors request_id service_v3",
			},
			exact: "[pipeline:api_v3]\npipeline = cors request_id service_v3\n",
		},
		{
			name: "after position insertion",
			spec: types.PipelineSpec{
				Name:         "api_v3",
				BasePipeline: "cors request_id authtoken keystonecontext service_v3",
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "audit_middleware:filter_factory",
						Position:      types.PipelinePosition{After: "authtoken"},
						Config:        map[string]string{"audit_map": "/etc/keystone/audit_map.yaml"},
					},
				},
			},
			contains: []string{
				"pipeline = cors request_id authtoken audit keystonecontext service_v3",
				"[filter:audit]",
				"paste.filter_factory = audit_middleware:filter_factory",
				"audit_map = /etc/keystone/audit_map.yaml",
			},
		},
		{
			name: "before position insertion",
			spec: types.PipelineSpec{
				Name:         "api_v3",
				BasePipeline: "cors request_id authtoken keystonecontext service_v3",
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "rate_limit",
						FilterFactory: "rate_limit:filter_factory",
						Position:      types.PipelinePosition{Before: "authtoken"},
					},
				},
			},
			contains: []string{
				"pipeline = cors request_id rate_limit authtoken keystonecontext service_v3",
			},
		},
		{
			name: "multiple middleware",
			spec: types.PipelineSpec{
				Name:         "api_v3",
				BasePipeline: "cors request_id authtoken keystonecontext service_v3",
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "audit_middleware:filter_factory",
						Position:      types.PipelinePosition{After: "authtoken"},
					},
					{
						Name:          "rate_limit",
						FilterFactory: "rate_limit:filter_factory",
						Position:      types.PipelinePosition{Before: "keystonecontext"},
					},
				},
			},
			contains: []string{
				"pipeline = cors request_id authtoken audit rate_limit keystonecontext service_v3",
			},
		},
		{
			name: "filter blocks with sorted config keys",
			spec: types.PipelineSpec{
				Name:         "api_v3",
				BasePipeline: "cors request_id service_v3",
				Filters: []types.FilterSpec{
					{
						Name:    "cors",
						Factory: "oslo_middleware:filter_factory",
						Config: map[string]string{
							"oslo_config_project": "keystone",
							"allowed_origin":      "http://example.com",
						},
					},
				},
			},
			contains: []string{
				"[filter:cors]",
				"paste.filter_factory = oslo_middleware:filter_factory",
				"allowed_origin = http://example.com",
				"oslo_config_project = keystone",
			},
		},
		{
			name: "empty base pipeline appends middleware",
			spec: types.PipelineSpec{
				Name:         "api_v3",
				BasePipeline: "",
				Middleware: []types.MiddlewareSpec{
					{
						Name:          "audit",
						FilterFactory: "audit_middleware:filter_factory",
						Position:      types.PipelinePosition{},
					},
					{
						Name:          "cors",
						FilterFactory: "cors:filter_factory",
						Position:      types.PipelinePosition{},
					},
				},
			},
			contains: []string{
				"pipeline = audit cors",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := RenderPastePipeline(tt.spec)

			if tt.exact != "" {
				g.Expect(result).To(Equal(tt.exact))
			}
			for _, substr := range tt.contains {
				g.Expect(result).To(ContainSubstring(substr))
			}
		})
	}

	// Filter sections are sorted by name (cross-section ordering).
	t.Run("filter sections sorted by name", func(t *testing.T) {
		g := NewGomegaWithT(t)

		spec := types.PipelineSpec{
			Name:         "api_v3",
			BasePipeline: "service_v3",
			Middleware: []types.MiddlewareSpec{
				{
					Name:          "zebra",
					FilterFactory: "zebra:factory",
					Position:      types.PipelinePosition{},
				},
			},
			Filters: []types.FilterSpec{
				{
					Name:    "alpha",
					Factory: "alpha:factory",
				},
			},
		}

		result := RenderPastePipeline(spec)

		alphaIdx := strings.Index(result, "[filter:alpha]")
		zebraIdx := strings.Index(result, "[filter:zebra]")
		g.Expect(alphaIdx).To(BeNumerically("<", zebraIdx))
	})
}

func TestRenderPluginConfig(t *testing.T) {
	tests := []struct {
		name        string
		plugins     []types.PluginSpec
		expectedLen int
		expected    map[string]map[string]string
	}{
		{
			name: "single plugin",
			plugins: []types.PluginSpec{
				{
					Name:    "keycloak-backend",
					Section: "keycloak",
					Config: map[string]string{
						"url":   "https://keycloak.example.com",
						"realm": "openstack",
					},
				},
			},
			expectedLen: 1,
			expected: map[string]map[string]string{
				"keycloak": {"url": "https://keycloak.example.com", "realm": "openstack"},
			},
		},
		{
			name: "multiple plugins in different sections",
			plugins: []types.PluginSpec{
				{
					Name:    "keycloak-backend",
					Section: "keycloak",
					Config:  map[string]string{"url": "https://keycloak.example.com"},
				},
				{
					Name:    "audit",
					Section: "audit_middleware",
					Config:  map[string]string{"enabled": "true"},
				},
			},
			expectedLen: 2,
			expected: map[string]map[string]string{
				"keycloak":         {"url": "https://keycloak.example.com"},
				"audit_middleware": {"enabled": "true"},
			},
		},
		{
			name:        "empty slice returns empty map",
			plugins:     []types.PluginSpec{},
			expectedLen: 0,
		},
		{
			name:        "nil slice returns empty map",
			plugins:     nil,
			expectedLen: 0,
		},
		{
			name: "same section merges with last-wins",
			plugins: []types.PluginSpec{
				{
					Name:    "plugin-a",
					Section: "shared",
					Config: map[string]string{
						"key1": "value1",
						"key2": "value2-a",
					},
				},
				{
					Name:    "plugin-b",
					Section: "shared",
					Config: map[string]string{
						"key2": "value2-b",
						"key3": "value3",
					},
				},
			},
			expectedLen: 1,
			expected: map[string]map[string]string{
				"shared": {"key1": "value1", "key2": "value2-b", "key3": "value3"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			result := RenderPluginConfig(tt.plugins)

			g.Expect(result).NotTo(BeNil())
			g.Expect(result).To(HaveLen(tt.expectedLen))

			for section, expectedKVs := range tt.expected {
				g.Expect(result).To(HaveKey(section))
				for k, v := range expectedKVs {
					g.Expect(result[section]).To(HaveKeyWithValue(k, v))
				}
			}
		})
	}
}
