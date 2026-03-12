// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	corev1 "k8s.io/api/core/v1"
)

// Feature: CC-0004

// ImageSpec defines a container image reference.
type ImageSpec struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

// DatabaseSpec supports managed (ClusterRef) and brownfield (explicit) modes.
// Exactly one of ClusterRef or Host must be set.
type DatabaseSpec struct {
	// ClusterRef references a MariaDB CR in the cluster (managed mode).
	// +optional
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	// Host is the database hostname (brownfield mode).
	// +optional
	Host string `json:"host,omitempty"`
	// Port is the database port (brownfield mode, default 3306).
	// +optional
	Port int32 `json:"port,omitempty"`
	// Database is the database name within the cluster.
	Database string `json:"database"`
	// SecretRef references the K8s Secret with credentials.
	SecretRef SecretRefSpec `json:"secretRef"`
}

// MessagingSpec supports managed (ClusterRef) and brownfield (explicit) modes.
// Exactly one of ClusterRef or Hosts must be set.
type MessagingSpec struct {
	// ClusterRef references a RabbitMQ CR in the cluster (managed mode).
	// +optional
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	// Hosts is the list of RabbitMQ endpoints (brownfield mode).
	// +optional
	Hosts []string `json:"hosts,omitempty"`
	// SecretRef references the K8s Secret with credentials.
	SecretRef SecretRefSpec `json:"secretRef"`
}

// CacheSpec supports managed (ClusterRef) and brownfield (explicit) modes.
// Exactly one of ClusterRef or Servers must be set.
type CacheSpec struct {
	// ClusterRef references a Memcached CR in the cluster (managed mode).
	// +optional
	ClusterRef *corev1.LocalObjectReference `json:"clusterRef,omitempty"`
	// Backend is the cache backend (e.g. dogpile.cache.pymemcache).
	Backend string `json:"backend"`
	// Servers is the list of cache server endpoints (brownfield mode).
	// +optional
	Servers []string `json:"servers,omitempty"`
	// Replicas is the number of Memcached pod replicas in the referenced cluster
	// (managed mode). Used to generate the correct number of StatefulSet pod
	// endpoints. Only used when ClusterRef is set.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`
}

// SecretRefSpec references a Kubernetes Secret.
type SecretRefSpec struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// PolicySpec defines oslo.policy override configuration for an OpenStack service.
type PolicySpec struct {
	// Rules contains inline policy rule overrides.
	// Keys are oslo.policy rule names (e.g., "compute:create").
	// Values are oslo.policy rule definitions (e.g., "role:admin").
	// Inline rules take precedence over ConfigMap rules.
	// +optional
	Rules map[string]string `json:"rules,omitempty"`
	// ConfigMapRef references a user-provided ConfigMap containing a
	// "policy.yaml" key with rule overrides.
	// +optional
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
}

// PluginSpec defines a service plugin/driver configuration.
type PluginSpec struct {
	// Name of the plugin (e.g., "keystone-keycloak-backend")
	Name string `json:"name"`
	// ConfigSection is the INI section name (e.g., "keycloak")
	ConfigSection string `json:"configSection"`
	// Config contains key-value pairs for the plugin's INI section
	Config map[string]string `json:"config,omitempty"`
}

// PipelinePosition defines where middleware is inserted in api-paste.ini.
type PipelinePosition string

const (
	// PipelinePositionBefore inserts middleware before the base filters in the pipeline.
	PipelinePositionBefore PipelinePosition = "before"
	// PipelinePositionAfter inserts middleware after the base filters but before
	// the terminal application in the pipeline.
	PipelinePositionAfter PipelinePosition = "after"
)

// MiddlewareSpec defines a WSGI middleware filter for api-paste.ini.
type MiddlewareSpec struct {
	// Name of the filter (e.g., "audit")
	Name string `json:"name"`
	// FilterFactory is the Python entry point (e.g., "audit_middleware:filter_factory")
	FilterFactory string `json:"filterFactory"`
	// Position defines where in the pipeline this filter is inserted
	Position PipelinePosition `json:"position"`
	// Config contains key-value pairs for the filter section
	Config map[string]string `json:"config,omitempty"`
}
