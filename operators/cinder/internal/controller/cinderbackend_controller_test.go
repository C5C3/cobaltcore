// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonconditions "github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// projectedVolumeDeployment returns the volume Deployment of one backend exactly
// as the Cinder-side step builds it, so the observation is exercised against the
// object production writes rather than a hand-made stand-in.
func projectedVolumeDeployment(cinder *cinderv1alpha1.Cinder, name string) *appsv1.Deployment {
	return buildVolumeDeployment(cinder, testBackendProjection(name),
		workloadArtifacts(), workloadDigests{}, testEgressPort)
}

// projectedBackendSecret returns the projection Secret that volume Deployment
// mounts, carrying the rendered section of the named backend.
func projectedBackendSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testBackendProjection(name).secretName,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			backendConfDataKey: []byte("[" + name + "]\nvolume_backend_name = " + name + "\n"),
		},
	}
}

// newCinderBackendTestReconciler builds a CinderBackendReconciler over a fake
// client pre-loaded with objs. The Resolver stays nil, the always-local mode
// every single-cluster test runs in.
func newCinderBackendTestReconciler(objs ...client.Object) *CinderBackendReconciler {
	return &CinderBackendReconciler{
		Client:   cinderFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(10),
	}
}

// backendRequest builds the reconcile request for a backend fixture.
func backendRequest(backend *cinderv1alpha1.CinderBackend) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backend)}
}

// getCinderBackend re-reads a backend from the given client.
func getCinderBackend(t *testing.T, c client.Client, name string) *cinderv1alpha1.CinderBackend {
	t.Helper()
	var backend cinderv1alpha1.CinderBackend
	if err := c.Get(context.Background(), objectKey(name), &backend); err != nil {
		t.Fatalf("re-reading CinderBackend %s: %v", name, err)
	}
	return &backend
}

// backendCondition returns one of a backend's conditions, or nil.
func backendCondition(backend *cinderv1alpha1.CinderBackend, conditionType string) *metav1.Condition {
	return commonconditions.GetCondition(backend.Status.Conditions, conditionType)
}

// The finalizer is what keeps a deleted backend around until its volume service
// has been unregistered, so it has to be installed before the first projection
// rather than alongside it: a backend deleted in that window would otherwise
// leave its host identity in the service registry forever.
func TestCinderBackendReconcile_InstallsTheServiceRemoveFinalizerFirst(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testCinderBackend("nfs1")
	r := newCinderBackendTestReconciler(validCinder(), backend)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass),
		"the next pass reconciles the persisted object rather than the stale one")

	updated := getCinderBackend(t, r.Client, "nfs1")
	g.Expect(controllerutil.ContainsFinalizer(updated, CinderBackendServiceRemoveFinalizer)).To(BeTrue())
	g.Expect(updated.Status.Conditions).To(BeEmpty(),
		"the pass returned before any observation could report on the backend")
}

// An NFS export is mounted with the pod's own identity, so the credential gate
// is satisfied by construction. It is still reported: the Cinder-side projection
// gates on this very condition, and an absent one reads as "not ready yet".
func TestCinderBackendReconcile_CredentialsNotRequiredForNFS(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testCinderBackend("nfs1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	r := newCinderBackendTestReconciler(validCinder(), backend)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"nothing is projected yet, so the projection gate polls")

	updated := getCinderBackend(t, r.Client, "nfs1")
	creds := backendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds).NotTo(BeNil())
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(creds.Reason).To(Equal(conditionReasonCredentialsNotRequired))

	// The aggregate mirrors the projection that has not landed yet, and
	// ObservedGeneration is stamped.
	ready := backendCondition(updated, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
}

// reconcileRBDBackend runs one pass of the backend controller over an RBD
// backend that already carries its finalizer, next to the shared Cinder and
// objs, and returns the result, the re-read backend and the error.
func reconcileRBDBackend(t *testing.T, backend *cinderv1alpha1.CinderBackend, objs ...client.Object) (
	reconcile.Result, *cinderv1alpha1.CinderBackend, error,
) {
	t.Helper()
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	r := newCinderBackendTestReconciler(append([]client.Object{validCinder(), backend}, objs...)...)
	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	return result, getCinderBackend(t, r.Client, backend.Name), err
}

// expectRBDKeyWaiting asserts the gate held an RBD backend on its key: the pass
// polls, CredentialsReady is False with the given message, and the projection
// observation never ran.
func expectRBDKeyWaiting(t *testing.T, result reconcile.Result, err error,
	updated *cinderv1alpha1.CinderBackend, wantMessage string,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(err).NotTo(HaveOccurred(), "a key that is not there yet is a waiting state, not a failure")
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"an absent Secret emits no event, so the gate polls")

	creds := backendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds).NotTo(BeNil())
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForCredentials))
	g.Expect(creds.Message).To(Equal(wantMessage))
	g.Expect(backendCondition(updated, conditionTypeConfigProjected)).To(BeNil(),
		"the parent never projects a backend that is not credential-ready, so there is nothing to observe")
	g.Expect(backendCondition(updated, "Ready").Status).To(Equal(metav1.ConditionFalse))
}

// An RBD backend whose key Secret does not exist, and that no ExternalSecret is
// about to produce, waits on the gate rather than reaching the projection.
func TestCinderBackendReconcile_RBDKeySecretMissingWaits(t *testing.T) {
	result, updated, err := reconcileRBDBackend(t, testRBDCinderBackend("rbd1"))
	expectRBDKeyWaiting(t, result, err, updated, "RBD key ExternalSecret openstack/rbd1-key not found yet")
}

// A Secret under the name the backend references that carries no userKey, while
// the ExternalSecret producing it already reports Ready, is a malformed Secret
// rather than one still syncing.
func TestCinderBackendReconcile_RBDKeySecretWithoutKeyWaits(t *testing.T) {
	secret := rbdKeySecret("rbd1", testRBDKey)
	secret.Data = map[string][]byte{"key": []byte(testRBDKey)}
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "rbd1-key", Namespace: testNamespace},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
			},
		},
	}

	result, updated, err := reconcileRBDBackend(t, testRBDCinderBackend("rbd1"), secret, es)
	expectRBDKeyWaiting(t, result, err, updated, "RBD key Secret exists but is missing expected keys")
}

// A userKey of whitespace alone is an empty key once trimmed: copied into the
// keyring it would fail every connection the driver opens.
func TestCinderBackendReconcile_RBDKeyEmptyWaits(t *testing.T) {
	result, updated, err := reconcileRBDBackend(t, testRBDCinderBackend("rbd1"), rbdKeySecret("rbd1", " \n"))
	expectRBDKeyWaiting(t, result, err, updated, `Secret "rbd1-key" carries an empty userKey`)
}

// A value that is not the base64 a cephx key is printed as is refused before it
// reaches the keyring, and the message never repeats the value.
func TestCinderBackendReconcile_RBDKeyNotBase64Waits(t *testing.T) {
	result, updated, err := reconcileRBDBackend(t, testRBDCinderBackend("rbd1"), rbdKeySecret("rbd1", "not a key!"))
	expectRBDKeyWaiting(t, result, err, updated,
		`Secret "rbd1-key" carries a userKey that is not a cephx key (base64 expected)`)
}

// The admission union rule guarantees spec.rbd on a type-RBD backend; one
// written past it has no key to resolve.
func TestCinderBackendReconcile_RBDBlockMissingWaits(t *testing.T) {
	backend := testRBDCinderBackend("rbd1")
	backend.Spec.RBD = nil
	result, updated, err := reconcileRBDBackend(t, backend)
	expectRBDKeyWaiting(t, result, err, updated, "spec.rbd is not set; no RBD key to resolve")
}

// A type the operator does not render cannot become credential-ready.
func TestCinderBackendReconcile_UnknownTypeWaits(t *testing.T) {
	backend := testRBDCinderBackend("rbd1")
	backend.Spec.Type = cinderv1alpha1.CinderBackendType("Ceph")
	result, updated, err := reconcileRBDBackend(t, backend)
	expectRBDKeyWaiting(t, result, err, updated, "spec.type Ceph is not a type this operator renders")
}

// A usable key opens the gate, and the pass goes on to observe the projection
// exactly as it does for an NFS backend.
func TestCinderBackendReconcile_RBDKeyPresentIsCredentialsAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	result, updated, err := reconcileRBDBackend(t, testRBDCinderBackend("rbd1"),
		rbdKeySecret("rbd1", testRBDKey+"\n"))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"nothing is projected yet, so the projection gate polls")
	creds := backendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(creds.Reason).To(Equal(conditionReasonCredentialsAvailable))
	g.Expect(creds.Message).To(Equal(`RBD key Secret "rbd1-key" carries the userKey data key`))

	projected := backendCondition(updated, conditionTypeConfigProjected)
	g.Expect(projected).NotTo(BeNil(), "the projection observation ran")
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
}

// A Secret Get that fails for any reason other than NotFound establishes
// nothing about the key, so a standing CredentialsReady=True stays: the
// Cinder-side projection gates on it, and demoting it on a cache blip would
// stop the backend's volume service.
func TestCinderBackendReconcile_RBDSecretReadErrorKeepsTheStandingClaim(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := credentialReadyRBDBackend("rbd1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	boom := errors.New("the apiserver is briefly unreachable")
	r := &CinderBackendReconciler{
		Client: cinderFakeClientBuilder(validCinder(), backend, rbdKeySecret("rbd1", testRBDKey)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, ok := obj.(*corev1.Secret); ok {
						return boom
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).To(MatchError(boom))
	creds := backendCondition(getCinderBackend(t, r.Client, "rbd1"), conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue), "unreadable is not missing")
}

// The gate reads the Secret a second time for the value's shape. A failure of
// that read is wrapped with the backend's name and keeps the standing claim as
// the first read's failure does.
func TestCinderBackendReconcile_RBDKeyValueReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := credentialReadyRBDBackend("rbd1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	boom := errors.New("the apiserver is briefly unreachable")
	secretGets := 0
	r := &CinderBackendReconciler{
		Client: cinderFakeClientBuilder(validCinder(), backend, rbdKeySecret("rbd1", testRBDKey)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, ok := obj.(*corev1.Secret); ok {
						if secretGets++; secretGets == 2 {
							return boom
						}
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).To(MatchError(ContainSubstring(`reading the RBD key of backend "rbd1"`)))
	g.Expect(errors.Is(err, boom)).To(BeTrue(), "the cause stays on the chain")
	creds := backendCondition(getCinderBackend(t, r.Client, "rbd1"), conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue), "unreadable is not missing")
}

// A parent that does not exist stops the pass at the first gate: without one
// there is nothing to say which cluster this backend's volume service runs on,
// so the Deployment is not read and the projection observation never runs.
func TestCinderBackendReconcile_NoParentCinderHoldsBeforeProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testCinderBackend("nfs1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	r := newCinderBackendTestReconciler(backend)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getCinderBackend(t, r.Client, "nfs1")
	creds := backendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForParent))
	g.Expect(backendCondition(updated, conditionTypeConfigProjected)).To(BeNil(),
		"the pass returned before the projection observation could run")
}

// A backend that already converged carries ConfigProjected=True observed against
// the parent's volume Deployment. Deleting the parent must demote that claim
// instead of leaving it standing behind the WaitingForParent hold.
func TestCinderBackendReconcile_ParentDeletionDemotesStaleConfigProjected(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testCinderBackend("nfs1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	backend.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeConfigProjected,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonConfigProjected,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	r := newCinderBackendTestReconciler(backend)

	_, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	updated := getCinderBackend(t, r.Client, "nfs1")
	projected := backendCondition(updated, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
}

// The projection observation is the backend's only proof that the running volume
// service was built from its section: the Cinder-side step reports its own
// aggregate, which says nothing about which backend the pods actually mount.
func TestCinderBackendReconcile_ConfigProjectedOnceTheVolumeDeploymentMountsTheSection(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	backend := testCinderBackend("nfs1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	r := newCinderBackendTestReconciler(cinder, backend,
		projectedVolumeDeployment(cinder, "nfs1"), projectedBackendSecret("nfs1"))

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "a converged backend polls nothing")

	updated := getCinderBackend(t, r.Client, "nfs1")
	projected := backendCondition(updated, conditionTypeConfigProjected)
	g.Expect(projected).NotTo(BeNil())
	g.Expect(projected.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(projected.Reason).To(Equal(conditionReasonConfigProjected))

	ready := backendCondition(updated, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue),
		"both sub-conditions are True, so the aggregate is too")
}

// Every break in the chain from the Deployment to the rendered section is a
// waiting state rather than a failure: each of them is what a backend applied
// before its parent converged looks like, and an error would back the workqueue
// off instead of polling for the projection.
func TestCinderBackendReconcile_BrokenProjectionChainWaitsWithoutError(t *testing.T) {
	cinder := workloadCinder()

	for _, tc := range []struct {
		name string
		objs func() []client.Object
	}{
		{
			name: "no volume Deployment yet",
			objs: func() []client.Object { return nil },
		},
		{
			name: "the Deployment carries no backend volume",
			objs: func() []client.Object {
				deploy := projectedVolumeDeployment(cinder, "nfs1")
				for i := range deploy.Spec.Template.Spec.Volumes {
					if deploy.Spec.Template.Spec.Volumes[i].Name == backendsVolumeName {
						deploy.Spec.Template.Spec.Volumes[i].Name = "something-else"
					}
				}
				return []client.Object{deploy, projectedBackendSecret("nfs1")}
			},
		},
		{
			name: "the projection Secret is gone",
			objs: func() []client.Object {
				return []client.Object{projectedVolumeDeployment(cinder, "nfs1")}
			},
		},
		{
			name: "the Secret carries no backend.conf",
			objs: func() []client.Object {
				secret := projectedBackendSecret("nfs1")
				secret.Data = map[string][]byte{sharesDataKey: []byte("nfs1.nfs.example.com:/exports/nfs1\n")}
				return []client.Object{projectedVolumeDeployment(cinder, "nfs1"), secret}
			},
		},
		{
			name: "another backend's section is rendered",
			objs: func() []client.Object {
				secret := projectedBackendSecret("nfs1")
				secret.Data[backendConfDataKey] = []byte("[nfs10]\nvolume_backend_name = nfs10\n")
				return []client.Object{projectedVolumeDeployment(cinder, "nfs1"), secret}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			backend := testCinderBackend("nfs1")
			backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
			objs := append([]client.Object{cinder, backend}, tc.objs()...)
			r := newCinderBackendTestReconciler(objs...)

			result, err := r.Reconcile(context.Background(), backendRequest(backend))

			g.Expect(err).NotTo(HaveOccurred(), "a projection that has not landed is not a failure")
			g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

			updated := getCinderBackend(t, r.Client, "nfs1")
			projected := backendCondition(updated, conditionTypeConfigProjected)
			g.Expect(projected).NotTo(BeNil())
			g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
		})
	}
}

// A Get that fails for any reason other than NotFound establishes nothing, so it
// must not demote a standing claim: a converged backend would otherwise flap on
// a cache blip and take the Cinder-side projection gate down with it.
func TestCinderBackendReconcile_ObservationErrorKeepsTheStandingClaim(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	backend := testCinderBackend("nfs1")
	backend.Finalizers = []string{CinderBackendServiceRemoveFinalizer}
	backend.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeConfigProjected,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonConfigProjected,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	boom := errors.New("the apiserver is briefly unreachable")
	r := &CinderBackendReconciler{
		Client: cinderFakeClientBuilder(cinder, backend).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, ok := obj.(*appsv1.Deployment); ok {
						return boom
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).To(MatchError(boom))
	updated := getCinderBackend(t, r.Client, "nfs1")
	projected := backendCondition(updated, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionTrue), "unobservable is not un-projected")
}

// A deleting backend is released by the Cinder-side volume step, which runs the
// service-remove Job against the parent's database. Until that lands, the
// backend has to say what it is waiting for — under its own reason, not the
// aggregation the skeleton would overwrite it with.
func TestCinderBackendReconcile_DeletingWithParentReportsDetaching(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	backend := detachingBackend("nfs1")
	r := newCinderBackendTestReconciler(cinder, backend)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue(), "the parent's watch wakes this controller when the Job completes")

	updated := getCinderBackend(t, r.Client, "nfs1")
	g.Expect(controllerutil.ContainsFinalizer(updated, CinderBackendServiceRemoveFinalizer)).To(BeTrue(),
		"only the parent may release a backend whose registry row still exists")
	ready := backendCondition(updated, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal(conditionReasonDetaching))
	g.Expect(ready.Message).To(ContainSubstring(serviceRemoveJobName(cinder, "nfs1")))
	g.Expect(collectEvents(recorder)).To(BeEmpty(), "a detach in progress is a status, not an event")
}

// With the parent gone the database that holds the service registry is gone too,
// so no Job can ever run and holding the finalizer would wedge the backend in
// Terminating forever. The release is unconditional, and the skipped
// unregistration is recorded rather than silent.
func TestCinderBackendReconcile_DeletingWithoutParentReleasesTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := detachingBackend("nfs1")
	r := newCinderBackendTestReconciler(backend)
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	// The fake client collects an object whose last finalizer is removed, so the
	// backend is gone rather than merely unheld.
	var gone cinderv1alpha1.CinderBackend
	err = r.Get(context.Background(), objectKey("nfs1"), &gone)
	g.Expect(err).To(HaveOccurred(), "releasing the last finalizer completes the deletion")

	g.Expect(collectEvents(recorder)).To(ContainElement(And(
		ContainSubstring(corev1.EventTypeNormal),
		ContainSubstring(eventReasonServiceRemoveSkipped),
		ContainSubstring("no service registry row can be removed"),
	)))
}

// A deleting backend this controller does not hold is none of its business: the
// CR is on its way out, so neither a status write nor an event would reach
// anyone, and the parent lookup is not worth making.
func TestCinderBackendReconcile_DeletingWithoutTheFinalizerIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	backend := testCinderBackend("nfs1")
	deleted := metav1.Now()
	backend.DeletionTimestamp = &deleted
	r := newCinderBackendTestReconciler(validCinder())
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	g.Expect(ok).To(BeTrue())

	result, err := r.reconcileDeleting(context.Background(), backend)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(backend.Status.Conditions).To(BeEmpty())
	g.Expect(collectEvents(recorder)).To(BeEmpty())
}

// A CR that vanished between the event and the Get is not an error: the
// workqueue would otherwise retry a reconcile for an object that no longer
// exists.
func TestCinderBackendReconcile_MissingCRIsDone(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newCinderBackendTestReconciler()

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: objectKey("gone")})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
}
