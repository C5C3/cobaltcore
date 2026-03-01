package types

import (
	"encoding/json"
	"reflect"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

func TestImageSpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := ImageSpec{Repository: "ghcr.io/example/keystone", Tag: "2025.2"}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded ImageSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestSecretRefSpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := SecretRefSpec{Name: "db-credentials", Key: "password"}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded SecretRefSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestDatabaseSpec_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		original DatabaseSpec
	}{
		{
			name: "with ClusterRef (managed MariaDB)",
			original: DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb-cluster"},
				Port:       3306,
				Database:   "keystone",
				SecretRef:  SecretRefSpec{Name: "db-secret", Key: "password"},
			},
		},
		{
			name: "with Host (brownfield)",
			original: DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: SecretRefSpec{Name: "db-secret", Key: "password"},
			},
		},
		{
			name: "minimal (neither ClusterRef nor Host)",
			original: DatabaseSpec{
				Database:  "keystone",
				SecretRef: SecretRefSpec{Name: "db-secret", Key: "password"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			data, err := json.Marshal(tc.original)
			g.Expect(err).NotTo(HaveOccurred())

			var decoded DatabaseSpec
			err = json.Unmarshal(data, &decoded)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(decoded).To(Equal(tc.original))
		})
	}
}

func TestMessagingSpec_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		original MessagingSpec
	}{
		{
			name: "with ClusterRef (managed RabbitMQ)",
			original: MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq-cluster"},
				Port:       5672,
				SecretRef:  SecretRefSpec{Name: "mq-secret", Key: "password"},
				Vhost:      "/openstack",
			},
		},
		{
			name: "with Host (brownfield)",
			original: MessagingSpec{
				Host:      "rabbitmq.example.com",
				Port:      5672,
				SecretRef: SecretRefSpec{Name: "mq-secret", Key: "password"},
				Vhost:     "/openstack",
			},
		},
		{
			name: "minimal (neither ClusterRef nor Host)",
			original: MessagingSpec{
				SecretRef: SecretRefSpec{Name: "mq-secret", Key: "password"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			data, err := json.Marshal(tc.original)
			g.Expect(err).NotTo(HaveOccurred())

			var decoded MessagingSpec
			err = json.Unmarshal(data, &decoded)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(decoded).To(Equal(tc.original))
		})
	}
}

func TestMessagingSpec_DualMode(t *testing.T) {
	tests := []struct {
		name       string
		spec       MessagingSpec
		hasCluster bool
		hasHost    bool
	}{
		{
			name: "ClusterRef set",
			spec: MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
				SecretRef:  SecretRefSpec{Name: "s", Key: "k"},
			},
			hasCluster: true,
			hasHost:    false,
		},
		{
			name: "Host set",
			spec: MessagingSpec{
				Host:      "rabbitmq.example.com",
				SecretRef: SecretRefSpec{Name: "s", Key: "k"},
			},
			hasCluster: false,
			hasHost:    true,
		},
		{
			name: "neither set",
			spec: MessagingSpec{
				SecretRef: SecretRefSpec{Name: "s", Key: "k"},
			},
			hasCluster: false,
			hasHost:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			data, err := json.Marshal(tc.spec)
			g.Expect(err).NotTo(HaveOccurred())

			var decoded MessagingSpec
			err = json.Unmarshal(data, &decoded)
			g.Expect(err).NotTo(HaveOccurred())

			if tc.hasCluster {
				g.Expect(decoded.ClusterRef).NotTo(BeNil())
			} else {
				g.Expect(decoded.ClusterRef).To(BeNil())
			}

			if tc.hasHost {
				g.Expect(decoded.Host).NotTo(BeEmpty())
			} else {
				g.Expect(decoded.Host).To(BeEmpty())
			}
		})
	}
}

func TestCacheSpec_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		original CacheSpec
	}{
		{
			name: "with ClusterRef",
			original: CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "memcached-cluster"},
			},
		},
		{
			name: "with Servers",
			original: CacheSpec{
				Servers: []string{"memcached-0:11211", "memcached-1:11211"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			data, err := json.Marshal(tc.original)
			g.Expect(err).NotTo(HaveOccurred())

			var decoded CacheSpec
			err = json.Unmarshal(data, &decoded)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(decoded).To(Equal(tc.original))
		})
	}
}

func TestPolicySpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := PolicySpec{
		ConfigMapRef: &corev1.LocalObjectReference{Name: "policy-overrides"},
		Inline:       map[string]string{"identity:list_users": "role:admin"},
	}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded PolicySpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestPluginSpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := PluginSpec{
		Name:    "ldap",
		Section: "identity",
		Config:  map[string]string{"url": "ldap://ldap.example.com", "user": "cn=admin"},
	}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded PluginSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestMiddlewareSpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := MiddlewareSpec{
		Name:          "cors",
		FilterFactory: "oslo_middleware.cors:filter_factory",
		Config:        map[string]string{"allowed_origin": "https://example.com"},
		Position:      PipelinePosition{Before: "keystoneauth"},
	}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded MiddlewareSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestFilterSpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := FilterSpec{
		Name:    "healthcheck",
		Factory: "oslo_middleware:Healthcheck.app_factory",
		Config:  map[string]string{"path": "/healthcheck"},
	}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded FilterSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestPipelineSpec_JSONRoundTrip(t *testing.T) {
	g := NewGomegaWithT(t)

	original := PipelineSpec{
		Name:         "keystone-public-api",
		BasePipeline: "public_api",
		Filters: []FilterSpec{
			{
				Name:    "healthcheck",
				Factory: "oslo_middleware:Healthcheck.app_factory",
				Config:  map[string]string{"path": "/healthcheck"},
			},
		},
		Middleware: []MiddlewareSpec{
			{
				Name:          "cors",
				FilterFactory: "oslo_middleware.cors:filter_factory",
				Config:        map[string]string{"allowed_origin": "https://example.com"},
				Position:      PipelinePosition{After: "debug"},
			},
		},
	}
	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded PipelineSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))
}

func TestPipelinePosition_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		original PipelinePosition
	}{
		{
			name:     "before only",
			original: PipelinePosition{Before: "keystoneauth"},
		},
		{
			name:     "after only",
			original: PipelinePosition{After: "debug"},
		},
		{
			name:     "both",
			original: PipelinePosition{Before: "keystoneauth", After: "debug"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			data, err := json.Marshal(tc.original)
			g.Expect(err).NotTo(HaveOccurred())

			var decoded PipelinePosition
			err = json.Unmarshal(data, &decoded)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(decoded).To(Equal(tc.original))
		})
	}
}

func TestStructFieldJSONTags(t *testing.T) {
	tests := []struct {
		name       string
		structType reflect.Type
		field      string
		tag        string
	}{
		{"ImageSpec.Repository", reflect.TypeOf(ImageSpec{}), "Repository", `json:"repository"`},
		{"ImageSpec.Tag", reflect.TypeOf(ImageSpec{}), "Tag", `json:"tag"`},
		{"SecretRefSpec.Name", reflect.TypeOf(SecretRefSpec{}), "Name", `json:"name"`},
		{"SecretRefSpec.Key", reflect.TypeOf(SecretRefSpec{}), "Key", `json:"key"`},
		{"DatabaseSpec.ClusterRef", reflect.TypeOf(DatabaseSpec{}), "ClusterRef", `json:"clusterRef,omitempty"`},
		{"DatabaseSpec.Host", reflect.TypeOf(DatabaseSpec{}), "Host", `json:"host,omitempty"`},
		{"DatabaseSpec.Port", reflect.TypeOf(DatabaseSpec{}), "Port", `json:"port,omitempty"`},
		{"DatabaseSpec.Database", reflect.TypeOf(DatabaseSpec{}), "Database", `json:"database"`},
		{"DatabaseSpec.SecretRef", reflect.TypeOf(DatabaseSpec{}), "SecretRef", `json:"secretRef"`},
		{"MessagingSpec.ClusterRef", reflect.TypeOf(MessagingSpec{}), "ClusterRef", `json:"clusterRef,omitempty"`},
		{"MessagingSpec.Host", reflect.TypeOf(MessagingSpec{}), "Host", `json:"host,omitempty"`},
		{"MessagingSpec.Port", reflect.TypeOf(MessagingSpec{}), "Port", `json:"port,omitempty"`},
		{"MessagingSpec.SecretRef", reflect.TypeOf(MessagingSpec{}), "SecretRef", `json:"secretRef"`},
		{"MessagingSpec.Vhost", reflect.TypeOf(MessagingSpec{}), "Vhost", `json:"vhost,omitempty"`},
		{"CacheSpec.ClusterRef", reflect.TypeOf(CacheSpec{}), "ClusterRef", `json:"clusterRef,omitempty"`},
		{"CacheSpec.Servers", reflect.TypeOf(CacheSpec{}), "Servers", `json:"servers,omitempty"`},
		{"PolicySpec.ConfigMapRef", reflect.TypeOf(PolicySpec{}), "ConfigMapRef", `json:"configMapRef,omitempty"`},
		{"PolicySpec.Inline", reflect.TypeOf(PolicySpec{}), "Inline", `json:"inline,omitempty"`},
		{"PluginSpec.Name", reflect.TypeOf(PluginSpec{}), "Name", `json:"name"`},
		{"PluginSpec.Section", reflect.TypeOf(PluginSpec{}), "Section", `json:"section"`},
		{"PluginSpec.Config", reflect.TypeOf(PluginSpec{}), "Config", `json:"config,omitempty"`},
		{"MiddlewareSpec.Name", reflect.TypeOf(MiddlewareSpec{}), "Name", `json:"name"`},
		{"MiddlewareSpec.FilterFactory", reflect.TypeOf(MiddlewareSpec{}), "FilterFactory", `json:"filterFactory"`},
		{"MiddlewareSpec.Config", reflect.TypeOf(MiddlewareSpec{}), "Config", `json:"config,omitempty"`},
		{"MiddlewareSpec.Position", reflect.TypeOf(MiddlewareSpec{}), "Position", `json:"position"`},
		{"FilterSpec.Name", reflect.TypeOf(FilterSpec{}), "Name", `json:"name"`},
		{"FilterSpec.Factory", reflect.TypeOf(FilterSpec{}), "Factory", `json:"factory"`},
		{"FilterSpec.Config", reflect.TypeOf(FilterSpec{}), "Config", `json:"config,omitempty"`},
		{"PipelineSpec.Name", reflect.TypeOf(PipelineSpec{}), "Name", `json:"name"`},
		{"PipelineSpec.BasePipeline", reflect.TypeOf(PipelineSpec{}), "BasePipeline", `json:"basePipeline"`},
		{"PipelineSpec.Filters", reflect.TypeOf(PipelineSpec{}), "Filters", `json:"filters,omitempty"`},
		{"PipelineSpec.Middleware", reflect.TypeOf(PipelineSpec{}), "Middleware", `json:"middleware,omitempty"`},
		{"PipelinePosition.Before", reflect.TypeOf(PipelinePosition{}), "Before", `json:"before,omitempty"`},
		{"PipelinePosition.After", reflect.TypeOf(PipelinePosition{}), "After", `json:"after,omitempty"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			field, found := tc.structType.FieldByName(tc.field)
			g.Expect(found).To(BeTrue(), "field %s not found on %s", tc.field, tc.structType.Name())
			g.Expect(string(field.Tag)).To(ContainSubstring(tc.tag))
		})
	}
}

func TestDatabaseSpec_DualMode(t *testing.T) {
	tests := []struct {
		name       string
		spec       DatabaseSpec
		hasCluster bool
		hasHost    bool
	}{
		{
			name: "ClusterRef set",
			spec: DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
				Database:   "keystone",
				SecretRef:  SecretRefSpec{Name: "s", Key: "k"},
			},
			hasCluster: true,
			hasHost:    false,
		},
		{
			name: "Host set",
			spec: DatabaseSpec{
				Host:      "db.example.com",
				Database:  "keystone",
				SecretRef: SecretRefSpec{Name: "s", Key: "k"},
			},
			hasCluster: false,
			hasHost:    true,
		},
		{
			name: "neither set",
			spec: DatabaseSpec{
				Database:  "keystone",
				SecretRef: SecretRefSpec{Name: "s", Key: "k"},
			},
			hasCluster: false,
			hasHost:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			data, err := json.Marshal(tc.spec)
			g.Expect(err).NotTo(HaveOccurred())

			var decoded DatabaseSpec
			err = json.Unmarshal(data, &decoded)
			g.Expect(err).NotTo(HaveOccurred())

			if tc.hasCluster {
				g.Expect(decoded.ClusterRef).NotTo(BeNil())
			} else {
				g.Expect(decoded.ClusterRef).To(BeNil())
			}

			if tc.hasHost {
				g.Expect(decoded.Host).NotTo(BeEmpty())
			} else {
				g.Expect(decoded.Host).To(BeEmpty())
			}
		})
	}
}

func TestPipelineSpec_Aggregation(t *testing.T) {
	g := NewGomegaWithT(t)

	original := PipelineSpec{
		Name:         "nova-api",
		BasePipeline: "default",
		Filters: []FilterSpec{
			{Name: "f1", Factory: "factory1", Config: map[string]string{"k": "v"}},
			{Name: "f2", Factory: "factory2"},
		},
		Middleware: []MiddlewareSpec{
			{
				Name:          "m1",
				FilterFactory: "mf1",
				Config:        map[string]string{"mk": "mv"},
				Position:      PipelinePosition{Before: "auth"},
			},
		},
	}

	data, err := json.Marshal(original)
	g.Expect(err).NotTo(HaveOccurred())

	var decoded PipelineSpec
	err = json.Unmarshal(data, &decoded)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(decoded).To(Equal(original))

	g.Expect(decoded.Name).To(Equal("nova-api"))
	g.Expect(decoded.BasePipeline).To(Equal("default"))
	g.Expect(decoded.Filters).To(HaveLen(2))
	g.Expect(decoded.Filters[0].Name).To(Equal("f1"))
	g.Expect(decoded.Filters[0].Config).To(HaveKeyWithValue("k", "v"))
	g.Expect(decoded.Middleware).To(HaveLen(1))
	g.Expect(decoded.Middleware[0].Position.Before).To(Equal("auth"))
}
