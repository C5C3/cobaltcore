// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the K-ORC admin-application-credential chain sub-reconcilers
// (CC-0110, REQ-010, REQ-011, REQ-012, REQ-014): reconcileKORC,
// reconcileAdminCredential, reconcileCatalog.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

const testAdminPassword = "super-secret-admin-password"

// korcTestScheme registers c5c3, client-go, K-ORC, and ESO types.
func korcTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := orcv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding K-ORC scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding ESO v1 scheme: %v", err)
	}
	if err := esov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding ESO v1alpha1 scheme: %v", err)
	}
	return s
}

// korcControlPlane builds a ControlPlane with a K-ORC admin-credential spec and
// KORCReady's predecessor (Infrastructure/Keystone) conditions left unset; tests
// add gate conditions as needed.
func korcControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					CloudCredentialsRef: c5c3v1alpha1.CloudCredentialsRef{
						CloudName:  "admin",
						SecretName: "k-orc-clouds-yaml",
					},
					PasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin", Key: "password"},
					ApplicationCredential: c5c3v1alpha1.ApplicationCredentialSpec{
						Rotation: c5c3v1alpha1.RotationSpec{Mode: c5c3v1alpha1.RotationModePasswordDriven},
					},
				},
			},
		},
	}
}

// adminPasswordSecret returns the admin-password Secret the hash is computed from.
func adminPasswordSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte(testAdminPassword)},
	}
}

func testPasswordHash() string {
	sum := sha256.Sum256([]byte(testAdminPassword))
	return hex.EncodeToString(sum[:])
}

func getAC(t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane) *orcv1alpha1.ApplicationCredential {
	t.Helper()
	ac := &orcv1alpha1.ApplicationCredential{}
	key := types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}
	if err := c.Get(context.Background(), key, ac); err != nil {
		t.Fatalf("getting ApplicationCredential %s: %v", key, err)
	}
	return ac
}

// --- 2.7: reconcileKORC mint + inversion ---

func TestReconcileKORC_RestrictedTrueGivesUnrestrictedFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted = ptr.To(true)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(ac.Spec.Resource).NotTo(BeNil())
	g.Expect(ac.Spec.Resource.Unrestricted).NotTo(BeNil())
	g.Expect(*ac.Spec.Resource.Unrestricted).To(BeFalse(),
		"Restricted=true MUST map to Unrestricted=false (critical inversion)")
}

func TestReconcileKORC_RestrictedFalseGivesUnrestrictedTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted = ptr.To(false)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(ac.Spec.Resource.Unrestricted).NotTo(BeNil())
	g.Expect(*ac.Spec.Resource.Unrestricted).To(BeTrue(),
		"Restricted=false MUST map to Unrestricted=true (critical inversion)")
}

func TestReconcileKORC_RestrictedNilDefaultsToUnrestrictedFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane() // Restricted left nil
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(*ac.Spec.Resource.Unrestricted).To(BeFalse(),
		"nil Restricted defaults to true (least privilege) => Unrestricted=false")
}

func TestReconcileKORC_AccessRulesProjected(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	cp.Spec.KORC.AdminCredential.ApplicationCredential.AccessRules = []c5c3v1alpha1.AccessRule{
		{Service: "identity", Method: "GET", Path: "/v3/users"},
		{Service: "compute", Method: "POST", Path: "/v2.1/servers"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(ac.Spec.Resource.AccessRules).To(HaveLen(2))

	first := ac.Spec.Resource.AccessRules[0]
	g.Expect(first.Path).NotTo(BeNil())
	g.Expect(*first.Path).To(Equal("/v3/users"))
	g.Expect(first.Method).NotTo(BeNil())
	g.Expect(string(*first.Method)).To(Equal("GET"))
	g.Expect(first.ServiceRef).NotTo(BeNil())
	g.Expect(string(*first.ServiceRef)).To(Equal("identity"))

	second := ac.Spec.Resource.AccessRules[1]
	g.Expect(string(*second.Method)).To(Equal("POST"))
	g.Expect(*second.Path).To(Equal("/v2.1/servers"))
}

func TestReconcileKORC_OwnerRefAndCloudCredsAndManagementPolicy(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	// Owner reference set to the ControlPlane.
	g.Expect(ac.OwnerReferences).To(HaveLen(1))
	g.Expect(ac.OwnerReferences[0].Name).To(Equal("cp"))
	g.Expect(ac.OwnerReferences[0].Kind).To(Equal("ControlPlane"))

	// CloudCredentialsRef.SecretName matches the admin clouds.yaml secret.
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal("k-orc-clouds-yaml"))
	g.Expect(ac.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))

	// SecretRef points at the operator-owned minted-credential Secret.
	g.Expect(string(ac.Spec.Resource.SecretRef)).To(Equal(adminAppCredentialSecretName(cp)))
	// UserRef derived from the CloudName (DECISION).
	g.Expect(string(ac.Spec.Resource.UserRef)).To(Equal("admin"))
}

func TestReconcileKORC_PasswordHashAnnotationStamped(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(ac.Annotations).To(HaveKeyWithValue(adminPasswordHashAnnotation, testPasswordHash()))
}

func TestReconcileKORC_KORCReadyTrueWhenACAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// Pre-create an Available AC so KORCReady flips True on this pass.
	existing := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)},
		Status: orcv1alpha1.ApplicationCredentialStatus{
			ID: ptr.To("ac-id-123"),
			Conditions: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionTrue,
				Reason:             orcv1alpha1.ConditionReasonSuccess,
				Message:            "ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret(), existing).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	// Status reflects the observed AC.
	g.Expect(cp.Status.AdminApplicationCredential).NotTo(BeNil())
	g.Expect(cp.Status.AdminApplicationCredential.ID).To(Equal("ac-id-123"))
	g.Expect(cp.Status.AdminApplicationCredential.LastRotation).NotTo(BeNil())
}

func TestReconcileKORC_KORCReadyFalseWhileACNotAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForApplicationCredential"))
}

// MISSING-CRD: a no-match error on the AC CR must surface KORCReady=False with no panic / no hard error.
func TestReconcileKORC_MissingCRDNoPanic(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*orcv1alpha1.ApplicationCredential); ok {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "openstack.k-orc.cloud", Kind: "ApplicationCredential"}}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	g.Expect(func() {
		res, err := r.reconcileKORC(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "missing CRD must NOT return a hard error")
		g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	}).NotTo(Panic())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("KORCCRDNotInstalled"))
}

func TestReconcileKORC_WaitsForAdminPassword(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// No admin-password Secret present.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAdminPassword"))
}

// --- 2.7: reconcileAdminCredential push + ES gate ---

func TestReconcileAdminCredential_GatedOnKORC(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane() // KORCReady absent
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminCredentialReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForKORC"))
}

// TestReconcileAdminCredential_FreshClusterWithoutCloudsYamlSeedDoesNotPush is
// the NON-MASKING bootstrap test (CC-0110, FC4/TE9). It reproduces a FRESH
// cluster: KORC has minted the AC, but the k-orc-clouds-yaml ExternalSecret is
// absent (its OpenBao path openstack/keystone/admin/app-credential is empty
// until deploy/openbao/bootstrap/write-bootstrap-secrets.sh seeds a
// password-based bootstrap clouds.yaml). It asserts the gate holds AND, crucially,
// that NO PushSecret is created — proving the OpenBao path is never written while
// the clouds.yaml is unseeded. This is exactly the circular dependency the
// bootstrap seed breaks: without the seed the ExternalSecret never goes Ready, so
// the push that would populate the path never runs. Unlike the happy-path tests,
// this one deliberately does NOT pre-populate a Ready clouds.yaml ExternalSecret,
// so a regression that drops the gate (or silently pushes) fails here.
func TestReconcileAdminCredential_FreshClusterWithoutCloudsYamlSeedDoesNotPush(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	// No k-orc-clouds-yaml ExternalSecret => WaitForExternalSecret returns (false,nil).
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminCredentialReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForCloudsYaml"))

	// The push that writes openstack/keystone/admin/app-credential to OpenBao must
	// NOT have happened — the OpenBao path stays empty, which is precisely why a
	// fresh cluster needs the bootstrap seed to break the cycle.
	ps := &esov1alpha1.PushSecret{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no PushSecret may be created while the clouds.yaml ExternalSecret is unseeded; "+
			"otherwise the bootstrap cycle is masked")
}

func TestReconcileAdminCredential_PushSecretBuiltAndReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyCloudsYamlES(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminCredentialReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	// PushSecret created with the right store + remote path.
	ps := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps)).To(Succeed())
	g.Expect(ps.Spec.SecretStoreRefs).To(HaveLen(1))
	g.Expect(ps.Spec.SecretStoreRefs[0].Name).To(Equal(openBaoClusterStoreName))
	g.Expect(ps.Spec.Selector.Secret.Name).To(Equal(adminAppCredentialSecretName(cp)))
	g.Expect(ps.Spec.Data).To(HaveLen(1))
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(adminAppCredentialRemoteKey))
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal("openstack/keystone/admin/app-credential"))

	// Operator-owned minted-credential Secret ensured (clobber-safe: data untouched).
	sec := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp),
	}, sec)).To(Succeed())
}

func TestReconcileAdminCredential_PushSecretClobberSafeNoChurn(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyCloudsYamlES(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ps1 := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps1)).To(Succeed())
	rv1 := ps1.ResourceVersion

	// Second reconcile must not churn the PushSecret (same spec => no Update).
	_, err = r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ps2 := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps2)).To(Succeed())
	g.Expect(ps2.ResourceVersion).To(Equal(rv1), "repeated reconcile must not churn the PushSecret")
}

// --- 2.8: re-mint (hash) ---

func TestReconcileKORC_HashMismatchRemintsAndStampsNewHash(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// Pre-create an Available AC stamped with a STALE hash and an old ID.
	existing := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminAppCredentialName(cp),
			Namespace:   childNamespace(cp),
			Annotations: map[string]string{adminPasswordHashAnnotation: "stale-hash"},
		},
		Status: orcv1alpha1.ApplicationCredentialStatus{
			ID: ptr.To("old-id"),
			Conditions: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionTrue,
				Reason:             orcv1alpha1.ConditionReasonSuccess,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	// Seed prior status so a re-mint (ID change) is observable as a new rotation.
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{ID: "old-id"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret(), existing).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(ac.Annotations).To(HaveKeyWithValue(adminPasswordHashAnnotation, testPasswordHash()),
		"hash mismatch must re-stamp the annotation with the new password hash")
}

func TestReconcileKORC_HashMatchNoRemint(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// First pass mints the AC (owner ref + spec + hash annotation stamped).
	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac1 := getAC(t, c, cp)
	g.Expect(ac1.Annotations).To(HaveKeyWithValue(adminPasswordHashAnnotation, testPasswordHash()))
	rvBefore := ac1.ResourceVersion
	specBefore := *ac1.Spec.Resource.DeepCopy()

	// Second pass: the admin password is unchanged, so the hash matches and the
	// desired spec is identical => create-or-update must be a no-op (no re-mint,
	// no churn).
	_, err = r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac2 := getAC(t, c, cp)
	g.Expect(ac2.Annotations).To(HaveKeyWithValue(adminPasswordHashAnnotation, testPasswordHash()))
	g.Expect(ac2.ResourceVersion).To(Equal(rvBefore),
		"hash match with identical spec must be a no-op (no AC update / no re-mint)")
	// Beyond the no-churn ResourceVersion check, assert the AC ResourceSpec itself
	// is byte-for-byte unchanged so a future spurious mutation (e.g. re-deriving
	// UserRef/SecretRef differently) is caught even if it happened to keep the
	// ResourceVersion stable (CC-0110, TE7 full-mutation-cycle).
	g.Expect(*ac2.Spec.Resource).To(Equal(specBefore),
		"hash match must not otherwise mutate the ApplicationCredential ResourceSpec")
}

// --- 2.8: reconcileCatalog ---

func TestReconcileCatalog_GatedOnAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane() // AdminCredentialReady absent
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAdminCredential"))
	g.Expect(cp.Status.CatalogReady).To(BeFalse())
}

func TestReconcileCatalog_RegistersServiceAndEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneServiceName(cp), Namespace: childNamespace(cp),
	}, svc)).To(Succeed())
	g.Expect(svc.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(svc.Spec.Resource.Type).To(Equal("identity"))
	g.Expect(svc.Spec.CloudCredentialsRef.SecretName).To(Equal("k-orc-clouds-yaml"))
	g.Expect(svc.OwnerReferences).To(HaveLen(1))
	g.Expect(svc.OwnerReferences[0].Name).To(Equal("cp"))

	ep := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneEndpointName(cp), Namespace: childNamespace(cp),
	}, ep)).To(Succeed())
	g.Expect(ep.Spec.Resource.Interface).To(Equal("public"))
	g.Expect(ep.Spec.Resource.URL).NotTo(BeEmpty())
	g.Expect(string(ep.Spec.Resource.ServiceRef)).To(Equal(keystoneServiceName(cp)))
	g.Expect(ep.OwnerReferences).To(HaveLen(1))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cp.Status.CatalogReady).To(BeTrue())
}

func TestReconcileCatalog_Idempotent(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	svc1 := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneServiceName(cp), Namespace: childNamespace(cp),
	}, svc1)).To(Succeed())
	ep1 := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneEndpointName(cp), Namespace: childNamespace(cp),
	}, ep1)).To(Succeed())

	// Second reconcile must not churn either CR.
	_, err = r.reconcileCatalog(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	svc2 := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneServiceName(cp), Namespace: childNamespace(cp),
	}, svc2)).To(Succeed())
	ep2 := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneEndpointName(cp), Namespace: childNamespace(cp),
	}, ep2)).To(Succeed())

	g.Expect(svc2.ResourceVersion).To(Equal(svc1.ResourceVersion), "Service must not churn on re-reconcile")
	g.Expect(ep2.ResourceVersion).To(Equal(ep1.ResourceVersion), "Endpoint must not churn on re-reconcile")
}

func TestReconcileCatalog_MissingCRDNoPanic(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*orcv1alpha1.Service); ok {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "openstack.k-orc.cloud", Kind: "Service"}}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	g.Expect(func() {
		res, err := r.reconcileCatalog(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred(), "missing CRD must NOT return a hard error")
		g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	}).NotTo(Panic())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("KORCCRDNotInstalled"))
}

// --- helpers ---

func setKORCReady(cp *c5c3v1alpha1.ControlPlane) {
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKORCReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "ApplicationCredentialMinted",
		Message:            "minted",
	})
}

func setAdminCredentialReady(cp *c5c3v1alpha1.ControlPlane) {
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAdminCredentialReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "AdminCredentialReady",
		Message:            "ready",
	})
}

// readyCloudsYamlES builds a Ready k-orc-clouds-yaml ExternalSecret in the SAME
// namespace as the K-ORC resource CRs (childNamespace(cp)) under the spec's
// CloudCredentialsRef.SecretName, mirroring the C1 co-location the reconciler
// gate now resolves against (CC-0110).
func readyCloudsYamlES(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if name == "" {
		name = korcCloudsYamlSecretName
	}
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
			},
		},
	}
}
