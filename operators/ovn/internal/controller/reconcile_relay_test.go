// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// testRelayClusterIP is the address the API server assigns the relay Service in
// the tests that get that far. The fake client assigns none, so the tests stamp
// it the way the API server would.
const testRelayClusterIP = "10.96.0.77"

// relayOVNCentral is the fixture of a CR that asks for relays and has both
// database addresses published.
func relayOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := publishEndpoints(testOVNCentral())
	cr.UID = "ovn-central-uid"
	cr.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 2}
	return cr
}

// ownedRelayChildren builds the two relay children as an earlier pass of this
// CR left them behind, controller reference included.
func ownedRelayChildren(t *testing.T, cr *ovnv1alpha1.OVNCentral) (*appsv1.Deployment, *corev1.Service) {
	t.Helper()

	scheme := newTestScheme(t)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ovn-sb-relay", Namespace: testNamespace}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ovn-sb-relay", Namespace: testNamespace}}
	for _, obj := range []client.Object{deploy, svc} {
		if err := controllerutil.SetControllerReference(cr, obj, scheme); err != nil {
			t.Fatalf("setting the controller reference on %s: %v", obj.GetName(), err)
		}
	}
	return deploy, svc
}

// assignRelayClusterIP stamps the cluster IP the API server would have assigned
// the relay Service.
func assignRelayClusterIP(ctx context.Context, t *testing.T, r *OVNCentralReconciler) {
	t.Helper()

	var svc corev1.Service
	if err := r.Get(ctx, centralKey("ovn-sb-relay"), &svc); err != nil {
		t.Fatalf("reading the relay Service: %v", err)
	}
	svc.Spec.ClusterIP = testRelayClusterIP
	if err := r.Update(ctx, &svc); err != nil {
		t.Fatalf("assigning the relay Service a cluster IP: %v", err)
	}
}

// Clearing spec.relay takes the relays with it. Leaving them running would keep
// a tier the CR no longer describes serving reads from a database it no longer
// tracks.
func TestReconcileRelay_ClearedSpecDeletesTheChildren(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	deploy, svc := ownedRelayChildren(t, cr)
	cr.Spec.Relay = nil
	cr.Status.RelayAddress = "ssl:10.96.0.77:6642"
	r := newTestOVNCentralReconciler(t, cr, deploy, svc)

	res, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-sb-relay"), &appsv1.Deployment{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-sb-relay"), &corev1.Service{}))).To(BeTrue())

	cond := ovnCentralCondition(cr, conditionTypeRelayReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonRelayNotRequired))
	g.Expect(cr.Status.RelayAddress).To(BeEmpty(),
		"a client handed the address of a deleted Service waits out its own timeout")
}

// A Deployment of the same name that this CR never created belongs to somebody
// else. Deleting it would take down a workload the operator does not own.
func TestReconcileRelay_ClearedSpecLeavesAForeignDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	cr.Spec.Relay = nil
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ovn-sb-relay", Namespace: testNamespace},
	}
	r := newTestOVNCentralReconciler(t, cr, foreign)

	_, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, centralKey("ovn-sb-relay"), &appsv1.Deployment{})).To(Succeed(),
		"an object this CR does not control is left where it is")
	g.Expect(ovnCentralCondition(cr, conditionTypeRelayReady).Reason).To(Equal(conditionReasonRelayNotRequired))
}

// The relay is configured with the Southbound address and nothing else, so it
// cannot be projected before the endpoint step has published one.
func TestReconcileRelay_WaitsForTheSouthboundAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	cr.Status.Southbound.InternalDbAddress = ""
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))

	cond := ovnCentralCondition(cr, conditionTypeRelayReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForEndpoints))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-sb-relay"), &appsv1.Deployment{}))).To(BeTrue())
}

// The first pass creates both children and finds the Service without an address
// yet. That is reported ahead of the Deployment's own state: relays nothing can
// reach are unreachable however many of them are running.
func TestReconcileRelay_ServiceWithoutAClusterIPIsPending(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))

	var deploy appsv1.Deployment
	g.Expect(r.Get(ctx, centralKey("ovn-sb-relay"), &deploy)).To(Succeed())
	g.Expect(*deploy.Spec.Replicas).To(BeEquivalentTo(2))
	g.Expect(r.Get(ctx, centralKey("ovn-sb-relay"), &corev1.Service{})).To(Succeed())

	cond := ovnCentralCondition(cr, conditionTypeRelayReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonServicePending))
	g.Expect(cr.Status.RelayAddress).To(BeEmpty())
}

// The address is published as soon as the Service carries one, even while the
// pods behind it are still starting: a chassis that connects early retries
// against an address that is already correct.
func TestReconcileRelay_ProgressingPublishesTheAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileRelay(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	assignRelayClusterIP(ctx, t, r)

	res, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	cond := ovnCentralCondition(cr, conditionTypeRelayReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentProgressing))
	g.Expect(cr.Status.RelayAddress).To(Equal("ssl:" + testRelayClusterIP + ":6642"))
}

func TestReconcileRelay_ReadyPublishesTheAddress(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileRelay(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	assignRelayClusterIP(ctx, t, r)
	g.Expect(simulators.SimulateDeploymentReady(ctx, r.Client, centralKey("ovn-sb-relay"), 2)).To(Succeed())

	res, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "an available relay tier is not polled")

	cond := ovnCentralCondition(cr, conditionTypeRelayReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(cr.Status.RelayAddress).To(Equal("ssl:"+testRelayClusterIP+":6642"),
		"the chassis are pointed at the relays through this field")
}

// A target cluster that grants the operator no deployments verb fails here. The
// condition has to flip on that pass: the aggregate Ready is re-derived from the
// sub-conditions at the new observedGeneration, so a condition left untouched
// would report the failed pass as ready.
func TestReconcileRelay_ApplyErrorIsDeploymentError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "Deployment" {
					return apierrors.NewForbidden(appsv1.Resource("deployments"), "ovn-sb-relay", nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring sb-relay Deployment")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnCentralCondition(cr, conditionTypeRelayReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentError))

	// The Service is never reached, so a CR whose relay tier failed to apply
	// publishes no address for it.
	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-sb-relay"), &corev1.Service{}))).To(BeTrue())
	g.Expect(cr.Status.RelayAddress).To(BeEmpty())
}

// The relay tier holds a keypair of its own. Handing it the Southbound
// database's server keypair would put the database's identity in the one tier
// every chassis in the fleet keeps an open connection to, so a single relay pod
// compromise would yield the credential the database is authenticated by.
func TestBuildRelayDeployment_MountsTheRelaysOwnKeypair(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := relayOVNCentral()

	deploy := buildRelayDeployment(cr)

	var tls *corev1.Volume
	for i := range deploy.Spec.Template.Spec.Volumes {
		if deploy.Spec.Template.Spec.Volumes[i].Name == tlsVolumeName {
			tls = &deploy.Spec.Template.Spec.Volumes[i]
		}
	}
	g.Expect(tls).NotTo(BeNil())
	g.Expect(tls.Secret.SecretName).To(Equal(relayName(cr)))
	g.Expect(tls.Secret.SecretName).NotTo(Equal(raftServerSecretName(cr, southboundDB(cr))))
}

// Clearing spec.relay takes the tier's Certificate with it: a keypair the
// configured CA signed and nothing authenticates with any more is one more copy
// of that CA's trust lying around.
func TestReconcileRelay_ClearedSpecDeletesTheCertificate(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := relayOVNCentral()
	deploy, svc := ownedRelayChildren(t, cr)
	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "ovn-sb-relay", Namespace: testNamespace},
	}
	if err := controllerutil.SetControllerReference(cr, cert, newTestScheme(t)); err != nil {
		t.Fatalf("setting the controller reference on the relay Certificate: %v", err)
	}
	cr.Spec.Relay = nil
	r := newTestOVNCentralReconciler(t, cr, deploy, svc, cert)

	_, err := r.reconcileRelay(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-sb-relay"), &certmanagerv1.Certificate{}))).
		To(BeTrue())
}
