// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
			CredentialKeys: CredentialKeysSpec{
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
	g.Expect(k.Spec.CredentialKeys.MaxActiveKeys).To(Equal(int32(3)))
	g.Expect(k.Spec.Cache.Backend).To(Equal("dogpile.cache.pymemcache"))
	g.Expect(k.Spec.Bootstrap.AdminUser).To(Equal("admin"))
	g.Expect(k.Spec.Bootstrap.Region).To(Equal("RegionOne"))
	// REQ-004 (CC-0042): Verify Resources defaults are applied.
	g.Expect(k.Spec.Resources).NotTo(BeNil())
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryRequest))
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPURequest))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryLimit))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPULimit))
}

func TestDefault_DoesNotSetFernetRotationSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// Fernet.RotationSchedule relies on the Kubebuilder +default marker only
	// (plan decision #3, CC-0011). The webhook must NOT set it.
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Fernet.RotationSchedule).To(BeEmpty())
	g.Expect(k.Spec.CredentialKeys.RotationSchedule).To(BeEmpty())
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
	g.Expect(err.Error()).To(ContainSubstring("credentialKeys"))
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
			CredentialKeys: CredentialKeysSpec{
				RotationSchedule: "0 */12 * * *",
				MaxActiveKeys:    5,
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
	g.Expect(k.Spec.CredentialKeys.RotationSchedule).To(Equal("0 */12 * * *"))
	g.Expect(k.Spec.CredentialKeys.MaxActiveKeys).To(Equal(int32(5)))
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

// --- UWSGI defaulting tests (CC-0040, REQ-002) ---

func TestDefault_UWSGINilRemainsNil(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI).To(BeNil())
}

func TestDefault_UWSGIZeroValuedDefaultsProcessesAndThreads(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(2)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	// HTTPKeepAlive is NOT defaulted in the webhook — the CRD schema
	// default (+kubebuilder:default=true) handles it in the normal admission
	// path. The webhook cannot distinguish "not set" from "explicitly false"
	// for a bool, so it does not attempt to default it (CC-0040, REQ-002).
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(BeFalse())
}

func TestDefault_UWSGIDefaultsProcessesAndThreadsOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// When HTTPKeepAlive is already true but processes/threads are zero,
	// the struct is NOT fully zero-valued — still default processes and threads.
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				HTTPKeepAlive: true,
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(2)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(BeTrue())
}

func TestDefault_UWSGIDoesNotOverwriteHTTPKeepAlive(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// httpKeepAlive=false must never be overwritten by the webhook — the user
	// may have explicitly set it. The CRD schema default (+kubebuilder:default=true)
	// handles the normal admission case; the webhook never touches HTTPKeepAlive
	// because bool's zero value is indistinguishable from explicit false (CC-0040).
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				Processes:     4,
				HTTPKeepAlive: false,
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(4)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(BeFalse())
}

func TestDefault_UWSGIZeroProcessesAndThreadsDoNotOverrideExplicitFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// Edge case: httpKeepAlive=false with zero processes/threads. This simulates
	// a CR that bypasses CRD schema defaults (e.g. kubectl patch, upgrades).
	// The webhook must NOT flip httpKeepAlive to true (CC-0040, REQ-002).
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				HTTPKeepAlive: false,
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(2)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(BeFalse())
}

func TestDefault_UWSGIPreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				Processes:     8,
				Threads:       4,
				HTTPKeepAlive: true,
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(8)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(4)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(BeTrue())
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

// --- CredentialKeys MaxActiveKeys validation tests (CC-0036, REQ-009) ---

func TestValidate_CredentialKeysMaxActiveKeysBelowMinimumRejected(t *testing.T) {
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
			k.Spec.CredentialKeys.MaxActiveKeys = tc.val
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("credentialKeys"))
			g.Expect(err.Error()).To(ContainSubstring("maxActiveKeys"))
		})
	}
}

func TestValidate_CredentialKeysMaxActiveKeysZeroAllowed(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.CredentialKeys.MaxActiveKeys = 0

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_CredentialKeysMaxActiveKeysThreeAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.CredentialKeys.MaxActiveKeys = 3

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- CredentialKeys Cron validation tests (CC-0036, REQ-005) ---

func TestValidate_CredentialKeysValidCronExpressions(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"0 0 * * 0",
		"*/5 * * * *",
		"0 */6 * * *",
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.CredentialKeys.RotationSchedule = expr
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestValidate_CredentialKeysInvalidCronExpressions(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"not-a-cron",
		"* * *",
		"60 * * * *",
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.CredentialKeys.RotationSchedule = expr
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("credentialKeys"))
			g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
		})
	}
}

func TestValidate_CredentialKeysEmptyRotationScheduleReturnsRequiredError(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.CredentialKeys.RotationSchedule = ""

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialKeys"))
	g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
}

// --- Cron validation tests (CC-0011, REQ-002) ---

func TestValidate_ValidCronExpressions(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"0 0 * * 0",   // weekly at midnight Sunday
		"*/5 * * * *", // every 5 minutes
		"0 */6 * * *", // every 6 hours
		"30 2 1 * *",  // 2:30 AM on the 1st of each month
		"0 0 * * 1-5", // midnight weekdays
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
		"* * *",      // too few fields
		"60 * * * *", // minute out of range
		"* 25 * * *", // hour out of range
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

// --- UWSGI validation tests (CC-0040, REQ-003) ---

func TestValidate_UWSGIValidAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     4,
		Threads:       2,
		HTTPKeepAlive: true,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_UWSGINilSkipsValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// uwsgi is nil by default in validKeystone()

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_UWSGIProcessesBelowMinimumRejected(t *testing.T) {
	w := &KeystoneWebhook{}
	cases := []struct {
		name string
		val  int32
	}{
		{"zero", 0},
		{"negative", -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.UWSGI = &UWSGISpec{
				Processes:     tc.val,
				Threads:       2,
				HTTPKeepAlive: true,
			}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("uwsgi"))
			g.Expect(err.Error()).To(ContainSubstring("processes"))
		})
	}
}

func TestValidate_UWSGIThreadsBelowMinimumRejected(t *testing.T) {
	w := &KeystoneWebhook{}
	cases := []struct {
		name string
		val  int32
	}{
		{"zero", 0},
		{"negative", -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.UWSGI = &UWSGISpec{
				Processes:     2,
				Threads:       tc.val,
				HTTPKeepAlive: true,
			}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("uwsgi"))
			g.Expect(err.Error()).To(ContainSubstring("threads"))
		})
	}
}

func TestValidate_UWSGIBothInvalidReportsBothErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     0,
		Threads:       0,
		HTTPKeepAlive: true,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("processes"))
	g.Expect(err.Error()).To(ContainSubstring("threads"))
}

func TestValidate_UWSGIBoundaryValueOneAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     1,
		Threads:       1,
		HTTPKeepAlive: false,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Autoscaling validation tests (CC-0038, REQ-001) ---

func TestValidate_Autoscaling_Valid_CPUOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	cpu := int32(80)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          5,
		TargetCPUUtilization: &cpu,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_Autoscaling_Valid_MemoryOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	mem := int32(70)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:             5,
		TargetMemoryUtilization: &mem,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_Autoscaling_Valid_Both(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	cpu := int32(80)
	mem := int32(70)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:             5,
		TargetCPUUtilization:    &cpu,
		TargetMemoryUtilization: &mem,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_Autoscaling_Invalid_NoTargets(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas: 5,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("autoscaling"))
	g.Expect(err.Error()).To(ContainSubstring("targetCPUUtilization or targetMemoryUtilization"))
}

func TestValidate_Autoscaling_Invalid_MaxReplicasZero(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	cpu := int32(80)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          0,
		TargetCPUUtilization: &cpu,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("maxReplicas"))
}

func TestValidate_Autoscaling_Invalid_MinExceedsMax(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	cpu := int32(80)
	min := int32(10)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MinReplicas:          &min,
		MaxReplicas:          5,
		TargetCPUUtilization: &cpu,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("minReplicas"))
	g.Expect(err.Error()).To(ContainSubstring("must not exceed maxReplicas"))
}

func TestValidate_Autoscaling_Invalid_ImplicitMinExceedsMax(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Replicas = 5
	cpu := int32(80)
	// MinReplicas is nil — defaults to spec.replicas (5) in the reconciler,
	// which exceeds maxReplicas (3). Validation must reject this (CC-0038).
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          3,
		TargetCPUUtilization: &cpu,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("maxReplicas"))
	g.Expect(err.Error()).To(ContainSubstring("spec.replicas"))
}

func TestValidate_Autoscaling_Valid_ImplicitMinEqualsMax(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Replicas = 5
	cpu := int32(80)
	// MinReplicas is nil — defaults to spec.replicas (5), which equals maxReplicas.
	// This is a valid edge case (CC-0038).
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          5,
		TargetCPUUtilization: &cpu,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_Autoscaling_Invalid_CPUUtilizationOutOfRange(t *testing.T) {
	w := &KeystoneWebhook{}
	cases := []struct {
		name string
		val  int32
	}{
		{"zero", 0},
		{"negative", -1},
		{"above_100", 101},
		{"far_above", 150},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			v := tc.val
			k.Spec.Autoscaling = &AutoscalingSpec{
				MaxReplicas:          5,
				TargetCPUUtilization: &v,
			}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("targetCPUUtilization"))
			g.Expect(err.Error()).To(ContainSubstring("between 1 and 100"))
		})
	}
}

func TestValidate_Autoscaling_Invalid_MemoryUtilizationOutOfRange(t *testing.T) {
	w := &KeystoneWebhook{}
	cases := []struct {
		name string
		val  int32
	}{
		{"zero", 0},
		{"negative", -1},
		{"above_100", 101},
		{"far_above", 150},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			v := tc.val
			k.Spec.Autoscaling = &AutoscalingSpec{
				MaxReplicas:             5,
				TargetMemoryUtilization: &v,
			}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("targetMemoryUtilization"))
			g.Expect(err.Error()).To(ContainSubstring("between 1 and 100"))
		})
	}
}

func TestValidate_Autoscaling_Valid_BoundaryValues(t *testing.T) {
	w := &KeystoneWebhook{}
	cases := []struct {
		name string
		cpu  int32
		mem  int32
	}{
		{"min_boundary", 1, 1},
		{"max_boundary", 100, 100},
		{"mid_range", 50, 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			cpu := tc.cpu
			mem := tc.mem
			k.Spec.Autoscaling = &AutoscalingSpec{
				MaxReplicas:             5,
				TargetCPUUtilization:    &cpu,
				TargetMemoryUtilization: &mem,
			}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestValidate_Autoscaling_Nil_IsValid(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Autoscaling = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- NetworkPolicy validation tests (CC-0039, REQ-001) ---

func TestValidate_NetworkPolicy_Nil_IsValid(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.NetworkPolicy = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_NetworkPolicy_WithIngress_IsValid(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.NetworkPolicy = &NetworkPolicySpec{
		Ingress: []NetworkPolicyIngressSource{
			{NamespaceSelector: map[string]string{"kubernetes.io/metadata.name": "envoy-gateway-system"}},
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_NetworkPolicy_EmptyIngress_Rejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.NetworkPolicy = &NetworkPolicySpec{
		Ingress: []NetworkPolicyIngressSource{},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("networkPolicy"))
	g.Expect(err.Error()).To(ContainSubstring("ingress"))
}

// --- ValidateCreate and ValidateUpdate consistency (CC-0011, REQ-005, REQ-006) ---

func TestValidateCreate_RunsAllValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone()
	k.Spec.Replicas = 0
	k.Spec.Fernet.MaxActiveKeys = 1
	k.Spec.Fernet.RotationSchedule = "bad-cron"
	k.Spec.CredentialKeys.MaxActiveKeys = 1
	k.Spec.CredentialKeys.RotationSchedule = "bad-cron"
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
	// REQ-001 (CC-0038): Break autoscaling — set out-of-range utilization target.
	invalidCPU := int32(0)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          5,
		TargetCPUUtilization: &invalidCPU,
	}
	// REQ-001 (CC-0039): Break networkPolicy — set empty ingress.
	k.Spec.NetworkPolicy = &NetworkPolicySpec{
		Ingress: []NetworkPolicyIngressSource{},
	}
	// REQ-007 (CC-0065): Break gateway — empty hostname and empty parentRef.name.
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: ""},
		Hostname:  "",
	}
	// REQ-004 (CC-0042): Break resources — CPU request exceeds limit.
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1000m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}
	// REQ-001 (CC-0040): Break uWSGI — processes and threads below minimum.
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 0,
		Threads:   0,
	}
	// REQ-004 (CC-0075): Break PriorityClassName — nonexistent class.
	pcn := "nonexistent-class"
	k.Spec.PriorityClassName = &pcn
	// REQ-005 (CC-0075): Break TSC — wrong label selectors.
	k.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "wrong",
					"app.kubernetes.io/instance": k.Name,
				},
			},
		},
	}

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
	g.Expect(errMsg).To(ContainSubstring("credentialKeys"))
	g.Expect(errMsg).To(ContainSubstring("targetCPUUtilization"))
	g.Expect(errMsg).To(ContainSubstring("networkPolicy"))
	g.Expect(errMsg).To(ContainSubstring("resources"))
	g.Expect(errMsg).To(ContainSubstring("uwsgi"))
	g.Expect(errMsg).To(ContainSubstring("priorityClassName"))
	g.Expect(errMsg).To(ContainSubstring("topologySpreadConstraints"))
	// REQ-007 (CC-0065): Verify gateway validation participates in error
	// accumulation — both hostname and parentRef.name errors must surface.
	g.Expect(errMsg).To(ContainSubstring("gateway"))
	g.Expect(errMsg).To(ContainSubstring("hostname"))
	g.Expect(errMsg).To(ContainSubstring("parentRef"))
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

// TestValidateUpdate_ResourcesRequestExceedsLimit verifies that ValidateUpdate
// delegates resource validation correctly by submitting resources where the CPU
// request exceeds the limit (CC-0042, REQ-004).
func TestValidateUpdate_ResourcesRequestExceedsLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	old := validKeystone()
	updated := validKeystone()
	updated.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1000m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}

	_, err := w.ValidateUpdate(context.Background(), old, updated)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resources"))
}

func TestValidateDelete_AlwaysAllows(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()

	warnings, err := w.ValidateDelete(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeNil())
}

// --- Resources defaulting tests (CC-0042, REQ-004) ---

func TestDefault_ResourcesSetWhenNil(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Resources).NotTo(BeNil())
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryRequest))
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPURequest))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryLimit))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPULimit))
}

// TestDefault_ResourcesSetWhenEmpty verifies that `resources: {}` (non-nil but
// empty ResourceRequirements) triggers defaulting. Without this, the container
// gets no resources (BestEffort QoS) and HPA breaks (CC-0042).
func TestDefault_ResourcesSetWhenEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Resources: &corev1.ResourceRequirements{},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Resources).NotTo(BeNil())
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryRequest))
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPURequest))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, DefaultMemoryLimit))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, DefaultCPULimit))
}

func TestDefault_ResourcesPreservedWhenExplicit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("1Gi"),
					corev1.ResourceCPU:    resource.MustParse("200m"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("2Gi"),
					corev1.ResourceCPU:    resource.MustParse("1"),
				},
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("1Gi")))
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("200m")))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("2Gi")))
	g.Expect(k.Spec.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("1")))
}

// TestDefault_ResourcesPreservedWhenPartial verifies that partially-set resources
// (e.g., only Requests set, Limits empty) are not overwritten by the defaulting
// webhook. This ensures users can opt into a Guaranteed QoS by setting only
// requests, or any other partial configuration (CC-0042).
func TestDefault_ResourcesPreservedWhenPartial(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("512Mi"),
					corev1.ResourceCPU:    resource.MustParse("250m"),
				},
				// Limits intentionally empty — user only sets requests.
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	// Requests must be preserved as-is.
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("512Mi")))
	g.Expect(k.Spec.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("250m")))
	// Limits must remain empty — the webhook must not inject defaults.
	g.Expect(k.Spec.Resources.Limits).To(BeEmpty())
}

// --- Resources validation tests (CC-0042, REQ-004) ---

func TestValidate_ResourcesValidAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_ResourcesCPURequestExceedsLimitRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resources"))
	g.Expect(err.Error()).To(ContainSubstring("cpu"))
	g.Expect(err.Error()).To(ContainSubstring("must not exceed limit"))
}

func TestValidate_ResourcesMemoryRequestExceedsLimitRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resources"))
	g.Expect(err.Error()).To(ContainSubstring("memory"))
	g.Expect(err.Error()).To(ContainSubstring("must not exceed limit"))
}

func TestValidate_ResourcesBothExceedReportsBothErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cpu"))
	g.Expect(err.Error()).To(ContainSubstring("memory"))
}

func TestValidate_ResourcesNilAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Resources = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_ResourcesRequestsOnlyAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- TrustFlush cron validation tests (CC-0057, REQ-008) ---

func TestValidate_TrustFlush_ValidCronAccepted(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"0 * * * *",   // hourly (default)
		"*/5 * * * *", // every 5 minutes
		"30 2 * * 0",  // 2:30 AM on Sundays
		"0 0 1 * *",   // midnight on the 1st of each month
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.TrustFlush = &TrustFlushSpec{Schedule: expr}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestValidate_TrustFlush_InvalidCronRejected(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		"not-a-cron",
		"* * *",      // too few fields
		"60 * * * *", // minute out of range
		"* 25 * * *", // hour out of range
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.TrustFlush = &TrustFlushSpec{Schedule: expr}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("trustFlush"))
			g.Expect(err.Error()).To(ContainSubstring("schedule"))
		})
	}
}

func TestValidate_TrustFlush_EmptyScheduleRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.TrustFlush = &TrustFlushSpec{Schedule: ""}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("trustFlush"))
	g.Expect(err.Error()).To(ContainSubstring("schedule"))
}

func TestValidate_TrustFlush_NilPassesValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.TrustFlush = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Gateway validation tests (CC-0065, REQ-007) ---

func TestValidate_GatewayValid(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gateway"},
		Hostname:  "keystone.example.com",
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_GatewayValidWithOptionalFields(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{
			Name:        "openstack-gateway",
			Namespace:   "openstack-infra",
			SectionName: "https",
		},
		Hostname:    "keystone.example.com",
		Path:        "/identity",
		Annotations: map[string]string{"gateway.envoyproxy.io/rate-limit": "10rps"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_GatewayEmptyHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gateway"},
		Hostname:  "",
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("gateway"))
	g.Expect(err.Error()).To(ContainSubstring("hostname"))
}

func TestValidate_GatewayEmptyParentRefName(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: ""},
		Hostname:  "keystone.example.com",
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("gateway"))
	g.Expect(err.Error()).To(ContainSubstring("parentRef"))
	g.Expect(err.Error()).To(ContainSubstring("name"))
}

func TestValidate_GatewayNil_Accepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- PriorityClass validation (CC-0075, REQ-004) ---

// newFakeClient returns a controller-runtime fake client with the core scheduling
// API types registered. Additional objects can be pre-populated.
func newFakeClient(objs ...runtime.Object) *fake.ClientBuilder {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	return b
}

func TestValidate_PriorityClassNameExistsAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	pc := &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "system-cluster-critical"},
		Value:      1000000,
	}
	c := newFakeClient(pc).Build()
	w := &KeystoneWebhook{Client: c}
	k := validKeystone()
	k.Name = "my-ks"
	pcn := "system-cluster-critical"
	k.Spec.PriorityClassName = &pcn

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PriorityClassNameNotFoundRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	c := newFakeClient().Build()
	w := &KeystoneWebhook{Client: c}
	k := validKeystone()
	k.Name = "my-ks"
	pcn := "nonexistent-class"
	k.Spec.PriorityClassName = &pcn

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("priorityClassName"))
}

// TestValidate_PriorityClassNameNilOrEmpty_SkipsValidation verifies that
// PriorityClassName validation is skipped when the field is nil (unset) and
// the webhook has no Client. Both the nil-PriorityClassName guard and the
// nil-Client guard independently cause the validation to be bypassed.
func TestValidate_PriorityClassNameNilOrEmpty_SkipsValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	// Client is nil — even if PriorityClassName were set and non-empty,
	// the nil-Client guard would skip the lookup. Here PriorityClassName
	// is also nil (unset in validKeystone), so both guards apply.
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Name = "my-ks"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- TopologySpreadConstraints label selector validation (CC-0075, REQ-005) ---

func TestValidate_TopologySpreadConstraintCorrectLabelsAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Name = "my-ks"
	k.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "keystone",
					"app.kubernetes.io/instance": "my-ks",
				},
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_TopologySpreadConstraintWrongLabelsRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Name = "my-ks"
	k.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "wrong-name",
					"app.kubernetes.io/instance": "my-ks",
				},
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("topologySpreadConstraints"))
	g.Expect(err.Error()).To(ContainSubstring("labelSelector"))
}

func TestValidate_TopologySpreadConstraintNilSelectorRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Name = "my-ks"
	k.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     nil,
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("labelSelector"))
}

func TestValidate_TopologySpreadConstraintMatchExpressionsRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Name = "my-ks"
	k.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "keystone",
					"app.kubernetes.io/instance": "my-ks",
				},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "env",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("matchExpressions"))
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
