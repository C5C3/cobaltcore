// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validControlPlane returns a ControlPlane with all required fields set to
// valid values. Tests modify this baseline to exercise specific rules.
func validControlPlane() *ControlPlane {
	return &ControlPlane{
		Spec: ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					Host:      "db.example.com",
					Port:      3306,
					Database:  "openstack",
					SecretRef: commonv1.SecretRefSpec{Name: "db-creds"},
				},
				Cache: commonv1.CacheSpec{
					Backend: "dogpile.cache.pymemcache",
					Servers: []string{"mc:11211"},
				},
			},
			Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{},
			},
			KORC: KORCSpec{
				AdminCredential: AdminCredentialSpec{
					CloudCredentialsRef: CloudCredentialsRef{CloudName: "admin"},
					PasswordSecretRef:   commonv1.SecretRefSpec{Name: "admin-pw"},
				},
			},
		},
	}
}

// --- Defaulting webhook tests ---

func TestDefault_SetsZeroValueDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := &ControlPlane{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Region).To(Equal(DefaultRegion))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName).To(Equal(DefaultCloudCredentialsSecretName))
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).NotTo(BeNil())
	g.Expect(*cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).To(BeTrue())
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode).To(Equal(RotationModePasswordDriven))

	// the eight well-known database/cache/admin-credential defaults on a
	// bare &ControlPlane{}.
	infra := cp.Spec.Infrastructure
	g.Expect(infra.Database.Database).To(Equal(DefaultDatabaseName))
	g.Expect(infra.Database.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(infra.Database.ClusterRef).NotTo(BeNil())
	g.Expect(infra.Database.ClusterRef.Name).To(Equal(DefaultDatabaseClusterRefName))
	// database.secretRef.key is intentionally NOT defaulted.
	g.Expect(infra.Database.SecretRef.Key).To(BeEmpty())
	g.Expect(infra.Cache.Backend).To(Equal(DefaultCacheBackend))
	g.Expect(infra.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(infra.Cache.ClusterRef.Name).To(Equal(DefaultCacheClusterRefName))
	cred := cp.Spec.KORC.AdminCredential
	g.Expect(cred.PasswordSecretRef.Name).To(Equal(DefaultAdminPasswordSecretName))
	g.Expect(cred.PasswordSecretRef.Key).To(Equal(DefaultAdminPasswordSecretKey))
	g.Expect(cred.CloudCredentialsRef.CloudName).To(Equal(DefaultCloudName))
	// admin identity (P1) defaults.
	g.Expect(cred.UserName).To(Equal(DefaultAdminUserName))
	g.Expect(cred.ProjectName).To(Equal(DefaultAdminProjectName))
	g.Expect(cred.DomainName).To(Equal(DefaultAdminDomainName))
}

// TestDefault_IsIdempotent verifies applying Default twice produces the same
// result.
func TestDefault_IsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := &ControlPlane{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	first := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Region).To(Equal(first.Spec.Region))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName).
		To(Equal(first.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName))
	g.Expect(*cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).
		To(Equal(*first.Spec.KORC.AdminCredential.ApplicationCredential.Restricted))
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode).
		To(Equal(first.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode))

	// the eight new defaults are identical on a second pass.
	g.Expect(cp.Spec.Infrastructure.Database.Database).
		To(Equal(first.Spec.Infrastructure.Database.Database))
	g.Expect(cp.Spec.Infrastructure.Database.SecretRef.Name).
		To(Equal(first.Spec.Infrastructure.Database.SecretRef.Name))
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).
		To(Equal(first.Spec.Infrastructure.Database.ClusterRef.Name))
	g.Expect(cp.Spec.Infrastructure.Cache.Backend).
		To(Equal(first.Spec.Infrastructure.Cache.Backend))
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).
		To(Equal(first.Spec.Infrastructure.Cache.ClusterRef.Name))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name).
		To(Equal(first.Spec.KORC.AdminCredential.PasswordSecretRef.Name))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Key).
		To(Equal(first.Spec.KORC.AdminCredential.PasswordSecretRef.Key))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName).
		To(Equal(first.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName))
}

// TestDefault_PreservesExplicitValues verifies the defaulting webhook never
// overwrites operator-supplied values, including an explicit restricted:false
// The *bool lets us distinguish unset from explicit false.
func TestDefault_PreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	restricted := false
	cp := &ControlPlane{
		Spec: ControlPlaneSpec{
			Region: "EU-West",
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "my-db"},
					Database:   "mydb",
					SecretRef:  commonv1.SecretRefSpec{Name: "mydb-creds"},
				},
				Cache: commonv1.CacheSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "my-cache"},
					Backend:    "dogpile.cache.memcached",
				},
			},
			KORC: KORCSpec{
				AdminCredential: AdminCredentialSpec{
					CloudCredentialsRef: CloudCredentialsRef{
						CloudName:  "operator",
						SecretName: "custom-clouds-yaml",
					},
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "my-admin", Key: "adminpw"},
					UserName:          "brownfield-admin",
					ProjectName:       "platform-admin",
					DomainName:        "heimdall",
					ApplicationCredential: ApplicationCredentialSpec{
						Restricted: &restricted,
						Rotation:   RotationSpec{Mode: RotationModeManual},
					},
				},
			},
		},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Region).To(Equal("EU-West"))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName).To(Equal("custom-clouds-yaml"))
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).NotTo(BeNil())
	g.Expect(*cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted).To(BeFalse())
	g.Expect(cp.Spec.KORC.AdminCredential.ApplicationCredential.Rotation.Mode).To(Equal(RotationModeManual))

	// every explicitly-supplied well-known field is preserved.
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).To(Equal("my-db"))
	g.Expect(cp.Spec.Infrastructure.Database.Database).To(Equal("mydb"))
	g.Expect(cp.Spec.Infrastructure.Database.SecretRef.Name).To(Equal("mydb-creds"))
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).To(Equal("my-cache"))
	g.Expect(cp.Spec.Infrastructure.Cache.Backend).To(Equal("dogpile.cache.memcached"))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name).To(Equal("my-admin"))
	g.Expect(cp.Spec.KORC.AdminCredential.PasswordSecretRef.Key).To(Equal("adminpw"))
	g.Expect(cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName).To(Equal("operator"))
	// explicit non-default admin identity (P1) is preserved, not overwritten.
	g.Expect(cp.Spec.KORC.AdminCredential.UserName).To(Equal("brownfield-admin"))
	g.Expect(cp.Spec.KORC.AdminCredential.ProjectName).To(Equal("platform-admin"))
	g.Expect(cp.Spec.KORC.AdminCredential.DomainName).To(Equal("heimdall"))
}

// TestDefault_DoesNotInventModeForBrownfield verifies the defaulting webhook
// never coerces an explicit brownfield database/cache into managed mode: when a
// brownfield discriminator (database.host / cache.servers) is set, the matching
// clusterRef is left nil so the validating webhook's XOR check still passes,
// while the mode-neutral leaves are still defaulted.
func TestDefault_DoesNotInventModeForBrownfield(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// Case A: brownfield database (host set) — database.clusterRef stays nil.
	cpDB := &ControlPlane{
		Spec: ControlPlaneSpec{
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{Host: "db.example.com"},
			},
		},
	}
	g.Expect(w.Default(context.Background(), cpDB)).To(Succeed())
	g.Expect(cpDB.Spec.Infrastructure.Database.ClusterRef).To(BeNil(),
		"brownfield host must not get an invented managed clusterRef")
	g.Expect(cpDB.Spec.Infrastructure.Database.Host).To(Equal("db.example.com"))
	// Mode-neutral leaves are still defaulted in brownfield mode.
	g.Expect(cpDB.Spec.Infrastructure.Database.Database).To(Equal(DefaultDatabaseName))
	g.Expect(cpDB.Spec.Infrastructure.Database.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(cpDB.Spec.Infrastructure.Cache.Backend).To(Equal(DefaultCacheBackend))

	// Case B: brownfield cache (servers set) — cache.clusterRef stays nil.
	cpCache := &ControlPlane{
		Spec: ControlPlaneSpec{
			Infrastructure: &InfrastructureSpec{
				Cache: commonv1.CacheSpec{Servers: []string{"mc:11211"}},
			},
		},
	}
	g.Expect(w.Default(context.Background(), cpCache)).To(Succeed())
	g.Expect(cpCache.Spec.Infrastructure.Cache.ClusterRef).To(BeNil(),
		"brownfield servers must not get an invented managed clusterRef")
	g.Expect(cpCache.Spec.Infrastructure.Cache.Servers).To(ConsistOf("mc:11211"))
	// Mode-neutral leaves are still defaulted in brownfield mode.
	g.Expect(cpCache.Spec.Infrastructure.Database.Database).To(Equal(DefaultDatabaseName))
	g.Expect(cpCache.Spec.Infrastructure.Database.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(cpCache.Spec.Infrastructure.Cache.Backend).To(Equal(DefaultCacheBackend))
}

// TestDefault_FillsEmptyNameOnPresentClusterRef covers the defaulting webhook's
// middle branch for both database and cache: a managed-mode clusterRef object
// that is present but carries an empty Name (the CRD schema permits a bare `{}`
// clusterRef). The webhook must fill the well-known managed name in place —
// preserving the existing clusterRef pointer — so the validating webhook's
// database/cache XOR check still passes after defaulting. Without this case the `else if clusterRef.Name == ""` arm of Default
// is unexercised.
func TestDefault_FillsEmptyNameOnPresentClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// clusterRef present but Name empty, with host/servers unset => managed mode.
	cp := &ControlPlane{
		Spec: ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Infrastructure: &InfrastructureSpec{
				Database: commonv1.DatabaseSpec{ClusterRef: &corev1.LocalObjectReference{}},
				Cache:    commonv1.CacheSpec{ClusterRef: &corev1.LocalObjectReference{}},
			},
		},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	// The empty Name is filled in place; the original clusterRef pointer is kept.
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).To(Equal(DefaultDatabaseClusterRefName),
		"present-but-empty database clusterRef.name must be filled with the managed default")
	g.Expect(cp.Spec.Infrastructure.Database.Host).To(BeEmpty(),
		"filling the managed clusterRef name must not invent a brownfield host")
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
	g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).To(Equal(DefaultCacheClusterRefName),
		"present-but-empty cache clusterRef.name must be filled with the managed default")
	g.Expect(cp.Spec.Infrastructure.Cache.Servers).To(BeEmpty(),
		"filling the managed clusterRef name must not invent brownfield servers")

	// The defaulted spec must satisfy the database/cache XOR (exactly one side set).
	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a filled managed clusterRef must satisfy the database/cache XOR after defaulting")
}

// externalControlPlane returns a minimal, valid External-mode ControlPlane: the
// sketch CR from the issue (mode + external.authURL + the required
// korc.adminCredential.passwordSecretRef), with no infrastructure block. Tests
// modify this baseline to exercise the External-mode defaulting and validation.
func externalControlPlane() *ControlPlane {
	return &ControlPlane{
		Spec: ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Services: ServicesSpec{
				Keystone: &ServiceKeystoneSpec{
					Mode: KeystoneModeExternal,
					External: &ExternalKeystoneSpec{
						AuthURL: "https://keystone.example.com/v3",
					},
				},
			},
			KORC: KORCSpec{
				AdminCredential: AdminCredentialSpec{
					CloudCredentialsRef: CloudCredentialsRef{CloudName: "admin"},
					PasswordSecretRef:   commonv1.SecretRefSpec{Name: "admin-pw"},
				},
			},
		},
	}
}

// TestDefault_ExternalModeDoesNotInventInfrastructure verifies the defaulting
// webhook never invents a managed database/cache block in External mode
// (spec.infrastructure stays nil) while it still materializes the external
// block's own defaults (endpointType -> public, caBundleSecretRef.key -> ca.crt).
func TestDefault_ExternalModeDoesNotInventInfrastructure(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{Name: "brownfield-keystone-ca"}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Infrastructure).To(BeNil(),
		"External mode must not invent a managed infrastructure block")
	ext := cp.Spec.Services.Keystone.External
	g.Expect(ext.EndpointType).To(Equal(DefaultExternalEndpointType),
		"external.endpointType must default to public")
	g.Expect(ext.CABundleSecretRef).NotTo(BeNil())
	g.Expect(ext.CABundleSecretRef.Key).To(Equal(DefaultCABundleSecretKey),
		"external.caBundleSecretRef.key must default to ca.crt")
	// The admin identity defaults still apply in External mode.
	g.Expect(cp.Spec.KORC.AdminCredential.UserName).To(Equal(DefaultAdminUserName))
	g.Expect(cp.Spec.KORC.AdminCredential.ProjectName).To(Equal(DefaultAdminProjectName))
	g.Expect(cp.Spec.KORC.AdminCredential.DomainName).To(Equal(DefaultAdminDomainName))
}

// TestDefault_ExternalModePreservesExplicitEndpointType verifies an explicit
// endpointType / caBundle key is preserved rather than overwritten in External
// mode (the error-path counterpart to the zero-value defaulting above).
func TestDefault_ExternalModePreservesExplicitEndpointType(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.EndpointType = ExternalEndpointTypeInternal
	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{
		Name: "brownfield-keystone-ca", Key: "tls-ca.pem",
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	ext := cp.Spec.Services.Keystone.External
	g.Expect(ext.EndpointType).To(Equal(ExternalEndpointTypeInternal))
	g.Expect(ext.CABundleSecretRef.Key).To(Equal("tls-ca.pem"))
}

// TestDefault_ManagedModeAllocatesInfrastructureWhenNil locks today's
// omit-infrastructure contract through the pointer flip: an explicit Managed-mode
// (or unset-keystone) CR that omits spec.infrastructure still gets the block
// materialized and the managed clusterRefs invented, exactly as before.
func TestDefault_ManagedModeAllocatesInfrastructureWhenNil(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, tc := range []struct {
		name string
		ks   *ServiceKeystoneSpec
	}{
		{"explicit managed mode", &ServiceKeystoneSpec{Mode: KeystoneModeManaged}},
		{"unset mode (defaults managed)", &ServiceKeystoneSpec{}},
		{"unset keystone service", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &ControlPlane{Spec: ControlPlaneSpec{Services: ServicesSpec{Keystone: tc.ks}}}
			g.Expect(w.Default(context.Background(), cp)).To(Succeed())

			g.Expect(cp.Spec.Infrastructure).NotTo(BeNil(),
				"a non-External CR must get its infrastructure block materialized")
			g.Expect(cp.Spec.Infrastructure.Database.ClusterRef).NotTo(BeNil())
			g.Expect(cp.Spec.Infrastructure.Database.ClusterRef.Name).To(Equal(DefaultDatabaseClusterRefName))
			g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef).NotTo(BeNil())
			g.Expect(cp.Spec.Infrastructure.Cache.ClusterRef.Name).To(Equal(DefaultCacheClusterRefName))
		})
	}
}

// TestDefault_ExternalModeIsIdempotent verifies applying Default twice to an
// External-mode CR produces the same result — in particular that the second pass
// does not invent an infrastructure block.
func TestDefault_ExternalModeIsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	first := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Infrastructure).To(BeNil())
	g.Expect(cp.Spec.Services.Keystone.External.EndpointType).
		To(Equal(first.Spec.Services.Keystone.External.EndpointType))
	g.Expect(cp.Spec.Services.Keystone.Mode).To(Equal(first.Spec.Services.Keystone.Mode))
}

// --- Validation webhook tests ---

func TestValidateCreate_AcceptsValidControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), validControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateCreate_AcceptsUnsetKeystoneService(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Staged adoption / externally-managed Keystone: services.keystone unset.
	cp.Spec.Services.Keystone = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a ControlPlane with services.keystone unset must be admitted")
}

func TestValidateCreate_RejectsBadOpenStackRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.OpenStackRelease = "2025"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("openStackRelease"))
}

func TestValidateCreate_AcceptsNamespacedSecretStoreRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(),
		"a ControlPlane selecting a namespaced SecretStore must be admitted")
}

func TestValidateCreate_RejectsSecretStoreRefEmptyName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("secretStoreRef"))
	g.Expect(err.Error()).To(ContainSubstring("name"))
}

func TestValidateCreate_RejectsSecretStoreRefUnknownKind(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreRefKind("Bogus"), Name: "some-store",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("secretStoreRef"))
	g.Expect(err.Error()).To(ContainSubstring("kind"))
}

// TestValidateUpdate_AllowsSecretStoreRefSwitch verifies the store reference is
// mutable — switching stores is a supported operation (the operator moves the
// key material in place), so it must NOT be treated as an immutable field.
func TestValidateUpdate_AllowsSecretStoreRefSwitch(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := validControlPlane()
	newCP := validControlPlane()
	newCP.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store",
	}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred(),
		"switching spec.secretStoreRef must be allowed on update")
}

func TestValidateCreate_RejectsKeystoneImageTagAndDigestBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Override the Keystone image with BOTH a tag and a digest — XOR violation.
	cp.Spec.Services.Keystone.Image = &commonv1.ImageSpec{
		Repository: "ghcr.io/c5c3/keystone",
		Tag:        "2025.2",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest"))
}

func TestValidateCreate_RejectsDatabaseBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Both clusterRef AND host set — XOR violation.
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

func TestValidateCreate_RejectsDatabaseNeitherSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	// Neither clusterRef NOR host set — XOR violation.
	cp.Spec.Infrastructure.Database.Host = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

// TestValidateCreate_RejectsDynamicCredentialsWithoutClusterRef verifies the
// defense-in-depth mirror of the shared DatabaseSpec CEL rule: engine-issued
// credentials (Dynamic) require managed mode (clusterRef set).
func TestValidateCreate_RejectsDynamicCredentialsWithoutClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane() // brownfield (Host set, ClusterRef nil)
	cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring("requires clusterRef"))
}

// TestValidateCreate_AcceptsDynamicCredentialsWithClusterRef verifies Dynamic is
// accepted in managed mode.
func TestValidateCreate_AcceptsDynamicCredentialsWithClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := managedControlPlane()
	cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsDatabaseReplicasTwo verifies that a managed-mode
// ControlPlane requesting database.replicas: 2 is rejected. The managed MariaDB
// projection turns any replicas>1 into a Galera cluster, and a two-node Galera
// cluster cannot hold a quorum majority, so a single pod disruption takes the
// whole database offline. The CRD marker only enforces Minimum=1, making this
// webhook the enforcement point.
func TestValidateCreate_RejectsDatabaseReplicasTwo(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := managedControlPlane()
	cp.Spec.Infrastructure.Database.Replicas = 2

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("replicas"))
	g.Expect(err.Error()).To(ContainSubstring("quorum"))
}

// TestValidateCreate_AcceptsQuorumSafeDatabaseReplicas verifies that the
// quorum-safe replica counts — 1 (standalone) and 3 (Galera with a majority) —
// pass validation, so the replicas>1==2 guard does not over-restrict legitimate
// topologies.
func TestValidateCreate_AcceptsQuorumSafeDatabaseReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, replicas := range []int32{1, 3, 5} {
		cp := managedControlPlane()
		cp.Spec.Infrastructure.Database.Replicas = replicas
		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "replicas=%d should be accepted", replicas)
	}
}

func TestValidateCreate_RejectsCacheBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

func TestValidateCreate_RejectsCacheNeitherSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure.Cache.Servers = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

func TestValidateCreate_RejectsMissingPasswordSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("passwordSecretRef"))
}

// --- Policy rule name/value tests (#479) ---
//
// The c5c3 webhook previously validated policy rules not at all, so an invalid
// rule on spec.global or spec.services.keystone.policyOverrides wedged the
// control plane indirectly via the keystone webhook. The validate() method now
// delegates to the shared policy.ValidatePolicyRules on both fields.

func TestValidateCreate_RejectsEmptyGlobalPolicyRuleName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("global"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule name must not be empty"))
}

func TestValidateCreate_RejectsEmptyGlobalPolicyRuleValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": ""}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("global"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule value must not be empty"))
}

func TestValidateCreate_RejectsEmptyServicePolicyRuleValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": ""},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("policyOverrides"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule value must not be empty"))
}

func TestValidateCreate_AcceptsValidPolicyRules(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": "role:admin"}}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:list_user": "role:reader"},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AccumulatesAllErrors puts EVERY validation rule into a
// broken state simultaneously and asserts the returned error names every field,
// pinning the webhook's no-short-circuit (accumulate-all) contract. If a future change short-circuits on the first error, this test
// fails because the later field substrings go missing.
func TestValidateCreate_AccumulatesAllErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()

	// Break every rule at once.
	cp.Spec.OpenStackRelease = "2025" // bad release pattern
	// Database: host is already set in the baseline; adding clusterRef makes BOTH
	// set => XOR violation.
	cp.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	// Cache: servers already set in the baseline; adding clusterRef => XOR violation.
	cp.Spec.Infrastructure.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	// Required passwordSecretRef.name missing.
	cp.Spec.KORC.AdminCredential.PasswordSecretRef.Name = ""
	// Unsupported rotation interval (not a whole number of days).
	cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: 5 * time.Hour}
	// Policy rules: an empty name on the global policy and an empty value on the
	// per-service override (the empty-value path is the issue #479 addition). Both
	// must participate in the aggregated error.
	cp.Spec.GlobalPolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": ""},
	}
	// Dedicated backing services: a Dynamic credentialsMode the dedicated database
	// cannot support, and a Horizon dedicated cache colliding with the Keystone one.
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef:      &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			CredentialsMode: commonv1.CredentialsModeDynamic,
			Database:        "keystone",
			SecretRef:       commonv1.SecretRefSpec{Name: "keystone-db"},
		},
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())

	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("openStackRelease"), "release pattern error must be present")
	g.Expect(msg).To(ContainSubstring("database"), "database XOR error must be present")
	g.Expect(msg).To(ContainSubstring("cache"), "cache XOR error must be present")
	g.Expect(msg).To(ContainSubstring("passwordSecretRef"), "required passwordSecretRef error must be present")
	g.Expect(msg).To(ContainSubstring("rotationInterval"), "rotation interval error must be present")
	g.Expect(msg).To(ContainSubstring("global"), "global policy rule-name error must be present")
	g.Expect(msg).To(ContainSubstring("policyOverrides"), "per-service policy rule-value error must be present")
	g.Expect(msg).To(ContainSubstring("policy rule name must not be empty"))
	g.Expect(msg).To(ContainSubstring("policy rule value must not be empty"))
	g.Expect(msg).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"),
		"dedicated Dynamic-credentials error must be present")
	g.Expect(msg).To(ContainSubstring("horizon.dedicatedBackingServices.cache.clusterRef.name"),
		"dedicated cache collision error must be present")
}

// TestValidateCreate_RejectsBadRotationInterval verifies a rotationInterval the
// reconciler's intervalToCron cannot represent is rejected at admission rather
// than surfacing as a steady-state KeystoneReady=False.
func TestValidateCreate_RejectsBadRotationInterval(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, bad := range []time.Duration{5 * time.Hour, 25 * time.Hour, -24 * time.Hour, 0} {
		cp := validControlPlane()
		cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: bad}

		_, err := w.ValidateCreate(context.Background(), cp)
		// A zero Duration is the same as "unset" (nil pointer is the unset case; a
		// &Duration{0} is an explicit zero), which the rule treats as invalid.
		g.Expect(err).To(HaveOccurred(), "interval %v must be rejected", bad)
		g.Expect(err.Error()).To(ContainSubstring("rotationInterval"))
	}
}

// TestValidateCreate_AcceptsDailyAndWeeklyRotationIntervals verifies the
// rotationInterval values intervalToCron supports (any positive whole number of
// days, including the canonical 24h daily and 168h weekly) pass admission
func TestValidateCreate_AcceptsDailyAndWeeklyRotationIntervals(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	for _, ok := range []time.Duration{24 * time.Hour, 48 * time.Hour, 168 * time.Hour, 336 * time.Hour} {
		cp := validControlPlane()
		cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: ok}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "interval %v must be accepted", ok)
	}
}

// TestValidateCreate_RejectsGatewayWithoutHostname verifies that configuring a
// gateway without a hostname is rejected at admission, so the reconciler never
// derives an empty "https:///v3" public endpoint (#476).
func TestValidateCreate_RejectsGatewayWithoutHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		// Hostname intentionally empty.
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("hostname"))
}

// TestValidateCreate_AcceptsGatewayWithHostname verifies a gateway carrying a
// non-empty hostname passes admission (#476).
func TestValidateCreate_AcceptsGatewayWithHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.127-0-0-1.nip.io",
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsNilGateway verifies the gateway hostname check does
// not fire when no gateway is configured (the field is optional) (#476).
func TestValidateCreate_AcceptsNilGateway(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateUpdate_AcceptsValidChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := validControlPlane()
	newCP := validControlPlane()
	newCP.Spec.OpenStackRelease = "2026.1"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// managedControlPlane returns a valid managed-mode ControlPlane: database and
// cache point at managed clusterRefs (not brownfield host/servers). The
// immutability tests start from this baseline so a clusterRef name or a mode
// flip is the only delta under test.
func managedControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
		Database:   "openstack",
		SecretRef:  commonv1.SecretRefSpec{Name: "db-creds"},
	}
	cp.Spec.Infrastructure.Cache = commonv1.CacheSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
		Backend:    "dogpile.cache.pymemcache",
	}
	cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName = "k-orc-clouds-yaml"
	return cp
}

// TestValidateUpdate_RejectsDatabaseModeFlip verifies that flipping the database
// between managed (clusterRef) and brownfield (host) mode is rejected on UPDATE,
// since the previously-projected MariaDB child would otherwise be orphaned (#476).
func TestValidateUpdate_RejectsDatabaseModeFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// managed -> brownfield.
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
		Host: "db.example.com", Database: "openstack", SecretRef: commonv1.SecretRefSpec{Name: "db-creds"},
	}
	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database mode"))

	// brownfield -> managed (the reverse direction).
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database mode"))
}

// TestValidateUpdate_RejectsDatabaseClusterRefRename verifies that renaming a
// managed database clusterRef is rejected on UPDATE, since the old MariaDB child
// would otherwise be orphaned while a new one is provisioned (#476).
func TestValidateUpdate_RejectsDatabaseClusterRefRename(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db-2"}

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("clusterRef.name"))
}

// TestValidateUpdate_RejectsCacheModeFlipAndRename verifies the cache mode flip
// and managed clusterRef rename are both rejected on UPDATE (#476).
func TestValidateUpdate_RejectsCacheModeFlipAndRename(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// managed -> brownfield (servers) cache mode flip.
	oldCP := managedControlPlane()
	flipped := managedControlPlane()
	flipped.Spec.Infrastructure.Cache = commonv1.CacheSpec{
		Servers: []string{"mc:11211"}, Backend: "dogpile.cache.pymemcache",
	}
	_, err := w.ValidateUpdate(context.Background(), oldCP, flipped)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache mode"))

	// managed clusterRef rename.
	renamed := managedControlPlane()
	renamed.Spec.Infrastructure.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-memcached-2"}
	_, err = w.ValidateUpdate(context.Background(), oldCP, renamed)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("clusterRef.name"))
}

// TestValidateUpdate_RejectsCloudSecretNameChange verifies that renaming
// cloudCredentialsRef.secretName is rejected on UPDATE, since the old K-ORC
// clouds.yaml ExternalSecret would otherwise be leaked (#476).
func TestValidateUpdate_RejectsCloudSecretNameChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName = "renamed-clouds-yaml"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("secretName"))
}

// TestValidateUpdate_AllowsMutableFieldChanges verifies that updates which only
// touch mutable fields (replicas, an openStackRelease upgrade) are accepted on
// an otherwise-unchanged managed ControlPlane, so the immutability guard does
// not over-restrict legitimate edits (#476, #466). Region is now immutable
// (#466), so it is deliberately left unchanged here.
func TestValidateUpdate_AllowsMutableFieldChanges(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()

	newCP := managedControlPlane()
	newCP.Spec.OpenStackRelease = "2026.1"
	replicas := int32(3)
	newCP.Spec.Services.Keystone.Replicas = &replicas

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsDatabaseNameChange verifies that renaming the shared
// database is rejected on UPDATE: the name is projected verbatim into the
// Keystone child's now-immutable spec.database.database, so a rename here would
// wedge the reconcile loop (#466). Only the database name changes, so the mode
// and clusterRef.name immutability checks stay satisfied.
func TestValidateUpdate_RejectsDatabaseNameChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.Database = "renamed"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database name is immutable"))
}

// TestValidateUpdate_RejectsDatabaseReplicasChange verifies that changing
// database.replicas is rejected on UPDATE: the count is projected into the owned
// MariaDB child's replica count and derived Galera topology, so editing it on a
// live control plane would drive a destructive scale-down or Galera toggle
// (3->1). Both directions are exercised so neither a scale-up nor a scale-down
// slips through.
func TestValidateUpdate_RejectsDatabaseReplicasChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// 3 -> 1 (scale down / Galera toggle off).
	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.Replicas = 3
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.Replicas = 1

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database replicas is immutable"))

	// 1 -> 3 (the reverse direction).
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database replicas is immutable"))
}

// TestValidateUpdate_RejectsDatabaseStorageSizeChange verifies that changing
// database.storageSize is rejected on UPDATE: the size is projected into the owned
// MariaDB child's spec.storage.size, which the mariadb-operator refuses to resize
// on a live CR, so freezing it at admission surfaces the constraint with a clear
// message. Both grow and shrink are exercised so neither slips through.
func TestValidateUpdate_RejectsDatabaseStorageSizeChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// 512Mi -> 100Gi (grow).
	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = "512Mi"
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = "100Gi"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database storageSize is immutable"))

	// 100Gi -> 512Mi (shrink, the reverse direction).
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database storageSize is immutable"))
}

// TestValidateUpdate_AcceptsUnchangedDatabaseStorageSize guards against the
// immutability check over-firing: an UPDATE that leaves storageSize untouched (here
// while editing a mutable field) must still be accepted.
func TestValidateUpdate_AcceptsUnchangedDatabaseStorageSize(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = "512Mi"
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = "512Mi"
	replicas := int32(3)
	newCP.Spec.Services.Keystone.Replicas = &replicas

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AcceptsStorageSizeMigrationFromEmpty covers a ControlPlane
// created before storageSize existed: "" is persisted, yet its live MariaDB was
// provisioned at DefaultDatabaseStorageSize. A first UPDATE that pins the field
// to that default (the size it already runs at) must be admitted as a one-time
// migration rather than rejected as a resize. Both the empty->default direction
// and the (defaulting-bypassed) default->empty direction are exercised.
func TestValidateUpdate_AcceptsStorageSizeMigrationFromEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// "" (pre-existing) -> the default it already runs at.
	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = ""
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = DefaultDatabaseStorageSize

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())

	// The reverse direction (field cleared back to the default) is equally a no-op.
	_, err = w.ValidateUpdate(context.Background(), newCP, oldCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsStorageSizeResizeFromEmpty guards the other half of
// the migration normalization: pinning a pre-existing ("") ControlPlane to a
// size OTHER than the default it already runs at is a real resize the
// mariadb-operator would refuse, so it must still be rejected.
func TestValidateUpdate_RejectsStorageSizeResizeFromEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	oldCP := managedControlPlane()
	oldCP.Spec.Infrastructure.Database.StorageSize = ""
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure.Database.StorageSize = "512Mi"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database storageSize is immutable"))
}

// TestValidateUpdate_RejectsRegionChange verifies that changing the region is
// rejected on UPDATE: the region is projected verbatim into the Keystone child's
// now-immutable spec.bootstrap.region (#466).
func TestValidateUpdate_RejectsRegionChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Region = "EU-West"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("region is immutable"))
}

// TestValidateUpdate_RejectsOpenStackReleaseDowngrade verifies that lowering the
// openStackRelease is rejected on UPDATE, because Keystone DB migrations are
// forward-only (#466). Both a year downgrade and a same-year minor downgrade are
// exercised.
func TestValidateUpdate_RejectsOpenStackReleaseDowngrade(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// Year downgrade: 2025.2 -> 2024.1.
	oldCP := managedControlPlane()
	yearDown := managedControlPlane()
	yearDown.Spec.OpenStackRelease = "2024.1"
	_, err := w.ValidateUpdate(context.Background(), oldCP, yearDown)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))

	// Same-year minor downgrade: 2025.2 -> 2025.1.
	minorDown := managedControlPlane()
	minorDown.Spec.OpenStackRelease = "2025.1"
	_, err = w.ValidateUpdate(context.Background(), oldCP, minorDown)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("downgrade"))
}

// TestValidateUpdate_RejectsNonCadenceReleaseMinor guards the regression where a
// regex-valid but non-cadence minor was silently admitted on UPDATE. OpenStack
// ships only YYYY.1 and YYYY.2; before the release pattern was tightened to
// ^\d{4}\.[12]$, patching a live 2025.2 to 2025.9 passed validate() (whose regex
// accepted any single-digit minor) while validateReleaseNotDowngraded returned
// nil (release.ParseRelease rejects minor 9), admitting an edit that had been
// rejected before validateReleaseNotDowngraded delegated to ParseRelease.
func TestValidateUpdate_RejectsNonCadenceReleaseMinor(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	nonCadence := managedControlPlane()
	nonCadence.Spec.OpenStackRelease = "2025.9"

	_, err := w.ValidateUpdate(context.Background(), oldCP, nonCadence)
	g.Expect(err).To(HaveOccurred(),
		"a non-cadence openStackRelease minor must be rejected on UPDATE")
	g.Expect(err.Error()).To(ContainSubstring("openStackRelease"))
}

// TestValidateUpdate_AcceptsOpenStackReleaseUpgrade verifies that raising the
// openStackRelease is accepted (the monotonic-upgrade happy path) (#466).
func TestValidateUpdate_AcceptsOpenStackReleaseUpgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.OpenStackRelease = "2026.1"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AcceptsSameOpenStackRelease verifies that re-applying the
// same openStackRelease is accepted, so the downgrade guard does not fire on a
// no-op update (#466).
func TestValidateUpdate_AcceptsSameOpenStackRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateDelete(context.Background(), &ControlPlane{})
	g.Expect(err).NotTo(HaveOccurred())
}

// --- One-ControlPlane-per-namespace tests ---

// webhookScheme builds a runtime.Scheme with the c5c3 API types registered, for
// the fake client backing the one-ControlPlane-per-namespace tests.
func webhookScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	g := NewGomegaWithT(t)
	s := runtime.NewScheme()
	g.Expect(AddToScheme(s)).To(Succeed())
	return s
}

// TestValidateCreate_RejectsSecondControlPlaneInNamespace verifies the
// one-ControlPlane-per-namespace contract: a CREATE is Forbidden when another
// ControlPlane already exists in the same namespace.
func TestValidateCreate_RejectsSecondControlPlaneInNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	existing := validControlPlane()
	existing.Name = "incumbent"
	existing.Namespace = "tenant-a"
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(existing).Build()
	w := &ControlPlaneWebhook{Client: c}

	second := validControlPlane()
	second.Name = "newcomer"
	second.Namespace = "tenant-a"

	_, err := w.ValidateCreate(context.Background(), second)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("incumbent"))
	g.Expect(err.Error()).To(ContainSubstring("tenant-a"))
}

// TestValidateCreate_AllowsFirstControlPlane_AndUpdate verifies the first CREATE
// in an empty namespace is allowed, and that UPDATE never trips the
// one-per-namespace check even though the CR is present.
func TestValidateCreate_AllowsFirstControlPlane_AndUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).Build()
	w := &ControlPlaneWebhook{Client: c}

	first := validControlPlane()
	first.Name = "first"
	first.Namespace = "tenant-b"
	_, err := w.ValidateCreate(context.Background(), first)
	g.Expect(err).NotTo(HaveOccurred())

	cWith := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(first).Build()
	wWith := &ControlPlaneWebhook{Client: cWith}
	updated := first.DeepCopy()
	updated.Spec.OpenStackRelease = "2026.1"
	_, err = wWith.ValidateUpdate(context.Background(), first, updated)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- services.horizon validation ---

// TestValidateCreate_AcceptsHorizonBlock verifies a minimal (empty) horizon
// block passes validation — every ServiceHorizonSpec field is optional.
func TestValidateCreate_AcceptsHorizonBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsHorizonGatewayWithoutHostname mirrors the keystone
// gateway hostname rule for the horizon service block.
func TestValidateCreate_RejectsHorizonGatewayWithoutHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.gateway.hostname"))
}

// TestValidateCreate_RejectsHorizonImageTagAndDigestBothSet mirrors the
// ImageSpec tag/digest XOR defense-in-depth check for the horizon override.
func TestValidateCreate_RejectsHorizonImageTagAndDigestBothSet(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Image: &commonv1.ImageSpec{
			Repository: "ghcr.io/c5c3/horizon",
			Tag:        "2025.2",
			Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest"))
}

// TestValidateCreate_RejectsHorizonEmptySecretKeyRefName covers the error path
// where secretKeyRef is present but carries no name.
func TestValidateCreate_RejectsHorizonEmptySecretKeyRefName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		SecretKeyRef: &commonv1.SecretRefSpec{Name: ""},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.secretKeyRef.name"))
}

// --- External-mode validation matrix ---

// TestValidateCreate_AcceptsMinimalExternalControlPlane is the acceptance proof
// for the issue's sketch CR: mode: External + external.authURL +
// korc.adminCredential.passwordSecretRef, no infrastructure block.
func TestValidateCreate_AcceptsMinimalExternalControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), externalControlPlane())
	g.Expect(err).NotTo(HaveOccurred(),
		"the minimal External-mode sketch CR must be admitted")
}

// TestValidateCreate_RejectsExternalModeWithoutExternalBlock verifies the
// external block is required in External mode.
func TestValidateCreate_RejectsExternalModeWithoutExternalBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("external is required when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsExternalBlockInManagedMode verifies the external
// block may only be set in External mode.
func TestValidateCreate_RejectsExternalBlockInManagedMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane() // keystone mode unset (=> Managed)
	cp.Spec.Services.Keystone.External = &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.external"))
	g.Expect(err.Error()).To(ContainSubstring("may only be set when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsManagedOnlyFieldsInExternalMode verifies each
// managed-only Keystone field is forbidden in External mode, each with a message
// naming the offending field.
func TestValidateCreate_RejectsManagedOnlyFieldsInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	replicas := int32(3)

	tests := []struct {
		name       string
		mutate     func(ks *ServiceKeystoneSpec)
		wantSubstr string
	}{
		{"replicas", func(ks *ServiceKeystoneSpec) { ks.Replicas = &replicas }, "services.keystone.replicas"},
		{"image", func(ks *ServiceKeystoneSpec) {
			ks.Image = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
		}, "services.keystone.image"},
		{"policyOverrides", func(ks *ServiceKeystoneSpec) {
			ks.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"a": "b"}}
		}, "services.keystone.policyOverrides"},
		{"rotationInterval", func(ks *ServiceKeystoneSpec) {
			ks.RotationInterval = &metav1.Duration{Duration: 24 * time.Hour}
		}, "services.keystone.rotationInterval"},
		{"gateway", func(ks *ServiceKeystoneSpec) {
			ks.Gateway = &commonv1.GatewaySpec{Hostname: "k.example.com"}
		}, "services.keystone.gateway"},
		{"publicEndpoint", func(ks *ServiceKeystoneSpec) {
			ks.PublicEndpoint = "https://k.example.com/v3"
		}, "services.keystone.publicEndpoint"},
		{"federationProxyImage", func(ks *ServiceKeystoneSpec) {
			ks.FederationProxyImage = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
		}, "services.keystone.federationProxyImage"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := externalControlPlane()
			tc.mutate(cp.Spec.Services.Keystone)

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSubstr))
			g.Expect(err.Error()).To(ContainSubstring("External"))
		})
	}
}

// TestValidateCreate_RejectsInfrastructureInExternalMode verifies
// spec.infrastructure is forbidden in External mode.
func TestValidateCreate_RejectsInfrastructureInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Infrastructure = &InfrastructureSpec{
		Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
		Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure"))
	g.Expect(err.Error()).To(ContainSubstring("forbidden when services.keystone.mode is External"))
}

// TestValidateCreate_RejectsHorizonInExternalMode verifies services.horizon is
// forbidden in External mode (P2).
func TestValidateCreate_RejectsHorizonInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon"))
	g.Expect(err.Error()).To(ContainSubstring("External"))
}

// TestValidateCreate_RejectsMissingInfrastructureInManagedMode verifies
// spec.infrastructure is required for a non-External ControlPlane (preserving
// today's contract now that the Go field is optional). This is the webhook-only
// path — only reachable when Default() (which materializes the block) is
// bypassed, exactly what a direct validate() call exercises.
func TestValidateCreate_RejectsMissingInfrastructureInManagedMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure"))
	g.Expect(err.Error()).To(ContainSubstring("is required unless services.keystone.mode is External"))
}

// TestValidateCreate_RejectsMissingInfrastructureWithUnsetKeystone verifies the
// same requirement when services.keystone is unset (staged adoption is still a
// Managed control plane at the infrastructure layer).
func TestValidateCreate_RejectsMissingInfrastructureWithUnsetKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone = nil
	cp.Spec.Infrastructure = nil

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.infrastructure"))
}

// TestValidateCreate_RejectsBadExternalAuthURL verifies a missing or malformed
// external.authURL is rejected. The hostless cases (https://, http:///v3) guard
// the SSRF-hardening: the coarse ^https?:// prefix accepted them, but the
// net/url-based gate requires a real host before the reconciler dials it.
func TestValidateCreate_RejectsBadExternalAuthURL(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// missing.
	cpMissing := externalControlPlane()
	cpMissing.Spec.Services.Keystone.External.AuthURL = ""
	_, err := w.ValidateCreate(context.Background(), cpMissing)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("authURL is required"))

	for _, bad := range []string{
		"keystone.example.com",          // no scheme
		"ftp://keystone.example.com/v3", // wrong scheme
		"https://",                      // scheme only, no host
		"http:///v3",                    // path but empty host
	} {
		cpBad := externalControlPlane()
		cpBad.Spec.Services.Keystone.External.AuthURL = bad
		_, err = w.ValidateCreate(context.Background(), cpBad)
		g.Expect(err).To(HaveOccurred(), "expected %q to be rejected", bad)
		g.Expect(err.Error()).To(ContainSubstring("authURL"), "for input %q", bad)
	}
}

// TestValidateCreate_RejectsOverLongExternalAuthURL mirrors the MaxLength=2048
// marker. The CRD Pattern is end-unanchored, so a multi-kilobyte path is otherwise
// admissible — and the reconciler interpolates authURL into
// status.conditions[].message, whose 32768-byte cap is a WHOLE-OBJECT constraint:
// one over-long message makes every condition unpersistable and the reconciler
// spins in a backoff loop with no condition to diagnose it by.
func TestValidateCreate_RejectsOverLongExternalAuthURL(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	prefix := "https://keystone.example.com/"

	atCap := externalControlPlane()
	atCap.Spec.Services.Keystone.External.AuthURL = prefix + strings.Repeat("a", maxExternalAuthURLBytes-len(prefix))
	_, err := w.ValidateCreate(context.Background(), atCap)
	g.Expect(err).NotTo(HaveOccurred(), "an authURL exactly at the cap is admissible")

	overCap := externalControlPlane()
	overCap.Spec.Services.Keystone.External.AuthURL = prefix + strings.Repeat("a", maxExternalAuthURLBytes-len(prefix)+1)
	_, err = w.ValidateCreate(context.Background(), overCap)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.external.authURL"))
	g.Expect(err.Error()).To(ContainSubstring("at most 2048 bytes"))
}

// TestValidateCreate_RejectsEmptyCABundleSecretRefName verifies a present-but-
// nameless caBundleSecretRef is rejected in External mode.
func TestValidateCreate_RejectsEmptyCABundleSecretRefName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.CABundleSecretRef = &commonv1.SecretRefSpec{Name: ""}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.external.caBundleSecretRef.name"))
}

// TestValidateCreate_RejectsPlaintextAuthURLWithCABundleSecretRef pins the coupling
// of the scheme to the CA bundle. An http:// endpoint never performs a TLS
// handshake, so the bundle is never consulted — yet every operator-visible signal
// says trust is enforced: the mint blocks on WaitingForCABundle until the Secret
// exists and `cacert` is projected into both credentials Secrets. Meanwhile K-ORC
// POSTs the admin password over the unencrypted connection on every mint and
// re-mint. Admission must reject the pair rather than silently void the bundle.
//
// Plain http:// WITHOUT a caBundleSecretRef stays admissible: it claims no
// transport security, so it misleads nobody.
func TestValidateCreate_RejectsPlaintextAuthURLWithCABundleSecretRef(t *testing.T) {
	cases := []struct {
		name      string
		authURL   string
		caBundle  *commonv1.SecretRefSpec
		wantError bool
	}{
		{
			name:      "http with a CA bundle is rejected",
			authURL:   "http://keystone.example.com/v3",
			caBundle:  &commonv1.SecretRefSpec{Name: "keystone-ca"},
			wantError: true,
		},
		{
			name:     "https with a CA bundle is accepted",
			authURL:  "https://keystone.example.com/v3",
			caBundle: &commonv1.SecretRefSpec{Name: "keystone-ca"},
		},
		{
			name:    "http without a CA bundle is accepted",
			authURL: "http://keystone.example.com/v3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := externalControlPlane()
			cp.Spec.Services.Keystone.External.AuthURL = tc.authURL
			cp.Spec.Services.Keystone.External.CABundleSecretRef = tc.caBundle

			_, err := w.ValidateCreate(context.Background(), cp)
			if !tc.wantError {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.keystone.external.authURL"))
			g.Expect(err.Error()).To(ContainSubstring("must use scheme https when caBundleSecretRef is set"))
		})
	}
}

// TestValidateCreate_AccumulatesAllExternalModeErrors puts every External-mode
// rule into a broken state at once (external missing, infrastructure present,
// horizon present, all six managed-only fields set) and asserts the returned
// error names every field, pinning the no-short-circuit contract for the matrix.
// --- External-mode catalog stewardship (spec.services.keystone.external.catalog) ---

// TestValidateCreate_AcceptsExternalCatalogSpec proves the whole catalog surface
// admits with non-default values: a disambiguation filter plus a managed entry
// carrying two distinct interfaces.
func TestValidateCreate_AcceptsExternalCatalogSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		IdentityServiceName: "keystone-legacy",
		ManagedEntries: []ExternalCatalogEntrySpec{{
			Type: "image",
			Name: "glance",
			Endpoints: []ExternalCatalogEndpointSpec{
				{Interface: ExternalEndpointTypePublic, URL: "https://glance.example.com"},
				{Interface: ExternalEndpointTypeInternal, URL: "http://glance.svc:9292"},
			},
		}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsIdentityManagedCatalogEntry pins the conservative
// invariant: the identity entry is import-owned, so declaring it as a managed
// entry — the one way the opt-in could clobber the external catalog's own
// identity row — is refused.
func TestValidateCreate_RejectsIdentityManagedCatalogEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: "identity"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("owned by the External-mode imports"))
}

func TestValidateCreate_RejectsDuplicateCatalogEntryTypes(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: "image"}, {Type: "image"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[1].type"))
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
}

func TestValidateCreate_RejectsInvalidCatalogEntryType(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: "Image_Service"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("DNS-1123 label"))
}

// TestValidateCreate_RejectsEmptyCatalogEntryType covers the zero-value edge the
// CRD MinLength marker guards, for a caller that bypassed schema admission.
func TestValidateCreate_RejectsEmptyCatalogEntryType(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: ""}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].type"))
}

func TestValidateCreate_RejectsBadCatalogEndpointURL(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for _, tc := range []struct {
		name    string
		url     string
		wantMsg string
	}{
		{"no scheme", "glance.example.com", "http(s) URL"},
		{"no host", "https://", "must include a host"},
		{"empty", "", "http(s) URL"},
		{"over the K-ORC cap", "https://glance.example.com/" + strings.Repeat("a", 1024), "at most 1024 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := externalControlPlane()
			cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
				ManagedEntries: []ExternalCatalogEntrySpec{{
					Type:      "image",
					Endpoints: []ExternalCatalogEndpointSpec{{Interface: ExternalEndpointTypePublic, URL: tc.url}},
				}},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].endpoints[0].url"))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
		})
	}
}

func TestValidateCreate_RejectsDuplicateCatalogEndpointInterface(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{
			Type: "image",
			Endpoints: []ExternalCatalogEndpointSpec{
				{Interface: ExternalEndpointTypePublic, URL: "https://a.example.com"},
				{Interface: ExternalEndpointTypePublic, URL: "https://b.example.com"},
			},
		}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].endpoints[1].interface"))
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
}

// TestValidateCreate_RejectsEmptyCatalogEndpointInterface covers the zero-value
// edge: an endpoint with no interface would otherwise be projected onto K-ORC's
// required Interface field.
func TestValidateCreate_RejectsEmptyCatalogEndpointInterface(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{
			Type:      "image",
			Endpoints: []ExternalCatalogEndpointSpec{{URL: "https://glance.example.com"}},
		}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].endpoints[0].interface"))
}

// TestValidateCreate_RejectsOffEnumCatalogEndpointInterface closes the gap the
// CRD enum alone leaves for a caller that bypasses schema admission: the
// interface is embedded verbatim in a child CR name, so an off-enum value like
// "Public" yields a name that is not a DNS-1123 subdomain and wedges the
// reconcile in backoff instead of failing at admission.
func TestValidateCreate_RejectsOffEnumCatalogEndpointInterface(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{
			Type:      "image",
			Endpoints: []ExternalCatalogEndpointSpec{{Interface: "Public", URL: "https://glance.example.com"}},
		}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].endpoints[0].interface"))
	g.Expect(err.Error()).To(ContainSubstring("Unsupported value"))
}

// TestValidateCreate_RejectsCatalogEntryNameWithComma pins the K-ORC parity the
// entry name previously lacked: it is cast to K-ORC's OpenStackName, whose own
// CRD Pattern is `^[^,]+$`. A comma admitted here would be rejected by the K-ORC
// CRD when the child Service CR is submitted, wedging the reconcile.
func TestValidateCreate_RejectsCatalogEntryNameWithComma(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: "image", Name: "glance,v2"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].name"))
	g.Expect(err.Error()).To(ContainSubstring("must not contain a comma"))
}

// TestValidateCreate_RejectsTooManyManagedCatalogEntries mirrors the MaxItems
// marker for a schema-bypassing caller: each entry amplifies into managed K-ORC
// CRs and therefore into writes against a third-party production Keystone.
func TestValidateCreate_RejectsTooManyManagedCatalogEntries(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	entries := make([]ExternalCatalogEntrySpec, maxManagedCatalogEntries+1)
	for i := range entries {
		entries[i] = ExternalCatalogEntrySpec{Type: fmt.Sprintf("svc-%d", i)}
	}
	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{ManagedEntries: entries}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries"))
	g.Expect(err.Error()).To(ContainSubstring("must have at most 32 items"))
}

// TestValidateCreate_RejectsOverlongCatalogEntryChildName covers the rule no CRD
// marker can express: metadata.name on the ControlPlane is bounded only by the
// apiserver's 253 bytes, and the child K-ORC CR name composes it with the entry
// type. Admitting the pair and discovering the overflow at CreateOrUpdate leaves
// the ControlPlane wedged in CatalogEntryError backoff.
func TestValidateCreate_RejectsOverlongCatalogEntryChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", 240)
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: "image"}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("managedEntries[0].type"))
	g.Expect(err.Error()).To(ContainSubstring("253-byte Kubernetes object-name limit"))
}

// TestValidateCreate_AcceptsCatalogEntryChildNameAtTheLimit is the other side of
// the bound: a child name of exactly 253 bytes is admissible, so the checks reject
// only what the apiserver would. BOTH composed names sit exactly on the limit
// here — the identity Endpoint import, which External mode creates whatever the
// catalog block says, is the binding constraint on metadata.name, and it leaves
// the entry type exactly the room the entry-child check permits.
func TestValidateCreate_AcceptsCatalogEntryChildNameAtTheLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", maxObjectNameBytes-identityImportChildNameOverhead)
	entryType := strings.Repeat("b", maxObjectNameBytes-catalogEntryChildNameOverhead-len(cp.Name))
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		ManagedEntries: []ExternalCatalogEntrySpec{{Type: entryType}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsOverlongIdentityImportChildName pins the guard the
// entry-child check alone left open: External mode composes
// "{cp}-identity-endpoint-internal" unconditionally, so a ControlPlane name that
// fits every declared entry — or one with no managedEntries at all, where the
// entry check never runs — can still overflow the apiserver's 253-byte
// metadata.name cap and wedge ensureExternalCatalogImports in ImportError backoff.
func TestValidateCreate_RejectsOverlongIdentityImportChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	// One byte past the bound, and no catalog block at all: the entry-child check
	// cannot fire, so only the mode-level guard stands between this CR and the wedge.
	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", maxObjectNameBytes-identityImportChildNameOverhead+1)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("metadata.name"))
	g.Expect(err.Error()).To(ContainSubstring("identity Endpoint import CR name would be 254 bytes"))
}

// TestValidateCreate_ManagedModeAcceptsLongName proves the identity-import guard
// is scoped to the mode that creates those imports: Managed mode composes only
// "{cp}-identity-endpoint", so a name the External guard rejects stays admissible.
func TestValidateCreate_ManagedModeAcceptsLongName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := managedControlPlane()
	cp.Name = strings.Repeat("a", maxObjectNameBytes-identityImportChildNameOverhead+1)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsIdentityServiceNameWithComma closes the parity gap
// managedEntries[].name had already closed: identityServiceName is cast to K-ORC's
// OpenStackName on the Service import filter, whose own CRD Pattern is `^[^,]+$`.
// A comma admitted here is rejected by the K-ORC CRD when the import CR is
// submitted, wedging the reconcile in ImportError backoff instead of failing at
// admission with a field the operator can act on.
func TestValidateCreate_RejectsIdentityServiceNameWithComma(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{IdentityServiceName: "keystone,v3"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("catalog.identityServiceName"))
	g.Expect(err.Error()).To(ContainSubstring("must not contain a comma"))
}

// TestValidateCreate_ExternalCatalogIgnoredOutsideExternalMode proves the catalog
// block needs no dedicated Managed-mode rule: it lives under `external`, which is
// already forbidden outside External mode, so the existing rule catches it.
func TestValidateCreate_ExternalCatalogIgnoredOutsideExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := managedControlPlane()
	cp.Spec.Services.Keystone.External = &ExternalKeystoneSpec{
		AuthURL: "https://keystone.example.com/v3",
		Catalog: &ExternalCatalogSpec{ManagedEntries: []ExternalCatalogEntrySpec{{Type: "identity"}}},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("may only be set when services.keystone.mode is External"))
}

func TestValidateCreate_AccumulatesAllExternalModeErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	replicas := int32(3)

	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", 240)       // the identity Endpoint import name overflows 253 bytes
	cp.Spec.Services.Keystone.External = nil // external missing
	cp.Spec.Services.Keystone.Replicas = &replicas
	cp.Spec.Services.Keystone.Image = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{Rules: map[string]string{"a": "b"}}
	cp.Spec.Services.Keystone.RotationInterval = &metav1.Duration{Duration: 24 * time.Hour}
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{Hostname: "k.example.com"}
	cp.Spec.Services.Keystone.PublicEndpoint = "https://k.example.com/v3"
	cp.Spec.Services.Keystone.FederationProxyImage = &commonv1.ImageSpec{Repository: "r", Tag: "t"}
	cp.Spec.Infrastructure = &InfrastructureSpec{
		Database: commonv1.DatabaseSpec{Host: "db", Database: "d", SecretRef: commonv1.SecretRefSpec{Name: "s"}},
		Cache:    commonv1.CacheSpec{Backend: "b", Servers: []string{"mc:11211"}},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("identity Endpoint import CR name"), "import-child-name error must be present")
	g.Expect(msg).To(ContainSubstring("external is required"), "external-required error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.replicas"), "replicas-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.image"), "image-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.policyOverrides"), "policyOverrides-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.rotationInterval"), "rotationInterval-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.gateway"), "gateway-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.publicEndpoint"), "publicEndpoint-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.keystone.federationProxyImage"), "federationProxyImage-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("spec.infrastructure"), "infrastructure-forbidden error must be present")
	g.Expect(msg).To(ContainSubstring("services.horizon"), "horizon-forbidden error must be present")
}

// TestValidateCreate_FederationProxyImageDefenseInDepth covers the Managed-mode
// image checks that mirror the commonv1.ImageSpec markers: they surface on the
// ControlPlane the operator edits rather than as an opaque projection rejection
// on the Keystone child.
func TestValidateCreate_FederationProxyImageDefenseInDepth(t *testing.T) {
	w := &ControlPlaneWebhook{}

	tests := []struct {
		name       string
		image      *commonv1.ImageSpec
		wantSubstr string
	}{
		{"empty repository", &commonv1.ImageSpec{Tag: "dev"}, "federationProxyImage.repository must be set"},
		{"neither tag nor digest", &commonv1.ImageSpec{Repository: "r"}, "exactly one of federationProxyImage.tag or federationProxyImage.digest"},
		{
			"both tag and digest",
			&commonv1.ImageSpec{Repository: "r", Tag: "dev", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
			"exactly one of federationProxyImage.tag or federationProxyImage.digest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Keystone.FederationProxyImage = tc.image

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSubstr))
		})
	}
}

// TestValidateCreate_AcceptsDigestPinnedFederationProxyImage pins the happy
// path the override exists for: an immutable digest pin, and a locally built
// tag for the e2e suite.
func TestValidateCreate_AcceptsDigestPinnedFederationProxyImage(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, image := range map[string]*commonv1.ImageSpec{
		"digest pin": {
			Repository: "ghcr.io/c5c3/keystone-federation-proxy",
			Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		"local tag": {Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "dev"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Keystone.FederationProxyImage = image

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestValidateCreate_HorizonPublicEndpointMustBeURL covers the defense-in-depth
// URL parse: Keystone matches the derived WebSSO origin verbatim, so a value
// with no host could never match any dashboard.
func TestValidateCreate_HorizonPublicEndpointMustBeURL(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"missing host": "https://",
		"wrong scheme": "ftp://horizon.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: endpoint}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.horizon.publicEndpoint"))
		})
	}
}

// TestValidateCreate_HorizonPublicEndpointMustBeABareOrigin covers the shapes the
// ^https?:// Pattern marker lets through and validateHTTPURL happily parses: the
// derived origin (publicEndpoint + "/auth/websso/") is projected onto the
// Keystone child's trusted_dashboard, which Keystone compares byte-for-byte
// against what the dashboard sends — so a path, query or fragment produces an
// origin that matches nothing, and every federated login fails only AFTER the
// user has authenticated at the identity provider.
//
// The gateway is deliberately left unset in each case: the rule holds on the
// gateway-less path too, which is where the scheme/host rules stop applying.
func TestValidateCreate_HorizonPublicEndpointMustBeABareOrigin(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, endpoint := range map[string]string{
		"query":    "https://horizon.example.com?utm=1",
		"fragment": "https://horizon.example.com#top",
		"path":     "https://horizon.example.com/dashboard",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: endpoint}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.horizon.publicEndpoint"))
			g.Expect(err.Error()).To(ContainSubstring("must be a bare origin"))
		})
	}
}

// TestValidateCreate_AcceptsHorizonPublicEndpointWithPort pins the case the
// override exists for: a dashboard published off the default HTTPS port. The
// trailing-slash form is accepted too — horizonPublicEndpoint trims it before
// appending the WebSSO path.
func TestValidateCreate_AcceptsHorizonPublicEndpointWithPort(t *testing.T) {
	for _, endpoint := range []string{
		"https://horizon.example.com:8443",
		"https://horizon.example.com:8443/",
	} {
		t.Run(endpoint, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: endpoint}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

// TestValidateCreate_RejectsUnusableGatewayHostname guards the shapes the
// reconciler cannot derive a browser-facing origin from. The check lives on
// BOTH service blocks because both hostnames feed a projection: horizon's the
// Keystone child's trusted_dashboard, keystone's the Horizon child's
// websso.keystoneURL. Without it a control character in a horizon field is
// caught only by the KEYSTONE child's webhook, taking a healthy Keystone
// projection down with an error naming neither the field nor the ControlPlane.
func TestValidateCreate_RejectsUnusableGatewayHostname(t *testing.T) {
	w := &ControlPlaneWebhook{}

	hostnames := map[string]string{
		"control character": "horizon.example.com\nx",
		"wildcard":          "*.example.com",
		"embedded port":     "horizon.example.com:8443",
		"carries a path":    "horizon.example.com/dashboard",
		"carries a scheme":  "https://horizon.example.com",
	}
	for name, hostname := range hostnames {
		t.Run("horizon/"+name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{
				Gateway: &commonv1.GatewaySpec{
					Hostname:  hostname,
					ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
				},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.horizon.gateway.hostname"))
		})

		t.Run("keystone/"+name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := validControlPlane()
			cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
				Hostname:  hostname,
				ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("services.keystone.gateway.hostname"))
		})
	}
}

// TestValidateCreate_AcceptsBareGatewayHostname pins the happy path: a concrete
// DNS name, the only shape the derived origins can be built from.
func TestValidateCreate_AcceptsBareGatewayHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		Hostname:  "keystone.127-0-0-1.nip.io",
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{
			Hostname:  "horizon.127-0-0-1.nip.io",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsOverlongGatewayHostname guards the derived origins
// against a hostname long enough to overrun the children's own MaxLength
// markers: the API server would reject a projected child the operator never
// wrote, wedging the ControlPlane behind a field name that appears nowhere in
// its spec.
func TestValidateCreate_RejectsOverlongGatewayHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	hostname := strings.Repeat("a", maxGatewayHostnameLen-len(".example.com")+1) + ".example.com"
	g.Expect(hostname).To(HaveLen(maxGatewayHostnameLen + 1))

	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Gateway: &commonv1.GatewaySpec{
			Hostname:  hostname,
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.gateway.hostname"))
	g.Expect(err.Error()).To(ContainSubstring("maximum DNS name length"))
}

// TestValidateCreate_HorizonPublicEndpointMustAgreeWithGateway pins the rule the
// field's own godoc documents: Django derives the WebSSO origin it sends from
// the request Host header — i.e. from gateway.hostname — and Keystone compares
// it verbatim. A divergent host is rejected only after the user has already
// typed their corporate password into the IdP, with nothing on the ControlPlane
// recording why.
func TestValidateCreate_HorizonPublicEndpointMustAgreeWithGateway(t *testing.T) {
	w := &ControlPlaneWebhook{}
	gateway := func() *commonv1.GatewaySpec {
		return &commonv1.GatewaySpec{
			Hostname:  "horizon.example.com",
			ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		}
	}

	t.Run("divergent host", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Gateway:        gateway(),
			PublicEndpoint: "https://dashboard.example.com",
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.horizon.publicEndpoint"))
		g.Expect(err.Error()).To(ContainSubstring(`must equal services.horizon.gateway.hostname "horizon.example.com"`))
	})

	t.Run("http scheme behind a TLS-terminating gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Gateway:        gateway(),
			PublicEndpoint: "http://horizon.example.com",
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("scheme must be https"))
	})

	t.Run("matching host with a non-default port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{
			Gateway:        gateway(),
			PublicEndpoint: "https://horizon.example.com:8443",
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "Gateway API hostnames carry no port, so the port may only differ")
	})
}

// TestValidateCreate_WarnsOnCleartextHorizonPublicEndpoint covers the gateway-less
// dashboard, where an http origin is a legal (if unwise) development setup that
// the CRD Pattern deliberately allows. Keystone POSTs the unscoped WebSSO token
// to that origin, so the downgrade must at least be surfaced.
func TestValidateCreate_WarnsOnCleartextHorizonPublicEndpoint(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: "http://horizon.example.com"}

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(1))
		g.Expect(warnings[0]).To(ContainSubstring("bearer token in cleartext"))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := validControlPlane()
		cp.Spec.Services.Horizon = &ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com"}

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// TestValidateCreate_AccumulatesAllExternalCatalogErrors is the catalog analogue
// of the accumulator above, and must stay separate from it: the catalog block
// hangs off `external`, which that test deliberately nils out to exercise the
// external-required rule, so no catalog path is reachable there. Every catalog
// rule is broken at once here, proving validateExternalCatalog accumulates rather
// than short-circuits on the first offending entry.
func TestValidateCreate_AccumulatesAllExternalCatalogErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := externalControlPlane()
	cp.Name = strings.Repeat("a", 240) // every composed child CR name overflows 253 bytes
	cp.Spec.Services.Keystone.External.Catalog = &ExternalCatalogSpec{
		IdentityServiceName: "keystone,v3", // comma: not a K-ORC OpenStackName
		ManagedEntries: []ExternalCatalogEntrySpec{
			{Type: "identity"},                         // import-owned
			{Type: "Image_Service", Name: "glance,v2"}, // not a DNS-1123 label; comma in the name
			{ // duplicate interface, off-enum interface, unusable URL, and a duplicate of entry [0]
				Type: "identity",
				Endpoints: []ExternalCatalogEndpointSpec{
					{Interface: ExternalEndpointTypePublic, URL: "https://ok.example.com"},
					{Interface: ExternalEndpointTypePublic, URL: "not-a-url"},
					{Interface: "Public", URL: "https://off-enum.example.com"},
				},
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	msg := err.Error()
	g.Expect(msg).To(ContainSubstring("catalog.identityServiceName"), "identityServiceName-comma error must be present")
	g.Expect(msg).To(ContainSubstring("owned by the External-mode imports"), "identity-entry error must be present")
	g.Expect(msg).To(ContainSubstring("managedEntries[1].type"), "entry-type-pattern error must be present")
	g.Expect(msg).To(ContainSubstring("managedEntries[1].name"), "entry-name-comma error must be present")
	g.Expect(msg).To(ContainSubstring("managedEntries[2].type"), "duplicate-entry-type error must be present")
	g.Expect(msg).To(ContainSubstring("managedEntries[2].endpoints[1].interface"), "duplicate-interface error must be present")
	g.Expect(msg).To(ContainSubstring("managedEntries[2].endpoints[1].url"), "endpoint-URL error must be present")
	g.Expect(msg).To(ContainSubstring("managedEntries[2].endpoints[2].interface"), "off-enum-interface error must be present")
	g.Expect(msg).To(ContainSubstring("253-byte Kubernetes object-name limit"), "child-CR-name error must be present")
}

// --- Mode transition gating ---

// TestValidateUpdate_RejectsManagedToExternal verifies flipping a live managed
// ControlPlane to External mode is rejected outright.
func TestValidateUpdate_RejectsManagedToExternal(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := externalControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cannot be changed to External"))
}

// TestValidateUpdate_RejectsExternalToManaged verifies switching a live External
// ControlPlane back to Managed is rejected with the phase-3 takeover message.
func TestValidateUpdate_RejectsExternalToManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := externalControlPlane()
	newCP := managedControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("phase-3"))
}

// TestValidateUpdate_RejectsExternalToNilKeystone verifies removing the keystone
// service from a live External ControlPlane (also a move away from External) is
// rejected.
func TestValidateUpdate_RejectsExternalToNilKeystone(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := externalControlPlane()
	newCP := externalControlPlane()
	newCP.Spec.Services.Keystone = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
}

// TestValidateUpdate_AllowsNilKeystoneToManaged verifies staged adoption is
// preserved: adding a Managed keystone service to a control plane that had none
// is accepted (neither revision is External).
func TestValidateUpdate_AllowsNilKeystoneToManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	oldCP.Spec.Services.Keystone = nil
	newCP := managedControlPlane() // keystone present, mode unset (=> Managed)

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AllowsExternalUnchanged verifies a no-op update of an
// External ControlPlane (both revisions External, same spec) is accepted, so the
// gating does not over-fire on a same-mode update.
func TestValidateUpdate_AllowsExternalUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := externalControlPlane()
	newCP := externalControlPlane()

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_RejectsInfrastructurePresenceFlip verifies removing the
// infrastructure block on a mode-unchanged managed ControlPlane is rejected by
// the presence-flip guard (defense-in-depth for webhook-bypassed states).
func TestValidateUpdate_RejectsInfrastructurePresenceFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := managedControlPlane()
	newCP := managedControlPlane()
	newCP.Spec.Infrastructure = nil

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("infrastructure presence is immutable"))
}

// --- Service-account webhook tests (spec.korc.serviceAccounts) ---

// saControlPlane returns a managed ControlPlane carrying one minimal valid
// service account so tests can mutate a single aspect to exercise one rule.
func saControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Spec.KORC.ServiceAccounts = []ServiceAccountSpec{{
		Name:    "nova",
		Project: ServiceAccountProjectSpec{Name: "service"},
	}}
	return cp
}

func TestValidateCreate_AcceptsValidServiceAccount(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), saControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestDefault_MaterializesServiceAccountUserName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].UserName = ""

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.KORC.ServiceAccounts[0].UserName).To(Equal("nova"),
		"an empty userName must default to the account name")
}

func TestDefault_PreservesExplicitServiceAccountUserName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].UserName = "nova-svc"

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.KORC.ServiceAccounts[0].UserName).To(Equal("nova-svc"))
}

func TestValidateCreate_RejectsServiceAccountBadName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Name = "Nova_Service"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].name"))
}

func TestValidateCreate_RejectsServiceAccountMissingProjectName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Project.Name = ""

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].project.name"))
}

func TestValidateCreate_RejectsServiceAccountAdminIdentityCollision(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	// The admin identity defaults to user "admin" in domain "Default"; an entry
	// resolving to the same identity would take over the admin user.
	cp.Spec.KORC.ServiceAccounts[0].UserName = "admin"
	cp.Spec.KORC.ServiceAccounts[0].DomainName = "Default"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("equals the admin identity"))
}

func TestValidateCreate_RejectsServiceAccountDuplicateIdentity(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	// Two entries with distinct account names but the same effective user in the
	// same domain: they would project two managed Users onto one Keystone user.
	cp.Spec.KORC.ServiceAccounts = append(cp.Spec.KORC.ServiceAccounts, ServiceAccountSpec{
		Name:     "nova-secondary",
		UserName: "nova",
		Project:  ServiceAccountProjectSpec{Name: "service"},
	})

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[1].userName"))
}

func TestValidateCreate_RejectsServiceAccountDuplicateManagedProject(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Project = ServiceAccountProjectSpec{Name: "service", Create: true}
	cp.Spec.KORC.ServiceAccounts = append(cp.Spec.KORC.ServiceAccounts, ServiceAccountSpec{
		Name:    "glance",
		Project: ServiceAccountProjectSpec{Name: "service", Create: true},
	})

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[1].project.name"))
}

func TestValidateCreate_AcceptsTwoReferencedAccountsSharingAProject(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	// Both reference (create: false) the same project — that is legal; only two
	// create:true entries naming one project collide.
	cp.Spec.KORC.ServiceAccounts = append(cp.Spec.KORC.ServiceAccounts, ServiceAccountSpec{
		Name:    "glance",
		Project: ServiceAccountProjectSpec{Name: "service"},
	})

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateUpdate_RejectsServiceAccountUserNameChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := saControlPlane()
	oldCP.Spec.KORC.ServiceAccounts[0].UserName = "nova"
	newCP := saControlPlane()
	newCP.Spec.KORC.ServiceAccounts[0].UserName = "nova-renamed"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("userName is immutable"))
}

func TestValidateUpdate_RejectsServiceAccountProjectCreateFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := saControlPlane()
	newCP := saControlPlane()
	newCP.Spec.KORC.ServiceAccounts[0].Project.Create = true

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("project.create is immutable"))
}

func TestValidateUpdate_AcceptsServiceAccountAdoptFlip(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := saControlPlane()
	oldCP.Spec.KORC.ServiceAccounts[0].UserName = "nova"
	newCP := saControlPlane()
	newCP.Spec.KORC.ServiceAccounts[0].UserName = "nova"
	// Flipping adopt to true is the documented collision remediation — it must
	// stay mutable.
	newCP.Spec.KORC.ServiceAccounts[0].Adopt = true

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsOverlongServiceAccountChildName pins the child-CR
// name-length bound the CRD markers cannot express: metadata.name is bounded only
// by the apiserver's 253 bytes, and the longest child name a roles-less account
// mints — the password Secret "{cp}-service-account-{name}-password-vN" — composes
// it with serviceAccountChildNameOverhead and the account name. Admitting an
// overflowing pair would wedge reconcileServiceAccounts at CreateOrUpdate.
func TestValidateCreate_RejectsOverlongServiceAccountChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := saControlPlane()
	// One byte past the bound for the single "nova" account.
	cp.Name = strings.Repeat("a", maxObjectNameBytes-serviceAccountChildNameOverhead-len("nova")+1)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].name"))
	g.Expect(err.Error()).To(ContainSubstring("253-byte Kubernetes object-name limit"))
}

// TestValidateCreate_AcceptsServiceAccountChildNameAtTheLimit is the other side of
// the bound: the longest composed child name sitting exactly on 253 bytes is
// admissible, so the check rejects only what the apiserver would.
func TestValidateCreate_AcceptsServiceAccountChildNameAtTheLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := saControlPlane()
	cp.Name = strings.Repeat("a", maxObjectNameBytes-serviceAccountChildNameOverhead-len("nova"))

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsOverlongServiceAccountRoleAssignmentChildName pins the
// WIDER bound an account that declares roles pays: it also mints the managed
// RoleAssignment "{cp}-service-account-{name}-assign-{slug}", 12 bytes longer than
// the password Secret. A pair the roles-less budget admits must be rejected once
// roles are declared, or reconcileServiceAccounts wedges minting the assignment.
func TestValidateCreate_RejectsOverlongServiceAccountRoleAssignmentChildName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	// One byte past the roles bound — but comfortably inside the roles-less one.
	cp.Name = strings.Repeat("a", maxObjectNameBytes-serviceAccountRoleChildNameOverhead-len("nova")+1)

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].name"))
	g.Expect(err.Error()).To(ContainSubstring("253-byte Kubernetes object-name limit"))
}

// TestValidateCreate_AcceptsServiceAccountRoleAssignmentChildNameAtTheLimit is the
// other side of the roles bound: the RoleAssignment sitting exactly on 253 bytes is
// admissible.
func TestValidateCreate_AcceptsServiceAccountRoleAssignmentChildNameAtTheLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	cp := saControlPlane()
	cp.Spec.KORC.ServiceAccounts[0].Roles = []string{"member"}
	cp.Name = strings.Repeat("a", maxObjectNameBytes-serviceAccountRoleChildNameOverhead-len("nova"))

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateUpdate_AcceptsRolelessServiceAccountInTheRoleAssignmentBand guards the
// upgrade one-way door: projecting roles as RoleAssignments widened the account-keyed
// child-name overhead, so a ControlPlane an earlier operator level admitted can sit
// in the band between the two budgets. A roles-less account mints no assignment, so
// its name must stay admissible — otherwise EVERY later edit to that CR (an
// openStackRelease bump, an unrelated field) is rejected after the operator upgrade,
// with no escape hatch that is not itself a spec edit.
func TestValidateUpdate_AcceptsRolelessServiceAccountInTheRoleAssignmentBand(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	oldCP := saControlPlane()
	// Inside the roles-less budget, one byte past the roles budget: exactly the band
	// widening the overhead unconditionally would have made unmaintainable.
	oldCP.Name = strings.Repeat("a", maxObjectNameBytes-serviceAccountRoleChildNameOverhead-len("nova")+1)
	g.Expect(len(oldCP.Name)+serviceAccountChildNameOverhead+len("nova")).
		To(BeNumerically("<=", maxObjectNameBytes), "fixture must sit inside the roles-less budget")

	newCP := oldCP.DeepCopy()
	newCP.Spec.OpenStackRelease = "2026.1"

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsServiceAccountTargetNamespaceOwn pins that targeting
// the ControlPlane's own namespace is admissible: it always has a tenant store,
// so the own-or-dedicated rule never has to consult the dedicated set.
func TestValidateCreate_AcceptsServiceAccountTargetNamespaceOwn(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.KORC.ServiceAccounts[0].TargetNamespace = "openstack"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_AcceptsServiceAccountTargetNamespaceDedicated pins that a
// namespace already assigned to a service via spec.services.<svc>.namespace is a
// valid delivery target: it is provisioned its own openbao-tenant-store.
func TestValidateCreate_AcceptsServiceAccountTargetNamespaceDedicated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.KORC.ServiceAccounts[0].TargetNamespace = "identity"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsServiceAccountTargetNamespaceUnassigned pins the rule
// the CRD schema cannot express: a namespace this ControlPlane neither owns nor
// placed a service in has no tenant store to deliver through, and the error points
// at the remedy.
func TestValidateCreate_RejectsServiceAccountTargetNamespaceUnassigned(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.KORC.ServiceAccounts[0].TargetNamespace = "shared-services"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].targetNamespace"))
	g.Expect(err.Error()).To(ContainSubstring("assign the namespace to a service via spec.services.<svc>.namespace first"))
}

// TestValidateCreate_RejectsServiceAccountTargetNamespaceBadPattern mirrors the
// RFC-1123 Pattern marker for webhook-bypassed callers: the value names a
// Kubernetes namespace.
func TestValidateCreate_RejectsServiceAccountTargetNamespaceBadPattern(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := saControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.KORC.ServiceAccounts[0].TargetNamespace = "Shared_NS"

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].targetNamespace"))
	g.Expect(err.Error()).To(ContainSubstring("RFC-1123 label"))
}

// TestValidateUpdate_RejectsServiceAccountTargetNamespaceChange pins the per-entry
// freeze: flipping targetNamespace on a live entry would strand the delivered
// Secret, while leaving it unchanged passes.
func TestValidateUpdate_RejectsServiceAccountTargetNamespaceChange(t *testing.T) {
	base := func() *ControlPlane {
		cp := saControlPlane()
		cp.Namespace = "openstack"
		cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
		}
		cp.Spec.KORC.ServiceAccounts[0].TargetNamespace = "identity"
		return cp
	}

	t.Run("flipping targetNamespace on a live entry is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := base()
		newCP := base()
		newCP.Spec.KORC.ServiceAccounts[0].TargetNamespace = "openstack"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("serviceAccounts[0].targetNamespace"))
		g.Expect(err.Error()).To(ContainSubstring("targetNamespace is immutable"))
	})

	t.Run("an unchanged targetNamespace passes", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := base()
		newCP := base()

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

// --- Per-service dedicated backing services ---

// dedicatedControlPlane returns a ControlPlane whose Keystone service opts into a
// dedicated database AND cache, and whose Horizon dashboard opts into a dedicated
// cache. The SHARED block stays brownfield (the validControlPlane baseline), so
// the managed clusterRef names below cannot collide with a shared instance unless
// a test makes them.
func dedicatedControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		},
		Cache: &commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
			Backend:    commonv1.DefaultCacheBackend,
		},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}
	return cp
}

// TestDefault_DedicatedBackingServicesLeaves verifies a declared dedicated block
// takes the same leaf defaults as the shared one, with a managed clusterRef name
// DERIVED from the ControlPlane so it cannot collide with the shared instance,
// and with credentialsMode materialized to Static (a dedicated managed database
// cannot draw engine-issued credentials).
func TestDefault_DedicatedBackingServicesLeaves(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "prod"
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{},
		Cache:    &commonv1.CacheSpec{},
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{Cache: &commonv1.CacheSpec{}},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Keystone.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).NotTo(BeNil())
	g.Expect(db.ClusterRef.Name).To(Equal("prod" + DedicatedKeystoneDatabaseClusterRefSuffix))
	g.Expect(db.Database).To(Equal(DefaultDatabaseName))
	g.Expect(db.SecretRef.Name).To(Equal(DefaultDatabaseSecretName))
	g.Expect(db.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic),
		"a dedicated managed database is Static-only: no per-instance OpenBao engine role exists")

	ksCache := cp.Spec.Services.Keystone.DedicatedBackingServices.Cache
	g.Expect(ksCache.ClusterRef).NotTo(BeNil())
	g.Expect(ksCache.ClusterRef.Name).To(Equal("prod" + DedicatedKeystoneCacheClusterRefSuffix))
	g.Expect(ksCache.Backend).To(Equal(DefaultCacheBackend))

	hzCache := cp.Spec.Services.Horizon.DedicatedBackingServices.Cache
	g.Expect(hzCache.ClusterRef).NotTo(BeNil())
	g.Expect(hzCache.ClusterRef.Name).To(Equal("prod" + DedicatedHorizonCacheClusterRefSuffix))

	// The derived names must differ from each other AND from the shared defaults,
	// otherwise two instances would resolve to one child CR.
	g.Expect(db.ClusterRef.Name).NotTo(Equal(DefaultDatabaseClusterRefName))
	g.Expect(ksCache.ClusterRef.Name).NotTo(Equal(hzCache.ClusterRef.Name))
	g.Expect(ksCache.ClusterRef.Name).NotTo(Equal(DefaultCacheClusterRefName))

	// Defaulting must be idempotent on the dedicated leaves too.
	before := cp.DeepCopy()
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services).To(Equal(before.Spec.Services))
}

// TestDefault_DoesNotInventDedicatedBackingServices pins the shared-by-default
// contract: a service that does not opt in must come out of the defaulting
// webhook with NO dedicated block, so its projection keeps using the
// ControlPlane-wide instances.
func TestDefault_DoesNotInventDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	g.Expect(cp.Spec.Services.Keystone.DedicatedBackingServices).To(BeNil())
	g.Expect(cp.Spec.Services.Horizon.DedicatedBackingServices).To(BeNil())
	g.Expect(cp.DedicatedKeystoneDatabase()).To(BeNil())
	g.Expect(cp.DedicatedKeystoneCache()).To(BeNil())
	g.Expect(cp.DedicatedHorizonCache()).To(BeNil())
}

// TestDefault_DedicatedBrownfieldNotCoercedIntoManaged mirrors
// TestDefault_DoesNotInventModeForBrownfield for the dedicated blocks: an
// explicit external endpoint must never grow a managed clusterRef, which would
// make the block fail its own XOR check.
func TestDefault_DedicatedBrownfieldNotCoercedIntoManaged(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{
		Database: &commonv1.DatabaseSpec{Host: "keystone-db.example.com", Port: 3306},
		Cache:    &commonv1.CacheSpec{Servers: []string{"keystone-mc:11211"}},
	}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())

	db := cp.Spec.Services.Keystone.DedicatedBackingServices.Database
	g.Expect(db.ClusterRef).To(BeNil(), "a brownfield dedicated database must not grow a managed clusterRef")
	g.Expect(db.CredentialsMode).To(BeEmpty(), "Static is only materialized for a MANAGED dedicated database")
	g.Expect(cp.Spec.Services.Keystone.DedicatedBackingServices.Cache.ClusterRef).To(BeNil())

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred(), "a brownfield dedicated instance must survive its own XOR check")
}

func TestValidateCreate_AcceptsDedicatedBackingServices(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}

	_, err := w.ValidateCreate(context.Background(), dedicatedControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsEmptyDedicatedBlock rejects an opt-in that requests
// nothing — the webhook mirror of the at-least-one-class CEL rule.
func TestValidateCreate_RejectsEmptyDedicatedBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("at least one backing-service class"))
}

// TestValidateCreate_RejectsDedicatedDatabaseXOR verifies the dedicated database
// inherits the managed-vs-brownfield XOR of the shared block: both modes set, or
// neither, is rejected.
func TestValidateCreate_RejectsDedicatedDatabaseXOR(t *testing.T) {
	tests := []struct {
		name string
		db   commonv1.DatabaseSpec
	}{
		{name: "both clusterRef and host", db: commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-db"},
			Host:       "db.example.com",
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		}},
		{name: "neither clusterRef nor host", db: commonv1.DatabaseSpec{
			Database:  "keystone",
			SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			db := tc.db
			cp.Spec.Services.Keystone.DedicatedBackingServices = &KeystoneDedicatedBackingServicesSpec{Database: &db}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("exactly one of clusterRef or host must be set"))
			g.Expect(err.Error()).To(ContainSubstring("dedicatedBackingServices.database"))
		})
	}
}

// TestValidateCreate_RejectsDedicatedCacheXOR is the cache twin of
// TestValidateCreate_RejectsDedicatedDatabaseXOR, on the Horizon block so both
// services' dedicated caches are covered.
func TestValidateCreate_RejectsDedicatedCacheXOR(t *testing.T) {
	tests := []struct {
		name  string
		cache commonv1.CacheSpec
	}{
		{name: "both clusterRef and servers", cache: commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "cp-horizon-cache"},
			Servers:    []string{"mc:11211"},
			Backend:    commonv1.DefaultCacheBackend,
		}},
		{name: "neither clusterRef nor servers", cache: commonv1.CacheSpec{
			Backend: commonv1.DefaultCacheBackend,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			cp := validControlPlane()
			cache := tc.cache
			cp.Spec.Services.Horizon = &ServiceHorizonSpec{
				DedicatedBackingServices: &HorizonDedicatedBackingServicesSpec{Cache: &cache},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("exactly one of clusterRef or servers must be set"))
			g.Expect(err.Error()).To(ContainSubstring("horizon.dedicatedBackingServices.cache"))
		})
	}
}

// TestValidateCreate_RejectsDynamicCredentialsOnDedicatedDatabase pins the one
// constraint a dedicated database carries that the shared block does not: the
// OpenBao database engine is bootstrapped once per NAMESPACE against the shared
// cluster, so no engine role can issue credentials for a dedicated instance and
// an admitted Dynamic dedicated database would wedge on an ExternalSecret that
// can never sync.
func TestValidateCreate_RejectsDynamicCredentialsOnDedicatedDatabase(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := dedicatedControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode Dynamic is not supported on a dedicated database"))
}

// TestValidateCreate_RejectsDedicatedDatabaseReplicasTwo verifies the
// Galera-quorum rule applies to a dedicated database exactly as it does to the
// shared one — the projection that makes 2 unsafe is the same.
func TestValidateCreate_RejectsDedicatedDatabaseReplicasTwo(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := dedicatedControlPlane()
	cp.Spec.Services.Keystone.DedicatedBackingServices.Database.Replicas = 2

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("2 cannot hold a majority"))
	g.Expect(err.Error()).To(ContainSubstring("dedicatedBackingServices.database.replicas"))
}

// TestValidateCreate_RejectsDedicatedClusterRefCollision covers both collision
// axes: a dedicated instance named like the SHARED one, and two services'
// dedicated instances of the same class named alike. Either would make two
// projections resolve to one child CR and silently void the isolation the opt-in
// exists for.
func TestValidateCreate_RejectsDedicatedClusterRefCollision(t *testing.T) {
	t.Run("dedicated database collides with the shared database", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		cp := dedicatedControlPlane()
		// Make the shared block managed, then point the dedicated database at it.
		cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
			Database:   "openstack",
			SecretRef:  commonv1.SecretRefSpec{Name: "db-creds"},
		}
		cp.Spec.Services.Keystone.DedicatedBackingServices.Database.ClusterRef = &corev1.LocalObjectReference{Name: "openstack-db"}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
		g.Expect(err.Error()).To(ContainSubstring("openstack-db"))
	})

	t.Run("two dedicated caches collide with each other", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		cp := dedicatedControlPlane()
		cp.Spec.Services.Horizon.DedicatedBackingServices.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "cp-keystone-cache"}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
		g.Expect(err.Error()).To(ContainSubstring("cp-keystone-cache"))
	})
}

// TestValidateCreate_RejectsDedicatedInExternalMode verifies the webhook mirror of
// the External-mode CEL forbid rule: an External ControlPlane provisions no
// backing services at all, so there is nothing to make dedicated.
func TestValidateCreate_RejectsDedicatedInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &ServiceKeystoneSpec{
		Mode:     KeystoneModeExternal,
		External: &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
		DedicatedBackingServices: &KeystoneDedicatedBackingServicesSpec{
			Cache: &commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: "cp-keystone-cache"},
				Backend:    commonv1.DefaultCacheBackend,
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.dedicatedBackingServices"))
	g.Expect(err.Error()).To(ContainSubstring("no backing services are provisioned at all"))
}

// TestValidateUpdate_RejectsDedicatedPresenceFlip pins the transition freeze: a
// live service cannot be moved between shared and dedicated backing services, in
// either direction, at either granularity (the whole block, or one class within
// it). The freeze is webhook-only precisely so a later transition feature can
// relax it to a gated migration.
func TestValidateUpdate_RejectsDedicatedPresenceFlip(t *testing.T) {
	tests := []struct {
		name           string
		oldCP, newCP   func() *ControlPlane
		wantSubstrings []string
	}{
		{
			name:  "shared -> dedicated block",
			oldCP: validControlPlane,
			newCP: dedicatedControlPlane,
		},
		{
			name:  "dedicated -> shared block",
			oldCP: dedicatedControlPlane,
			newCP: validControlPlane,
		},
		{
			name: "adding a dedicated class to an existing block",
			oldCP: func() *ControlPlane {
				cp := dedicatedControlPlane()
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database = nil
				return cp
			},
			newCP:          dedicatedControlPlane,
			wantSubstrings: []string{"dedicatedBackingServices.database"},
		},
		{
			name:  "removing a dedicated class from an existing block",
			oldCP: dedicatedControlPlane,
			newCP: func() *ControlPlane {
				cp := dedicatedControlPlane()
				cp.Spec.Services.Keystone.DedicatedBackingServices.Cache = nil
				return cp
			},
			wantSubstrings: []string{"dedicatedBackingServices.cache"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}

			_, err := w.ValidateUpdate(context.Background(), tc.oldCP(), tc.newCP())
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(
				"switching a service between shared and dedicated backing services",
			))
			for _, want := range tc.wantSubstrings {
				g.Expect(err.Error()).To(ContainSubstring(want))
			}
		})
	}
}

// TestValidateUpdate_RejectsDedicatedLeafChanges verifies a dedicated instance
// that stays declared is frozen on the same create-only leaves as the shared
// block: renaming its child CR, its logical database, or reshaping its topology
// would orphan the live instance or drive a destructive update on it.
func TestValidateUpdate_RejectsDedicatedLeafChanges(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(cp *ControlPlane)
		wantMsg string
	}{
		{
			name: "managed clusterRef rename",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.ClusterRef = &corev1.LocalObjectReference{Name: "renamed-db"}
			},
			wantMsg: "managed database clusterRef.name is immutable",
		},
		{
			name: "database name change",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.Database = "keystone2"
			},
			wantMsg: "database name is immutable",
		},
		{
			name: "replicas change",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.Replicas = 3
			},
			wantMsg: "database replicas is immutable after creation",
		},
		{
			name: "storageSize change",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Keystone.DedicatedBackingServices.Database.StorageSize = "512Mi"
			},
			wantMsg: "database storageSize is immutable after creation",
		},
		{
			name: "cache mode flip",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon.DedicatedBackingServices.Cache.ClusterRef = nil
				cp.Spec.Services.Horizon.DedicatedBackingServices.Cache.Servers = []string{"mc:11211"}
			},
			wantMsg: "cache mode (managed clusterRef vs brownfield servers) is immutable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			oldCP := dedicatedControlPlane()
			// Pin storageSize on the old object so the "" -> default normalization
			// does not mask the resize test.
			oldCP.Spec.Services.Keystone.DedicatedBackingServices.Database.StorageSize = "100Gi"
			newCP := dedicatedControlPlane()
			newCP.Spec.Services.Keystone.DedicatedBackingServices.Database.StorageSize = "100Gi"
			tc.mutate(newCP)

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
		})
	}
}

// TestValidateUpdate_AcceptsDedicatedCacheReplicasChange pins the other half of
// the immutability contract — only what genuinely cannot change is frozen. A
// cache replica count is reconciled in place on the owned Memcached (scaling a
// cache loses no data), so it stays mutable on a dedicated instance exactly as it
// is on the shared one.
func TestValidateUpdate_AcceptsDedicatedCacheReplicasChange(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	oldCP := dedicatedControlPlane()
	oldCP.Spec.Services.Keystone.DedicatedBackingServices.Cache.Replicas = 1
	newCP := dedicatedControlPlane()
	newCP.Spec.Services.Keystone.DedicatedBackingServices.Cache.Replicas = 3

	_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- per-service namespace validation (issue #646) ---

// namespacedControlPlane is the baseline for the per-service namespace tests: a
// ControlPlane in "openstack" that places Keystone in an operator-owned
// namespace and the dashboard in a pre-existing one.
func namespacedControlPlane() *ControlPlane {
	cp := validControlPlane()
	cp.Name = "cp"
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name:      "identity",
		Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Namespace: &ServiceNamespaceSpec{
			Name:      "dashboard",
			Lifecycle: ServiceNamespaceLifecycleExternal,
		},
	}
	return cp
}

// TestDefault_ServiceNamespaceLifecycle verifies the defaulting webhook
// materializes the Managed lifecycle on a DECLARED assignment — and never
// invents an assignment for a service that declared none.
func TestDefault_ServiceNamespaceLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{Name: "identity"}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{}

	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Keystone.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleManaged))
	g.Expect(cp.Spec.Services.Horizon.Namespace).To(BeNil())

	// Idempotent, and an explicit lifecycle is preserved.
	cp.Spec.Services.Keystone.Namespace.Lifecycle = ServiceNamespaceLifecycleExternal
	g.Expect(w.Default(context.Background(), cp)).To(Succeed())
	g.Expect(cp.Spec.Services.Keystone.Namespace.Lifecycle).To(Equal(ServiceNamespaceLifecycleExternal))
}

// TestValidateCreate_AcceptsServiceNamespaces verifies the baseline shape — one
// Managed and one External assignment — is admissible.
func TestValidateCreate_AcceptsServiceNamespaces(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	_, err := w.ValidateCreate(context.Background(), namespacedControlPlane())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidateCreate_RejectsNamespaceEqualToControlPlane pins the no-op guard:
// naming the ControlPlane's own namespace is not "place it here", it is the
// shape that would make the operator claim ownership of — and at teardown delete
// — the namespace the ControlPlane itself lives in.
func TestValidateCreate_RejectsNamespaceEqualToControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{Name: "openstack"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.namespace.name"))
	g.Expect(err.Error()).To(ContainSubstring("must differ from the ControlPlane's own namespace"))
}

// TestValidateCreate_RejectsInvalidNamespaceName mirrors the RFC-1123 Pattern
// marker for webhook-bypassed callers: the value names a Kubernetes namespace.
func TestValidateCreate_RejectsInvalidNamespaceName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{Name: "Identity_NS"}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("RFC-1123 label"))
}

// TestValidateCreate_RejectsNamespaceLifecycleConflict pins the co-location rule:
// services sharing a namespace share its backing services and its tenant store,
// so they cannot disagree on who owns it — one declaration would have teardown
// delete the namespace the other declared untouchable.
func TestValidateCreate_RejectsNamespaceLifecycleConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Namespace = "openstack"
	cp.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
		Name: "services", Lifecycle: ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Horizon = &ServiceHorizonSpec{
		Namespace: &ServiceNamespaceSpec{Name: "services", Lifecycle: ServiceNamespaceLifecycleExternal},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.horizon.namespace.lifecycle"))
	g.Expect(err.Error()).To(ContainSubstring("must declare the same lifecycle"))
}

// TestValidateCreate_RejectsNamespaceInExternalMode verifies the webhook mirror
// of the External-mode CEL forbid rule: no Keystone workload is deployed, so
// there is nothing to place.
func TestValidateCreate_RejectsNamespaceInExternalMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Infrastructure = nil
	cp.Spec.Services.Keystone = &ServiceKeystoneSpec{
		Mode:      KeystoneModeExternal,
		External:  &ExternalKeystoneSpec{AuthURL: "https://keystone.example.com/v3"},
		Namespace: &ServiceNamespaceSpec{Name: "identity"},
	}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("services.keystone.namespace"))
	g.Expect(err.Error()).To(ContainSubstring("there is nothing to place"))
}

// TestValidateCreate_RejectsNamespaceClaimedByOtherControlPlane pins the
// tenant-key invariant in BOTH directions: a namespace already occupied by
// another ControlPlane cannot be claimed as a service namespace, and a
// ControlPlane cannot be created into a namespace another one already claims as
// its service namespace. The claim is cluster-wide — the incumbent lives in a
// different namespace than the newcomer, so a namespace-scoped List would miss it.
func TestValidateCreate_RejectsNamespaceClaimedByOtherControlPlane(t *testing.T) {
	t.Run("service namespace collides with another ControlPlane's own namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := validControlPlane()
		incumbent.Name = "other"
		incumbent.Namespace = "identity"
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
		w := &ControlPlaneWebhook{Client: c}

		_, err := w.ValidateCreate(context.Background(), namespacedControlPlane())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("services.keystone.namespace.name"))
		g.Expect(err.Error()).To(ContainSubstring("already occupied by ControlPlane \"other\""))
	})

	t.Run("service namespace collides with another ControlPlane's service namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := namespacedControlPlane()
		incumbent.Name = "other"
		incumbent.Namespace = "other-ns"
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
		w := &ControlPlaneWebhook{Client: c}

		newcomer := namespacedControlPlane()
		newcomer.Name = "newcomer"
		newcomer.Namespace = "newcomer-ns"

		_, err := w.ValidateCreate(context.Background(), newcomer)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("identity"))
		g.Expect(err.Error()).To(ContainSubstring("belongs to at most one ControlPlane"))
	})

	t.Run("own namespace collides with another ControlPlane's service namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		incumbent := namespacedControlPlane()
		incumbent.Name = "other"
		incumbent.Namespace = "other-ns"
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(incumbent).Build()
		w := &ControlPlaneWebhook{Client: c}

		// A plain ControlPlane created INTO the namespace the incumbent already
		// claims for its Keystone service.
		newcomer := validControlPlane()
		newcomer.Name = "newcomer"
		newcomer.Namespace = "identity"

		_, err := w.ValidateCreate(context.Background(), newcomer)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("metadata.namespace"))
		g.Expect(err.Error()).To(ContainSubstring("already claimed as a service namespace"))
	})

	t.Run("a ControlPlane does not collide with itself", func(t *testing.T) {
		g := NewGomegaWithT(t)
		self := namespacedControlPlane()
		c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithObjects(self).Build()
		w := &ControlPlaneWebhook{Client: c}

		g.Expect(w.validateNamespaceClaims(context.Background(), self)).NotTo(HaveOccurred())
	})
}

// TestValidateUpdate_RejectsServiceNamespaceChanges pins the create-only freeze:
// presence, name, and lifecycle are all immutable, because moving a live service
// across namespaces would strand its backing services, its tenant store, and
// every OpenBao path scoped by the old namespace.
func TestValidateUpdate_RejectsServiceNamespaceChanges(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ControlPlane)
		wantSub string
	}{
		{
			name:    "removing the assignment",
			mutate:  func(cp *ControlPlane) { cp.Spec.Services.Keystone.Namespace = nil },
			wantSub: "spec.services.keystone.namespace",
		},
		{
			name:    "renaming the namespace",
			mutate:  func(cp *ControlPlane) { cp.Spec.Services.Keystone.Namespace.Name = "identity-2" },
			wantSub: "spec.services.keystone.namespace.name",
		},
		{
			name: "flipping the lifecycle",
			mutate: func(cp *ControlPlane) {
				cp.Spec.Services.Horizon.Namespace.Lifecycle = ServiceNamespaceLifecycleManaged
			},
			wantSub: "spec.services.horizon.namespace.lifecycle",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			w := &ControlPlaneWebhook{}
			oldCP := namespacedControlPlane()
			newCP := oldCP.DeepCopy()
			tc.mutate(newCP)

			_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
			g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
		})
	}

	t.Run("adding an assignment to a live ControlPlane", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := validControlPlane()
		oldCP.Namespace = "openstack"
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Keystone.Namespace = &ServiceNamespaceSpec{
			Name: "identity", Lifecycle: ServiceNamespaceLifecycleManaged,
		}

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("the namespace a service is placed in is immutable"))
	})

	t.Run("an unchanged assignment is not rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w := &ControlPlaneWebhook{}
		oldCP := namespacedControlPlane()
		newCP := oldCP.DeepCopy()
		newCP.Spec.OpenStackRelease = "2026.1"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
	})
}
