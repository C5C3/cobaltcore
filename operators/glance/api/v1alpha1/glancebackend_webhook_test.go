// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// validGlanceBackend returns a minimal valid S3-typed GlanceBackend the per-rule
// tests mutate one field of, so every rejection is attributable to exactly one
// rule.
func validGlanceBackend() *GlanceBackend {
	return &GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "garage-s3", Namespace: "openstack"},
		Spec: GlanceBackendSpec{
			GlanceRef: GlanceRefSpec{Name: "glance"},
			Type:      GlanceBackendTypeS3,
			S3: &S3BackendSpec{
				Host:                 "https://s3.example.com",
				Bucket:               "glance-images",
				CredentialsSecretRef: SecretNameRefSpec{Name: "garage-s3-credentials"},
				BucketURLFormat:      "path",
			},
		},
	}
}

func glanceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("adding glance scheme: %v", err)
	}
	return s
}

func TestGlanceBackendDefault_BucketURLFormat(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceBackendWebhook{}

	// Empty: filled with "path".
	empty := validGlanceBackend()
	empty.Spec.S3.BucketURLFormat = ""
	g.Expect(w.Default(context.Background(), empty)).To(gomega.Succeed())
	g.Expect(empty.Spec.S3.BucketURLFormat).To(gomega.Equal("path"))

	// Explicit value preserved.
	explicit := validGlanceBackend()
	explicit.Spec.S3.BucketURLFormat = "virtual"
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.S3.BucketURLFormat).To(gomega.Equal("virtual"))
}

func TestGlanceBackendValidate_AcceptsValidBackend(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceBackendWebhook{}

	_, err := w.ValidateCreate(context.Background(), validGlanceBackend())
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// Both directions of the type/s3 union: type S3 without spec.s3, and spec.s3
// present alongside a non-S3 type value (unrepresentable via the enum, so a
// bogus value is used to exercise the XOR).
func TestGlanceBackendValidate_RejectsUnionMismatch(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceBackendWebhook{}

	missingS3 := validGlanceBackend()
	missingS3.Spec.S3 = nil
	_, err := w.ValidateCreate(context.Background(), missingS3)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one backend block matching spec.type"))

	wrongType := validGlanceBackend()
	wrongType.Spec.Type = GlanceBackendType("Ceph")
	_, err = w.ValidateCreate(context.Background(), wrongType)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one backend block matching spec.type"))
}

func TestGlanceBackendValidate_RejectsReservedStoreNames(t *testing.T) {
	names := []string{
		"default", "DEFAULT", "database", "keystone_authtoken", "glance_store",
		"paste_deploy", "oslo_policy", "os_glance_staging_store", "os_glance_tasks_store",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceBackendWebhook{}

			b := validGlanceBackend()
			b.Name = name
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring("reserved or operator-managed Glance store section"))
		})
	}
}

func TestGlanceBackendValidate_RejectsReservedStorePrefix(t *testing.T) {
	for _, name := range []string{"os_glance_foo", "os-glance-foo"} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceBackendWebhook{}

			b := validGlanceBackend()
			b.Name = name
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(`reserved "os_glance_" / "os-glance-" store-section prefix`))
		})
	}
}

func TestGlanceBackendValidate_AcceptsNormalName(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceBackendWebhook{}

	b := validGlanceBackend()
	b.Name = "garage-primary"
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestGlanceBackendValidate_ExtraOptions(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]string
		wantSub string
	}{
		{
			name:    "bad key pattern rejected",
			options: map[string]string{"bad key": "x"},
			wantSub: "option name must match",
		},
		{
			name:    "newline in key rejected",
			options: map[string]string{"foo\ns3_store_host = evil": "x"},
			wantSub: "option name must match",
		},
		{
			name:    "denylisted typed-field option rejected",
			options: map[string]string{"s3_store_bucket": "other"},
			wantSub: `option "s3_store_bucket" is owned by`,
		},
		{
			name:    "denylisted store_description rejected",
			options: map[string]string{"store_description": "primary"},
			wantSub: `option "store_description" is owned by`,
		},
		{
			name:    "control-char value rejected",
			options: map[string]string{"s3_store_thread_pools": "10\ns3_store_bucket = evil"},
			wantSub: "must not contain newline or carriage-return",
		},
		{
			name:    "empty key rejected",
			options: map[string]string{"": "x"},
			wantSub: "option name must not be empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &GlanceBackendWebhook{}

			b := validGlanceBackend()
			b.Spec.ExtraOptions = tc.options
			_, err := w.ValidateCreate(context.Background(), b)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

func TestGlanceBackendValidate_ExtraOptionsAllowsBenignOption(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &GlanceBackendWebhook{}

	b := validGlanceBackend()
	b.Spec.ExtraOptions = map[string]string{"s3_store_thread_pools": "10"}
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestGlanceBackendValidate_SiblingDefault(t *testing.T) {
	g := gomega.NewWithT(t)
	s := glanceScheme(t)

	existing := validGlanceBackend()
	existing.Name = "existing-default"
	existing.Spec.IsDefault = true

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	w := &GlanceBackendWebhook{Client: c}

	// Second default for the same Glance: rejected.
	b := validGlanceBackend()
	b.Name = "new-default"
	b.Spec.IsDefault = true
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("exactly one default store is allowed"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("existing-default"))

	// Default for a DIFFERENT Glance: accepted.
	other := validGlanceBackend()
	other.Name = "other-default"
	other.Spec.IsDefault = true
	other.Spec.GlanceRef.Name = "glance-other"
	_, err = w.ValidateCreate(context.Background(), other)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// On UPDATE the object under validation appears in the sibling List and must not
// collide with itself.
func TestGlanceBackendValidate_SiblingDefaultSkipsSelfOnUpdate(t *testing.T) {
	g := gomega.NewWithT(t)
	s := glanceScheme(t)

	self := validGlanceBackend()
	self.Spec.IsDefault = true
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(self).Build()
	w := &GlanceBackendWebhook{Client: c}

	updated := validGlanceBackend()
	updated.Spec.IsDefault = true
	updated.Spec.S3.Region = "eu-de-1"
	_, err := w.ValidateUpdate(context.Background(), self, updated)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A Terminating sibling default must not block a replacement default for the
// same Glance (recreate-during-teardown).
func TestGlanceBackendValidate_SiblingDefaultIgnoresTerminating(t *testing.T) {
	g := gomega.NewWithT(t)
	s := glanceScheme(t)

	terminating := validGlanceBackend()
	terminating.Name = "old-default"
	terminating.Spec.IsDefault = true
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"glance.openstack.c5c3.io/backend"}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(terminating).Build()
	w := &GlanceBackendWebhook{Client: c}

	b := validGlanceBackend()
	b.Name = "new-default"
	b.Spec.IsDefault = true
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// A List failure must surface as an admission error rather than silently
// admitting a possibly-conflicting second default.
func TestGlanceBackendValidate_SiblingDefaultListErrorSurfaced(t *testing.T) {
	g := gomega.NewWithT(t)
	s := glanceScheme(t)

	c := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("boom")
			},
		}).Build()
	w := &GlanceBackendWebhook{Client: c}

	b := validGlanceBackend()
	b.Spec.IsDefault = true
	_, err := w.ValidateCreate(context.Background(), b)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("listing GlanceBackends"))
}
