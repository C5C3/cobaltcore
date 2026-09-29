// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// testChassisClientSecretName is the copy the step writes for the shared
// chassis fixture.
const testChassisClientSecretName = testOVNChassisName + "-ovn-client"

// clientSecretChassis is the shared chassis fixture with the UID the API server
// would have stamped, so the controller reference on the copy names it.
func clientSecretChassis() *ovnv1alpha1.OVNChassis {
	cr := testOVNChassis()
	cr.UID = "ovn-chassis-uid"
	return cr
}

// crossClusterCentral is what the central step resolves for a chassis whose
// central projects onto edge-1: the node addresses, the source Secret's name,
// and no Secret for the pods yet, which is this step's to name.
func crossClusterCentral() resolvedCentral {
	return resolvedCentral{
		ovnRemote:               testSouthboundNodeAddress,
		nbAddress:               testNorthboundNodeAddress,
		sbAddress:               testSouthboundNodeAddress,
		centralTargetClusterRef: &commonv1.TargetClusterRefSpec{Name: "edge-1"},
		sourceClientSecretName:  testClientSecretName,
	}
}

// sourceClientSecret is the client Secret cert-manager writes for the central,
// carrying the given data.
func sourceClientSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testClientSecretName, Namespace: testNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	}
}

// clientIdentity is a complete client identity, plus the key cert-manager adds
// beside it that the copy leaves out.
func clientIdentity() map[string][]byte {
	return map[string][]byte{
		"tls.crt":           []byte("client-cert"),
		"tls.key":           []byte("client-key"),
		"ca.crt":            []byte("ca-cert"),
		"cert-manager.io/x": []byte("not part of the identity"),
	}
}

// resolvedCentralReady is CentralReady as the central step leaves it before the
// client-Secret step runs.
func resolvedCentralReady(cr *ovnv1alpha1.OVNChassis) {
	cr.Status.Conditions = []metav1.Condition{{
		Type:   conditionTypeCentralReady,
		Status: metav1.ConditionTrue,
		Reason: conditionReasonCentralResolved,
	}}
}

// A chassis on its central's cluster keeps mounting the central's Secret
// directly, as it always has: the step writes nothing and publishes the
// source's name.
func TestReconcileClientSecret_SameClusterMountsTheCentralsSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	r := newTestOVNChassisReconciler(t, cr, sourceClientSecret(clientIdentity()))

	name, res, err := r.reconcileClientSecret(ctx, r.Client, cr, resolvedCentral{
		ovnRemote:              testSouthboundAddress,
		sbAddress:              testSouthboundAddress,
		clientSecretName:       testClientSecretName,
		sameCluster:            true,
		sourceClientSecretName: testClientSecretName,
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(Equal(testClientSecretName))
	g.Expect(cr.Status.ClientSecretName).To(Equal(testClientSecretName))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, chassisKey(testChassisClientSecretName), &corev1.Secret{}))).
		To(BeTrue(), "no copy is written for a chassis on its central's cluster")
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Status).To(Equal(metav1.ConditionTrue))
}

// Across a cluster boundary the step copies the three keys of the client
// identity into <chassis>-ovn-client, claims it for the chassis, and names it
// for the pods. A second pass over an unchanged source writes nothing.
func TestReconcileClientSecret_CopiesTheClientIdentity(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)

	updates := 0
	c := ovnChassisFakeClientBuilder(t, cr, sourceClientSecret(clientIdentity())).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					updates++
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	name, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(Equal(testChassisClientSecretName))
	g.Expect(cr.Status.ClientSecretName).To(Equal(testChassisClientSecretName))
	g.Expect(ovnChassisCondition(cr, conditionTypeCentralReady).Status).To(Equal(metav1.ConditionTrue))

	var copied corev1.Secret
	g.Expect(r.Get(ctx, chassisKey(testChassisClientSecretName), &copied)).To(Succeed())
	g.Expect(copied.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(copied.Data).To(Equal(map[string][]byte{
		"tls.crt": []byte("client-cert"),
		"tls.key": []byte("client-key"),
		"ca.crt":  []byte("ca-cert"),
	}))
	g.Expect(metav1.IsControlledBy(&copied, cr)).To(BeTrue(),
		"a local copy is reaped with the chassis through its controller reference")
	g.Expect(copied.Labels).To(And(
		HaveKeyWithValue("app.kubernetes.io/name", "ovnchassis"),
		HaveKeyWithValue("app.kubernetes.io/instance", testOVNChassisName),
		HaveKeyWithValue("app.kubernetes.io/component", "ovn-client"),
	))

	_, _, err = r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updates).To(BeZero(), "an up-to-date copy is not rewritten")
}

// A placed chassis whose central stays on the management cluster: the source is
// read locally and the copy lands on the target, marked by the ownership labels
// the teardown sweeps by rather than by a reference to a UID the target does
// not know.
func TestReconcileClientSecret_CopiesOntoTheChassisTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	r := newTestOVNChassisReconciler(t, cr, sourceClientSecret(clientIdentity()))
	target := ovnChassisFakeClientBuilder(t).Build()
	children := mctestutil.RemoteChildren(t, r.Client, target)

	central := crossClusterCentral()
	central.centralTargetClusterRef = nil

	name, _, err := r.reconcileClientSecret(ctx, children, cr, central)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(Equal(testChassisClientSecretName))
	var copied corev1.Secret
	g.Expect(target.Get(ctx, chassisKey(testChassisClientSecretName), &copied)).To(Succeed())
	g.Expect(copied.OwnerReferences).To(BeEmpty())
	owned, err := commonmulticluster.Controls(r.Scheme, cr, &copied)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(owned).To(BeTrue(), "the copy carries the ownership labels the sweep selects by")
	g.Expect(apierrors.IsNotFound(r.Get(ctx, chassisKey(testChassisClientSecretName), &corev1.Secret{}))).
		To(BeTrue(), "nothing is written beside the source on the central's cluster")
}

// A chassis on the management cluster whose central projects onto a target: the
// source exists on the target alone, so the copy can only carry its values if
// the step read it through the central's children client rather than through
// the chassis's own.
func TestReconcileClientSecret_ReadsTheSourceOnTheCentralsCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	r := newTestOVNChassisReconciler(t, cr)
	target := ovnChassisFakeClientBuilder(t, sourceClientSecret(clientIdentity())).Build()
	r.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: target})

	name, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(Equal(testChassisClientSecretName))
	var copied corev1.Secret
	g.Expect(r.Get(ctx, chassisKey(testChassisClientSecretName), &copied)).To(Succeed())
	g.Expect(copied.Data).To(Equal(map[string][]byte{
		"tls.crt": []byte("client-cert"),
		"tls.key": []byte("client-key"),
		"ca.crt":  []byte("ca-cert"),
	}))
	g.Expect(apierrors.IsNotFound(target.Get(ctx, chassisKey(testChassisClientSecretName), &corev1.Secret{}))).
		To(BeTrue(), "the copy lands on the chassis's cluster, not beside the source")
}

// A renewal, or a hand edit, is repaired to exactly the three source values: a
// stale certificate authenticates against nothing, and an extra key would
// survive in a Secret the operator owns.
func TestReconcileClientSecret_CopyRepairsDrift(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	r := newTestOVNChassisReconciler(t, cr, sourceClientSecret(clientIdentity()))

	_, _, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())
	g.Expect(err).NotTo(HaveOccurred())

	var drifted corev1.Secret
	g.Expect(r.Get(ctx, chassisKey(testChassisClientSecretName), &drifted)).To(Succeed())
	drifted.Data["tls.crt"] = []byte("edited")
	drifted.Data["foo"] = []byte("bar")
	g.Expect(r.Update(ctx, &drifted)).To(Succeed())

	_, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	var repaired corev1.Secret
	g.Expect(r.Get(ctx, chassisKey(testChassisClientSecretName), &repaired)).To(Succeed())
	g.Expect(repaired.Data).To(Equal(map[string][]byte{
		"tls.crt": []byte("client-cert"),
		"tls.key": []byte("client-key"),
		"ca.crt":  []byte("ca-cert"),
	}))
}

// The central names its client Secret before cert-manager has issued it, so an
// absent source is an ordinary wait, and nothing is copied in the meantime.
func TestReconcileClientSecret_SourceNotFoundWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	r := newTestOVNChassisReconciler(t, cr)

	name, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(name).To(BeEmpty())
	g.Expect(cr.Status.ClientSecretName).To(BeEmpty())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretPending))
	g.Expect(cond.Message).To(ContainSubstring(testNamespace + "/" + testClientSecretName))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, chassisKey(testChassisClientSecretName), &corev1.Secret{}))).To(BeTrue())
}

// A source that misses one of the three keys, or carries one empty, is not a
// usable identity yet. The message names the key, and nothing is copied: a
// partial copy would mount as a keypair ovn-controller cannot load.
func TestReconcileClientSecret_IncompleteSourceWaits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(map[string][]byte)
		wantKey string
	}{
		{"without ca.crt", func(d map[string][]byte) { delete(d, "ca.crt") }, "ca.crt"},
		{"with an empty tls.key", func(d map[string][]byte) { d["tls.key"] = nil }, "tls.key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := clientSecretChassis()
			resolvedCentralReady(cr)
			data := clientIdentity()
			tc.mutate(data)
			r := newTestOVNChassisReconciler(t, cr, sourceClientSecret(data))

			name, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(name).To(BeEmpty())

			cond := ovnChassisCondition(cr, conditionTypeCentralReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretIncomplete))
			g.Expect(cond.Message).To(Equal("OVN client Secret " + testNamespace + "/" + testClientSecretName +
				" carries no " + tc.wantKey + " yet"))
			g.Expect(apierrors.IsNotFound(r.Get(ctx, chassisKey(testChassisClientSecretName), &corev1.Secret{}))).To(BeTrue())
		})
	}
}

// A read of the source that fails for any other reason than NotFound is the
// operator's own problem: it is returned wrapped, with the API error still
// unwrappable, and the condition flips so Ready cannot stay stale-True.
func TestReconcileClientSecret_SourceReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	readErr := apierrors.NewForbidden(corev1.Resource("secrets"), testClientSecretName, nil)

	c := ovnChassisFakeClientBuilder(t, cr, sourceClientSecret(clientIdentity())).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == testClientSecretName {
					return readErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).To(MatchError("reading OVN client Secret " + testNamespace + "/" + testClientSecretName +
		": " + readErr.Error()))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretReadError))
}

// A Secret under the copy's name that the chassis did not create is refused, not
// adopted: overwriting it would hand the client key to whoever reads a Secret
// somebody else provisioned.
func TestReconcileClientSecret_ForeignSecretIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testChassisClientSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte("somebody else's")},
	}
	r := newTestOVNChassisReconciler(t, cr, sourceClientSecret(clientIdentity()), foreign)

	name, _, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).To(MatchError(ContainSubstring("exists and is not owned by this OVNChassis")))
	g.Expect(name).To(BeEmpty())
	g.Expect(cr.Status.ClientSecretName).To(BeEmpty())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretCopyFailed))

	var untouched corev1.Secret
	g.Expect(r.Get(ctx, chassisKey(testChassisClientSecretName), &untouched)).To(Succeed())
	g.Expect(untouched.Data).To(Equal(map[string][]byte{"password": []byte("somebody else's")}))
}

// A copy the chassis's cluster refuses to store fails the pass with the API
// error wrapped and still unwrappable.
func TestReconcileClientSecret_WriteFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	createErr := apierrors.NewForbidden(corev1.Resource("secrets"), testChassisClientSecretName, nil)

	c := ovnChassisFakeClientBuilder(t, cr, sourceClientSecret(clientIdentity())).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return createErr
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).To(MatchError("creating the OVN client Secret copy " + testNamespace + "/" +
		testChassisClientSecretName + ": " + createErr.Error()))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretCopyFailed))
}

// A copy the chassis's cluster refuses to read back fails the pass the same way,
// before anything is written.
func TestReconcileClientSecret_CopyReadFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	getErr := apierrors.NewForbidden(corev1.Resource("secrets"), testChassisClientSecretName, nil)

	c := ovnChassisFakeClientBuilder(t, cr, sourceClientSecret(clientIdentity())).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == testChassisClientSecretName {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).To(MatchError("reading the OVN client Secret copy " + testNamespace + "/" +
		testChassisClientSecretName + ": " + getErr.Error()))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretCopyFailed))
}

// A renewal the chassis's cluster refuses to store fails the pass the same way:
// the repair of an existing copy is the write every certificate renewal goes
// through.
func TestReconcileClientSecret_RenewalWriteFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	updateErr := apierrors.NewForbidden(corev1.Resource("secrets"), testChassisClientSecretName, nil)

	c := ovnChassisFakeClientBuilder(t, cr, sourceClientSecret(clientIdentity())).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok && obj.GetName() == testChassisClientSecretName {
					return updateErr
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, _, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())
	g.Expect(err).NotTo(HaveOccurred())

	var source corev1.Secret
	g.Expect(r.Get(ctx, chassisKey(testClientSecretName), &source)).To(Succeed())
	source.Data["tls.crt"] = []byte("renewed-client-cert")
	g.Expect(r.Update(ctx, &source)).To(Succeed())

	_, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).To(MatchError("updating the OVN client Secret copy " + testNamespace + "/" +
		testChassisClientSecretName + ": " + updateErr.Error()))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonClientSecretCopyFailed))
}

// The central's cluster not resolving is a wait, not an error: it is what an
// operator restart looks like until the provider has engaged the cluster.
func TestReconcileClientSecret_UnresolvableCentralClusterWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := clientSecretChassis()
	resolvedCentralReady(cr)
	r := newTestOVNChassisReconciler(t, cr)
	r.Resolver = unresolvableResolver{}

	name, res, err := r.reconcileClientSecret(ctx, r.Client, cr, crossClusterCentral())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(name).To(BeEmpty())

	cond := ovnChassisCondition(cr, conditionTypeCentralReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
}
