// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// validKeystone returns a Keystone with all required fields set to valid values.
// Tests modify this baseline to exercise specific validation rules.
func validKeystone() *Keystone {
	return &Keystone{
		Spec: KeystoneSpec{
			Deployment: DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database:   commonv1.DatabaseSpec{Host: "db.example.com", Port: 3306, Database: "keystone", SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"}},
			Cache:      commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
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

// --- Defaulting webhook tests ---

func TestDefault_SetsZeroValueDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Deployment.Replicas).To(Equal(int32(3)))
	g.Expect(k.Spec.Fernet.MaxActiveKeys).To(Equal(int32(3)))
	g.Expect(k.Spec.CredentialKeys.MaxActiveKeys).To(Equal(int32(3)))
	g.Expect(k.Spec.Cache.Backend).To(Equal("dogpile.cache.pymemcache"))
	g.Expect(k.Spec.Bootstrap.AdminUser).To(Equal("admin"))
	g.Expect(k.Spec.Bootstrap.Region).To(Equal("RegionOne"))
	// Verify Resources defaults are applied.
	g.Expect(k.Spec.Deployment.Resources).NotTo(BeNil())
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, commonv1.DefaultMemoryRequest()))
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, commonv1.DefaultCPURequest()))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, commonv1.DefaultMemoryLimit()))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, commonv1.DefaultCPULimit()))
}

func TestDefault_DoesNotSetFernetRotationSchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// Fernet.RotationSchedule relies on the Kubebuilder +default marker only
	// (plan decision #3). The webhook must NOT set it.
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

	// After Default() the webhook-managed fields are set — including
	// Spec.TrustFlush, which now materializes to a populated
	// struct so the trust-flush CronJob is created by default. Cache, Database,
	// and the rotationSchedule fields (CRD-schema-defaulted, not webhook-defaulted)
	// are still zero-valued — the spec must not pass validation.
	g.Expect(k.Spec.TrustFlush).NotTo(BeNil(),
		"defaulting webhook materializes spec.trustFlush")
	g.Expect(k.Spec.TrustFlush.Schedule).To(Equal(DefaultTrustFlushSchedule))

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("cache"))
	g.Expect(err.Error()).To(ContainSubstring("database"))
	g.Expect(err.Error()).To(ContainSubstring("rotationSchedule"))
	g.Expect(err.Error()).To(ContainSubstring("credentialKeys"))
	// the validating webhook must accept the webhook-defaulted
	// trust-flush schedule (DefaultTrustFlushSchedule), so no trustFlush error is raised.
	g.Expect(err.Error()).NotTo(ContainSubstring("trustFlush"))
}

func TestDefault_PreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Deployment: DeploymentSpec{Replicas: 5},
			Cache:      commonv1.CacheSpec{Backend: "dogpile.cache.memcache"},
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

	g.Expect(k.Spec.Deployment.Replicas).To(Equal(int32(5)))
	g.Expect(k.Spec.Fernet.RotationSchedule).To(Equal("0 */6 * * *"))
	g.Expect(k.Spec.Fernet.MaxActiveKeys).To(Equal(int32(7)))
	g.Expect(k.Spec.CredentialKeys.RotationSchedule).To(Equal("0 */12 * * *"))
	g.Expect(k.Spec.CredentialKeys.MaxActiveKeys).To(Equal(int32(5)))
	g.Expect(k.Spec.Cache.Backend).To(Equal("dogpile.cache.memcache"))
	g.Expect(k.Spec.Bootstrap.AdminUser).To(Equal("custom-admin"))
	g.Expect(k.Spec.Bootstrap.Region).To(Equal("EU-West"))
}

// TestDefault_TrustFlushNil_MaterializesDefaultSpec verifies that the defaulting
// webhook materializes a populated TrustFlushSpec when the pointer is nil so the
// trust-flush CronJob is created by default. The leaf
// +kubebuilder:default markers on Schedule and Suspend remain in place as
// defense-in-depth for callers that bypass the webhook.
func TestDefault_TrustFlushNil_MaterializesDefaultSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.TrustFlush).NotTo(BeNil())
	g.Expect(k.Spec.TrustFlush.Schedule).To(Equal(DefaultTrustFlushSchedule))
	g.Expect(k.Spec.TrustFlush.Suspend).To(BeFalse())
	g.Expect(k.Spec.TrustFlush.Args).To(BeEmpty())
}

// TestDefault_TrustFlushSet_PreservesExplicitValues verifies that the defaulting
// webhook never overwrites a user-supplied TrustFlushSpec.
func TestDefault_TrustFlushSet_PreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			TrustFlush: &TrustFlushSpec{
				Schedule: "*/30 * * * *",
				Suspend:  true,
				Args:     []string{"--date", "2026-01-01"},
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.TrustFlush).NotTo(BeNil())
	g.Expect(k.Spec.TrustFlush.Schedule).To(Equal("*/30 * * * *"))
	g.Expect(k.Spec.TrustFlush.Suspend).To(BeTrue())
	g.Expect(k.Spec.TrustFlush.Args).To(Equal([]string{"--date", "2026-01-01"}))
}

// --- PasswordRotation defaulting tests ---

// TestDefault_PasswordRotationNil_NotMaterialized verifies the defaulting
// webhook does NOT materialize spec.passwordRotation when it is
// absent. Scheduled admin-password rotation is strictly opt-in, so a
// CR that never set the block must keep the feature off.
func TestDefault_PasswordRotationNil_NotMaterialized(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.PasswordRotation).To(BeNil())
}

// TestDefault_PasswordRotationDisabled_NotDefaulted verifies the leaf defaults
// are NOT filled when the block is present but disabled — the sub-reconciler
// tears everything down when disabled, so there is nothing to default.
func TestDefault_PasswordRotationDisabled_NotDefaulted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			PasswordRotation: &PasswordRotationSpec{Enabled: false},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.PasswordRotation).NotTo(BeNil())
	g.Expect(k.Spec.PasswordRotation.Schedule).To(BeEmpty())
	g.Expect(k.Spec.PasswordRotation.PasswordLength).To(Equal(int32(0)))
}

// TestDefault_PasswordRotationEnabled_MaterializesScheduleAndLength verifies the
// defaulting webhook fills Schedule and PasswordLength when rotation is enabled
// and those leaves carry zero values.
func TestDefault_PasswordRotationEnabled_MaterializesScheduleAndLength(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			PasswordRotation: &PasswordRotationSpec{Enabled: true},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.PasswordRotation).NotTo(BeNil())
	g.Expect(k.Spec.PasswordRotation.Schedule).To(Equal(DefaultPasswordRotationSchedule))
	g.Expect(k.Spec.PasswordRotation.PasswordLength).To(Equal(DefaultPasswordRotationLength))
}

// TestDefault_PasswordRotationEnabled_PreservesExplicitValues verifies the
// defaulting webhook never overwrites operator-supplied Schedule or
// PasswordLength when rotation is enabled.
func TestDefault_PasswordRotationEnabled_PreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			PasswordRotation: &PasswordRotationSpec{
				Enabled:        true,
				Schedule:       "*/30 * * * *",
				PasswordLength: 40,
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.PasswordRotation.Schedule).To(Equal("*/30 * * * *"))
	g.Expect(k.Spec.PasswordRotation.PasswordLength).To(Equal(int32(40)))
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

// --- UWSGI defaulting tests ---

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
	// HTTPKeepAlive is now a nil-preserving *bool, so the webhook restores the
	// documented default (true) when the pointer is nil — a zero-valued UWSGI
	// block gets keep-alive enabled.
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeTrue()))
}

func TestDefault_UWSGIDefaultsProcessesAndThreadsOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// When HTTPKeepAlive is already true but processes/threads are zero,
	// the struct is NOT fully zero-valued — still default processes and threads.
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				HTTPKeepAlive: ptr.To(true),
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(2)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeTrue()))
}

func TestDefault_UWSGIDoesNotOverwriteHTTPKeepAlive(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// An explicit httpKeepAlive=false must never be overwritten by the webhook —
	// the pointer distinguishes "unset" (nil, which the webhook defaults to true)
	// from an explicit false, so the false is preserved verbatim.
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				Processes:     4,
				HTTPKeepAlive: ptr.To(false),
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(4)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeFalse()))
}

func TestDefault_UWSGIZeroProcessesAndThreadsDoNotOverrideExplicitFalse(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// Edge case: httpKeepAlive=false with zero processes/threads. This simulates
	// a CR that bypasses CRD schema defaults (e.g. kubectl patch, upgrades).
	// The webhook must NOT flip httpKeepAlive to true.
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				HTTPKeepAlive: ptr.To(false),
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(2)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(1)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeFalse()))
}

func TestDefault_UWSGIPreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			UWSGI: &UWSGISpec{
				Processes:     8,
				Threads:       4,
				HTTPKeepAlive: ptr.To(true),
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.UWSGI.Processes).To(Equal(int32(8)))
	g.Expect(k.Spec.UWSGI.Threads).To(Equal(int32(4)))
	g.Expect(k.Spec.UWSGI.HTTPKeepAlive).To(HaveValue(BeTrue()))
}

// --- Logging defaulting tests ---

func TestDefault_LoggingNilMaterializesDefaultSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Logging).NotTo(BeNil())
	g.Expect(k.Spec.Logging.Format).To(Equal("text"))
	g.Expect(k.Spec.Logging.Level).To(Equal("INFO"))
	g.Expect(k.Spec.Logging.Debug).To(HaveValue(BeFalse()))
	g.Expect(k.Spec.Logging.PerLoggerLevels).To(BeNil())
}

func TestDefault_LoggingPreservesExplicitValues(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Logging: &LoggingSpec{
				Format:          "json",
				Level:           "DEBUG",
				Debug:           ptr.To(true),
				PerLoggerLevels: map[string]string{"keystone.middleware": "DEBUG"},
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Logging.Format).To(Equal("json"))
	g.Expect(k.Spec.Logging.Level).To(Equal("DEBUG"))
	g.Expect(k.Spec.Logging.Debug).To(HaveValue(BeTrue()))
	g.Expect(k.Spec.Logging.PerLoggerLevels).To(HaveKeyWithValue("keystone.middleware", "DEBUG"))
}

func TestDefault_LoggingPartialFillsZeroValuesOnly(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// Mirrors TestDefault_UWSGIDefaultsProcessesAndThreadsOnly — when
	// the pointer is non-nil but fields are unset, fill the strings and, because
	// Debug is now a nil-preserving *bool, restore its documented default (false).
	k := &Keystone{
		Spec: KeystoneSpec{
			Logging: &LoggingSpec{
				Format: "json",
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Logging.Format).To(Equal("json"))
	g.Expect(k.Spec.Logging.Level).To(Equal("INFO"))
	g.Expect(k.Spec.Logging.Debug).To(HaveValue(BeFalse()))
	g.Expect(k.Spec.Logging.PerLoggerLevels).To(BeNil())
}

// --- Replicas validation tests ---

func TestValidate_ReplicasZeroRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Replicas = 0

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("replicas"))
}

func TestValidate_ReplicasNegativeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Replicas = -1

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("replicas"))
}

func TestValidate_ReplicasOneAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Replicas = 1

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- MaxActiveKeys validation tests ---

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

// --- CredentialKeys MaxActiveKeys validation tests ---

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

// --- CredentialKeys Cron validation tests ---

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

// --- Cron validation tests ---

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

// --- Cache mutual-exclusivity tests ---

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

// --- Database mutual-exclusivity tests ---

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

// --- Duplicate plugin detection tests ---

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

// --- Policy source requirement tests ---

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

// --- Empty policy rule name/value tests ---

func TestValidate_EmptyPolicyRuleNameRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"": "role:admin"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("policy rule name must not be empty"))
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

// TestValidate_EmptyPolicyRuleValueRejected covers the gap the audit reported
// (issue #479): a rule with an empty value previously passed admission and
// reached oslo.policy. The webhook now delegates to policy.ValidatePolicyRules,
// which rejects it with "policy rule value must not be empty".
func TestValidate_EmptyPolicyRuleValueRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"identity:get_user": ""},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("policy rule value must not be empty"))
	g.Expect(err.Error()).To(ContainSubstring("policyOverrides"))
}

// --- UWSGI validation tests ---

func TestValidate_UWSGIValidAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.UWSGI = &UWSGISpec{
		Processes:     4,
		Threads:       2,
		HTTPKeepAlive: ptr.To(true),
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
				HTTPKeepAlive: ptr.To(true),
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
				HTTPKeepAlive: ptr.To(true),
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
		HTTPKeepAlive: ptr.To(true),
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
		HTTPKeepAlive: ptr.To(false),
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Logging validation tests ---

func TestValidate_LoggingNilSkipsValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// logging is nil by default in validKeystone()

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_LoggingValidAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Logging = &LoggingSpec{
		Format:          "json",
		Level:           "DEBUG",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"keystone.middleware": "DEBUG", "sqlalchemy.engine": "WARNING"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_LoggingRejectsUnknownFormat(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Logging = &LoggingSpec{
		Format: "yaml",
		Level:  "INFO",
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("logging.format"))
	g.Expect(err.Error()).To(ContainSubstring("yaml"))
}

func TestValidate_LoggingRejectsUnknownLevel(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Logging = &LoggingSpec{
		Format: "text",
		Level:  "TRACE",
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("logging.level"))
	g.Expect(err.Error()).To(ContainSubstring("TRACE"))
}

func TestValidate_LoggingPerLoggerLevelsRejectsUnknownLevel(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Logging = &LoggingSpec{
		Format:          "text",
		Level:           "INFO",
		PerLoggerLevels: map[string]string{"keystone.middleware": "VERBOSE"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("logging.perLoggerLevels"))
	g.Expect(err.Error()).To(ContainSubstring("keystone.middleware"))
	g.Expect(err.Error()).To(ContainSubstring("VERBOSE"))
}

func TestValidate_LoggingPerLoggerLevelsRejectsEmptyKey(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Logging = &LoggingSpec{
		Format:          "text",
		Level:           "INFO",
		PerLoggerLevels: map[string]string{"": "DEBUG"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("logging.perLoggerLevels"))
	g.Expect(err.Error()).To(ContainSubstring("must not be empty"))
}

// --- Autoscaling validation tests ---

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
	k.Spec.Deployment.Replicas = 5
	cpu := int32(80)
	// MinReplicas is nil — defaults to spec.deployment.replicas (5) in the reconciler,
	// which exceeds maxReplicas (3). Validation must reject this.
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          3,
		TargetCPUUtilization: &cpu,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("maxReplicas"))
	g.Expect(err.Error()).To(ContainSubstring("spec.deployment.replicas"))
}

func TestValidate_Autoscaling_Valid_ImplicitMinEqualsMax(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Replicas = 5
	cpu := int32(80)
	// MinReplicas is nil — defaults to spec.deployment.replicas (5), which equals maxReplicas.
	// This is a valid edge case.
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

// --- NetworkPolicy validation tests ---

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
			{NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "envoy-gateway-system"}}},
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

// --- ValidateCreate and ValidateUpdate consistency ---

func TestValidateCreate_RunsAllValidations(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone()
	k.Spec.Deployment.Replicas = 0
	// Break image — set BOTH tag and digest so the tag/digest XOR fires. Every
	// new image validation hook must participate in the aggregated error.
	k.Spec.Image.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	k.Spec.Fernet.MaxActiveKeys = 1
	k.Spec.Fernet.RotationSchedule = "bad-cron"
	k.Spec.CredentialKeys.MaxActiveKeys = 1
	k.Spec.CredentialKeys.RotationSchedule = "bad-cron"
	k.Spec.Plugins = []commonv1.PluginSpec{
		{Name: "a", ConfigSection: "dup"},
		{Name: "b", ConfigSection: "dup"},
	}
	// Break policyOverrides on BOTH rule constraints: an empty rule name and an
	// empty rule value (the empty-value path is the issue #479 addition). Both
	// must participate in the aggregated error.
	k.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{
			"":                  "rule:admin",
			"identity:get_user": "",
		},
	}
	// Break cache mutual-exclusivity — set both clusterRef and servers.
	k.Spec.Cache.ClusterRef = &corev1.LocalObjectReference{Name: "memcached"}
	// Break database mutual-exclusivity — set both clusterRef and host.
	k.Spec.Database.ClusterRef = &corev1.LocalObjectReference{Name: "mariadb"}
	// Break database TLS — out-of-enum mode and enabled
	// with both certificate secret refs missing. Every new TLS validation
	// hook must participate in the aggregated error, matching the
	///-style regression guard so a future short-circuit before
	// reaching k.Spec.Database.TLS is caught here rather than only at e2e time.
	k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode: "bogus",
	}
	// Break autoscaling — set out-of-range utilization target.
	invalidCPU := int32(0)
	k.Spec.Autoscaling = &AutoscalingSpec{
		MaxReplicas:          5,
		TargetCPUUtilization: &invalidCPU,
	}
	// Break networkPolicy — set empty ingress.
	k.Spec.NetworkPolicy = &NetworkPolicySpec{
		Ingress: []NetworkPolicyIngressSource{},
	}
	// Break gateway — empty hostname and empty parentRef.name.
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: ""},
		Hostname:  "",
	}
	// Break resources — CPU request exceeds limit.
	k.Spec.Deployment.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1000m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}
	// +/// Break uWSGI —
	// processes/threads below minimum, harakiri exceeds the drain window, and
	// httpKeepAliveTimeout is set while httpKeepAlive is false (which also breaches 's minimum=1 check since the value is 0). This ensures
	// the aggregate test catches regressions in every new uWSGI hook,
	// guarding against the-style omission flagged in review #1.
	breakingHarakiri := int32(50)
	breakingKeepAliveTimeout := int32(0)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:            0,
		Threads:              0,
		Harakiri:             &breakingHarakiri,
		HTTPKeepAlive:        ptr.To(false),
		HTTPKeepAliveTimeout: &breakingKeepAliveTimeout,
	}
	//// Break graceful-termination fields —
	// preStopSleepSeconds (30) is not strictly less than terminationGracePeriodSeconds
	// (10), so the cross-field rule fires with an error message mentioning both
	// terminationGracePeriodSeconds and preStopSleepSeconds.
	grace := int64(10)
	preStop := int64(30)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace
	k.Spec.Deployment.PreStopSleepSeconds = &preStop
	// Break deployment strategy — Recreate with a RollingUpdate
	// block is rejected by the Deployment controller and must be caught early.
	k.Spec.Deployment.Strategy = &appsv1.DeploymentStrategy{
		Type:          appsv1.RecreateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{},
	}
	// Break logging — invalid Format enum, invalid Level
	// enum, an empty per-logger key, and an invalid per-logger value level.
	// Every new logging validation hook must participate in the aggregated
	// error, matching the/-style regression guard pattern so a
	// future change that short-circuits before reaching k.Spec.Logging is
	// caught here rather than only at e2e time.
	k.Spec.Logging = &LoggingSpec{
		Format: "yaml",
		Level:  "TRACE",
		PerLoggerLevels: map[string]string{
			"":     "INFO",
			"amqp": "VERBOSE",
		},
	}
	// Break PriorityClassName — nonexistent class.
	pcn := "nonexistent-class"
	k.Spec.Deployment.PriorityClassName = &pcn
	// Break TSC — wrong label selectors.
	k.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
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
	/// Break passwordRotation — enabled with an invalid
	// cron schedule, a below-minimum passwordLength, and a cleared
	// adminPasswordSecretRef. Every new passwordRotation validation hook must
	// participate in the aggregated error so a future change that short-circuits
	// before reaching the block is caught here rather than only at e2e time,
	// matching the//-style regression guard above.
	k.Spec.Bootstrap.AdminPasswordSecretRef.Name = ""
	k.Spec.PasswordRotation = &PasswordRotationSpec{
		Enabled:        true,
		Schedule:       "bad-cron",
		PasswordLength: 8,
	}
	// Break federation.proxyImage — empty repository AND the tag/digest XOR
	// (both set). Every new federation validation hook must participate in the
	// aggregated error, matching the regression-guard pattern above.
	k.Spec.Federation = &FederationSpec{
		ProxyImage: &commonv1.ImageSpec{
			Repository: "",
			Tag:        "latest",
			Digest:     "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		},
		// Break federation.trustedDashboards on BOTH hooks — a duplicate origin
		// and a non-http(s) entry — plus the extraConfig conflict below, so
		// every trustedDashboards path participates in the aggregated error
		// rather than short-circuiting on the first violation.
		TrustedDashboards: []string{
			"https://horizon.example.com/auth/websso/",
			"https://horizon.example.com/auth/websso/",
			"ftp://horizon.example.com/auth/websso/",
		},
	}
	k.Spec.ExtraConfig = map[string]map[string]string{
		"federation": {"trusted_dashboard": "https://other.example.com/auth/websso/"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	errMsg := err.Error()
	g.Expect(errMsg).To(ContainSubstring("replicas"))
	g.Expect(errMsg).To(ContainSubstring("image"))
	g.Expect(errMsg).To(ContainSubstring("maxActiveKeys"))
	g.Expect(errMsg).To(ContainSubstring("rotationSchedule"))
	g.Expect(errMsg).To(ContainSubstring("configSection"))
	g.Expect(errMsg).To(ContainSubstring("policyOverrides"))
	g.Expect(errMsg).To(ContainSubstring("policy rule name must not be empty"))
	g.Expect(errMsg).To(ContainSubstring("policy rule value must not be empty"))
	g.Expect(errMsg).To(ContainSubstring("cache"))
	g.Expect(errMsg).To(ContainSubstring("database"))
	g.Expect(errMsg).To(ContainSubstring("credentialKeys"))
	g.Expect(errMsg).To(ContainSubstring("targetCPUUtilization"))
	g.Expect(errMsg).To(ContainSubstring("networkPolicy"))
	g.Expect(errMsg).To(ContainSubstring("resources"))
	g.Expect(errMsg).To(ContainSubstring("uwsgi"))
	g.Expect(errMsg).To(ContainSubstring("priorityClassName"))
	g.Expect(errMsg).To(ContainSubstring("topologySpreadConstraints"))
	// Verify gateway validation participates in error
	// accumulation — both hostname and parentRef.name errors must surface.
	g.Expect(errMsg).To(ContainSubstring("gateway"))
	g.Expect(errMsg).To(ContainSubstring("hostname"))
	g.Expect(errMsg).To(ContainSubstring("parentRef"))
	/////////
	// Every new graceful-termination validation hook must participate in the
	// aggregated error, matching the-style regression guard for
	// priorityClassName/topologySpreadConstraints above.
	g.Expect(errMsg).To(ContainSubstring("terminationGracePeriodSeconds"))
	g.Expect(errMsg).To(ContainSubstring("preStopSleepSeconds"))
	g.Expect(errMsg).To(ContainSubstring("harakiri"))
	g.Expect(errMsg).To(ContainSubstring("httpKeepAliveTimeout"))
	g.Expect(errMsg).To(ContainSubstring("strategy"))
	// every new logging validation path must participate
	// in the aggregated error so a future short-circuit is caught here.
	g.Expect(errMsg).To(ContainSubstring("logging"))
	g.Expect(errMsg).To(ContainSubstring("format"))
	g.Expect(errMsg).To(ContainSubstring("level"))
	g.Expect(errMsg).To(ContainSubstring("perLoggerLevels"))
	g.Expect(errMsg).To(ContainSubstring("logger name must not be empty"))
	// every new database-TLS validation path must
	// participate in the aggregated error so a future short-circuit before
	// reaching k.Spec.Database.TLS is caught here rather than only at e2e time.
	g.Expect(errMsg).To(ContainSubstring("tls.mode"))
	g.Expect(errMsg).To(ContainSubstring("caBundleSecretRef.name"))
	g.Expect(errMsg).To(ContainSubstring("clientCertSecretRef.name"))
	/// every passwordRotation validation path (schedule,
	// passwordLength, adminPasswordSecretRef) must participate in the aggregated
	// error so a future short-circuit before reaching the block is caught here.
	g.Expect(errMsg).To(ContainSubstring("passwordRotation"))
	g.Expect(errMsg).To(ContainSubstring("passwordLength"))
	g.Expect(errMsg).To(ContainSubstring("adminPasswordSecretRef"))
	// Every trustedDashboards validation path (duplicate, non-URL scheme, and
	// the extraConfig conflict) must participate in the aggregated error.
	g.Expect(errMsg).To(ContainSubstring("trustedDashboards"))
	g.Expect(errMsg).To(ContainSubstring("Duplicate value"))
	g.Expect(errMsg).To(ContainSubstring("scheme must be http or https"))
	g.Expect(errMsg).To(ContainSubstring("trusted_dashboard is managed via spec.federation.trustedDashboards"))
	// every federation validation path (proxyImage repository +
	// tag/digest XOR) must participate in the aggregated error.
	g.Expect(errMsg).To(ContainSubstring("federation.proxyImage"))
	g.Expect(errMsg).To(ContainSubstring("proxyImage.repository must be set"))
	g.Expect(errMsg).To(ContainSubstring("exactly one of proxyImage.tag or proxyImage.digest"))
}

// TestValidate_FederationProxyImage covers the federation.proxyImage
// defense-in-depth checks in isolation: a nil federation block and a nil
// proxyImage are both valid (activation is backend-driven), a set proxyImage
// needs a repository and exactly one of tag/digest.
func TestValidate_FederationProxyImage(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}

	// nil federation: valid.
	k := validKeystone()
	k.Spec.Federation = nil
	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())

	// federation with nil proxyImage: valid (backends stay pending with a
	// FederationProxyImageMissing warning at reconcile time instead).
	k = validKeystone()
	k.Spec.Federation = &FederationSpec{}
	_, err = w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())

	// valid proxyImage: accepted.
	k = validKeystone()
	k.Spec.Federation = &FederationSpec{
		ProxyImage: &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone-federation-proxy", Tag: "latest"},
	}
	_, err = w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())

	// missing repository: rejected.
	k = validKeystone()
	k.Spec.Federation = &FederationSpec{
		ProxyImage: &commonv1.ImageSpec{Tag: "latest"},
	}
	_, err = w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("proxyImage.repository must be set"))

	// neither tag nor digest: rejected by the XOR.
	k = validKeystone()
	k.Spec.Federation = &FederationSpec{
		ProxyImage: &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone-federation-proxy"},
	}
	_, err = w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of proxyImage.tag or proxyImage.digest"))
}

// managedKeystone returns a valid managed-mode Keystone (ClusterRef set, Host
// unset) suitable for the credentialsMode tests, which require managed mode.
func managedKeystone() *Keystone {
	k := validKeystone()
	k.Spec.Database = commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	return k
}

// TestDefault_CredentialsModeDefaultsToStatic verifies the defaulting webhook
// stamps CredentialsMode=Static when the field is empty, and leaves an explicit
// Dynamic value untouched.
func TestDefault_CredentialsModeDefaultsToStatic(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}

	empty := validKeystone()
	empty.Spec.Database.CredentialsMode = ""
	g.Expect(w.Default(context.Background(), empty)).To(Succeed())
	g.Expect(empty.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeStatic))

	dynamic := managedKeystone()
	dynamic.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic
	g.Expect(w.Default(context.Background(), dynamic)).To(Succeed())
	g.Expect(dynamic.Spec.Database.CredentialsMode).To(Equal(commonv1.CredentialsModeDynamic))
}

// TestValidateCreate_RejectsDynamicCredentialsWithoutClusterRef verifies the
// defense-in-depth mirror of the CEL rule: Dynamic requires managed mode. It is
// a dedicated test rather than a case in TestValidateCreate_RunsAllValidations
// because triggering it requires ClusterRef nil, which is mutually exclusive
// with that test's "both clusterRef and host set" database break.
// TestValidate_TrustedDashboardsAcceptsMultipleOrigins pins the happy path:
// several distinct origins — including one on a non-default port, the case
// the ControlPlane publicEndpoint override exists for — are accepted.
func TestValidate_TrustedDashboardsAcceptsMultipleOrigins(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone()
	k.Spec.Federation = &FederationSpec{
		TrustedDashboards: []string{
			"https://horizon.example.com/auth/websso/",
			"https://horizon.example.com:8443/auth/websso/",
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidate_TrustedDashboardsRejectsDuplicates guards the duplicate check:
// the same origin twice would render trusted_dashboard twice and signals a
// copy-paste error rather than intent.
func TestValidate_TrustedDashboardsRejectsDuplicates(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone()
	k.Spec.Federation = &FederationSpec{
		TrustedDashboards: []string{
			"https://horizon.example.com/auth/websso/",
			"https://horizon.example.com/auth/websso/",
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("trustedDashboards[1]"))
	g.Expect(err.Error()).To(ContainSubstring("Duplicate value"))
}

// TestValidate_TrustedDashboardsRejectsNonURL covers the defense-in-depth URL
// parse alongside the items:Pattern marker: a scheme-only value would parse
// but carry no host, so it could never match any dashboard origin.
func TestValidate_TrustedDashboardsRejectsNonURL(t *testing.T) {
	w := &KeystoneWebhook{Client: newFakeClient().Build()}

	for name, origin := range map[string]string{
		"missing host": "https:///auth/websso/",
		"wrong scheme": "ftp://horizon.example.com/auth/websso/",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.Federation = &FederationSpec{TrustedDashboards: []string{origin}}

			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("trustedDashboards[0]"))
		})
	}
}

// TestValidate_TrustedDashboardsWarnsOnCleartextOrigin covers the scheme
// downgrade the items:Pattern marker deliberately allows. trusted_dashboard is
// where Keystone POSTs the unscoped WebSSO token after a federated login, so an
// http origin hands a full-privilege bearer token to any on-path observer. It is
// not rejected — a plain-http lab dashboard is a legal setup — but it must not
// be silent either.
func TestValidate_TrustedDashboardsWarnsOnCleartextOrigin(t *testing.T) {
	w := &KeystoneWebhook{Client: newFakeClient().Build()}

	t.Run("http warns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		k := validKeystone()
		k.Spec.Federation = &FederationSpec{
			TrustedDashboards: []string{
				"https://horizon.example.com/auth/websso/",
				"http://legacy.example.com/auth/websso/",
			},
		}

		warnings, err := w.ValidateCreate(context.Background(), k)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(1), "only the http origin warns")
		g.Expect(warnings[0]).To(ContainSubstring("http://legacy.example.com/auth/websso/"))
		g.Expect(warnings[0]).To(ContainSubstring("cleartext"))
	})

	t.Run("https is silent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		k := validKeystone()
		k.Spec.Federation = &FederationSpec{
			TrustedDashboards: []string{"https://horizon.example.com/auth/websso/"},
		}

		warnings, err := w.ValidateCreate(context.Background(), k)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// TestValidate_TrustedDashboardsRejectsExtraConfigConflict guards the silent
// contradiction: extraConfig wins MergeDefaults, so declaring the option in
// both places would drop the typed list from the rendered config unnoticed.
func TestValidate_TrustedDashboardsRejectsExtraConfigConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone()
	k.Spec.Federation = &FederationSpec{
		TrustedDashboards: []string{"https://horizon.example.com/auth/websso/"},
	}
	k.Spec.ExtraConfig = map[string]map[string]string{
		"federation": {"trusted_dashboard": "https://other.example.com/auth/websso/"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("trusted_dashboard is managed via spec.federation.trustedDashboards"))
}

// TestValidate_ExtraConfigTrustedDashboardAloneIsAllowed pins the escape hatch:
// the conflict rule must only fire when the typed field is ALSO set, so a
// pre-typed-field CR that declares the option only in extraConfig keeps
// validating.
func TestValidate_ExtraConfigTrustedDashboardAloneIsAllowed(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone()
	k.Spec.ExtraConfig = map[string]map[string]string{
		"federation": {"trusted_dashboard": "https://horizon.example.com/auth/websso/"},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidateCreate_RejectsDynamicCredentialsWithoutClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := validKeystone() // brownfield (Host set, ClusterRef nil)
	k.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("credentialsMode"))
	g.Expect(err.Error()).To(ContainSubstring("requires clusterRef"))
}

// TestValidateCreate_AcceptsDynamicCredentialsWithClusterRef verifies Dynamic is
// accepted in managed mode.
func TestValidateCreate_AcceptsDynamicCredentialsWithClusterRef(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{Client: newFakeClient().Build()}
	k := managedKeystone()
	k.Spec.Database.CredentialsMode = commonv1.CredentialsModeDynamic

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
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
// request exceeds the limit.
func TestValidateUpdate_ResourcesRequestExceedsLimit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	old := validKeystone()
	updated := validKeystone()
	updated.Spec.Deployment.Resources = &corev1.ResourceRequirements{
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

// --- Resources defaulting tests ---

func TestDefault_ResourcesSetWhenNil(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Deployment.Resources).NotTo(BeNil())
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, commonv1.DefaultMemoryRequest()))
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, commonv1.DefaultCPURequest()))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, commonv1.DefaultMemoryLimit()))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, commonv1.DefaultCPULimit()))
}

// TestDefault_ResourcesSetWhenEmpty verifies that `resources: {}` (non-nil but
// empty ResourceRequirements) triggers defaulting. Without this, the container
// gets no resources (BestEffort QoS) and HPA breaks.
func TestDefault_ResourcesSetWhenEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Deployment: DeploymentSpec{Resources: &corev1.ResourceRequirements{}},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Deployment.Resources).NotTo(BeNil())
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, commonv1.DefaultMemoryRequest()))
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, commonv1.DefaultCPURequest()))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, commonv1.DefaultMemoryLimit()))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, commonv1.DefaultCPULimit()))
}

func TestDefault_ResourcesPreservedWhenExplicit(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Deployment: DeploymentSpec{
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
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("1Gi")))
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("200m")))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("2Gi")))
	g.Expect(k.Spec.Deployment.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("1")))
}

// TestDefault_ResourcesPreservedWhenPartial verifies that partially-set resources
// (e.g., only Requests set, Limits empty) are not overwritten by the defaulting
// webhook. This ensures users can opt into a Guaranteed QoS by setting only
// requests, or any other partial configuration.
func TestDefault_ResourcesPreservedWhenPartial(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := &Keystone{
		Spec: KeystoneSpec{
			Deployment: DeploymentSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("512Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
					// Limits intentionally empty — user only sets requests.
				},
			},
		},
	}

	g.Expect(w.Default(context.Background(), k)).To(Succeed())

	// Requests must be preserved as-is.
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("512Mi")))
	g.Expect(k.Spec.Deployment.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("250m")))
	// Limits must remain empty — the webhook must not inject defaults.
	g.Expect(k.Spec.Deployment.Resources.Limits).To(BeEmpty())
}

// --- Resources validation tests ---

func TestValidate_ResourcesValidAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Resources = &corev1.ResourceRequirements{
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
	k.Spec.Deployment.Resources = &corev1.ResourceRequirements{
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
	k.Spec.Deployment.Resources = &corev1.ResourceRequirements{
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
	k.Spec.Deployment.Resources = &corev1.ResourceRequirements{
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
	k.Spec.Deployment.Resources = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_ResourcesRequestsOnlyAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- TrustFlush cron validation tests ---

func TestValidate_TrustFlush_ValidCronAccepted(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		DefaultTrustFlushSchedule, // hourly (default)
		"*/5 * * * *",             // every 5 minutes
		"30 2 * * 0",              // 2:30 AM on Sundays
		"0 0 1 * *",               // midnight on the 1st of each month
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

// TestValidate_TrustFlushDefaulted_AcceptsHourlySchedule verifies that the
// hourly schedule injected by the defaulting webhook passes
// the cron-parsing block in validate(). Together with the Default() materialization
// test, this protects the webhook → validate handoff against any future change
// that would otherwise reject the defaulted value.
func TestValidate_TrustFlushDefaulted_AcceptsHourlySchedule(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.TrustFlush = nil

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.TrustFlush).NotTo(BeNil())
	g.Expect(k.Spec.TrustFlush.Schedule).To(Equal(DefaultTrustFlushSchedule))

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- PasswordRotation validation tests ---

func TestValidate_PasswordRotation_ValidCronAccepted(t *testing.T) {
	w := &KeystoneWebhook{}
	expressions := []string{
		DefaultPasswordRotationSchedule, // monthly (default)
		"*/5 * * * *",                   // every 5 minutes
		"30 2 * * 0",                    // 2:30 AM on Sundays
		"0 0 1 * *",                     // midnight on the 1st of each month
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			g := NewGomegaWithT(t)
			k := validKeystone()
			k.Spec.PasswordRotation = &PasswordRotationSpec{Enabled: true, Schedule: expr}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestValidate_PasswordRotation_InvalidCronRejected(t *testing.T) {
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
			k.Spec.PasswordRotation = &PasswordRotationSpec{Enabled: true, Schedule: expr}
			_, err := w.ValidateCreate(context.Background(), k)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("passwordRotation"))
			g.Expect(err.Error()).To(ContainSubstring("schedule"))
		})
	}
}

func TestValidate_PasswordRotation_EmptyScheduleRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PasswordRotation = &PasswordRotationSpec{Enabled: true, Schedule: ""}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("passwordRotation"))
	g.Expect(err.Error()).To(ContainSubstring("schedule"))
}

// TestValidate_PasswordRotation_DisabledNotValidated verifies a malformed cron
// is tolerated when the block is present but disabled — validation is gated on
// Enabled, mirroring the defaulting webhook.
func TestValidate_PasswordRotation_DisabledNotValidated(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PasswordRotation = &PasswordRotationSpec{Enabled: false, Schedule: "not-a-cron"}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PasswordRotation_NilPassesValidation(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PasswordRotation = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestValidate_PasswordRotation_MissingAdminSecretRefRejected verifies the
// Part-1 prerequisite invariant (boundary 3): enabling rotation without
// an admin-password Secret reference is rejected, because the rotated password
// must round-trip through that Secret to reach Keystone.
func TestValidate_PasswordRotation_MissingAdminSecretRefRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Bootstrap.AdminPasswordSecretRef.Name = ""
	k.Spec.PasswordRotation = &PasswordRotationSpec{Enabled: true, Schedule: DefaultPasswordRotationSchedule}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("adminPasswordSecretRef"))
}

// TestValidate_PasswordRotation_ShortPasswordLengthRejected verifies the
// defense-in-depth passwordLength guard an explicit
// passwordLength below the +kubebuilder:validation:Minimum=24 floor is rejected
// at admission even when CRD schema validation is bypassed, mirroring the
// maxActiveKeys / harakiri defense-in-depth checks.
func TestValidate_PasswordRotation_ShortPasswordLengthRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PasswordRotation = &PasswordRotationSpec{
		Enabled:        true,
		Schedule:       DefaultPasswordRotationSchedule,
		PasswordLength: 8,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("passwordLength"))
	g.Expect(err.Error()).To(ContainSubstring("at least 24"))
}

// TestValidate_PasswordRotation_MinimumPasswordLengthAccepted verifies the
// passwordLength guard uses a strict "< 24" boundary the
// exact minimum (24) is accepted, proving the defense-in-depth check matches
// the +kubebuilder:validation:Minimum=24 marker rather than rejecting it.
func TestValidate_PasswordRotation_MinimumPasswordLengthAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.PasswordRotation = &PasswordRotationSpec{
		Enabled:        true,
		Schedule:       DefaultPasswordRotationSchedule,
		PasswordLength: 24,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Gateway validation tests ---

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

// --- publicEndpoint / gateway.hostname consistency tests ---

func TestValidate_GatewayPublicEndpointHostMatchesAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.127-0-0-1.nip.io",
	}
	k.Spec.Bootstrap.PublicEndpoint = "https://keystone.127-0-0-1.nip.io:8443/v3"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_GatewayPublicEndpointWithoutPortAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	k.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com/v3"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_GatewayPublicEndpointHostMismatchRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	k.Spec.Bootstrap.PublicEndpoint = "https://keystone.other.example.com/v3"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("publicEndpoint"))
	g.Expect(err.Error()).To(ContainSubstring("keystone.other.example.com"))
	g.Expect(err.Error()).To(ContainSubstring("keystone.example.com"))
}

func TestValidate_GatewayPublicEndpointNonHTTPSRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	k.Spec.Bootstrap.PublicEndpoint = "http://keystone.example.com/v3"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("publicEndpoint"))
	g.Expect(err.Error()).To(ContainSubstring("https"))
}

func TestValidate_GatewayPublicEndpointMalformedRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = &GatewaySpec{
		ParentRef: GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.example.com",
	}
	// url.Parse rejects control characters (and a few other malformed inputs)
	// outright, surfacing the schema-level "must be a valid URL" branch.
	k.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com/\x00/v3"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("publicEndpoint"))
}

func TestValidate_PublicEndpointWithoutGatewayAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// publicEndpoint set without spec.gateway is the legacy path: the operator
	// writes it into the catalog but does not create an HTTPRoute. No
	// hostname-consistency rule applies.
	k.Spec.Gateway = nil
	k.Spec.Bootstrap.PublicEndpoint = "https://keystone.legacy.example.com/v3"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_GatewayNil_Accepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Gateway = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- PriorityClass validation ---

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
	k.Spec.Deployment.PriorityClassName = &pcn

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
	k.Spec.Deployment.PriorityClassName = &pcn

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

// --- TopologySpreadConstraints label selector validation ---

func TestValidate_TopologySpreadConstraintCorrectLabelsAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Name = "my-ks"
	k.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
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
	k.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
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
	k.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
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
	k.Spec.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
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

// --- Graceful termination: termination grace / preStop sleep range checks ---

func TestValidate_TerminationGracePeriodBelowMinRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(9)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("terminationGracePeriodSeconds"))
	g.Expect(err.Error()).To(ContainSubstring("at least 10"))
}

func TestValidate_TerminationGracePeriodAtMinAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(10)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_TerminationGracePeriodNilAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.TerminationGracePeriodSeconds = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PreStopSleepSecondsNegativeRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	preStop := int64(-1)
	k.Spec.Deployment.PreStopSleepSeconds = &preStop

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("preStopSleepSeconds"))
}

func TestValidate_PreStopSleepSecondsZeroAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	preStop := int64(0)
	k.Spec.Deployment.PreStopSleepSeconds = &preStop

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PreStopSleepSecondsNilAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.PreStopSleepSeconds = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Graceful termination: preStop vs grace ordering ---

func TestValidate_PreStopEqualsTerminationGraceRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(30)
	preStop := int64(30)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace
	k.Spec.Deployment.PreStopSleepSeconds = &preStop

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("preStopSleepSeconds"))
	g.Expect(err.Error()).To(ContainSubstring("terminationGracePeriodSeconds"))
}

func TestValidate_PreStopExceedsTerminationGraceRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(30)
	preStop := int64(45)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace
	k.Spec.Deployment.PreStopSleepSeconds = &preStop

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("preStopSleepSeconds"))
}

func TestValidate_PreStopStrictlyLessAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(60)
	preStop := int64(5)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace
	k.Spec.Deployment.PreStopSleepSeconds = &preStop

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_PreStopAndGraceNilAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.TerminationGracePeriodSeconds = nil
	k.Spec.Deployment.PreStopSleepSeconds = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Graceful termination: UWSGI harakiri / keepAliveTimeout range checks ---

func TestValidate_UWSGIHarakiriBelowMinRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	harakiri := int32(0)
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 2,
		Threads:   1,
		Harakiri:  &harakiri,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("harakiri"))
	g.Expect(err.Error()).To(ContainSubstring("at least 1"))
}

func TestValidate_UWSGIHarakiriAtMinAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	harakiri := int32(1)
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 2,
		Threads:   1,
		Harakiri:  &harakiri,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_UWSGIKeepAliveTimeoutBelowMinRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	timeout := int32(0)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        ptr.To(true),
		HTTPKeepAliveTimeout: &timeout,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("httpKeepAliveTimeout"))
	g.Expect(err.Error()).To(ContainSubstring("at least 1"))
}

func TestValidate_UWSGIKeepAliveTimeoutAtMinAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	timeout := int32(1)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        ptr.To(true),
		HTTPKeepAliveTimeout: &timeout,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Graceful termination: harakiri within drain window ---

func TestValidate_HarakiriAtDrainBoundaryRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(30)
	preStop := int64(5)
	// drain = 30 - 5 = 25; harakiri == drain must be rejected.
	harakiri := int32(25)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace
	k.Spec.Deployment.PreStopSleepSeconds = &preStop
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 2,
		Threads:   1,
		Harakiri:  &harakiri,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("harakiri"))
}

func TestValidate_HarakiriAboveDrainRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	grace := int64(30)
	preStop := int64(5)
	harakiri := int32(40)
	k.Spec.Deployment.TerminationGracePeriodSeconds = &grace
	k.Spec.Deployment.PreStopSleepSeconds = &preStop
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 2,
		Threads:   1,
		Harakiri:  &harakiri,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("harakiri"))
}

func TestValidate_HarakiriWithinDrainAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// Defaults resolved: grace=30, preStop=5, drain=25; harakiri=20 < 25.
	harakiri := int32(20)
	k.Spec.UWSGI = &UWSGISpec{
		Processes: 2,
		Threads:   1,
		Harakiri:  &harakiri,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Graceful termination: keepAliveTimeout requires keepAlive ---

func TestValidate_KeepAliveTimeoutWithoutKeepAliveRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	timeout := int32(5)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        ptr.To(false),
		HTTPKeepAliveTimeout: &timeout,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("httpKeepAliveTimeout"))
	g.Expect(err.Error()).To(ContainSubstring("httpKeepAlive"))
}

func TestValidate_KeepAliveTimeoutWithKeepAliveAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	timeout := int32(5)
	k.Spec.UWSGI = &UWSGISpec{
		Processes:            2,
		Threads:              1,
		HTTPKeepAlive:        ptr.To(true),
		HTTPKeepAliveTimeout: &timeout,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Graceful termination: deployment strategy sanity ---

func TestValidate_StrategyRecreateWithRollingUpdateBlockRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)
	k.Spec.Deployment.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("strategy"))
	g.Expect(err.Error()).To(ContainSubstring("rollingUpdate"))
}

func TestValidate_StrategyRecreateWithoutRollingUpdateAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_StrategyRollingUpdateAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)
	k.Spec.Deployment.Strategy = &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_StrategyNilAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Deployment.Strategy = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

// --- Database TLS validation/defaulting tests ---

// validDatabaseTLS returns a fully-populated, valid DatabaseTLSSpec that tests
// mutate to exercise individual rules (mirrors the validKeystone() pattern).
func validDatabaseTLS() *commonv1.DatabaseTLSSpec {
	return &commonv1.DatabaseTLSSpec{
		Mode:                "require",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca-bundle", Key: "ca.crt"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "keystone-db-client", Key: "tls.crt"},
	}
}

func TestValidate_TLSNilAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Database.TLS = nil

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_TLSValidAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Database.TLS = validDatabaseTLS()

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_ImageTagAndDigestBothSetRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// validKeystone() already sets a tag; adding a digest violates the XOR.
	k.Spec.Image.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest"))
}

func TestValidate_ImageNeitherTagNorDigestRejected(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Image.Tag = ""
	k.Spec.Image.Digest = ""

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of image.tag or image.digest"))
}

func TestValidate_ImageDigestOnlyAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Image.Tag = ""
	k.Spec.Image.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_TLSDisabledWithoutSecretRefsAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	// mode: "disabled" means TLS is off, so the secret refs are not required;
	// the disabled block is accepted with no cert references.
	k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{Mode: "disabled"}

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestValidate_TLSAllFourModesAccepted(t *testing.T) {
	g := NewGomegaWithT(t)
	for _, mode := range []string{"prefer", "require", "verify-ca", "verify-full"} {
		w := &KeystoneWebhook{}
		k := validKeystone()
		k.Spec.Database.TLS = validDatabaseTLS()
		k.Spec.Database.TLS.Mode = mode

		_, err := w.ValidateCreate(context.Background(), k)
		g.Expect(err).NotTo(HaveOccurred(), "mode %q should be accepted", mode)
	}
}

func TestValidate_RejectsOutOfEnumTLSMode(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	k := validKeystone()
	k.Spec.Database.TLS = validDatabaseTLS()
	k.Spec.Database.TLS.Mode = "bogus"

	_, err := w.ValidateCreate(context.Background(), k)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.database.tls.mode"))
	g.Expect(err.Error()).To(ContainSubstring("bogus"))
}

func TestValidate_RejectsEnabledWithoutSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)

	t.Run("missing caBundleSecretRef", func(t *testing.T) {
		w := &KeystoneWebhook{}
		k := validKeystone()
		k.Spec.Database.TLS = validDatabaseTLS()
		k.Spec.Database.TLS.CABundleSecretRef.Name = ""

		_, err := w.ValidateCreate(context.Background(), k)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.database.tls.caBundleSecretRef.name"))
	})

	t.Run("missing clientCertSecretRef", func(t *testing.T) {
		w := &KeystoneWebhook{}
		k := validKeystone()
		k.Spec.Database.TLS = validDatabaseTLS()
		k.Spec.Database.TLS.ClientCertSecretRef.Name = ""

		_, err := w.ValidateCreate(context.Background(), k)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.database.tls.clientCertSecretRef.name"))
	})

	t.Run("both refs missing reports both errors", func(t *testing.T) {
		w := &KeystoneWebhook{}
		k := validKeystone()
		k.Spec.Database.TLS = validDatabaseTLS()
		k.Spec.Database.TLS.CABundleSecretRef.Name = ""
		k.Spec.Database.TLS.ClientCertSecretRef.Name = ""

		_, err := w.ValidateCreate(context.Background(), k)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("caBundleSecretRef.name"))
		g.Expect(err.Error()).To(ContainSubstring("clientCertSecretRef.name"))
	})
}

func TestDefault_DoesNotMaterializeTLSBlock(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}
	// A CR with no tls block (the upgrade scenario) must remain plaintext:
	// Default() must not allocate the pointer.
	k := validKeystone()
	k.Spec.Database.TLS = nil

	g.Expect(w.Default(context.Background(), k)).To(Succeed())
	g.Expect(k.Spec.Database.TLS).To(BeNil())

	// An explicitly-disabled block must stay disabled: mode: "disabled" is a
	// non-empty mode, so Default() leaves it untouched and IsEnabled stays false.
	k2 := validKeystone()
	k2.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{Mode: "disabled"}

	g.Expect(w.Default(context.Background(), k2)).To(Succeed())
	g.Expect(k2.Spec.Database.TLS).NotTo(BeNil())
	g.Expect(k2.Spec.Database.TLS.Mode).To(Equal("disabled"))
	g.Expect(k2.Spec.Database.TLS.IsEnabled()).To(BeFalse())
}

func TestDefault_DefaultsModeOnlyWhenBlockPresent(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &KeystoneWebhook{}

	t.Run("empty mode defaulted to baseline when block present", func(t *testing.T) {
		k := validKeystone()
		k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{Mode: ""}

		g.Expect(w.Default(context.Background(), k)).To(Succeed())
		g.Expect(k.Spec.Database.TLS.Mode).To(Equal(DefaultDatabaseTLSMode))
		g.Expect(DefaultDatabaseTLSMode).To(Equal("require"))
		// A present block with an empty mode means "on": defaulting to require
		// makes IsEnabled true.
		g.Expect(k.Spec.Database.TLS.IsEnabled()).To(BeTrue())
	})

	t.Run("explicit mode preserved", func(t *testing.T) {
		k := validKeystone()
		k.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{Mode: "verify-full"}

		g.Expect(w.Default(context.Background(), k)).To(Succeed())
		g.Expect(k.Spec.Database.TLS.Mode).To(Equal("verify-full"))
	})

	t.Run("nil block not materialized", func(t *testing.T) {
		k := validKeystone()
		k.Spec.Database.TLS = nil

		g.Expect(w.Default(context.Background(), k)).To(Succeed())
		g.Expect(k.Spec.Database.TLS).To(BeNil())
	})
}

// --- Interface compliance ---

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
