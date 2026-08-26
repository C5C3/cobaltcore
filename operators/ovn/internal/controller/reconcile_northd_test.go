// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// northd is configured with the two addresses and has no discovery of its own,
// so nothing may be applied before the endpoint step has published them: a
// Deployment carrying an empty --ovnnb-db would crash-loop its pods until the
// next pass rewrote the template.
func TestReconcileNorthd_WaitsForThePublishedAddresses(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	cr.Status.Northbound.InternalDbAddress = testNorthboundAddress
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileNorthd(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))

	cond := ovnCentralCondition(cr, conditionTypeNorthdReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForEndpoints))

	g.Expect(apierrors.IsNotFound(r.Get(ctx, centralKey("ovn-northd"), &appsv1.Deployment{}))).
		To(BeTrue(), "one address is no address: nothing may be applied yet")
}

func TestReconcileNorthd_ProgressingUntilTheDeploymentIsAvailable(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishEndpoints(testOVNCentral())
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileNorthd(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	var deploy appsv1.Deployment
	g.Expect(r.Get(ctx, centralKey("ovn-northd"), &deploy)).To(Succeed())
	g.Expect(*deploy.Spec.Replicas).To(BeEquivalentTo(commonv1.DefaultReplicas))

	cond := ovnCentralCondition(cr, conditionTypeNorthdReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentProgressing))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty(),
		"the installed image is the pods' image, and no pod runs it yet")
}

func TestReconcileNorthd_ReadyRecordsTheInstalledImage(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishEndpoints(testOVNCentral())
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileNorthd(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.SimulateDeploymentReady(ctx, r.Client, centralKey("ovn-northd"),
		commonv1.DefaultReplicas)).To(Succeed())

	res, err := r.reconcileNorthd(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "an available Deployment is not polled")

	cond := ovnCentralCondition(cr, conditionTypeNorthdReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(cr.Status.InstalledImage).To(Equal(defaultOVNRepository + ":" + defaultOVNVersion))
}

// A target cluster that grants the operator no deployments verb fails here. The
// condition has to flip on that pass: the aggregate Ready is re-derived from the
// sub-conditions at the new observedGeneration, so a condition left untouched
// would report the failed pass as ready.
func TestReconcileNorthd_ApplyErrorIsDeploymentError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishEndpoints(testOVNCentral())

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "Deployment" {
					return apierrors.NewForbidden(appsv1.Resource("deployments"), "ovn-northd", nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileNorthd(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring northd Deployment")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnCentralCondition(cr, conditionTypeNorthdReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentError))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty())
}

// A CR that reached the controller with a zero replica count bypassed both the
// CRD default and the webhook. Rendered unnormalized it would scale northd to
// zero pods, which is a control plane that compiles nothing.
func TestEffectiveNorthd_NormalizesTheReplicaCountAndLeavesTheCRAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.Northd.Deployment.Replicas = 0

	northd := effectiveNorthd(cr)

	g.Expect(northd.Deployment.Replicas).To(Equal(commonv1.DefaultReplicas))
	g.Expect(northd.Deployment.Resources).NotTo(BeNil(),
		"a container without requests lands in the BestEffort QoS class")
	g.Expect(cr.Spec.Northd.Deployment.Replicas).To(BeEquivalentTo(0),
		"the resolution happens on a copy; the CR is written back at the end of the pass")
	g.Expect(cr.Spec.Northd.Deployment.Resources).To(BeNil())
}
