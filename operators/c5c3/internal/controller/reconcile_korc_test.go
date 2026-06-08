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
	"time"

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

	// CloudCredentialsRef.SecretName points at the operator-owned password-cloud
	// (NOT k-orc-clouds-yaml) so a delete+recreate re-mint can always re-authenticate
	// as admin. The CloudName is preserved from the spec.
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)))
	g.Expect(ac.Spec.CloudCredentialsRef.CloudName).To(Equal("admin"))
	g.Expect(ac.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))

	// SecretRef points at the operator-owned minted-credential Secret.
	g.Expect(string(ac.Spec.Resource.SecretRef)).To(Equal(adminAppCredentialSecretName(cp)))
	// UserRef is the cp.Name-scoped K-ORC User name (CC-0112, REQ-003).
	g.Expect(string(ac.Spec.Resource.UserRef)).To(Equal("cp-user-admin"))
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
	// Pre-create an Available AC stamped with the CURRENT password hash so KORCReady
	// flips True on this pass (a missing/mismatched hash would trigger a re-mint).
	existing := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminAppCredentialName(cp),
			Namespace:   childNamespace(cp),
			Annotations: map[string]string{adminPasswordHashAnnotation: testPasswordHash()},
		},
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
// absent (its per-CR OpenBao path openstack/keystone/{ns}/{name}/admin/app-credential
// is empty until deploy/openbao/bootstrap/write-bootstrap-secrets.sh seeds a
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

	// The push that writes the per-CR openstack/keystone/{ns}/{name}/admin/app-credential
	// path to OpenBao must NOT have happened — the path stays empty, which is precisely why a
	// fresh cluster needs the bootstrap seed to break the cycle.
	ps := &esov1alpha1.PushSecret{}
	err = c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no PushSecret may be created while the clouds.yaml ExternalSecret is unseeded; "+
			"otherwise the bootstrap cycle is masked")
}

// TestReconcileAdminCredential_MissingAppCredSecretDefers verifies the Get-and-fail
// flow: the operator-owned app-credential Secret is created with its "value" by
// ensureAppCredentialSecret during reconcileKORC, so by the time the credential is
// assembled it MUST exist. If it is absent (invariant violation), reconcileAdminCredential
// must defer with a clear reason — NOT CreateOrUpdate a fresh, value-less Secret and
// then push an empty credential to OpenBao.
func TestReconcileAdminCredential_MissingAppCredSecretDefers(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{ID: "test-ac-id"}
	// clouds.yaml ExternalSecret is Ready, but the app-credential Secret is absent.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, readyCloudsYamlES(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminCredentialReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAppCredentialSecret"))

	// No empty app-credential Secret may be created as a side effect — that Secret
	// is owned by ensureAppCredentialSecret, not by this assembly step.
	sec := &corev1.Secret{}
	secErr := c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp),
	}, sec)
	g.Expect(apierrors.IsNotFound(secErr)).To(BeTrue(),
		"a missing app-credential Secret must not be re-created here with an empty value")

	// And nothing may be pushed to OpenBao while the credential is unassembled.
	ps := &esov1alpha1.PushSecret{}
	psErr := c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps)
	g.Expect(apierrors.IsNotFound(psErr)).To(BeTrue(),
		"no PushSecret may be created while the app-credential Secret is missing")
}

func TestReconcileAdminCredential_PushSecretBuiltAndReady(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{ID: "test-ac-id"}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyCloudsYamlES(cp), mintedAppCredSecret(cp), readyAppCredPushSecret(cp)).Build()
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
	g.Expect(ps.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(adminAppCredentialRemoteKeyFor(cp)))

	// The operator-owned Secret now carries the assembled app-credential clouds.yaml
	// (id from cp.Status, secret from the preserved "value") for the OpenBao push.
	sec := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp),
	}, sec)).To(Succeed())
	g.Expect(sec.Data).To(HaveKey("value"), "the K-ORC-read \"value\" must be preserved")
	g.Expect(sec.Data).To(HaveKey("clouds.yaml"))
	g.Expect(string(sec.Data["clouds.yaml"])).To(ContainSubstring("application_credential_id"))
	g.Expect(string(sec.Data["clouds.yaml"])).To(ContainSubstring("test-ac-id"))
}

// TestAdminAppCredentialRemoteKeyFor_EmbedsNamespaceAndName locks in the per-CR
// OpenBao path (CC-0112, REQ-001): the RemoteKey is scoped by both the
// ControlPlane's Namespace and Name so two ControlPlanes never clobber each
// other's admin credential on the cluster-global OpenBao backend, and neither
// reuses the legacy flat single-AC path.
func TestAdminAppCredentialRemoteKeyFor_EmbedsNamespaceAndName(t *testing.T) {
	g := NewWithT(t)

	const legacyFlatKey = "openstack/keystone/admin/app-credential"

	cases := []struct {
		name      string
		namespace string
		cpName    string
		want      string
	}{
		{
			name:      "default control plane",
			namespace: "openstack",
			cpName:    "controlplane",
			want:      "openstack/keystone/openstack/controlplane/admin/app-credential",
		},
		{
			name:      "second tenant control plane",
			namespace: "tenant-b",
			cpName:    "cp-b",
			want:      "openstack/keystone/tenant-b/cp-b/admin/app-credential",
		},
	}

	keys := make([]string, 0, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cp := &c5c3v1alpha1.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: tc.cpName, Namespace: tc.namespace},
			}
			got := adminAppCredentialRemoteKeyFor(cp)
			g.Expect(got).To(Equal(tc.want))
			g.Expect(got).NotTo(Equal(legacyFlatKey),
				"the per-CR key must not collapse back to the legacy flat path")
		})
		keys = append(keys, tc.want)
	}

	// Two distinct ControlPlanes must produce distinct OpenBao paths.
	g.Expect(keys[0]).NotTo(Equal(keys[1]),
		"two ControlPlanes must not share a RemoteKey")
}

func TestReconcileAdminCredential_PushSecretClobberSafeNoChurn(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{ID: "test-ac-id"}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyCloudsYamlES(cp), mintedAppCredSecret(cp), readyAppCredPushSecret(cp)).Build()
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

// TestReconcileKORC_HashMismatchDeletesACForRemint asserts the re-mint trigger:
// K-ORC's AC actuator cannot re-mint in place, so a stale stamped hash (the admin
// password rotated) must DELETE the AC — driving K-ORC's finalizer to revoke the
// old Keystone credential — and regenerate the secret "value" so the recreated AC
// mints a fresh credential. KORCReady reports the transient ReMinting reason.
func TestReconcileKORC_HashMismatchDeletesACForRemint(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// Pre-create an Available AC stamped with a STALE hash (the rotation signal).
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
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{ID: "old-id"}
	// Seed the app-credential secret with a KNOWN value so the regeneration is observable.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, adminPasswordSecret(), existing, mintedAppCredSecret(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	// The AC was deleted (no finalizer in the fake client => removed immediately).
	deleted := &orcv1alpha1.ApplicationCredential{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialName(cp), Namespace: childNamespace(cp),
	}, deleted)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
		"a hash mismatch must delete the AC to trigger a re-mint")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ReMinting"))

	// The secret "value" was regenerated so the recreated AC mints a NEW credential.
	sec := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp),
	}, sec)).To(Succeed())
	g.Expect(sec.Data[appCredSecretValueKey]).NotTo(Equal([]byte("generated-app-cred-secret")),
		"the app-credential secret value must be regenerated on re-mint")
}

// TestReconcileKORC_RemintStalledAfterTimeout asserts the ReMintStalled escape: an
// AC stuck Terminating longer than remintStallTimeout (a finalizer K-ORC cannot
// clear because it cannot revoke the old credential) escalates KORCReady from the
// transient ReMinting reason to the operator-visible ReMintStalled reason.
func TestReconcileKORC_RemintStalledAfterTimeout(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// An AC mid-delete (DeletionTimestamp + finalizer) whose stamped hash mismatches,
	// terminating since well before the stall timeout.
	staleDeletion := metav1.NewTime(metav1.Now().Add(-2 * remintStallTimeout))
	existing := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:              adminAppCredentialName(cp),
			Namespace:         childNamespace(cp),
			Annotations:       map[string]string{adminPasswordHashAnnotation: "stale-hash"},
			Finalizers:        []string{"openstack.k-orc.cloud/applicationcredential"},
			DeletionTimestamp: &staleDeletion,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, adminPasswordSecret(), existing).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ReMintStalled"),
		"an AC Terminating past remintStallTimeout must escalate to ReMintStalled")
}

// TestReconcileKORC_RemintTerminatingReportsReMinting asserts that an AC mid-delete
// but WITHIN the stall timeout reports the transient ReMinting reason (not stalled).
func TestReconcileKORC_RemintTerminatingReportsReMinting(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	justDeleted := metav1.Now()
	existing := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:              adminAppCredentialName(cp),
			Namespace:         childNamespace(cp),
			Annotations:       map[string]string{adminPasswordHashAnnotation: "stale-hash"},
			Finalizers:        []string{"openstack.k-orc.cloud/applicationcredential"},
			DeletionTimestamp: &justDeleted,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, adminPasswordSecret(), existing).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeKORCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("ReMinting"))
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

// TestReconcileKORC_FreshMintUsesPasswordCloud asserts the first mint renders the
// operator-owned password-based clouds.yaml tracking the current admin password,
// and points the AC's CloudCredentialsRef at it (not k-orc-clouds-yaml).
func TestReconcileKORC_FreshMintUsesPasswordCloud(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	sec := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminPasswordCloudSecretName(cp), Namespace: childNamespace(cp),
	}, sec)).To(Succeed())
	clouds := string(sec.Data[appCredCloudsYAMLKey])
	g.Expect(clouds).To(ContainSubstring(testAdminPassword), "password-cloud must carry the live admin password")
	g.Expect(clouds).To(ContainSubstring("username:"), "password-cloud must be password-based")
	g.Expect(clouds).To(ContainSubstring("endpoint_type: internal"))
	g.Expect(sec.OwnerReferences).To(HaveLen(1), "password-cloud must be owned by the ControlPlane")

	ac := getAC(t, c, cp)
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)),
		"the AC must mint via the password-cloud, not k-orc-clouds-yaml")
}

// TestReconcileKORC_PasswordCloudTracksRotation asserts the password-cloud is
// re-rendered when the admin password rotates and is not churned otherwise.
func TestReconcileKORC_PasswordCloudTracksRotation(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	pcKey := types.NamespacedName{Name: adminPasswordCloudSecretName(cp), Namespace: childNamespace(cp)}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	sec1 := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), pcKey, sec1)).To(Succeed())
	g.Expect(string(sec1.Data[appCredCloudsYAMLKey])).To(ContainSubstring(testAdminPassword))

	// Unchanged password => no churn.
	_, err = r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	sec2 := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), pcKey, sec2)).To(Succeed())
	g.Expect(sec2.ResourceVersion).To(Equal(sec1.ResourceVersion),
		"an unchanged admin password must not churn the password-cloud")

	// Rotate the admin password; the password-cloud must track the new value.
	pw := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "keystone-admin", Namespace: "default"}, pw)).To(Succeed())
	pw.Data["password"] = []byte("rotated-admin-password")
	g.Expect(c.Update(context.Background(), pw)).To(Succeed())

	_, err = r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	sec3 := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), pcKey, sec3)).To(Succeed())
	g.Expect(string(sec3.Data[appCredCloudsYAMLKey])).To(ContainSubstring("rotated-admin-password"))
	g.Expect(string(sec3.Data[appCredCloudsYAMLKey])).NotTo(ContainSubstring(testAdminPassword),
		"the password-cloud must drop the stale password after rotation")
}

// TestReconcileKORC_RecreatePreservesRegeneratedValue asserts the recreate after a
// re-mint delete mints with the regenerated "value" (rather than generating yet
// another), stamps the current hash, and points at the password-cloud.
func TestReconcileKORC_RecreatePreservesRegeneratedValue(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	// State right after a re-mint delete: no AC, the app-cred secret carries a
	// freshly regenerated value.
	freshValue := []byte("freshly-regenerated-value")
	appSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp)},
		Data:       map[string][]byte{appCredSecretValueKey: freshValue},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret(), appSecret).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ac := getAC(t, c, cp)
	g.Expect(ac.Annotations).To(HaveKeyWithValue(adminPasswordHashAnnotation, testPasswordHash()))
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)))
	g.Expect(string(ac.Spec.Resource.SecretRef)).To(Equal(adminAppCredentialSecretName(cp)))

	sec := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp),
	}, sec)).To(Succeed())
	g.Expect(sec.Data[appCredSecretValueKey]).To(Equal(freshValue),
		"recreate must mint with the regenerated value, not generate a new one")
}

// TestReconcileKORC_FreshCredentialIDAdvancesLastRotation asserts that once the
// recreated AC reports a NEW credential id, status.adminApplicationCredential
// advances lastRotation (the observable signal a re-mint completed).
func TestReconcileKORC_FreshCredentialIDAdvancesLastRotation(t *testing.T) {
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	cp := korcControlPlane()
	oldRotation := metav1.NewTime(metav1.Now().Add(-time.Hour))
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{
		ID: "old-id", LastRotation: &oldRotation,
	}
	// A recreated, now-Available AC with a NEW id whose hash already matches.
	existing := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminAppCredentialName(cp),
			Namespace:   childNamespace(cp),
			Annotations: map[string]string{adminPasswordHashAnnotation: testPasswordHash()},
		},
		Status: orcv1alpha1.ApplicationCredentialStatus{
			ID: ptr.To("new-id"),
			Conditions: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionTrue,
				Reason:             orcv1alpha1.ConditionReasonSuccess,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, adminPasswordSecret(), existing, mintedAppCredSecret(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cp.Status.AdminApplicationCredential).NotTo(BeNil())
	g.Expect(cp.Status.AdminApplicationCredential.ID).To(Equal("new-id"))
	g.Expect(cp.Status.AdminApplicationCredential.LastRotation).NotTo(BeNil())
	g.Expect(cp.Status.AdminApplicationCredential.LastRotation.Time.After(oldRotation.Time)).To(BeTrue(),
		"a re-minted credential with a new id must advance lastRotation")
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

// mintedAppCredSecret returns the operator-owned Secret pre-populated with the
// generated application-credential "value" — the state reconcileKORC's
// ensureAppCredentialSecret leaves before reconcileAdminCredential assembles the
// clouds.yaml.
func mintedAppCredSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialSecretName(cp),
			Namespace: childNamespace(cp),
		},
		Data: map[string][]byte{appCredSecretValueKey: []byte("generated-app-cred-secret")},
	}
}

// readyAppCredPushSecret returns the admin app-credential PushSecret with the exact
// spec the operator builds (so EnsurePushSecret performs no update) plus a Ready
// condition — the state after ESO has successfully synced it to OpenBao. The
// AdminCredentialReady gate requires the PushSecret to be Ready.
func readyAppCredPushSecret(cp *c5c3v1alpha1.ControlPlane) *esov1alpha1.PushSecret {
	ps := adminAppCredentialPushSecret(cp)
	ps.Status.Conditions = []esov1alpha1.PushSecretStatusCondition{
		{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue},
	}
	return ps
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

// TestKeystoneEndpointURL_DerivesFromProjectedService locks in that the catalog
// Endpoint URL points at the PROJECTED Keystone Service ("{cp.Name}-keystone")
// rather than a hardcoded "keystone" — the keystone-operator names the Service
// after the projected Keystone CR, so a fixed name does not resolve (the K-ORC
// auth / catalog otherwise fails with "lookup keystone.<ns>.svc: no such host").
func TestKeystoneEndpointURL_DerivesFromProjectedService(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "controlplane", Namespace: "openstack"},
	}
	g.Expect(keystoneEndpointURL(cp)).
		To(Equal("http://controlplane-keystone.openstack.svc:5000/v3"))
}

// TestKeystoneCatalogURL_PrefersPublicEndpoint locks in that the catalog Endpoint
// registers the external publicEndpoint when Keystone is exposed via a Gateway,
// so the catalog matches what Keystone's own bootstrap advertises — while
// keystoneEndpointURL (K-ORC's in-cluster auth_url) is unaffected.
func TestKeystoneCatalogURL_PrefersPublicEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "controlplane", Namespace: "openstack"},
	}

	// No external exposure → in-cluster Service URL (unchanged behaviour).
	g.Expect(keystoneCatalogURL(cp)).
		To(Equal("http://controlplane-keystone.openstack.svc:5000/v3"))

	// A gateway without an explicit publicEndpoint → derived default-443 URL.
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
		Hostname:  "keystone.127-0-0-1.nip.io",
	}
	g.Expect(keystoneCatalogURL(cp)).
		To(Equal("https://keystone.127-0-0-1.nip.io/v3"))

	// An explicit publicEndpoint (carrying the kind :8443 host port) wins.
	cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.127-0-0-1.nip.io:8443/v3"
	g.Expect(keystoneCatalogURL(cp)).
		To(Equal("https://keystone.127-0-0-1.nip.io:8443/v3"))
}

// TestEnsureKORCAdminImports_CreatesUnmanagedUserAndDomain verifies that the
// admin ApplicationCredential's prerequisites are provisioned as UNMANAGED K-ORC
// imports — without them K-ORC blocks on "Waiting for User/admin to be created".
func TestEnsureKORCAdminImports_CreatesUnmanagedUserAndDomain(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: "k-orc-clouds-yaml", CloudName: "admin"}
	g.Expect(r.ensureKORCAdminImports(context.Background(), cp, credRef)).To(Succeed())

	var domain orcv1alpha1.Domain
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminDomainRef(cp), Namespace: childNamespace(cp),
	}, &domain)).To(Succeed())
	g.Expect(domain.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(domain.Spec.Import).NotTo(BeNil())
	g.Expect(domain.Spec.Import.Filter).NotTo(BeNil())
	g.Expect(domain.Spec.Import.Filter.Name).NotTo(BeNil())
	g.Expect(string(*domain.Spec.Import.Filter.Name)).To(Equal("Default"))
	g.Expect(domain.OwnerReferences).To(HaveLen(1))

	var user orcv1alpha1.User
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminUserRef(cp), Namespace: childNamespace(cp),
	}, &user)).To(Succeed())
	// The User CR name is cp.Name-scoped (CC-0112, REQ-003) so two ControlPlanes
	// in one namespace produce distinct User objects.
	g.Expect(user.Name).To(Equal(cp.Name + "-user-admin"))
	g.Expect(user.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
	g.Expect(user.Spec.Import).NotTo(BeNil())
	g.Expect(user.Spec.Import.Filter).NotTo(BeNil())
	g.Expect(user.Spec.Import.Filter.Name).NotTo(BeNil())
	g.Expect(string(*user.Spec.Import.Filter.Name)).To(Equal("admin"))
	g.Expect(user.Spec.Import.Filter.DomainRef).NotTo(BeNil())
	g.Expect(string(*user.Spec.Import.Filter.DomainRef)).To(Equal(adminDomainRef(cp)))
	g.Expect(user.OwnerReferences).To(HaveLen(1))
}

// TestAdminUserRef_IsControlPlaneScoped locks in that the K-ORC User CR name the
// admin ApplicationCredential references is scoped by cp.Name (CC-0112, REQ-003),
// mirroring adminDomainRef, so two ControlPlanes never collide on a shared "admin"
// User name — and that it no longer returns the bare OpenStack username.
func TestAdminUserRef_IsControlPlaneScoped(t *testing.T) {
	g := NewWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "controlplane", Namespace: "openstack"},
	}
	g.Expect(adminUserRef(cp)).To(Equal(cp.Name + "-user-admin"))
	g.Expect(adminUserRef(cp)).To(Equal("controlplane-user-admin"))
	g.Expect(adminUserRef(cp)).NotTo(Equal("admin"),
		"the User ref must be cp.Name-scoped, not the bare OpenStack username")
}

// TestReconcileKORC_CreatesAppCredentialSecretWithValue verifies that reconcileKORC
// provisions the operator-owned Secret with a generated "value" BEFORE the AC —
// K-ORC's managed ApplicationCredential reads Secret.Data["value"] to mint, so
// without this it blocks on "Waiting for Secret … to be created".
func TestReconcileKORC_CreatesAppCredentialSecretWithValue(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := korcControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, adminPasswordSecret()).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileKORC(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	sec := &corev1.Secret{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: adminAppCredentialSecretName(cp), Namespace: childNamespace(cp),
	}, sec)).To(Succeed(), "the app-credential Secret must exist before the AC is reconciled")
	g.Expect(sec.Data).To(HaveKey(appCredSecretValueKey))
	g.Expect(sec.Data[appCredSecretValueKey]).NotTo(BeEmpty(), "value must be a generated secret")
	g.Expect(sec.OwnerReferences).To(HaveLen(1))
}

// TestReconcileAdminCredential_WaitsForPushSecretSync verifies AdminCredentialReady
// does NOT go True merely because the PushSecret CR exists: until it has synced to
// OpenBao (Ready), a backend permission failure must surface as WaitingForPushSecret
// rather than a false-positive Ready (Befund #7).
func TestReconcileAdminCredential_WaitsForPushSecretSync(t *testing.T) {
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	cp := korcControlPlane()
	setKORCReady(cp)
	cp.Status.AdminApplicationCredential = &c5c3v1alpha1.AdminApplicationCredentialStatus{ID: "test-ac-id"}
	// No pre-existing Ready PushSecret — EnsurePushSecret creates it, but it has not
	// synced to the backend yet.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, readyCloudsYamlES(cp), mintedAppCredSecret(cp)).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminCredentialReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForPushSecret"))
}
