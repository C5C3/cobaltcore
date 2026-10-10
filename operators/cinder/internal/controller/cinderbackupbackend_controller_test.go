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

// reconcileRBDBackupBackend runs one pass of the backup backend controller over
// an RBD target next to the shared Cinder and objs, and returns the result, the
// re-read target and the error.
func reconcileRBDBackupBackend(t *testing.T, backupBackend *cinderv1alpha1.CinderBackupBackend,
	objs ...client.Object,
) (reconcile.Result, *cinderv1alpha1.CinderBackupBackend, error) {
	t.Helper()
	r := newCinderBackupBackendTestReconciler(append([]client.Object{validCinder(), backupBackend}, objs...)...)
	result, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))
	return result, getCinderBackupBackend(t, r.Client, backupBackend.Name), err
}

// expectRBDBackupKeyWaiting asserts the gate held an RBD backup target on its
// key: the pass polls, CredentialsReady is False with the given message, and the
// projection observation never ran.
func expectRBDBackupKeyWaiting(t *testing.T, result reconcile.Result, err error,
	updated *cinderv1alpha1.CinderBackupBackend, wantMessage string,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(err).NotTo(HaveOccurred(), "a key that is not there yet is a waiting state, not a failure")
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"an absent Secret emits no event, so the gate polls")

	creds := backupBackendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds).NotTo(BeNil())
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForCredentials))
	g.Expect(creds.Message).To(Equal(wantMessage))
	g.Expect(backupBackendCondition(updated, conditionTypeConfigProjected)).To(BeNil(),
		"the parent never projects a backup target that is not credential-ready, so there is nothing to observe")
	g.Expect(backupBackendCondition(updated, "Ready").Status).To(Equal(metav1.ConditionFalse))
}

// An RBD backup target whose key Secret does not exist, and that no
// ExternalSecret is about to produce, waits on the gate rather than reaching the
// projection.
func TestCinderBackupBackendReconcile_RBDKeySecretMissingWaits(t *testing.T) {
	result, updated, err := reconcileRBDBackupBackend(t, testRBDCinderBackupBackend("rbdbk"))
	expectRBDBackupKeyWaiting(t, result, err, updated, "RBD key ExternalSecret openstack/rbdbk-key not found yet")
}

// A Secret under the referenced name that carries no userKey, while the
// ExternalSecret producing it already reports Ready, is a malformed Secret
// rather than one still syncing.
func TestCinderBackupBackendReconcile_RBDKeySecretWithoutKeyWaits(t *testing.T) {
	secret := rbdKeySecret("rbdbk", testRBDKey)
	secret.Data = map[string][]byte{"key": []byte(testRBDKey)}
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "rbdbk-key", Namespace: testNamespace},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue},
			},
		},
	}

	result, updated, err := reconcileRBDBackupBackend(t, testRBDCinderBackupBackend("rbdbk"), secret, es)
	expectRBDBackupKeyWaiting(t, result, err, updated, "RBD key Secret exists but is missing expected keys")
}

// A userKey of whitespace alone is an empty key once trimmed: copied into the
// keyring it would fail every connection the backup driver opens.
func TestCinderBackupBackendReconcile_RBDKeyEmptyWaits(t *testing.T) {
	result, updated, err := reconcileRBDBackupBackend(t, testRBDCinderBackupBackend("rbdbk"),
		rbdKeySecret("rbdbk", " \n"))
	expectRBDBackupKeyWaiting(t, result, err, updated, `Secret "rbdbk-key" carries an empty userKey`)
}

// A value that is not the base64 a cephx key is printed as is refused before it
// reaches the keyring, and the message never repeats the value.
func TestCinderBackupBackendReconcile_RBDKeyNotBase64Waits(t *testing.T) {
	result, updated, err := reconcileRBDBackupBackend(t, testRBDCinderBackupBackend("rbdbk"),
		rbdKeySecret("rbdbk", "not a key!"))
	expectRBDBackupKeyWaiting(t, result, err, updated,
		`Secret "rbdbk-key" carries a userKey that is not a cephx key (base64 expected)`)
}

// The admission union rule guarantees spec.rbd on a type-RBD backup target; one
// written past it has no key to resolve.
func TestCinderBackupBackendReconcile_RBDBlockMissingWaits(t *testing.T) {
	backupBackend := testRBDCinderBackupBackend("rbdbk")
	backupBackend.Spec.RBD = nil
	result, updated, err := reconcileRBDBackupBackend(t, backupBackend)
	expectRBDBackupKeyWaiting(t, result, err, updated, "spec.rbd is not set; no RBD key to resolve")
}

// A type the operator does not render cannot become credential-ready.
func TestCinderBackupBackendReconcile_UnknownTypeWaits(t *testing.T) {
	backupBackend := testRBDCinderBackupBackend("rbdbk")
	backupBackend.Spec.Type = cinderv1alpha1.CinderBackupBackendType("Swift")
	result, updated, err := reconcileRBDBackupBackend(t, backupBackend)
	expectRBDBackupKeyWaiting(t, result, err, updated, "spec.type Swift is not a type this operator renders")
}

// A usable key opens the gate, and the pass goes on to observe the projection
// exactly as it does for an NFS target.
func TestCinderBackupBackendReconcile_RBDKeyPresentIsCredentialsAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	result, updated, err := reconcileRBDBackupBackend(t, testRBDCinderBackupBackend("rbdbk"),
		rbdKeySecret("rbdbk", testRBDKey+"\n"))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"nothing is projected yet, so the projection gate polls")
	creds := backupBackendCondition(updated, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(creds.Reason).To(Equal(conditionReasonCredentialsAvailable))
	g.Expect(creds.Message).To(Equal(`RBD key Secret "rbdbk-key" carries the userKey data key`))

	projected := backupBackendCondition(updated, conditionTypeConfigProjected)
	g.Expect(projected).NotTo(BeNil(), "the projection observation ran")
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
}

// A failed read of the key's value is wrapped with the backup target's name and
// establishes nothing about the key, so a standing CredentialsReady=True stays:
// the Cinder-side projection gates on it, and demoting it on a cache blip would
// delete the backup service.
func TestCinderBackupBackendReconcile_RBDSecretReadErrorKeepsTheStandingClaim(t *testing.T) {
	g := NewGomegaWithT(t)
	backupBackend := credentialReadyRBDBackupBackend("rbdbk")
	boom := errors.New("the apiserver is briefly unreachable")
	secretGets := 0
	r := &CinderBackupBackendReconciler{
		Client: cinderFakeClientBuilder(validCinder(), backupBackend, rbdKeySecret("rbdbk", testRBDKey)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					// The gate reads the Secret twice: once for its keys, once
					// for the value's shape. The second read fails.
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

	_, err := r.Reconcile(context.Background(), backupBackendRequest(backupBackend))

	g.Expect(err).To(MatchError(ContainSubstring(`reading the RBD key of backup backend "rbdbk"`)))
	g.Expect(errors.Is(err, boom)).To(BeTrue(), "the cause stays on the chain")
	creds := backupBackendCondition(getCinderBackupBackend(t, r.Client, "rbdbk"), conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionTrue), "unreadable is not missing")
	g.Expect(backupBackendCondition(getCinderBackupBackend(t, r.Client, "rbdbk"), conditionTypeConfigProjected)).
		To(BeNil(), "the pass returned before the projection observation could run")
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
