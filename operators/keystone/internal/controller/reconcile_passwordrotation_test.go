// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// pwRotationTestScheme returns a runtime.Scheme with all types needed for
// admin-password rotation tests.
func pwRotationTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = esov1alpha1.SchemeBuilder.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

// pwRotationTestKeystone returns a Keystone CR with admin-password rotation
// enabled (post-webhook leaf defaults materialized).
func pwRotationTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			Generation: 1,
			UID:        "test-uid",
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
				PasswordRotation: &keystonev1alpha1.PasswordRotationSpec{
					Enabled:        true,
					Schedule:       "0 0 1 * *",
					Suspend:        false,
					PasswordLength: 32,
				},
			},
		},
	}
}

// validTestPassword is a 40-character password — longer than the default
// generated length (32) and the minimum floor (24), so it passes validation.
const validTestPassword = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newPWRotationReconciler(objs ...client.Object) (*KeystoneReconciler, *record.FakeRecorder) {
	s := pwRotationTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rec := record.NewFakeRecorder(20)
	return &KeystoneReconciler{Client: c, Scheme: s, Recorder: rec}, rec
}

// drainEventReasons collects the reason token (2nd whitespace field) of every
// buffered FakeRecorder event. FakeRecorder formats events as
// "<Type> <Reason> <message>".
func drainEventReasons(rec *record.FakeRecorder) []string {
	var reasons []string
	for {
		select {
		case e := <-rec.Events:
			fields := strings.Fields(e)
			if len(fields) >= 2 {
				reasons = append(reasons, fields[1])
			}
		default:
			return reasons
		}
	}
}

// ---------------------------------------------------------------------------
// Task 2.1 (CC-0109) — embedded script
// ---------------------------------------------------------------------------

// TestAdminPasswordRotateScript_EmbeddedContent verifies the go:embed directive
// loads scripts/admin_password_rotate.sh into adminPasswordRotateScript. A
// broken embed silently yields an empty string that would fail the CronJob pod
// at runtime (CC-0109, REQ-006).
func TestAdminPasswordRotateScript_EmbeddedContent(t *testing.T) {
	g := NewWithT(t)

	g.Expect(adminPasswordRotateScript).NotTo(BeEmpty(),
		"adminPasswordRotateScript must not be empty — check go:embed directive")
	g.Expect(adminPasswordRotateScript).To(HavePrefix("#!/bin/sh\n"))
	g.Expect(adminPasswordRotateScript).To(ContainSubstring("SPDX-License-Identifier: Apache-2.0"))
	g.Expect(adminPasswordRotateScript).To(ContainSubstring("set -eu"))
	// Strong-password generation via the Python stdlib (no keystone-manage step).
	g.Expect(adminPasswordRotateScript).To(ContainSubstring("secrets.token_urlsafe"))
	// The Python heredoc for K8s API Secret patching is present.
	g.Expect(adminPasswordRotateScript).To(ContainSubstring("python3 << 'PYTHON'"))
}

// ---------------------------------------------------------------------------
// Task 2.2 (CC-0109) — name helpers, push-source/staging Secrets, split RBAC
// ---------------------------------------------------------------------------

func TestAdminPasswordRotation_NameHelpers(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	g.Expect(adminPasswordStagingSecretName(ks)).To(Equal("test-keystone-admin-password-rotation"))
	g.Expect(adminPasswordNextSecretName(ks)).To(Equal("test-keystone-admin-password-next"))
	g.Expect(adminPasswordRotateSAName(ks)).To(Equal("test-keystone-admin-password-rotate"))
	g.Expect(adminPasswordPushSecretName(ks)).To(Equal("test-keystone-admin-password-backup"))
}

func TestEnsureAdminPasswordPushSourceSecret_CreatesOwnedEmptySecret(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	r, _ := newPWRotationReconciler(ks)

	g.Expect(r.ensureAdminPasswordPushSourceSecret(context.Background(), ks)).To(Succeed())

	var sec corev1.Secret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}, &sec)).To(Succeed())
	// Operator owns the object; .data starts empty (applyAdminPasswordRotation owns data).
	g.Expect(sec.Data).To(BeEmpty())
	g.Expect(metav1.GetControllerOf(&sec)).NotTo(BeNil())
	g.Expect(metav1.GetControllerOf(&sec).UID).To(Equal(ks.UID))
}

func TestEnsureStagingSecret_AdminPasswordLabel(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	r, _ := newPWRotationReconciler(ks)

	g.Expect(r.ensureStagingSecret(context.Background(), ks, adminPasswordStagingSecretName(ks), "admin-password")).To(Succeed())

	var sec corev1.Secret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace,
	}, &sec)).To(Succeed())
	g.Expect(sec.Labels).To(HaveKeyWithValue(StagingSecretLabelKey, "admin-password"))
}

func TestEnsureAdminPasswordRotationRBAC_SplitRoleShape(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	r, _ := newPWRotationReconciler(ks)
	saName := adminPasswordRotateSAName(ks)

	g.Expect(r.ensureAdminPasswordRotationRBAC(context.Background(), ks)).To(Succeed())

	var sa corev1.ServiceAccount
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: ks.Namespace}, &sa)).To(Succeed())

	var role rbacv1.Role
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: ks.Namespace}, &role)).To(Succeed())
	g.Expect(role.Rules).To(HaveLen(2))

	// Match each rule by the Secret it scopes (ResourceNames) rather than by a
	// positional index, so adding or reordering a rule cannot silently
	// mis-assert (CC-0109).
	nextRule := findPolicyRuleByResourceName(role.Rules, adminPasswordNextSecretName(ks))
	g.Expect(nextRule).NotTo(BeNil(), "expected a rule scoped to the push-source Secret")
	// Read-only get on the operator-owned push-source Secret.
	g.Expect(nextRule.APIGroups).To(Equal([]string{""}))
	g.Expect(nextRule.Resources).To(Equal([]string{"secrets"}))
	g.Expect(nextRule.Verbs).To(Equal([]string{"get"}))

	stagingRule := findPolicyRuleByResourceName(role.Rules, adminPasswordStagingSecretName(ks))
	g.Expect(stagingRule).NotTo(BeNil(), "expected a rule scoped to the staging Secret")
	// get+patch scoped to the staging Secret only (no create/delete).
	g.Expect(stagingRule.APIGroups).To(Equal([]string{""}))
	g.Expect(stagingRule.Resources).To(Equal([]string{"secrets"}))
	g.Expect(stagingRule.Verbs).To(Equal([]string{"get", "patch"}))

	var rb rbacv1.RoleBinding
	g.Expect(r.Get(context.Background(), types.NamespacedName{Name: saName, Namespace: ks.Namespace}, &rb)).To(Succeed())
	g.Expect(rb.RoleRef.Name).To(Equal(saName))
	g.Expect(rb.Subjects).To(HaveLen(1))
	g.Expect(rb.Subjects[0].Name).To(Equal(saName))
}

// ---------------------------------------------------------------------------
// Task 2.3 (CC-0109) — CronJob builder
// ---------------------------------------------------------------------------

func TestAdminPasswordRotationCronJob_Shape(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	cj := adminPasswordRotationCronJob(ks, "test-keystone-admin-password-rotate-script-abc123")

	g.Expect(cj.Name).To(Equal("test-keystone-admin-password-rotate"))
	g.Expect(cj.Spec.Schedule).To(Equal("0 0 1 * *"))

	podSpec := cj.Spec.JobTemplate.Spec.Template.Spec
	g.Expect(podSpec.ServiceAccountName).To(Equal("test-keystone-admin-password-rotate"))
	g.Expect(podSpec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))

	container := findContainerByName(podSpec.Containers, "admin-password-rotate")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(Equal([]string{"/scripts/admin_password_rotate.sh"}))
	expectRestrictedSecurityContext(g, container)

	// SECRET_NAME points at the staging Secret (never the push-source Secret).
	env := envByName(container.Env)
	g.Expect(env).To(HaveKeyWithValue("SECRET_NAME", "test-keystone-admin-password-rotation"))
	g.Expect(env).To(HaveKey("PASSWORD_LENGTH"))
	g.Expect(env["PASSWORD_LENGTH"]).To(Equal("32"))
	// SECRET_NAMESPACE is a downward-API fieldRef, not a literal value.
	secretNamespaceEnv := findEnvVar(container.Env, "SECRET_NAMESPACE")
	g.Expect(secretNamespaceEnv).NotTo(BeNil())
	g.Expect(secretNamespaceEnv.ValueFrom).NotTo(BeNil())
	g.Expect(secretNamespaceEnv.ValueFrom.FieldRef.FieldPath).To(Equal("metadata.namespace"))

	// Script ConfigMap volume mounted read-only at /scripts with 0555 mode.
	g.Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{
		Name: "scripts", MountPath: "/scripts", ReadOnly: true,
	}))
	scriptsVol := findVolumeByName(podSpec.Volumes, "scripts")
	g.Expect(scriptsVol).NotTo(BeNil())
	g.Expect(scriptsVol.ConfigMap).NotTo(BeNil())
	g.Expect(scriptsVol.ConfigMap.Name).To(Equal("test-keystone-admin-password-rotate-script-abc123"))
	g.Expect(*scriptsVol.ConfigMap.DefaultMode).To(Equal(int32(0o555)))
}

func TestAdminPasswordRotationCronJob_Suspend(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	ks.Spec.Bootstrap.PasswordRotation.Suspend = true
	cj := adminPasswordRotationCronJob(ks, "script-cm")
	g.Expect(cj.Spec.Suspend).NotTo(BeNil())
	g.Expect(*cj.Spec.Suspend).To(BeTrue())

	ks.Spec.Bootstrap.PasswordRotation.Suspend = false
	cj2 := adminPasswordRotationCronJob(ks, "script-cm")
	g.Expect(cj2.Spec.Suspend).NotTo(BeNil())
	g.Expect(*cj2.Spec.Suspend).To(BeFalse())
}

// ---------------------------------------------------------------------------
// Task 2.4 (CC-0109) — validation and apply
// ---------------------------------------------------------------------------

func TestValidateAdminPasswordRotationOutput(t *testing.T) {
	g := NewWithT(t)

	// Valid.
	g.Expect(validateAdminPasswordRotationOutput(
		map[string][]byte{"password": []byte(validTestPassword)}, 24,
	)).To(Succeed())

	// Missing key.
	err := validateAdminPasswordRotationOutput(map[string][]byte{}, 24)
	g.Expect(errors.Is(err, ErrAdminPasswordMissing)).To(BeTrue())

	// Empty value.
	err = validateAdminPasswordRotationOutput(map[string][]byte{"password": {}}, 24)
	g.Expect(errors.Is(err, ErrAdminPasswordMissing)).To(BeTrue())

	// Too short.
	err = validateAdminPasswordRotationOutput(map[string][]byte{"password": []byte("short")}, 24)
	g.Expect(errors.Is(err, ErrAdminPasswordTooShort)).To(BeTrue())
}

func validCompletedAnnotation() map[string]string {
	return map[string]string{RotationCompletedAnnotation: "2026-06-01T00:00:00Z"}
}

func TestApplyAdminPasswordRotation_ValidCommit(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	pushSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminPasswordStagingSecretName(ks),
			Namespace:   ks.Namespace,
			Annotations: validCompletedAnnotation(),
		},
		Data: map[string][]byte{"password": []byte(validTestPassword)},
	}
	r, rec := newPWRotationReconciler(ks, pushSource, staging)

	applied, err := r.applyAdminPasswordRotation(context.Background(), ks, 24)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeTrue())

	// Push-source Secret carries the committed password and annotation.
	var committed corev1.Secret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}, &committed)).To(Succeed())
	g.Expect(committed.Data).To(HaveKeyWithValue("password", []byte(validTestPassword)))
	g.Expect(committed.Annotations).To(HaveKeyWithValue(RotationCompletedAnnotation, "2026-06-01T00:00:00Z"))

	// Staging Secret deleted.
	var leftover corev1.Secret
	getErr := r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace,
	}, &leftover)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue())

	g.Expect(drainEventReasons(rec)).To(ContainElement("AdminPasswordRotated"))
}

func TestApplyAdminPasswordRotation_RejectsShortPassword(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	pushSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminPasswordStagingSecretName(ks),
			Namespace:   ks.Namespace,
			Annotations: validCompletedAnnotation(),
		},
		Data: map[string][]byte{"password": []byte("too-short")},
	}
	r, rec := newPWRotationReconciler(ks, pushSource, staging)

	applied, err := r.applyAdminPasswordRotation(context.Background(), ks, 24)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())

	// Staging retained for inspection; push-source untouched.
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace,
	}, &corev1.Secret{})).To(Succeed())
	var committed corev1.Secret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}, &committed)).To(Succeed())
	g.Expect(committed.Data).To(BeEmpty())

	g.Expect(drainEventReasons(rec)).To(ContainElement("AdminPasswordRotationRejected"))
}

func TestApplyAdminPasswordRotation_AbsentStaging_NoOp(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	pushSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	r, _ := newPWRotationReconciler(ks, pushSource)

	applied, err := r.applyAdminPasswordRotation(context.Background(), ks, 24)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())
}

func TestApplyAdminPasswordRotation_MissingAnnotation_NoOp(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	pushSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace},
		Data:       map[string][]byte{"password": []byte(validTestPassword)},
	}
	r, _ := newPWRotationReconciler(ks, pushSource, staging)

	applied, err := r.applyAdminPasswordRotation(context.Background(), ks, 24)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())
	// Staging retained (no annotation yet → CronJob hasn't completed a write).
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace,
	}, &corev1.Secret{})).To(Succeed())
}

func TestApplyAdminPasswordRotation_MalformedAnnotation_Warns(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	pushSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminPasswordStagingSecretName(ks),
			Namespace:   ks.Namespace,
			Annotations: map[string]string{RotationCompletedAnnotation: "not-a-timestamp"},
		},
		Data: map[string][]byte{"password": []byte(validTestPassword)},
	}
	r, rec := newPWRotationReconciler(ks, pushSource, staging)

	applied, err := r.applyAdminPasswordRotation(context.Background(), ks, 24)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())
	g.Expect(drainEventReasons(rec)).To(ContainElement("AdminPasswordRotationAnnotationInvalid"))
}

// ---------------------------------------------------------------------------
// Task 2.5 (CC-0109) — PushSecret builder + clobber-safe gating
// ---------------------------------------------------------------------------

func TestAdminPasswordPushSecret_Shape(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	ps := adminPasswordPushSecret(ks)
	g.Expect(ps.Name).To(Equal("test-keystone-admin-password-backup"))
	g.Expect(ps.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(ps.Spec.SecretStoreRefs[0].Kind).To(Equal("ClusterSecretStore"))
	g.Expect(ps.Spec.SecretStoreRefs[0].Name).To(Equal("openbao-cluster-store"))
	g.Expect(ps.Spec.Selector.Secret.Name).To(Equal(adminPasswordNextSecretName(ks)))
	// Persistent bootstrap path: never purge OpenBao on PushSecret delete.
	g.Expect(ps.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyNone))
	// RemoteKey is the per-CR path bootstrap/{namespace}/{name}/admin (CC-0112, REQ-002).
	g.Expect(ps.Spec.Data).To(HaveLen(1))
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(fmt.Sprintf("bootstrap/%s/%s/admin", ks.Namespace, ks.Name)))
	g.Expect(ps.Spec.Data[0].Match.SecretKey).To(Equal("password"))
}

// TestAdminPasswordPushSecret_RemoteKeyIsPerCR pins CC-0112 (REQ-002): the
// admin-password RemoteKey embeds both the CR namespace and name as path
// segments, so two Keystone CRs sharing a Name in different namespaces resolve
// to distinct OpenBao leaves and never clobber each other's bootstrap secret.
func TestAdminPasswordPushSecret_RemoteKeyIsPerCR(t *testing.T) {
	g := NewWithT(t)

	ksA := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "openstack"},
	}
	ksB := &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "tenant-b"},
	}

	psA := adminPasswordPushSecret(ksA)
	psB := adminPasswordPushSecret(ksB)

	g.Expect(psA.Spec.Data).To(HaveLen(1))
	g.Expect(psB.Spec.Data).To(HaveLen(1))
	keyA := psA.Spec.Data[0].Match.RemoteRef.RemoteKey
	keyB := psB.Spec.Data[0].Match.RemoteRef.RemoteKey

	g.Expect(keyA).To(Equal("bootstrap/openstack/keystone/admin"))
	g.Expect(keyB).To(Equal("bootstrap/tenant-b/keystone/admin"))
	g.Expect(keyA).NotTo(Equal(keyB),
		"same-named CRs in different namespaces must produce distinct RemoteKeys (CC-0112)")
}

func TestAdminPasswordPushSourceReady_Gating(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()

	// Empty push-source → not ready.
	emptySource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	r, _ := newPWRotationReconciler(ks, emptySource)
	g.Expect(r.adminPasswordPushSourceReady(context.Background(), ks, 32)).To(BeFalse())

	// Populated with a valid password → ready.
	r2, _ := newPWRotationReconciler(ks, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace},
		Data:       map[string][]byte{"password": []byte(validTestPassword)},
	})
	g.Expect(r2.adminPasswordPushSourceReady(context.Background(), ks, 32)).To(BeTrue())
}

// ---------------------------------------------------------------------------
// Task 2.6 (CC-0109) — enabled-path entry, ordering, gating
// ---------------------------------------------------------------------------

func TestReconcilePasswordRotation_Enabled_HappyPath(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	r, _ := newPWRotationReconciler(ks)

	res, err := r.reconcilePasswordRotation(context.Background(), ks, "test-config-cm")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// Condition is True with the configured reason.
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePasswordRotationReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("PasswordRotationConfigured"))
	g.Expect(cond.ObservedGeneration).To(Equal(ks.Generation))

	ctx := context.Background()
	// Push-source + staging Secrets exist.
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace}, &corev1.Secret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace}, &corev1.Secret{})).To(Succeed())
	// RBAC exists.
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordRotateSAName(ks), Namespace: ks.Namespace}, &corev1.ServiceAccount{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordRotateSAName(ks), Namespace: ks.Namespace}, &rbacv1.Role{})).To(Succeed())
	// CronJob exists.
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordRotateCronJobName(ks), Namespace: ks.Namespace}, &batchv1.CronJob{})).To(Succeed())
	// Clobber-safe gate: no PushSecret yet (push-source has no valid password).
	psErr := r.Get(ctx, types.NamespacedName{Name: adminPasswordPushSecretName(ks), Namespace: ks.Namespace}, &esov1alpha1.PushSecret{})
	g.Expect(apierrors.IsNotFound(psErr)).To(BeTrue())
}

// TestReconcilePasswordRotation_Enabled_Suspended verifies the full enabled-path
// reconcile still materializes every Model B resource when Suspend=true — only
// the CronJob is paused (spec.suspend); the staging/push-source Secrets, split
// RBAC, and PasswordRotationReady condition are unchanged. Complements the
// builder-level TestAdminPasswordRotationCronJob_Suspend with a full
// sub-reconcile pass so a future change that conflated suspend with teardown is
// caught at the unit level (CC-0109, REQ-006).
func TestReconcilePasswordRotation_Enabled_Suspended(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	ks := pwRotationTestKeystone()
	ks.Spec.Bootstrap.PasswordRotation.Suspend = true
	r, _ := newPWRotationReconciler(ks)

	res, err := r.reconcilePasswordRotation(ctx, ks, "test-config-cm")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// The CronJob exists but is suspended — the feature is configured, not torn down.
	var cj batchv1.CronJob
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordRotateCronJobName(ks), Namespace: ks.Namespace}, &cj)).To(Succeed())
	g.Expect(cj.Spec.Suspend).NotTo(BeNil())
	g.Expect(*cj.Spec.Suspend).To(BeTrue())

	// Sibling resources are still ensured (suspend pauses scheduling only).
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace}, &corev1.Secret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordStagingSecretName(ks), Namespace: ks.Namespace}, &corev1.Secret{})).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordRotateSAName(ks), Namespace: ks.Namespace}, &rbacv1.Role{})).To(Succeed())

	// Condition still reports configured (not disabled).
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePasswordRotationReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("PasswordRotationConfigured"))
}

func TestReconcilePasswordRotation_ValidPushSource_EnsuresPushSecret(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	// Push-source already holds a valid committed password (a prior rotation).
	pushSource := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace},
		Data:       map[string][]byte{"password": []byte(validTestPassword)},
	}
	r, _ := newPWRotationReconciler(ks, pushSource)

	res, err := r.reconcilePasswordRotation(context.Background(), ks, "test-config-cm")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// PushSecret is ensured once the push-source holds a valid password.
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordPushSecretName(ks), Namespace: ks.Namespace,
	}, &esov1alpha1.PushSecret{})).To(Succeed())
}

func TestReconcilePasswordRotation_AppliesCompletedRotation_Requeues(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	pushSource := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminPasswordStagingSecretName(ks),
			Namespace:   ks.Namespace,
			Annotations: validCompletedAnnotation(),
		},
		Data: map[string][]byte{"password": []byte(validTestPassword)},
	}
	r, rec := newPWRotationReconciler(ks, pushSource, staging)

	res, err := r.reconcilePasswordRotation(context.Background(), ks, "test-config-cm")
	g.Expect(err).NotTo(HaveOccurred())
	// Apply short-circuits with a requeue (non-zero Result). IsZero() avoids the
	// deprecated Result.Requeue field while still asserting the requeue happened.
	g.Expect(res.IsZero()).To(BeFalse())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePasswordRotationReady)
	g.Expect(cond.Reason).To(Equal("AdminPasswordRotated"))
	g.Expect(drainEventReasons(rec)).To(ContainElement("AdminPasswordRotated"))

	// Committed onto push-source and staging cleared.
	var committed corev1.Secret
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordNextSecretName(ks), Namespace: ks.Namespace,
	}, &committed)).To(Succeed())
	g.Expect(committed.Data).To(HaveKeyWithValue("password", []byte(validTestPassword)))
}

// ---------------------------------------------------------------------------
// Task 2.7 (CC-0109) — disabled / teardown branch
// ---------------------------------------------------------------------------

func TestReconcilePasswordRotation_Disabled_TearsDownEverything(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// First, run the enabled path to materialize all Model B resources.
	ks := pwRotationTestKeystone()
	r, _ := newPWRotationReconciler(ks)
	_, err := r.reconcilePasswordRotation(ctx, ks, "test-config-cm")
	g.Expect(err).NotTo(HaveOccurred())

	// Sanity: CronJob exists before teardown.
	g.Expect(r.Get(ctx, types.NamespacedName{Name: adminPasswordRotateCronJobName(ks), Namespace: ks.Namespace}, &batchv1.CronJob{})).To(Succeed())

	// Now disable rotation and reconcile again.
	ks.Spec.Bootstrap.PasswordRotation.Enabled = false
	res, err := r.reconcilePasswordRotation(ctx, ks, "test-config-cm")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePasswordRotationReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("RotationDisabled"))

	// Every Model B resource is gone.
	expectNotFound(g, r, &batchv1.CronJob{}, adminPasswordRotateCronJobName(ks), ks.Namespace)
	expectNotFound(g, r, &corev1.Secret{}, adminPasswordStagingSecretName(ks), ks.Namespace)
	expectNotFound(g, r, &corev1.Secret{}, adminPasswordNextSecretName(ks), ks.Namespace)
	expectNotFound(g, r, &corev1.ServiceAccount{}, adminPasswordRotateSAName(ks), ks.Namespace)
	expectNotFound(g, r, &rbacv1.Role{}, adminPasswordRotateSAName(ks), ks.Namespace)
	expectNotFound(g, r, &rbacv1.RoleBinding{}, adminPasswordRotateSAName(ks), ks.Namespace)

	// No script ConfigMaps remain.
	var cms corev1.ConfigMapList
	g.Expect(r.List(ctx, &cms, client.InNamespace(ks.Namespace))).To(Succeed())
	for _, cm := range cms.Items {
		g.Expect(cm.Name).NotTo(HavePrefix(adminPasswordRotateScriptBaseName(ks)))
	}
}

func TestReconcilePasswordRotation_NilSpec_TeardownIdempotent(t *testing.T) {
	g := NewWithT(t)
	ks := pwRotationTestKeystone()
	ks.Spec.Bootstrap.PasswordRotation = nil
	r, _ := newPWRotationReconciler(ks)

	// Two consecutive teardown reconciles must both succeed (idempotent).
	for i := 0; i < 2; i++ {
		res, err := r.reconcilePasswordRotation(context.Background(), ks, "test-config-cm")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
	}
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypePasswordRotationReady)
	g.Expect(cond.Reason).To(Equal("RotationDisabled"))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func envByName(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		if e.ValueFrom == nil {
			out[e.Name] = e.Value
		}
	}
	return out
}

func findEnvVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func expectNotFound(g *WithT, r *KeystoneReconciler, obj client.Object, name, namespace string) {
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, obj)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %T %s to be deleted", obj, name)
}

// findPolicyRuleByResourceName returns the PolicyRule whose ResourceNames
// contains target, or nil if none match. Split-RBAC assertions use this to
// match each rule by the Secret it scopes rather than by a fragile positional
// index, so a future reorder or inserted rule cannot silently mis-assert
// (CC-0109).
func findPolicyRuleByResourceName(rules []rbacv1.PolicyRule, target string) *rbacv1.PolicyRule {
	for i := range rules {
		for _, name := range rules[i].ResourceNames {
			if name == target {
				return &rules[i]
			}
		}
	}
	return nil
}
