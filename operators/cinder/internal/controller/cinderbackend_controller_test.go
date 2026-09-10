// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

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
