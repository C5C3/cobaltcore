// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package satellite

import (
	"context"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// failingResolver stands in for a multicluster manager that cannot hand out the
// cluster a parent names. The testutil resolver always resolves, so the
// unresolvable arm needs its own fake.
type failingResolver struct{ err error }

func (f failingResolver) GetCluster(_ context.Context, _ mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, f.err
}

// A ConfigMap stands in for the parent CR: ResolveParentChildren only ever
// Gets it and asks the caller's closure for its target-cluster ref.
const (
	testGate       = "CredentialsReady"
	testProjection = "ConfigProjected"
	testGeneration = int64(7)
)

var testParentKey = client.ObjectKey{Namespace: "ns", Name: "name"}

func testParent() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: testParentKey.Namespace, Name: testParentKey.Name,
	}}
}

func testResolveParams(cl client.Client, conds *[]metav1.Condition, ref *commonv1.TargetClusterRefSpec) ResolveParams {
	return ResolveParams{
		Client:                  cl,
		Parent:                  &corev1.ConfigMap{},
		ParentKey:               testParentKey,
		ParentKind:              "Kind",
		TargetClusterRef:        func() *commonv1.TargetClusterRefSpec { return ref },
		Conditions:              conds,
		Generation:              testGeneration,
		GateConditionType:       testGate,
		WaitingForParentReason:  "WaitingForParent",
		WaitingForParentMessage: "parent Kind ns/name does not exist",
		RequeueAfter:            commonreconcile.RequeueSecretPolling,
	}
}

func TestResolveParentChildren(t *testing.T) {
	t.Run("a non-NotFound parent read fails the pass and writes no condition", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().
			WithScheme(clientgoscheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					return errors.New("boom")
				},
			}).
			Build()
		var conds []metav1.Condition

		children, result, err := ResolveParentChildren(context.Background(), testResolveParams(cl, &conds, nil))

		g.Expect(children).To(gomega.BeNil())
		g.Expect(result).To(gomega.Equal(ctrl.Result{}))
		g.Expect(err).To(gomega.MatchError("fetching parent Kind ns/name: boom"))
		g.Expect(conds).To(gomega.BeEmpty(), "an infrastructure failure is not a CR-level state")
	})

	t.Run("a missing parent gates and requeues", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		var conds []metav1.Condition

		children, result, err := ResolveParentChildren(context.Background(), testResolveParams(cl, &conds, nil))

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(children).To(gomega.BeNil())
		g.Expect(result.RequeueAfter).To(gomega.Equal(commonreconcile.RequeueSecretPolling))

		gate := conditions.GetCondition(conds, testGate)
		g.Expect(gate).NotTo(gomega.BeNil())
		g.Expect(gate.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(gate.Reason).To(gomega.Equal("WaitingForParent"))
		g.Expect(gate.Message).To(gomega.Equal("parent Kind ns/name does not exist"))
		g.Expect(gate.ObservedGeneration).To(gomega.Equal(testGeneration))
	})

	t.Run("a missing parent demotes a standing projection claim", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		conds := []metav1.Condition{{
			Type: testProjection, Status: metav1.ConditionTrue,
			Reason: "Projected", Message: "the section is mounted",
		}}

		p := testResolveParams(cl, &conds, nil)
		p.ProjectionConditionType = testProjection
		p.ProjectionDemotionReason = "WaitingForProjection"
		p.ProjectionDemotionMessage = "nothing mounts this satellite's section"
		_, _, err := ResolveParentChildren(context.Background(), p)

		g.Expect(err).NotTo(gomega.HaveOccurred())
		projected := conditions.GetCondition(conds, testProjection)
		g.Expect(projected).NotTo(gomega.BeNil())
		g.Expect(projected.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(projected.Reason).To(gomega.Equal("WaitingForProjection"))
		g.Expect(projected.Message).To(gomega.Equal("nothing mounts this satellite's section"))
		g.Expect(projected.ObservedGeneration).To(gomega.Equal(testGeneration))
	})

	t.Run("a missing parent leaves an absent projection condition absent", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		var conds []metav1.Condition

		p := testResolveParams(cl, &conds, nil)
		p.ProjectionConditionType = testProjection
		p.ProjectionDemotionReason = "WaitingForProjection"
		p.ProjectionDemotionMessage = "nothing mounts this satellite's section"
		_, _, err := ResolveParentChildren(context.Background(), p)

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(conditions.GetCondition(conds, testProjection)).To(gomega.BeNil(),
			"a satellite that never converged keeps the condition absent")
	})

	t.Run("a missing parent leaves an already False projection condition untouched", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		conds := []metav1.Condition{{
			Type: testProjection, Status: metav1.ConditionFalse,
			Reason: "RenderFailed", Message: "the credentials Secret is missing",
		}}

		p := testResolveParams(cl, &conds, nil)
		p.ProjectionConditionType = testProjection
		p.ProjectionDemotionReason = "WaitingForProjection"
		p.ProjectionDemotionMessage = "nothing mounts this satellite's section"
		_, _, err := ResolveParentChildren(context.Background(), p)

		g.Expect(err).NotTo(gomega.HaveOccurred())
		projected := conditions.GetCondition(conds, testProjection)
		g.Expect(projected.Reason).To(gomega.Equal("RenderFailed"))
		g.Expect(projected.Message).To(gomega.Equal("the credentials Secret is missing"))
	})

	t.Run("an opted-out caller keeps a True projection condition", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		conds := []metav1.Condition{{
			Type: testProjection, Status: metav1.ConditionTrue,
			Reason: "Projected", Message: "the section is mounted",
		}}

		_, _, err := ResolveParentChildren(context.Background(), testResolveParams(cl, &conds, nil))

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(conditions.GetCondition(conds, testProjection).Status).To(gomega.Equal(metav1.ConditionTrue))
	})

	t.Run("an unresolvable target gates on the shared reason", func(t *testing.T) {
		g := gomega.NewWithT(t)
		cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(testParent()).Build()
		var conds []metav1.Condition

		p := testResolveParams(cl, &conds, &commonv1.TargetClusterRefSpec{Name: "remote-a"})
		p.Resolver = failingResolver{err: mcruntime.ErrClusterNotFound}
		children, result, err := ResolveParentChildren(context.Background(), p)

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(children).To(gomega.BeNil())
		g.Expect(result.RequeueAfter).To(gomega.Equal(commonreconcile.RequeueSecretPolling))

		gate := conditions.GetCondition(conds, testGate)
		g.Expect(gate).NotTo(gomega.BeNil())
		g.Expect(gate.Status).To(gomega.Equal(metav1.ConditionFalse))
		g.Expect(gate.Reason).To(gomega.Equal(multicluster.TargetClusterUnavailable))
		g.Expect(gate.Message).To(gomega.Equal(mcruntime.ErrClusterNotFound.Error()))
	})

	t.Run("no resolver selects the local client", func(t *testing.T) {
		g := gomega.NewWithT(t)
		local := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(testParent()).Build()
		var conds []metav1.Condition

		children, result, err := ResolveParentChildren(context.Background(),
			testResolveParams(local, &conds, &commonv1.TargetClusterRefSpec{Name: "remote-a"}))

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(children).To(gomega.BeIdenticalTo(local))
		g.Expect(result.IsZero()).To(gomega.BeTrue())
		g.Expect(conds).To(gomega.BeEmpty())
	})

	t.Run("no target ref selects the local client", func(t *testing.T) {
		g := gomega.NewWithT(t)
		local := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(testParent()).Build()
		var conds []metav1.Condition

		p := testResolveParams(local, &conds, nil)
		p.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: local})
		children, result, err := ResolveParentChildren(context.Background(), p)

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(children).To(gomega.BeIdenticalTo(local))
		g.Expect(result.IsZero()).To(gomega.BeTrue())
		g.Expect(conds).To(gomega.BeEmpty())
	})

	t.Run("a resolvable target selects the remote client", func(t *testing.T) {
		g := gomega.NewWithT(t)
		local := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(testParent()).Build()
		onlyRemote := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "only-remote"}}
		remote := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(onlyRemote).Build()
		var conds []metav1.Condition

		p := testResolveParams(local, &conds, &commonv1.TargetClusterRefSpec{Name: "remote-a"})
		p.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{Client: remote})
		children, result, err := ResolveParentChildren(context.Background(), p)

		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(children).NotTo(gomega.BeNil())
		g.Expect(result.IsZero()).To(gomega.BeTrue())
		g.Expect(conds).To(gomega.BeEmpty())

		var got corev1.ConfigMap
		g.Expect(children.Get(context.Background(), client.ObjectKeyFromObject(onlyRemote), &got)).To(gomega.Succeed(),
			"the children client reads the target cluster, not the management one")
		g.Expect(local.Get(context.Background(), client.ObjectKeyFromObject(onlyRemote), &got)).NotTo(gomega.Succeed())
	})
}
