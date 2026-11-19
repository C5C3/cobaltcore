// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"cmp"
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/lifecycle"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	mctest "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// A PDB has conditions and observedGeneration without importing a service operator.
type parent = policyv1.PodDisruptionBudget

const finalizer = "test.c5c3.io/cleanup"

var request = ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "test", Name: "parent"}}

func newParent() *parent {
	return &parent{ObjectMeta: metav1.ObjectMeta{Name: request.Name, Namespace: request.Namespace, Generation: 3}}
}

func service(c client.Client, target *commonv1.TargetClusterRefSpec, suspended bool) lifecycle.Service[*parent, policyv1.PodDisruptionBudgetStatus] {
	return lifecycle.Service[*parent, policyv1.PodDisruptionBudgetStatus]{
		Client:          c,
		Finalizer:       finalizer,
		TargetCondition: "WorkReady",
		Skeleton: reconcile.Skeleton[*parent, policyv1.PodDisruptionBudgetStatus]{
			Conditions:               func(p *parent) *[]metav1.Condition { return &p.Status.Conditions },
			SubConditionTypes:        []string{"WorkReady"},
			RequireCurrentGeneration: true,
		},
		Fields: func(p *parent) lifecycle.Fields[policyv1.PodDisruptionBudgetStatus] {
			return lifecycle.Fields[policyv1.PodDisruptionBudgetStatus]{
				Target:             target,
				Suspended:          suspended,
				Status:             &p.Status,
				CopyStatus:         p.Status.DeepCopy,
				ObservedGeneration: &p.Status.ObservedGeneration,
			}
		},
	}
}

func TestReconcileMissingParentAndFailedFinalizerWrite(t *testing.T) {
	g := NewWithT(t)
	p := newParent()
	writeErr := errors.New("finalizer update failed")
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return writeErr },
	}).Build()
	s := service(c, nil, false)
	g.Expect(s.Reconcile(t.Context(), request, &parent{})).To(BeZero())

	g.Expect(c.Create(t.Context(), p)).To(Succeed())
	result, err := s.Reconcile(t.Context(), request, &parent{})
	g.Expect(result).To(BeZero())
	g.Expect(err).To(MatchError(writeErr))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(p.Finalizers).To(BeEmpty())
}

func TestReconcileFinalizersAndSteadyState(t *testing.T) {
	g := NewWithT(t)
	p := newParent()
	c := fake.NewClientBuilder().WithStatusSubresource(p).WithObjects(p).Build()
	remote := fake.NewClientBuilder().Build()
	s := service(c, &commonv1.TargetClusterRefSpec{Name: "target"}, false)
	s.Resolver = mctest.ResolverFor(mctest.TargetCluster{Client: remote})
	run := func() (ctrl.Result, error) { return s.Reconcile(t.Context(), request, &parent{}) }

	next := ctrl.Result{RequeueAfter: time.Minute}
	var workErr error
	syncCalls := 0
	s.Sync = func(_ context.Context, children client.Client, obj *parent) (ctrl.Result, error) {
		syncCalls++
		g.Expect(multicluster.LiveReader(children)).To(BeIdenticalTo(remote))
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type: "WorkReady", Status: metav1.ConditionTrue, Reason: "Synced", ObservedGeneration: obj.Generation,
		})
		return next, workErr
	}
	for _, want := range [][]string{{finalizer}, {finalizer, multicluster.RemoteChildrenFinalizer}} {
		g.Expect(run()).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}))
		g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
		g.Expect(p.Finalizers).To(ConsistOf(want))
	}
	g.Expect(syncCalls).To(BeZero(), "Sync must wait until both finalizers have been persisted")
	g.Expect(run()).To(Equal(next))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(meta.IsStatusConditionTrue(p.Status.Conditions, "Ready")).To(BeTrue())
	g.Expect(p.Status.ObservedGeneration).To(Equal(p.Generation))
	version := p.ResourceVersion
	g.Expect(run()).To(Equal(next))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(p.ResourceVersion).To(Equal(version))

	workErr = errors.New("sync failed")
	result, err := run()
	g.Expect(err).To(MatchError(workErr))
	g.Expect(result).To(Equal(next))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(meta.IsStatusConditionFalse(p.Status.Conditions, "Ready")).To(BeTrue())
}

type unavailableTarget struct{}

func (unavailableTarget) GetCluster(context.Context, mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, errors.New("target offline")
}

func TestReconcileSuspendConflictAndResume(t *testing.T) {
	g := NewWithT(t)
	p := newParent()
	c := fake.NewClientBuilder().WithObjects(p).WithStatusSubresource(p).
		WithInterceptorFuncs(testutil.ConflictFirstStatusUpdate()).Build()
	target := &commonv1.TargetClusterRefSpec{Name: "offline"}
	s := service(c, target, true)
	s.Resolver = unavailableTarget{}
	run := func() (ctrl.Result, error) { return s.Reconcile(t.Context(), request, &parent{}) }
	g.Expect(run()).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueNextPass}))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(p.Status.Conditions).To(BeEmpty())
	g.Expect(run()).To(BeZero())
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(meta.IsStatusConditionTrue(p.Status.Conditions, reconcile.ConditionTypeSuspended)).To(BeTrue())
	g.Expect(p.Finalizers).To(BeEmpty())

	s = service(c, target, false)
	s.Resolver = unavailableTarget{}
	g.Expect(run()).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueSecretPolling}))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(meta.FindStatusCondition(p.Status.Conditions, reconcile.ConditionTypeSuspended)).To(BeNil())
	g.Expect(meta.FindStatusCondition(p.Status.Conditions, "WorkReady")).To(HaveField("Reason", multicluster.TargetClusterUnavailable))
	g.Expect(meta.IsStatusConditionFalse(p.Status.Conditions, "Ready")).To(BeTrue())
	g.Expect(p.Finalizers).To(BeEmpty())
}

func TestReconcileDeletionWaitsForTarget(t *testing.T) {
	g := NewWithT(t)
	p := newParent()
	now := metav1.Now()
	p.DeletionTimestamp, p.Finalizers = &now, []string{finalizer, multicluster.RemoteChildrenFinalizer}
	c := fake.NewClientBuilder().WithObjects(p).WithStatusSubresource(p).Build()
	s := service(c, &commonv1.TargetClusterRefSpec{Name: "unavailable-during-cleanup"}, false)
	s.Resolver = unavailableTarget{}

	g.Expect(s.Reconcile(t.Context(), request, &parent{})).To(Equal(ctrl.Result{RequeueAfter: reconcile.RequeueSecretPolling}))
	g.Expect(c.Get(t.Context(), request.NamespacedName, p)).To(Succeed())
	g.Expect(p.Finalizers).To(ConsistOf(finalizer, multicluster.RemoteChildrenFinalizer))
	g.Expect(meta.FindStatusCondition(p.Status.Conditions, "WorkReady")).To(HaveField("Reason", multicluster.TargetClusterUnavailable))
}

func TestReconcileDeletionWhileSuspended(t *testing.T) {
	retry := ctrl.Result{RequeueAfter: time.Minute}
	failure := errors.New("cleanup failed")
	for _, tc := range []struct {
		name                        string
		finalizeResult, sweepResult ctrl.Result
		finalizeErr, sweepErr       error
		wantCalls                   []string
	}{
		{name: "finalizer requeues", finalizeResult: retry, wantCalls: []string{"finalize"}},
		{name: "finalizer fails", finalizeErr: failure, wantCalls: []string{"finalize"}},
		{name: "sweep requeues", sweepResult: retry, wantCalls: []string{"finalize", "sweep"}},
		{name: "sweep fails", sweepErr: failure, wantCalls: []string{"finalize", "sweep"}},
		{name: "cleanup completes", wantCalls: []string{"finalize", "sweep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			p := newParent()
			now := metav1.Now()
			p.DeletionTimestamp, p.Finalizers = &now, []string{finalizer}
			c := fake.NewClientBuilder().WithObjects(p).WithStatusSubresource(p).Build()
			s := service(c, nil, true)
			var calls []string
			s.Finalize = func(_ context.Context, children client.Client, _ *parent) (ctrl.Result, error) {
				g.Expect(children).To(BeIdenticalTo(c))
				calls = append(calls, "finalize")
				return tc.finalizeResult, tc.finalizeErr
			}
			s.Sweep = func(_ context.Context, children client.Client, _ *parent) (ctrl.Result, error) {
				g.Expect(children).To(BeIdenticalTo(c))
				calls = append(calls, "sweep")
				return tc.sweepResult, tc.sweepErr
			}
			result, err := s.Reconcile(t.Context(), request, &parent{})
			g.Expect(result).To(Equal(cmp.Or(tc.finalizeResult, tc.sweepResult)))
			if want := cmp.Or(tc.finalizeErr, tc.sweepErr); !errors.Is(err, want) {
				t.Fatalf("Reconcile returned %v, want %v", err, want)
			}
			g.Expect(calls).To(Equal(tc.wantCalls))
		})
	}
}
