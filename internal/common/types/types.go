package types

import (
	corev1 "k8s.io/api/core/v1"
)

// ImageSpec defines a container image reference.
type ImageSpec struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

// SecretRefSpec references a Kubernetes Secret by name and key.
type SecretRefSpec struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// DatabaseSpec defines database connection parameters.
// ClusterRef and Host are mutually exclusive: ClusterRef for managed MariaDB, Host for brownfield.
type DatabaseSpec struct {
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	Host       string                       `json:"host,omitempty"`
	Port       int32                        `json:"port,omitempty"`
	Database   string                       `json:"database"`
	SecretRef  SecretRefSpec                `json:"secretRef"`
}

// MessagingSpec defines RabbitMQ messaging parameters.
// ClusterRef and Host are mutually exclusive: ClusterRef for managed RabbitMQ, Host for brownfield.
type MessagingSpec struct {
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	Host       string                       `json:"host,omitempty"`
	Port       int32                        `json:"port,omitempty"`
	SecretRef  SecretRefSpec                `json:"secretRef"`
	Vhost      string                       `json:"vhost,omitempty"`
}

// CacheSpec defines Memcached cache parameters.
type CacheSpec struct {
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	Servers    []string                     `json:"servers,omitempty"`
}

// PolicySpec defines policy override sources.
type PolicySpec struct {
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
	Inline       map[string]string            `json:"inline,omitempty"`
}

// PluginSpec defines a plugin configuration section.
type PluginSpec struct {
	Name    string            `json:"name"`
	Section string            `json:"section"`
	Config  map[string]string `json:"config,omitempty"`
}

// PipelinePosition specifies where middleware is inserted in the pipeline.
type PipelinePosition struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// MiddlewareSpec defines a PasteDeploy middleware/filter to insert.
type MiddlewareSpec struct {
	Name          string            `json:"name"`
	FilterFactory string            `json:"filterFactory"`
	Config        map[string]string `json:"config,omitempty"`
	Position      PipelinePosition  `json:"position"`
}

// FilterSpec defines a PasteDeploy filter entry.
type FilterSpec struct {
	Name    string            `json:"name"`
	Factory string            `json:"factory"`
	Config  map[string]string `json:"config,omitempty"`
}

// PipelineSpec defines a complete PasteDeploy pipeline configuration.
type PipelineSpec struct {
	Name         string           `json:"name"`
	BasePipeline string           `json:"basePipeline"`
	Filters      []FilterSpec     `json:"filters,omitempty"`
	Middleware   []MiddlewareSpec `json:"middleware,omitempty"`
}
