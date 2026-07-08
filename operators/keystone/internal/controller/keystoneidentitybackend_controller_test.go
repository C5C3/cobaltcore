// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/identity"
	identityfake "github.com/c5c3/forge/operators/keystone/internal/identity/fake"
)

const testAdminPassword = "admin-pw"

// testKeystoneWithReadyAPI returns testKeystone() with KeystoneAPIReady=True
// so the backend controller's API gate passes.
func testKeystoneWithReadyAPI() *keystonev1alpha1.Keystone {
	ks := testKeystone()
	ks.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeKeystoneAPIReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonAPIHealthy,
		LastTransitionTime: metav1.Now(),
	}}
	return ks
}

// testAdminSecret returns the bootstrap admin password Secret referenced by
// testKeystone, carrying the password the fake identity server accepts.
func testAdminSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte(testAdminPassword)},
	}
}

// newBackendTestReconciler builds a KeystoneIdentityBackendReconciler wired
// to the given fake identity server and a fake client pre-loaded with objs.
func newBackendTestReconciler(srv *identityfake.Server, objs ...runtime.Object) *KeystoneIdentityBackendReconciler {
	s := testScheme()
	cb := fake.NewClientBuilder().WithScheme(s)
	for _, obj := range objs {
		cb = cb.WithRuntimeObjects(obj)
	}
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &keystonev1alpha1.KeystoneIdentityBackend{})
	return &KeystoneIdentityBackendReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
		IdentityClientFactory: func(_ string, creds identity.Credentials) identity.Client {
			// Redirect the cluster-local endpoint to the fake server; the
			// credentials flow through unchanged so bad-password paths stay
			// exercisable.
			return identity.NewHTTPClient(srv.Endpoint(), creds, nil)
		},
	}
}

// backendRequest builds the reconcile request for a backend fixture.
func backendRequest(b *keystonev1alpha1.KeystoneIdentityBackend) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)}
}

// getBackend re-reads the backend from the fake client.
func getBackend(t *testing.T, c client.Client, name string) *keystonev1alpha1.KeystoneIdentityBackend {
	t.Helper()
	var b keystonev1alpha1.KeystoneIdentityBackend
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &b); err != nil {
		t.Fatalf("re-reading backend %s: %v", name, err)
	}
	return &b
}

// expectBackendEvent asserts the reconciler's FakeRecorder received an event
// containing the substring.
func expectBackendEvent(g Gomega, r *KeystoneIdentityBackendReconciler, substring string) {
	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	g.Expect(fakeRecorder.Events).To(Receive(ContainSubstring(substring)))
}

// reconcileBackendTwice runs Reconcile twice: the first pass installs the
// finalizer and requeues, the second does the actual work.
func reconcileBackendTwice(t *testing.T, r *KeystoneIdentityBackendReconciler, b *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), backendRequest(b)); err != nil {
		t.Fatalf("finalizer pass: %v", err)
	}
	return r.Reconcile(context.Background(), backendRequest(b))
}

func TestBackendReconcile_ManageCreatesDomain(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDomainProvisioned))
	g.Expect(updated.Status.DomainID).NotTo(BeEmpty())
	g.Expect(srv.GetDomainByName("corp")).NotTo(BeNil())
	expectBackendEvent(g, r, "Normal DomainCreated")
}

func TestBackendReconcile_ManageNeverSeizesForeignDomain(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	srv.SeedDomain("corp", "someone else's", true)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDomainAlreadyExists))
	g.Expect(cond.Message).To(ContainSubstring("Adopt"))
	g.Expect(updated.Status.DomainID).To(BeEmpty(), "a foreign domain must never be recorded as ours")
	g.Expect(srv.MutatingRequests()).To(BeEmpty(), "no mutation may touch the foreign domain")
}

func TestBackendReconcile_AdoptResolvesAndNeverMutates(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "pre-existing", true)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Spec.Domain.Mode = keystonev1alpha1.DomainModeAdopt
	// Adopt must not even reconcile drift: a diverging description stays.
	backend.Spec.Domain.Description = "different description"
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDomainAdopted))
	g.Expect(updated.Status.DomainID).To(Equal(id))
	g.Expect(srv.MutatingRequests()).To(BeEmpty(), "Adopt must never mutate the domain")
	g.Expect(srv.GetDomain(id).Description).To(Equal("pre-existing"))
	expectBackendEvent(g, r, "Normal DomainAdopted")
}

func TestBackendReconcile_AdoptMissingDomainWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Spec.Domain.Mode = keystonev1alpha1.DomainModeAdopt
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueDatabaseWait))

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDomainNotFound))
}

func TestBackendReconcile_WaitsForKeystoneAPI(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystone() // no KeystoneAPIReady condition
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "the Keystone watch wakes us; no requeue timer")

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForKeystoneAPI))
	g.Expect(srv.Requests()).To(BeEmpty(), "no identity call before the API is ready")
}

func TestBackendReconcile_KeystoneNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, backend)

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonKeystoneNotFound))
	// The aggregate Ready mirrors the failure.
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
}

func TestBackendReconcile_AdminSecretMissingWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend) // admin Secret absent

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonAdminSecretUnavailable))
}

// testProjectedDeployment returns the Keystone Deployment carrying the
// domains volume pointing at the given Secret name.
func testProjectedDeployment(ks *keystonev1alpha1.Keystone, domainsSecretName string) *appsv1.Deployment {
	vol, _ := domainsVolumeAndMount(domainsSecretName)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: subResourceName(ks), Namespace: ks.Namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{vol}},
			},
		},
	}
}

func TestBackendReconcile_ConfigProjectedFlipsOnDeploymentObservation(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	domainsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-domains-abcd1234", Namespace: "default"},
		Data:       map[string][]byte{domainConfFileName("corp"): []byte("[ldap]\n")},
	}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret(),
		testProjectedDeployment(ks, "test-keystone-domains-abcd1234"), domainsSecret)

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	updated := getBackend(t, r.Client, "corp-ldap")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionTrue))
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal("AllReady"))
}

func TestBackendReconcile_ConfigNotProjectedRequeuesAsSafetyNet(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	// Deployment exists but has no domains volume yet.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: subResourceName(ks), Namespace: ks.Namespace},
	}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret(), deploy)

	result, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling),
		"a converged Keystone status emits no watch event, so the poll is the liveness backstop")

	updated := getBackend(t, r.Client, "corp-ldap")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
}

// deletingBackend returns a backend fixture mid-deletion with the finalizer
// installed and Status.DomainID recorded.
func deletingBackend(domainID string, policy keystonev1alpha1.DomainDeletionPolicy, mode keystonev1alpha1.DomainMode) *keystonev1alpha1.KeystoneIdentityBackend {
	b := testIdentityBackend("corp-ldap", "corp")
	b.Spec.Domain.DeletionPolicy = policy
	b.Spec.Domain.Mode = mode
	b.Status.DomainID = domainID
	now := metav1.Now()
	b.DeletionTimestamp = &now
	b.Finalizers = []string{identityBackendFinalizerName}
	return b
}

func TestBackendDelete_RetainSkipsIdentityAPI(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "", true)

	ks := testKeystoneWithReadyAPI()
	backend := deletingBackend(id, keystonev1alpha1.DomainDeletionPolicyRetain, keystonev1alpha1.DomainModeManage)
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	g.Expect(srv.Requests()).To(BeEmpty(), "Retain must not touch the identity API")
	g.Expect(srv.GetDomain(id)).NotTo(BeNil(), "the domain must survive")
	var gone keystonev1alpha1.KeystoneIdentityBackend
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "corp-ldap"}, &gone)
	g.Expect(err).To(HaveOccurred(), "finalizer released, backend gone")
}

func TestBackendDelete_DeletePolicyDisablesThenDeletes(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "", true)

	ks := testKeystoneWithReadyAPI()
	backend := deletingBackend(id, keystonev1alpha1.DomainDeletionPolicyDelete, keystonev1alpha1.DomainModeManage)
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	g.Expect(srv.GetDomain(id)).To(BeNil(), "the domain must be deleted")

	// Order contract: disable (PATCH) strictly before delete (DELETE).
	var patchIdx, deleteIdx int
	for i, req := range srv.Requests() {
		if strings.HasPrefix(req, "PATCH /v3/domains/") {
			patchIdx = i
		}
		if strings.HasPrefix(req, "DELETE /v3/domains/") {
			deleteIdx = i
		}
	}
	g.Expect(patchIdx).To(BeNumerically(">", 0))
	g.Expect(deleteIdx).To(BeNumerically(">", patchIdx), "domain must be disabled before deletion")

	expectBackendEvent(g, r, "Normal DomainDisabled")
	expectBackendEvent(g, r, "Normal DomainDeleted")
}

func TestBackendDelete_AdoptedDomainAlwaysRetained(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "", true)

	ks := testKeystoneWithReadyAPI()
	// Even an explicit Delete policy must not delete an ADOPTED domain.
	backend := deletingBackend(id, keystonev1alpha1.DomainDeletionPolicyDelete, keystonev1alpha1.DomainModeAdopt)
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(srv.Requests()).To(BeEmpty(), "adopted domains are never touched on deletion")
	g.Expect(srv.GetDomain(id)).NotTo(BeNil())
}

func TestBackendDelete_WaitsForDeProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "", true)

	ks := testKeystoneWithReadyAPI()
	backend := deletingBackend(id, keystonev1alpha1.DomainDeletionPolicyDelete, keystonev1alpha1.DomainModeManage)
	domainsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-domains-abcd1234", Namespace: "default"},
		Data:       map[string][]byte{domainConfFileName("corp"): []byte("[ldap]\n")},
	}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret(),
		testProjectedDeployment(ks, "test-keystone-domains-abcd1234"), domainsSecret)
	ctx := context.Background()

	// Still projected: the finalizer must hold and no domain call may fire.
	result, err := r.Reconcile(ctx, backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretPolling))
	g.Expect(srv.GetDomain(id)).NotTo(BeNil())
	g.Expect(backendHasFinalizer(t, r.Client, "corp-ldap")).To(BeTrue())

	// De-projection happened (the sub-reconciler re-rendered without us).
	var secret corev1.Secret
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-keystone-domains-abcd1234"}, &secret)).To(Succeed())
	secret.Data = map[string][]byte{}
	g.Expect(r.Client.Update(ctx, &secret)).To(Succeed())

	result, err = r.Reconcile(ctx, backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(srv.GetDomain(id)).To(BeNil(), "domain deleted only after de-projection")
}

// backendHasFinalizer re-reads the backend and reports whether
// the identity-backend finalizer is still present.
func backendHasFinalizer(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var b keystonev1alpha1.KeystoneIdentityBackend
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &b); err != nil {
		return false
	}
	for _, f := range b.Finalizers {
		if f == identityBackendFinalizerName {
			return true
		}
	}
	return false
}

func TestBackendDelete_KeystoneGoneFastPathReleasesFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "", true)

	// No Keystone CR at all: the whole stack is being torn down.
	backend := deletingBackend(id, keystonev1alpha1.DomainDeletionPolicyDelete, keystonev1alpha1.DomainModeManage)
	r := newBackendTestReconciler(srv, backend)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	var gone keystonev1alpha1.KeystoneIdentityBackend
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "corp-ldap"}, &gone)
	g.Expect(err).To(HaveOccurred(), "finalizer released despite the missing Keystone (fail open)")
	g.Expect(srv.GetDomain(id)).NotTo(BeNil(), "no API to clean through; the domain is retained")
}

func TestBackendDelete_AdminSecretGoneFailsOpenWithWarning(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)
	id := srv.SeedDomain("corp", "", true)

	ks := testKeystoneWithReadyAPI()
	backend := deletingBackend(id, keystonev1alpha1.DomainDeletionPolicyDelete, keystonev1alpha1.DomainModeManage)
	r := newBackendTestReconciler(srv, ks, backend) // admin Secret absent

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	g.Expect(srv.GetDomain(id)).NotTo(BeNil(), "domain retained when the credential is gone")
	expectBackendEvent(g, r, "Warning DomainDeleteFailed")
}

// The description drift path must only write when there is drift, and the
// domain this CR created must be re-recognized by its recorded ID.
func TestBackendReconcile_ManageReconcilesDescriptionDrift(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Spec.Domain.Description = "managed by forge"
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	created := srv.GetDomainByName("corp")
	g.Expect(created.Description).To(Equal("managed by forge"))

	// Out-of-band drift: description changed and domain disabled.
	g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "corp-ldap"}, backend)).To(Succeed())
	d := srv.GetDomain(created.ID)
	g.Expect(d).NotTo(BeNil())
	// Mutate server-side state directly.
	idc := identity.NewHTTPClient(srv.Endpoint(), identity.Credentials{Username: "admin", Password: testAdminPassword}, nil)
	g.Expect(idc.UpdateDomain(context.Background(), created.ID, ptr.To(false), ptr.To("drifted"))).To(Succeed())

	_, err = r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())

	repaired := srv.GetDomain(created.ID)
	g.Expect(repaired.Description).To(Equal("managed by forge"))
	g.Expect(repaired.Enabled).To(BeTrue(), "Manage mode re-enables its own domain")
}

// A provisioned DomainReady must ride out an identity-API outage: demoting it
// would trip the keystone-side D-gate, de-project the backend, and re-trigger
// the very Deployment rollout that made the API unreachable (the
// oidc-federation e2e oscillation). The transient failure surfaces as a
// reconcile error (workqueue backoff), not as a condition flip.
func TestBackendReconcile_ProvisionedDomainSurvivesIdentityAPIOutage(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())
	provisioned := getBackend(t, r.Client, "corp-ldap")
	g.Expect(commonconditions.GetCondition(provisioned.Status.Conditions, conditionTypeDomainReady).Status).
		To(Equal(metav1.ConditionTrue))

	// The identity API goes away (e.g. the projection rollout switched the
	// Service targetPort to a not-yet-ready sidecar).
	srv.Close()

	_, err = r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).To(HaveOccurred(), "the outage must surface through the error path")

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "an unreachable API says nothing about the provisioned domain")
	g.Expect(cond.Reason).To(Equal(conditionReasonDomainProvisioned))
	g.Expect(updated.Status.DomainID).NotTo(BeEmpty())
}

// A provisioned DomainReady must equally ride out a KeystoneAPIReady=False
// window: every OIDC attach rolls the Keystone Deployment, so the API-ready
// gate goes False transiently on the happy path.
func TestBackendReconcile_ProvisionedDomainSurvivesKeystoneAPIFlap(t *testing.T) {
	g := NewGomegaWithT(t)
	srv := identityfake.NewServer(testAdminPassword)
	t.Cleanup(srv.Close)

	ks := testKeystoneWithReadyAPI()
	backend := testIdentityBackend("corp-ldap", "corp")
	backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
	r := newBackendTestReconciler(srv, ks, backend, testAdminSecret())

	_, err := reconcileBackendTwice(t, r, backend)
	g.Expect(err).NotTo(HaveOccurred())

	// The projection rollout flips KeystoneAPIReady to False.
	var liveKS keystonev1alpha1.Keystone
	g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-keystone"}, &liveKS)).To(Succeed())
	commonconditions.SetCondition(&liveKS.Status.Conditions, metav1.Condition{
		Type:   conditionTypeKeystoneAPIReady,
		Status: metav1.ConditionFalse,
		Reason: "DeploymentNotReady",
	})
	g.Expect(r.Status().Update(context.Background(), &liveKS)).To(Succeed())

	requestsBefore := len(srv.Requests())
	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "the Keystone watch wakes us when the API returns")

	updated := getBackend(t, r.Client, "corp-ldap")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeDomainReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "a not-yet-ready Keystone must not demote the provisioned domain")
	g.Expect(cond.Reason).To(Equal(conditionReasonDomainProvisioned))
	g.Expect(srv.Requests()).To(HaveLen(requestsBefore), "no identity call while the API gate is closed")
}

// The transient/authoritative demotion split at the setter level: transient
// observation failures preserve a provisioned True, authoritative findings
// and initial provisioning still demote, and True always upserts.
func TestUpsertBackendCondition_TransientDemotionPreservesProvisioned(t *testing.T) {
	cases := []struct {
		name       string
		current    *metav1.Condition
		status     metav1.ConditionStatus
		reason     string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "transient demotion of provisioned True is dropped",
			current:    &metav1.Condition{Type: conditionTypeDomainReady, Status: metav1.ConditionTrue, Reason: conditionReasonDomainProvisioned},
			status:     metav1.ConditionFalse,
			reason:     conditionReasonIdentityAPIError,
			wantStatus: metav1.ConditionTrue,
			wantReason: conditionReasonDomainProvisioned,
		},
		{
			name:       "authoritative demotion of provisioned True lands",
			current:    &metav1.Condition{Type: conditionTypeDomainReady, Status: metav1.ConditionTrue, Reason: conditionReasonDomainProvisioned},
			status:     metav1.ConditionFalse,
			reason:     conditionReasonDomainAlreadyExists,
			wantStatus: metav1.ConditionFalse,
			wantReason: conditionReasonDomainAlreadyExists,
		},
		{
			name:       "transient demotion before first provisioning lands",
			current:    nil,
			status:     metav1.ConditionFalse,
			reason:     conditionReasonWaitingForKeystoneAPI,
			wantStatus: metav1.ConditionFalse,
			wantReason: conditionReasonWaitingForKeystoneAPI,
		},
		{
			name:       "transient demotion of an already-False condition lands",
			current:    &metav1.Condition{Type: conditionTypeDomainReady, Status: metav1.ConditionFalse, Reason: conditionReasonDomainNotFound},
			status:     metav1.ConditionFalse,
			reason:     conditionReasonAdminSecretUnavailable,
			wantStatus: metav1.ConditionFalse,
			wantReason: conditionReasonAdminSecretUnavailable,
		},
		{
			name:       "promotion to True always lands",
			current:    &metav1.Condition{Type: conditionTypeDomainReady, Status: metav1.ConditionFalse, Reason: conditionReasonWaitingForKeystoneAPI},
			status:     metav1.ConditionTrue,
			reason:     conditionReasonDomainProvisioned,
			wantStatus: metav1.ConditionTrue,
			wantReason: conditionReasonDomainProvisioned,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			backend := testIdentityBackend("corp-ldap", "corp")
			backend.Status = keystonev1alpha1.KeystoneIdentityBackendStatus{}
			if tc.current != nil {
				tc.current.LastTransitionTime = metav1.Now()
				backend.Status.Conditions = []metav1.Condition{*tc.current}
			}

			upsertBackendCondition(backend, conditionTypeDomainReady, tc.status, tc.reason, "test message")

			cond := commonconditions.GetCondition(backend.Status.Conditions, conditionTypeDomainReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}
