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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonconditions "github.com/c5c3/cobaltcore/internal/common/conditions"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// newCinderBackupBackendTestReconciler builds a CinderBackupBackendReconciler
// over a fake client pre-loaded with objs. The Resolver stays nil, the
// always-local mode every single-cluster test runs in.
func newCinderBackupBackendTestReconciler(objs ...client.Object) *CinderBackupBackendReconciler {
	return &CinderBackupBackendReconciler{
		Client:   cinderFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(10),
	}
}

// backupBackendRequest builds the reconcile request for a backup backend
// fixture.
func backupBackendRequest(backupBackend *cinderv1alpha1.CinderBackupBackend) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backupBackend)}
}

// getCinderBackupBackend re-reads a backup backend from the given client.
func getCinderBackupBackend(t *testing.T, c client.Client, name string) *cinderv1alpha1.CinderBackupBackend {
	t.Helper()
	var backupBackend cinderv1alpha1.CinderBackupBackend
	if err := c.Get(context.Background(), objectKey(name), &backupBackend); err != nil {
		t.Fatalf("re-reading CinderBackupBackend %s: %v", name, err)
	}
	return &backupBackend
}

// backupBackendCondition returns one of a backup backend's conditions, or nil.
func backupBackendCondition(backupBackend *cinderv1alpha1.CinderBackupBackend,
	conditionType string,
) *metav1.Condition {
	return commonconditions.GetCondition(backupBackend.Status.Conditions, conditionType)
}

// projectedBackupDeployment returns the cinder-backup Deployment exactly as the
// Cinder-side step builds it for the given projection.
func projectedBackupDeployment(cinder *cinderv1alpha1.Cinder, backup *backupProjection) *appsv1.Deployment {
	return buildBackupDeployment(cinder, backup, nil, workloadArtifacts(), workloadDigests{}, testEgressPort)
}

// An NFS export is mounted with the pod's own identity, so the credential gate
// is satisfied by construction. It is still reported: the Cinder-side projection
// gates on this very condition, and an absent one reads as "not ready yet".
func TestCinderBackupBackendReconcile_CredentialsNotRequiredForNFS(t *testing.T) {
	g := NewGomegaWithT(t)
	backupBackend := testCinderBackupBackend("backups")
	r := newCinderBackupBackendTestReconciler(validCinder(), backupBackend)

	result, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"nothing is projected yet, so the projection gate polls")

	updated := getCinderBackupBackend(t, r.Client, "backups")
	creds := backupBackendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds).NotTo(BeNil())
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(creds.Reason).To(Equal(conditionReasonCredentialsNotRequired))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
}

// A parent that does not exist stops the pass at the first gate: without one
// there is nothing to say which cluster the backup service writes from.
func TestCinderBackupBackendReconcile_NoParentCinderHoldsBeforeProjection(t *testing.T) {
	g := NewGomegaWithT(t)
	backupBackend := testCinderBackupBackend("backups")
	r := newCinderBackupBackendTestReconciler(backupBackend)

	result, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getCinderBackupBackend(t, r.Client, "backups")
	creds := backupBackendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForParent))
	g.Expect(backupBackendCondition(updated, conditionTypeConfigProjected)).To(BeNil(),
		"the pass returned before the projection observation could run")
}

// The Secret name is the whole observation: one Cinder renders at most one
// backup target, so the rendered document carries no per-backend section, while
// every backup backend renders under a base name of its own. A backup service
// built from a replaced backend therefore reads as not projected, which is the
// state it is in.
func TestCinderBackupBackendReconcile_ObservesTheMountedProjectionByName(t *testing.T) {
	cinder := workloadCinder()

	for _, tc := range []struct {
		name          string
		objs          func() []client.Object
		wantProjected bool
	}{
		{
			name: "the backup Deployment mounts this backend's projection",
			objs: func() []client.Object {
				return []client.Object{projectedBackupDeployment(cinder, testBackupProjection())}
			},
			wantProjected: true,
		},
		{
			name: "no backup Deployment yet",
			objs: func() []client.Object { return nil },
		},
		{
			name: "the Deployment was built from another backup backend",
			objs: func() []client.Object {
				other := testBackupProjection()
				other.name = "elsewhere"
				other.secretName = backupSecretBaseName(cinder, "elsewhere") + "-def456"
				return []client.Object{projectedBackupDeployment(cinder, other)}
			},
		},
		{
			// A base name must not match a longer one: "backups" is a prefix of
			// "backups-archive", and without the separator the two would observe
			// each other's projection.
			name: "a longer base name shares this one's prefix",
			objs: func() []client.Object {
				other := testBackupProjection()
				other.secretName = backupSecretBaseName(cinder, "backups-archive") + "-def456"
				return []client.Object{projectedBackupDeployment(cinder, other)}
			},
		},
		{
			name: "the Deployment carries no backup volume",
			objs: func() []client.Object {
				deploy := projectedBackupDeployment(cinder, testBackupProjection())
				for i := range deploy.Spec.Template.Spec.Volumes {
					if deploy.Spec.Template.Spec.Volumes[i].Name == backupVolumeName {
						deploy.Spec.Template.Spec.Volumes[i].Name = "something-else"
					}
				}
				return []client.Object{deploy}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			backupBackend := testCinderBackupBackend("backups")
			objs := append([]client.Object{cinder, backupBackend}, tc.objs()...)
			r := newCinderBackupBackendTestReconciler(objs...)

			result, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))

			g.Expect(err).NotTo(HaveOccurred(), "a projection that has not landed is not a failure")

			updated := getCinderBackupBackend(t, r.Client, "backups")
			projected := backupBackendCondition(updated, conditionTypeConfigProjected)
			g.Expect(projected).NotTo(BeNil())
			if !tc.wantProjected {
				g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
				g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
				return
			}
			g.Expect(projected.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(projected.Reason).To(Equal(conditionReasonConfigProjected))
			g.Expect(result.IsZero()).To(BeTrue(), "a converged backup backend polls nothing")
			g.Expect(backupBackendCondition(updated, "Ready").Status).To(Equal(metav1.ConditionTrue))
		})
	}
}

// A Get that fails for any reason other than NotFound establishes nothing, so it
// must not demote a standing claim: unobservable is not un-projected.
func TestCinderBackupBackendReconcile_ObservationErrorKeepsTheStandingClaim(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	backupBackend := testCinderBackupBackend("backups")
	backupBackend.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeConfigProjected,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonConfigProjected,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	boom := errors.New("the apiserver is briefly unreachable")
	r := &CinderBackupBackendReconciler{
		Client: cinderFakeClientBuilder(cinder, backupBackend).
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

	_, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))

	g.Expect(err).To(MatchError(boom))
	updated := getCinderBackupBackend(t, r.Client, "backups")
	g.Expect(backupBackendCondition(updated, conditionTypeConfigProjected).Status).
		To(Equal(metav1.ConditionTrue))
}

// A backup target holds no row in the service registry — cinder-backup registers
// under the Cinder's own host identity — so a deleting backup backend needs no
// finalizer and this controller has nothing to do for it.
func TestCinderBackupBackendReconcile_DeletingIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	backupBackend := testCinderBackupBackend("backups")
	deleted := metav1.Now()
	backupBackend.DeletionTimestamp = &deleted
	backupBackend.Finalizers = []string{"kubernetes.io/pvc-protection"}
	r := newCinderBackupBackendTestReconciler(validCinder(), backupBackend)

	result, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(getCinderBackupBackend(t, r.Client, "backups").Status.Conditions).To(BeEmpty(),
		"a deleting backup backend is not reported on")
}

// A CR that vanished between the event and the Get is not an error.
func TestCinderBackupBackendReconcile_MissingCRIsDone(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newCinderBackupBackendTestReconciler()

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: objectKey("gone")})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
}
