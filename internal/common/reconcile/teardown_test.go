// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// sweepKinds is the one child kind the sweeps below list.
var sweepKinds = []schema.GroupVersionKind{corev1.SchemeGroupVersion.WithKind("Secret")}

// sweepFixture is an owner on the management cluster and one child it
// projected onto a target cluster, with the writes each cluster saw.
type sweepFixture struct {
	mgmt     client.Client
	target   client.Client
	owner    *corev1.ConfigMap
	child    *corev1.Secret
	updates  int
	deletes  int
	recorder *record.FakeRecorder
}

// newSweepFixture seeds the owner with finalizers on the management cluster,
// whose Updates run mgmtFuncs, and its labelled child on the target cluster.
// The keep-alive finalizer keeps the fake from deleting the owner once the
// sweep released the remote-children finalizer.
func newSweepFixture(t *testing.T, mgmtFuncs interceptor.Funcs, finalizers ...string) *sweepFixture {
	t.Helper()
	g := gomega.NewWithT(t)
	f := &sweepFixture{recorder: record.NewFakeRecorder(10)}

	seeded := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "keystone", Namespace: "openstack", Finalizers: finalizers,
	}}
	labels, err := commonmulticluster.OwnerLabels(clientgoscheme.Scheme, seeded)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	f.child = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "keystone-credentials", Namespace: "openstack", Labels: labels,
	}}

	next := mgmtFuncs.Update
	mgmtFuncs.Update = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		f.updates++
		if next != nil {
			return next(ctx, c, obj, opts...)
		}
		return c.Update(ctx, obj, opts...)
	}
	f.mgmt = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(seeded).
		WithInterceptorFuncs(mgmtFuncs).Build()
	f.target = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(f.child).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				f.deletes++
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()

	f.owner = &corev1.ConfigMap{}
	g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(seeded), f.owner)).To(gomega.Succeed())
	return f
}

// sweep runs SweepRemoteChildren for the fixture's owner, with the target
// cluster as the children client and no resolver, so the sweep reads through
// the children client.
func (f *sweepFixture) sweep(ctx context.Context) (ctrl.Result, error) {
	return SweepRemoteChildren(ctx, f.mgmt, nil, f.recorder, clientgoscheme.Scheme, f.owner,
		&commonv1.TargetClusterRefSpec{Name: "remote-a"}, commonmulticluster.Remote(f.target), sweepKinds)
}

// ownerFinalizers reads the persisted owner and returns its finalizers.
func (f *sweepFixture) ownerFinalizers(g *gomega.WithT) []string {
	stored := &corev1.ConfigMap{}
	g.Expect(f.mgmt.Get(context.Background(), client.ObjectKeyFromObject(f.owner), stored)).To(gomega.Succeed())
	return stored.Finalizers
}

// childGone reports whether the projected child left the target cluster.
func (f *sweepFixture) childGone(g *gomega.WithT) bool {
	err := f.target.Get(context.Background(), client.ObjectKeyFromObject(f.child), &corev1.Secret{})
	if apierrors.IsNotFound(err) {
		return true
	}
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return false
}

// A CR that keeps its children on the management cluster never carries the
// finalizer, so the sweep touches neither cluster.
func TestSweepRemoteChildrenWithoutTheFinalizerDoesNothing(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newSweepFixture(t, interceptor.Funcs{}, keepFinalizer)

	result, err := f.sweep(context.Background())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.BeZero())
	g.Expect(f.deletes).To(gomega.BeZero())
	g.Expect(f.updates).To(gomega.BeZero())
	g.Expect(f.childGone(g)).To(gomega.BeFalse())
}

func TestSweepRemoteChildrenReleasesTheFinalizer(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newSweepFixture(t, interceptor.Funcs{}, commonmulticluster.RemoteChildrenFinalizer, keepFinalizer)

	result, err := f.sweep(context.Background())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.BeZero())
	g.Expect(f.childGone(g)).To(gomega.BeTrue())
	g.Expect(f.ownerFinalizers(g)).To(gomega.Equal([]string{keepFinalizer}))
}

// A release on a stale copy of the owner requeues without an error and leaves
// the finalizer persisted, so the next pass sweeps again from a fresh read
// instead of counting the conflict as a failed reconcile.
func TestSweepRemoteChildrenConflictRequeues(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newSweepFixture(t, testutil.ConflictFirstUpdate(),
		commonmulticluster.RemoteChildrenFinalizer, keepFinalizer)

	result, err := f.sweep(context.Background())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.Equal(ctrl.Result{RequeueAfter: RequeueNextPass}))
	g.Expect(f.ownerFinalizers(g)).To(gomega.ContainElement(commonmulticluster.RemoteChildrenFinalizer))
}

// An owner an earlier pass already released has nothing left to release, so
// the 404 ends the pass without an error.
func TestSweepRemoteChildrenOwnerGoneEndsThePass(t *testing.T) {
	g := gomega.NewWithT(t)
	f := newSweepFixture(t, testutil.NotFoundOnUpdate(), commonmulticluster.RemoteChildrenFinalizer)

	result, err := f.sweep(context.Background())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.BeZero())
}

func TestSweepRemoteChildrenWrapsAnotherUpdateError(t *testing.T) {
	g := gomega.NewWithT(t)
	injected := errors.New("etcd unavailable")
	f := newSweepFixture(t, interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return injected
		},
	}, commonmulticluster.RemoteChildrenFinalizer)

	result, err := f.sweep(context.Background())

	g.Expect(err).To(gomega.MatchError(injected))
	g.Expect(strings.HasPrefix(err.Error(), "removing remote-children finalizer:")).To(gomega.BeTrue(), err.Error())
	g.Expect(result).To(gomega.BeZero())
}
