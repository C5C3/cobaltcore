// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validKeystone returns a Keystone with all required fields set to valid values.
// Tests modify this baseline to exercise specific validation rules.
func validKeystone() *Keystone {
	return &Keystone{
		Spec: KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{Host: "db.example.com", Port: 3306, Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}},
			Cache:    commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Fernet: FernetSpec{
				RotationSchedule: "0 0 * * 0",
				MaxActiveKeys:    3,
			},
			Bootstrap: BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

// --- Defaulting webhook tests (CC-0011, REQ-001) ---

func TestDefault_SetsZeroValueDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Replicas).To(Equal(int32(3)))
	g.Expect(k.Spec.Fernet.MaxActiveKeys).To(Equal(int32(3)))
	g.Expect(k.Spec.Cache.Backend).To(Equal("dogpile.cache.pymemcache"))
	g.Expect(k.Spec.Bootstrap.AdminUser).To(Equal("admin"))
	g.Expect(k.Spec.Bootstrap.Region).To(Equal("RegionOne"))
}

func TestDefault_DoesNotSetFernetRotationSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// Fernet.RotationSchedule relies on the Kubebuilder +default marker only
	// (plan decision #3, CC-0011). The webhook must NOT set it.
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Fernet.RotationSchedule).To(BeEmpty())
}

func TestDefault_ZeroValueObjectRemainsInvalid(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}
	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	// After Default() the 5 mutable fields are set, but Cache, Database,
	// and RotationSchedule (CRD-schema-defaulted, not webhook-defaulted)
	// are still zero-valued — the spec must not pass validation.
	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
	g.Expect(err.Error()).To(ContainSubstring("database"))
	g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
}

func TestDefault_PreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Replicas: 5,
			Cache:    commonv1.CacheSpec{Backend: "dogpile.cache.memcache"},
			Fernet: FernetSpec{
				RotationSchedule: "0 */6 * * *",
				MaxActiveKeys:    7,
			},
			Bootstrap: BootstrapSpec{
				AdminUser: "custom-admin",
				Region:    "EU-West",
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Replicas).To(Equal(int32(5)))
	g.Expect(k.Spec.Fernet.RotationSchedule).To(Equal("0 */6 * * *"))
	g.Expect(k.Spec.Fernet.MaxActiveKeys).To(Equal(int32(7)))
	g.Expect(k.Spec.Cache.Backend).To(Equal("dogpile.cache.memcache"))
	g.Expect(k.Spec.Bootstrap.AdminUser).To(Equal("custom-admin"))
	g.Expect(k.Spec.Bootstrap.Region).To(Equal("EU-West"))
}

func TestDefault_CacheBackendAppliedWhenEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Cache: commonv1.CacheSpec{Servers: []string{"mc:11211"}},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Cache.Backend).To(Equal("dogpile.cache.pymemcache"))
}

// --- Replicas validation tests (CC-0011, REQ-007) ---

func TestValidate_ReplicasZeroRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Replicas = 0

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("replicas"))
}

func TestValidate_ReplicasNegativeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Replicas = -1

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("replicas"))
}

func TestValidate_ReplicasOneAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Replicas = 1

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- MaxActiveKeys validation tests (CC-0011, REQ-007) ---

func TestValidate_MaxActiveKeysBelowMinimumRejected(t *testing.T) {
	w := &KeystoneWebhook{}
	cases := []struct {
		name string
		val  int32
	}{
		{"one", 1},
		{"two", 2},
		{"negative", -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.Fernet.MaxActiveKeys = tc.val
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("maxActiveKeys"))
		})
	}
}

func TestValidate_MaxActiveKeysZeroAllowed(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// Zero is allowed because Default() converts 0 → 3 before validate() runs.
	// If validate() is called outside the normal admission path with 0,
	// it should not conflict with the defaulting logic.
	k.Spec.Fernet.MaxActiveKeys = 0

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_MaxActiveKeysThreeAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Fernet.MaxActiveKeys = 3

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_MaxActiveKeysAboveMinimumAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Fernet.MaxActiveKeys = 7

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Cron validation tests (CC-0011, REQ-002) ---

func TestValidate_ValidCronExpressions(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"0 0 * * 0",    // weekly at midnight Sunday
		"*/5 * * * *",  // every 5 minutes
		"0 */6 * * *",  // every 6 hours
		"30 2 1 * *",   // 2:30 AM on the 1st of each month
		"0 0 * * 1-5",  // midnight weekdays
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.Fernet.RotationSchedule = expr
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestValidate_InvalidCronExpressions(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"not-a-cron",
		"* * *",       // too few fields
		"60 * * * *",  // minute out of range
		"* 25 * * *",  // hour out of range
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.Fernet.RotationSchedule = expr
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
		})
	}
}

func TestValidate_EmptyRotationScheduleReturnsRequiredError(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Fernet.RotationSchedule = ""

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
	g.Expect(err.Error()).To(ContainSubstring("must be set"))
}

// --- Cache mutual-exclusivity tests (CC-0011, REQ-009) ---

func TestValidate_CacheWithServersOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// validKeystone() already uses Servers-only mode.

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_CacheWithClusterRefOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	k.Spec.Cache.Servers = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_CacheBothClusterRefAndServersRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	k.Spec.Cache.Servers = []string{"mc:11211"}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

func TestValidate_CacheNeitherClusterRefNorServersRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Cache.ClusterRef = nil
	k.Spec.Cache.Servers = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
}

// --- Database mutual-exclusivity tests (CC-0011, REQ-010) ---

func TestValidate_DatabaseWithHostOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// validKeystone() already uses Host-only mode.

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_DatabaseWithClusterRefOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	k.Spec.Database.Host = ""

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_DatabaseBothClusterRefAndHostRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	k.Spec.Database.Host = "db.example.com"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

func TestValidate_DatabaseNeitherClusterRefNorHostRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Database.ClusterRef = nil
	k.Spec.Database.Host = ""

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("database"))
}

// --- Duplicate plugin detection tests (CC-0011, REQ-003) ---

func TestValidate_UniquePlugins(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "plugin-a", ConfigSection: "section_a"},
		{Name: "plugin-b", ConfigSection: "section_b"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_DuplicatePluginConfigSection(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "plugin-a", ConfigSection: "section_a"},
		{Name: "plugin-b", ConfigSection: "section_a"}, // duplicate configSection
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("configSection"))
}

func TestValidate_NoPlugins(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Plugins = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Policy source requirement tests (CC-0011, REQ-004) ---

func TestValidate_PolicyOverridesNil(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PolicyOverridesWithRulesOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": "role:admin"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PolicyOverridesWithConfigMapRefOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		ConfigMapRef: &corev1.LocalObjectReference{Name: "keystone-policy"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PolicyOverridesWithBothSources(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules:        map[string]string{"identity:get_user": "role:admin"},
		ConfigMapRef: &corev1.LocalObjectReference{Name: "keystone-policy"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PolicyOverridesWithNoSources(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("policyOverrides"))
}

// --- Empty policy rule name tests (CC-0011, REQ-008) ---

func TestValidate_EmptyPolicyRuleNameRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"": "role:admin"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("rules"))
}

func TestValidate_NonEmptyPolicyRuleNamesAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"identity:get_user":  "role:admin",
			"identity:list_user": "role:reader",
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- ValidateCreate and ValidateUpdate consistency (CC-0011, REQ-005, REQ-006) ---

func TestValidateCreate_RunsAllValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Replicas = 0
	k.Spec.Fernet.MaxActiveKeys = 1
	k.Spec.Fernet.RotationSchedule = "bad-cron"
	k.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "a", ConfigSection: "dup"},
		{Name: "b", ConfigSection: "dup"},
	}
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"": "rule:admin"},
	}
	// REQ-009 (CC-0011): Break cache mutual-exclusivity — set both clusterRef and servers.
	k.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	// REQ-010 (CC-0011): Break database mutual-exclusivity — set both clusterRef and host.
	k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	errMsg := err.Error()
	g.Expect(errMsg).To(ContainSubstring("replicas"))
	g.Expect(errMsg).To(ContainSubstring("maxActiveKeys"))
	g.Expect(errMsg).To(ContainSubstring("rotationSchedule"))
	g.Expect(errMsg).To(ContainSubstring("configSection"))
	g.Expect(errMsg).To(ContainSubstring("policyOverrides"))
	g.Expect(errMsg).To(ContainSubstring("cache"))
	g.Expect(errMsg).To(ContainSubstring("database"))
}

func TestValidateUpdate_RunsSameValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	old := validKeystone()
	updated := validKeystone()
	updated.Spec.Fernet.RotationSchedule = "invalid"

	_, err := w.ValidateUpdate(context.Background(), old, updated)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
}

func TestValidateDelete_AlwaysAllows(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()

	warnings, err := w.ValidateDelete(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeNil())
}

// --- Interface compliance (CC-0011) ---

func TestKeystoneWebhook_ImplementsInterfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	// Compile-time interface checks are in keystone_webhook.go via var _ assertions.
	// This test serves as documentation.
	var w KeystoneWebhook
	g.Expect(w.Default(context.Background(), &Keystone{})).To(Succeed())
	_, _ = w.ValidateCreate(context.Background(), &Keystone{})
	_, _ = w.ValidateUpdate(context.Background(), &Keystone{}, &Keystone{})
	_, err := w.ValidateDelete(context.Background(), &Keystone{})
	g.Expect(err).NotTo(HaveOccurred())
}
