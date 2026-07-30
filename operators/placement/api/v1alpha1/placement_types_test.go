// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "placement.openstack.c5c3.io" {
		t.Errorf("expected group %q, got %q", "placement.openstack.c5c3.io", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Errorf("expected version %q, got %q", "v1alpha1", GroupVersion.Version)
	}
}

func TestSchemeBuilderRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	kinds := []struct {
		kind string
		want runtime.Object
	}{
		{"Placement", &Placement{}},
		{"PlacementList", &PlacementList{}},
	}
	for _, tc := range kinds {
		gvk := schema.GroupVersionKind{
			Group:   "placement.openstack.c5c3.io",
			Version: "v1alpha1",
			Kind:    tc.kind,
		}
		if _, err := scheme.New(gvk); err != nil {
			t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
		}
	}
}

func TestPlacementImplementsRuntimeObject(t *testing.T) {
	var _ runtime.Object = &Placement{}
	var _ runtime.Object = &PlacementList{}
}

// TestEffectiveKeystonePublicEndpoint covers the render-time fallback: an
// explicit public endpoint is returned verbatim, an empty one falls back to the
// internal keystoneEndpoint, and both empty yields empty (no default injected).
func TestEffectiveKeystonePublicEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		public   string
		want     string
	}{
		{
			name:     "explicit public endpoint is returned",
			endpoint: "http://placement-keystone.svc:5000/v3",
			public:   "https://keystone.example.com/v3",
			want:     "https://keystone.example.com/v3",
		},
		{
			name:     "empty public endpoint falls back to keystoneEndpoint",
			endpoint: "http://placement-keystone.svc:5000/v3",
			public:   "",
			want:     "http://placement-keystone.svc:5000/v3",
		},
		{
			name:     "both empty yields empty",
			endpoint: "",
			public:   "",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := PlacementSpec{
				KeystoneEndpoint:       tt.endpoint,
				KeystonePublicEndpoint: tt.public,
			}
			if got := spec.EffectiveKeystonePublicEndpoint(); got != tt.want {
				t.Errorf("EffectiveKeystonePublicEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestUWSGISpecDeepCopy verifies the optional pointer fields round-trip through
// DeepCopy with independent storage, so mutating a clone cannot alias the
// original.
func TestUWSGISpecDeepCopy(t *testing.T) {
	keepAlive := true
	harakiri := int32(30)
	in := &UWSGISpec{
		Processes:     4,
		Threads:       2,
		HTTPKeepAlive: &keepAlive,
		Harakiri:      &harakiri,
	}

	out := in.DeepCopy()
	if out.HTTPKeepAlive == in.HTTPKeepAlive {
		t.Errorf("DeepCopy did not allocate a new *bool for HTTPKeepAlive")
	}
	if out.Harakiri == in.Harakiri {
		t.Errorf("DeepCopy did not allocate a new *int32 for Harakiri")
	}
	*out.Harakiri = 99
	if *in.Harakiri != 30 {
		t.Errorf("DeepCopy aliased Harakiri: mutating clone changed original to %d", *in.Harakiri)
	}
	if out.HTTPKeepAliveTimeout != nil {
		t.Errorf("DeepCopy materialized a nil HTTPKeepAliveTimeout into %v", *out.HTTPKeepAliveTimeout)
	}
}
