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
	if GroupVersion.Group != "glance.openstack.c5c3.io" {
		t.Errorf("expected group %q, got %q", "glance.openstack.c5c3.io", GroupVersion.Group)
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
		{"Glance", &Glance{}},
		{"GlanceList", &GlanceList{}},
		{"GlanceBackend", &GlanceBackend{}},
		{"GlanceBackendList", &GlanceBackendList{}},
	}
	for _, tc := range kinds {
		gvk := schema.GroupVersionKind{
			Group:   "glance.openstack.c5c3.io",
			Version: "v1alpha1",
			Kind:    tc.kind,
		}
		if _, err := scheme.New(gvk); err != nil {
			t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
		}
	}
}

func TestGlanceImplementsRuntimeObject(t *testing.T) {
	var _ runtime.Object = &Glance{}
	var _ runtime.Object = &GlanceList{}
	var _ runtime.Object = &GlanceBackend{}
	var _ runtime.Object = &GlanceBackendList{}
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
			endpoint: "http://keystone.svc:5000/v3",
			public:   "https://keystone.example.com/v3",
			want:     "https://keystone.example.com/v3",
		},
		{
			name:     "empty public endpoint falls back to keystoneEndpoint",
			endpoint: "http://keystone.svc:5000/v3",
			public:   "",
			want:     "http://keystone.svc:5000/v3",
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
			spec := GlanceSpec{
				KeystoneEndpoint:       tt.endpoint,
				KeystonePublicEndpoint: tt.public,
			}
			if got := spec.EffectiveKeystonePublicEndpoint(); got != tt.want {
				t.Errorf("EffectiveKeystonePublicEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestS3BackendSpecDeepCopy verifies the optional *int32 pointer fields round-trip
// through DeepCopy with independent storage, so mutating a clone cannot alias the
// original.
func TestS3BackendSpecDeepCopy(t *testing.T) {
	size := int32(5)
	chunk := int32(10)
	in := &S3BackendSpec{
		Host:                 "https://s3.example.com",
		Bucket:               "glance-images",
		CredentialsSecretRef: SecretNameRefSpec{Name: "garage-s3-credentials"},
		LargeObjectSize:      &size,
		LargeObjectChunkSize: &chunk,
	}

	out := in.DeepCopy()
	if out.LargeObjectSize == in.LargeObjectSize {
		t.Errorf("DeepCopy did not allocate a new *int32 for LargeObjectSize")
	}
	if out.LargeObjectChunkSize == in.LargeObjectChunkSize {
		t.Errorf("DeepCopy did not allocate a new *int32 for LargeObjectChunkSize")
	}
	*out.LargeObjectSize = 99
	if *in.LargeObjectSize != 5 {
		t.Errorf("DeepCopy aliased LargeObjectSize: mutating clone changed original to %d", *in.LargeObjectSize)
	}
}

func TestS3CredentialsDataKeyConstants(t *testing.T) {
	if S3AccessKeyIDKey != "access-key-id" {
		t.Errorf("S3AccessKeyIDKey = %q, want %q", S3AccessKeyIDKey, "access-key-id")
	}
	if S3SecretAccessKeyKey != "secret-access-key" {
		t.Errorf("S3SecretAccessKeyKey = %q, want %q", S3SecretAccessKeyKey, "secret-access-key")
	}
}
