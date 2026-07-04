// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
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
			Infrastructure: InfrastructureSpec{
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
			Infrastructure: InfrastructureSpec{
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
			Infrastructure: InfrastructureSpec{
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
			Infrastructure: InfrastructureSpec{
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
			Infrastructure: InfrastructureSpec{
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
	cp.Spec.Global = &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}

	_, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("global"))
	g.Expect(err.Error()).To(ContainSubstring("policy rule name must not be empty"))
}

func TestValidateCreate_RejectsEmptyGlobalPolicyRuleValue(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := validControlPlane()
	cp.Spec.Global = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": ""}}

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
	cp.Spec.Global = &commonv1.PolicySpec{Rules: map[string]string{"identity:get_user": "role:admin"}}
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
	cp.Spec.Global = &commonv1.PolicySpec{Rules: map[string]string{"": "role:admin"}}
	cp.Spec.Services.Keystone.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": ""},
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
