// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the scaffold every order kind shares: the naming and ownership of
// the children, the admission gates, the pass result, the teardown and the
// label mapping. A KeystoneUser stands in for the order; the scaffold reads
// nothing kind-specific.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// orderFakeClient builds a fake client with the status subresources of the
// order kinds, which the reconcilers write through Status().Update. The K-ORC
// kinds carry no status subresource, so a test seeds and edits their status in
// place.
func orderFakeClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneUser{}, &c5c3v1alpha1.KeystoneProject{},
			&c5c3v1alpha1.KeystoneRoleAssignment{}, &c5c3v1alpha1.KeystoneCatalogEntry{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

// orderReconciler is what the harness drives: any order kind's reconciler.
type orderReconciler interface {
	Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error)
}

// orderHarness is one order reconciler with the clients of the two clusters an
// order's objects are spread over. order is the management client for an order
// on the management cluster.
type orderHarness struct {
	r           orderReconciler
	mgmt, order client.Client
	cluster     string
	key         types.NamespacedName
}

// newOrderHarness seeds the clients and builds the reconciler. For an order on a
// target cluster every object in kuTestNamespace goes to that cluster's client
// and everything else to the management client; for the management cluster
// both are one client.
func newOrderHarness(
	t *testing.T, cluster string, key types.NamespacedName, mgmtFuncs, orderFuncs *interceptor.Funcs,
	objs []client.Object, build func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler,
) *orderHarness {
	t.Helper()
	h := &orderHarness{cluster: cluster, key: key}
	var mgmtObjs, orderObjs []client.Object
	for _, obj := range objs {
		if cluster != c5c3v1alpha1.ManagementCluster && obj.GetNamespace() == kuTestNamespace {
			orderObjs = append(orderObjs, obj)
			continue
		}
		mgmtObjs = append(mgmtObjs, obj)
	}
	h.mgmt = orderFakeClient(t, mgmtFuncs, mgmtObjs...)
	h.order = h.mgmt
	var resolver commonmulticluster.ClusterResolver
	if cluster != c5c3v1alpha1.ManagementCluster {
		h.order = orderFakeClient(t, orderFuncs, orderObjs...)
		resolver = &childrenResolver{children: h.order}
	}
	h.r = build(h.mgmt, resolver)
	return h
}

func (h *orderHarness) reconcile(ctx context.Context) (ctrl.Result, error) {
	return h.r.Reconcile(ctx, mcreconcile.Request{
		Request:     reconcile.Request{NamespacedName: h.key},
		ClusterName: mcruntime.ClusterName(h.cluster),
	})
}

// reloadOrder reloads the harness's order from its cluster, or returns nil once
// it is gone.
func reloadOrder[T any, PT interface {
	*T
	client.Object
}](t *testing.T, h *orderHarness,
) PT {
	t.Helper()
	got := PT(new(T))
	if err := h.order.Get(context.Background(), h.key, got); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("reloading the order: %v", err)
	}
	return got
}

// labelledIn counts the objects of list's kind in the ControlPlane's namespace
// "default" that carry the order name label key names.
func labelledIn(t *testing.T, c client.Client, list client.ObjectList, key, name string) int {
	t.Helper()
	if err := c.List(context.Background(), list, client.InNamespace("default"), client.MatchingLabels{key: name}); err != nil {
		t.Fatalf("listing %T: %v", list, err)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		t.Fatalf("reading %T: %v", list, err)
	}
	return len(items)
}

// testOrderRef is the KeystoneUser fixture order as the scaffold sees it.
func testOrderRef(cluster string) orderRef {
	return orderRef{
		Name: kuTestName, Namespace: kuTestNamespace, Cluster: cluster,
		Segment: "user", Keys: keystoneUserLabelKeys,
	}
}

func TestOrderRef_ChildPrefixMatchesTheKeystoneUserNames(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneUserCR(kuTestNamespace)
	g.Expect(testOrderRef(c5c3v1alpha1.ManagementCluster).childPrefix()).
		To(Equal(keystoneUserChildPrefix(order, c5c3v1alpha1.ManagementCluster)))
	g.Expect(testOrderRef(kuTestCluster).childPrefix()).To(Equal(keystoneUserChildPrefix(order, kuTestCluster)))

	project := testOrderRef("")
	project.Segment = "project"
	g.Expect(project.childPrefix()).To(MatchRegexp(`^workflow-[0-9a-f]{8}-project-$`))
	g.Expect(strings.TrimSuffix(project.childPrefix(), "project-")).
		To(Equal(strings.TrimSuffix(testOrderRef("").childPrefix(), "user-")),
			"the segment is the only difference between two kinds' prefixes of one order identity")
}

func TestOrderRef_ClusterRefAndLocation(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(testOrderRef(c5c3v1alpha1.ManagementCluster).clusterRef()).To(BeNil())
	g.Expect(testOrderRef(c5c3v1alpha1.ManagementCluster).location()).To(Equal("the management cluster"))
	g.Expect(testOrderRef(kuTestCluster).clusterRef().Name).To(Equal(kuTestCluster))
	g.Expect(testOrderRef(kuTestCluster).location()).To(Equal(`target cluster "edge-1"`))
}

func TestOrderOwnership(t *testing.T) {
	o := testOrderRef(kuTestCluster)
	owner := keystoneUserCR(kuTestNamespace)
	user := func(name string, labels map[string]string, owners ...metav1.OwnerReference) *orcv1alpha1.User {
		return &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: labels, OwnerReferences: owners,
		}}
	}
	cases := []struct {
		name       string
		obj        client.Object
		isChild    bool
		ownedChild bool
	}{
		{name: "labelled inside the prefix", obj: user(o.childPrefix()+"user", o.childLabels()), isChild: true, ownedChild: true},
		{name: "labelled outside the prefix", obj: user("someone-elses", o.childLabels()), isChild: true},
		{name: "inside the prefix without labels", obj: user(o.childPrefix()+"user", nil)},
		{name: "labelled for the same order on the management cluster", obj: user(o.childPrefix()+"user",
			testOrderRef(c5c3v1alpha1.ManagementCluster).childLabels())},
		{name: "labelled without the cluster label", obj: user(o.childPrefix()+"user", map[string]string{
			keystoneUserNameLabel: kuTestName, keystoneUserNamespaceLabel: kuTestNamespace,
		})},
		{name: "controlled by the order", obj: user("workflow-credentials", nil, metav1.OwnerReference{
			APIVersion: c5c3v1alpha1.GroupVersion.String(), Kind: "KeystoneUser",
			Name: owner.Name, UID: owner.UID, Controller: ptr.To(true),
		}), isChild: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(isOrderChild(tc.obj, owner, o)).To(Equal(tc.isChild))
			g.Expect(ownsOrderChild(tc.obj, owner, o)).To(Equal(tc.ownedChild))
		})
	}
}

func TestClaimOrderChild_KeepsLabelsAndSetsNoOwner(t *testing.T) {
	g := NewGomegaWithT(t)

	o := testOrderRef(kuTestCluster)
	obj := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "u", Labels: map[string]string{"keep": "me"}}}
	claimOrderChild(obj, o)
	g.Expect(obj.Labels).To(Equal(map[string]string{
		"keep": "me", keystoneUserNameLabel: kuTestName, keystoneUserNamespaceLabel: kuTestNamespace,
		keystoneUserClusterLabel: kuTestCluster,
	}))
	g.Expect(obj.OwnerReferences).To(BeEmpty())

	bare := &orcv1alpha1.User{}
	claimOrderChild(bare, testOrderRef(c5c3v1alpha1.ManagementCluster))
	g.Expect(bare.Labels).To(HaveKeyWithValue(keystoneUserClusterLabel, ""), "the management cluster is the empty value")
}

func TestEnsureOrderChild_RefusesAnObjectTheOrderDidNotCreate(t *testing.T) {
	g := NewGomegaWithT(t)

	o := testOrderRef("")
	owner := keystoneUserCR(kuTestNamespace)
	stranger := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: o.childPrefix() + "user", Namespace: "default"}}
	c := kuFakeClient(t, nil, stranger)

	err := ensureOrderChild(context.Background(), c, c.Scheme(), owner, o,
		&orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: stranger.Name, Namespace: "default"}})
	g.Expect(err).To(MatchError(ContainSubstring("it was not created by this order")))
}

func TestOrderChildToRequests(t *testing.T) {
	keys := orderLabelKeys{Name: "example.io/order-name", Namespace: "example.io/order-namespace", Cluster: "example.io/order-cluster"}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}}
	cases := []struct {
		name   string
		labels map[string]string
		want   []mcreconcile.Request
	}{
		{
			name: "target cluster child",
			labels: map[string]string{
				keys.Name: kuTestName, keys.Namespace: kuTestNamespace, keys.Cluster: kuTestCluster,
			},
			want: []mcreconcile.Request{{Request: request, ClusterName: kuTestCluster}},
		},
		{
			name:   "management cluster child",
			labels: map[string]string{keys.Name: kuTestName, keys.Namespace: kuTestNamespace, keys.Cluster: ""},
			want:   []mcreconcile.Request{{Request: request}},
		},
		{
			name:   "no name label",
			labels: map[string]string{keys.Namespace: kuTestNamespace, keys.Cluster: kuTestCluster},
		},
		{
			name: "another kind's labels",
			labels: map[string]string{
				keystoneUserNameLabel: kuTestName, keystoneUserNamespaceLabel: kuTestNamespace,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			obj := &orcv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Labels: tc.labels}}
			g.Expect(orderChildToRequests(keys)(context.Background(), obj)).To(Equal(tc.want))
		})
	}
}

func TestOrderPassResult(t *testing.T) {
	asked := ctrl.Result{RequeueAfter: 3 * time.Second}
	refresh := ctrl.Result{RequeueAfter: 10 * time.Minute}
	cases := []struct {
		name      string
		cluster   string
		converged bool
		result    ctrl.Result
		want      ctrl.Result
	}{
		{name: "a requeue a leg asked for stands", cluster: kuTestCluster, converged: true, result: asked, want: asked},
		{name: "a converged order on the management cluster waits for an event", converged: true, want: ctrl.Result{}},
		{name: "a converged order on a target cluster refreshes", cluster: kuTestCluster, converged: true, want: refresh},
		{name: "an unconverged order on the management cluster refreshes", want: refresh},
		{name: "an unconverged order on a target cluster refreshes", cluster: kuTestCluster, want: refresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(orderPassResult(tc.cluster, tc.converged, tc.result)).To(Equal(tc.want))
		})
	}
}

func TestOrderAdmission(t *testing.T) {
	ref := c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"}
	boom := errors.New("boom")
	failControlPlaneRead := &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c5c3v1alpha1.ControlPlane); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
	cases := []struct {
		name       string
		cluster    string
		funcs      *interceptor.Funcs
		objs       []client.Object
		wantReason string
		wantResult ctrl.Result
		wantErr    string
	}{
		{
			name: "a cluster name no label value can carry", cluster: strings.Repeat("c", 64),
			objs:       []client.Object{keystoneUserControlPlane(assignOn(kuTestNamespace, strings.Repeat("c", 64)))},
			wantReason: reasonOrderClusterNameTooLong,
		},
		{
			name: "an absent plane on the management cluster", wantReason: reasonKeystoneServiceControlPlaneNotFound,
			wantResult: ctrl.Result{RequeueAfter: korcRequeueAfter},
		},
		{
			name: "an absent plane on a target cluster", cluster: kuTestCluster,
			wantReason: reasonOrderNamespaceNotAssigned, wantResult: ctrl.Result{RequeueAfter: time.Minute},
		},
		{
			name: "a plane without the entry", objs: []client.Object{keystoneUserControlPlane()},
			wantReason: reasonOrderNamespaceNotAssigned, wantResult: ctrl.Result{RequeueAfter: time.Minute},
		},
		{
			name: "a ControlPlane read error", funcs: failControlPlaneRead,
			objs:    []client.Object{keystoneUserControlPlane(assignOn(kuTestNamespace, ""))},
			wantErr: "fetching ControlPlane default/cp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			var reasons []string
			fail := func(reason, _ string) { reasons = append(reasons, reason) }

			cp, entry, result, err := orderAdmission(context.Background(), kuFakeClient(t, tc.funcs, tc.objs...),
				testOrderRef(tc.cluster), ref, fail)

			g.Expect(cp).To(BeNil())
			g.Expect(entry).To(BeNil())
			g.Expect(result).To(Equal(tc.wantResult))
			if tc.wantErr != "" {
				g.Expect(err).To(MatchError(boom))
				g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
				g.Expect(reasons).To(BeEmpty(), "a read error writes no condition")
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(reasons).To(Equal([]string{tc.wantReason}))
		})
	}

	t.Run("an assigned order is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		entry := assignOn(kuTestNamespace, kuTestCluster)
		entry.AllowedRoles = []string{"member"}
		c := kuFakeClient(t, nil, keystoneUserControlPlane(entry))

		cp, got, result, err := orderAdmission(context.Background(), c, testOrderRef(kuTestCluster), ref,
			func(reason, message string) { t.Fatalf("an admitted order writes no %s: %s", reason, message) })

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(client.ObjectKeyFromObject(cp)).To(Equal(client.ObjectKey{Namespace: "default", Name: "cp"}))
		g.Expect(got.AllowedRoles).To(Equal([]string{"member"}))
	})
}

func TestOrderAdminCredentialGate(t *testing.T) {
	g := NewGomegaWithT(t)

	var reason string
	fail := func(r, _ string) { reason = r }
	g.Expect(orderAdminCredentialGate(korcControlPlane(), fail)).To(BeFalse())
	g.Expect(reason).To(Equal(reasonWaitingForServiceAccountAdmin))

	reason = ""
	g.Expect(orderAdminCredentialGate(ksControlPlane(), fail)).To(BeTrue())
	g.Expect(reason).To(BeEmpty())
}

func TestOrderTeardown(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name         string
		withPlane    bool
		remaining    int
		sweepErr     error
		wantResult   ctrl.Result
		wantErr      bool
		wantReleased bool
	}{
		{
			name: "children remain with the plane present", withPlane: true, remaining: 2,
			wantResult: ctrl.Result{RequeueAfter: 10 * time.Second},
		},
		{name: "nothing remains", withPlane: true, wantReleased: true},
		{name: "a sweep error with the plane present", withPlane: true, sweepErr: boom, wantErr: true},
		{name: "a sweep error with the plane gone", sweepErr: boom, wantReleased: true},
		{name: "children remain with the plane gone", remaining: 2, wantReleased: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			order := keystoneUserCR(kuTestNamespace)
			order.DeletionTimestamp = ptr.To(metav1.Now())
			objs := []client.Object{order}
			if tc.withPlane {
				objs = append(objs, keystoneUserControlPlane())
			}
			c := kuFakeClient(t, nil, objs...)
			live := &c5c3v1alpha1.KeystoneUser{}
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(order), live)).To(Succeed())

			var sweptIn string
			result, err := orderTeardown(ctx, c, c, live, keystoneUserFinalizerName,
				client.ObjectKey{Namespace: "default", Name: "cp"},
				func(_ context.Context, childNS string) (int, error) {
					sweptIn = childNS
					return tc.remaining, tc.sweepErr
				})

			g.Expect(sweptIn).To(Equal("default"), "the children are swept in the plane's namespace")
			g.Expect(result).To(Equal(tc.wantResult))
			if tc.wantErr {
				g.Expect(err).To(MatchError(boom))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
			err = c.Get(ctx, client.ObjectKeyFromObject(order), live)
			if tc.wantReleased {
				g.Expect(err).To(HaveOccurred(), "the order is gone once its finalizer is released")
			} else {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(live.Finalizers).To(ContainElement(keystoneUserFinalizerName))
			}
		})
	}

	t.Run("an order without the finalizer is left alone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := keystoneUserCR(kuTestNamespace)
		order.Finalizers = nil
		result, err := orderTeardown(context.Background(), nil, nil, order, keystoneUserFinalizerName,
			client.ObjectKey{}, func(context.Context, string) (int, error) {
				t.Fatal("no sweep runs without the finalizer")
				return 0, nil
			})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
	})
}

func TestSweepOrderList(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	o := testOrderRef("")
	owner := keystoneUserCR(kuTestNamespace)
	labelled := func(name string) *orcv1alpha1.User {
		return &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: o.childLabels()}}
	}
	var deleted []string
	c := kuFakeClient(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, labelled(o.childPrefix()+"a"), labelled(o.childPrefix()+"b"), labelled(o.childPrefix()+"c"),
		labelled("outside-the-prefix"))

	n, err := sweepOrderList(ctx, c, owner, o, "default", &orcv1alpha1.UserList{},
		func(obj client.Object) bool { return obj.GetName() == o.childPrefix()+"c" }, nil)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(n).To(Equal(3))
	g.Expect(deleted).To(Equal([]string{o.childPrefix() + "c", o.childPrefix() + "a", o.childPrefix() + "b"}),
		"the selected item goes first, the rest in listed order, the foreign object never")

	t.Run("a skipped item is neither deleted nor counted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		kept := labelled(o.childPrefix() + "kept")
		c := kuFakeClient(t, nil, labelled(o.childPrefix()+"gone"), kept)

		n, err := sweepOrderList(ctx, c, owner, o, "default", &orcv1alpha1.UserList{}, nil,
			func(obj client.Object) bool { return obj.GetName() == kept.Name })

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n).To(Equal(1))
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(kept), &orcv1alpha1.User{})).To(Succeed())
		g.Expect(labelledIn(t, c, &orcv1alpha1.UserList{}, keystoneUserNameLabel, kuTestName)).To(Equal(1))
	})

	t.Run("a list error is wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		failing := kuFakeClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("boom")
			},
		})
		_, err := sweepOrderList(ctx, failing, owner, o, "default", &orcv1alpha1.UserList{}, nil, nil)
		g.Expect(err).To(MatchError(ContainSubstring("listing order *v1alpha1.UserList")))
	})

	t.Run("an empty list issues nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		n, err := sweepOrderList(ctx, kuFakeClient(t, nil), owner, o, "default", &orcv1alpha1.UserList{}, nil, nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(n).To(BeZero())
	})
}

func TestEnsureOrderSecret_ClaimsAndPreservesTheLiveData(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	o := testOrderRef(kuTestCluster)
	c := kuFakeClient(t, nil)
	generate := func(secret *corev1.Secret) error {
		if len(secret.Data["value"]) == 0 {
			secret.Data["value"] = []byte(time.Now().String())
		}
		return nil
	}

	g.Expect(ensureOrderSecret(ctx, c, o, o.childPrefix()+"secret", "default", generate)).To(Succeed())
	first := &corev1.Secret{}
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: "default", Name: o.childPrefix() + "secret"}, first)).To(Succeed())
	g.Expect(first.Labels).To(Equal(o.childLabels()))
	g.Expect(first.OwnerReferences).To(BeEmpty())

	g.Expect(ensureOrderSecret(ctx, c, o, o.childPrefix()+"secret", "default", generate)).To(Succeed())
	second := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(first), second)).To(Succeed())
	g.Expect(second.Data["value"]).To(Equal(first.Data["value"]), "a generated value survives the next pass")

	boom := errors.New("boom")
	err := ensureOrderSecret(ctx, c, o, o.childPrefix()+"other", "default",
		func(*corev1.Secret) error { return boom })
	g.Expect(err).To(MatchError(boom))
	g.Expect(apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "default", Name: o.childPrefix() + "other"},
		&corev1.Secret{}))).To(BeTrue(), "a failed mutate writes nothing")
}

func TestStampOrderPushAndFresh(t *testing.T) {
	const hashKey, beforeKey = "example.io/push-hash", "example.io/push-synced-before"
	key := types.NamespacedName{Namespace: "default", Name: "backup"}
	ready := []esov1alpha1.PushSecretStatusCondition{{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue}}

	t.Run("a new hash is stamped against the synced version", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		c := kuFakeClient(t, nil, &esov1alpha1.PushSecret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Status:     esov1alpha1.PushSecretStatus{Conditions: ready, SyncedResourceVersion: "1-old"},
		})

		g.Expect(stampOrderPush(ctx, c, key, hashKey, beforeKey, "h1")).To(Succeed())
		ps := &esov1alpha1.PushSecret{}
		g.Expect(c.Get(ctx, key, ps)).To(Succeed())
		g.Expect(ps.Annotations).To(Equal(map[string]string{hashKey: "h1", beforeKey: "1-old"}))
		g.Expect(orderPushFresh(ps, hashKey, beforeKey, "h1")).To(BeFalse(),
			"Ready from the previous push does not prove a push of the new hash")

		ps.Status.SyncedResourceVersion = "2-pushed"
		g.Expect(orderPushFresh(ps, hashKey, beforeKey, "h1")).To(BeTrue())
		g.Expect(orderPushFresh(ps, hashKey, beforeKey, "h2")).To(BeFalse(), "another document is not pushed")

		version := ps.ResourceVersion
		g.Expect(stampOrderPush(ctx, c, key, hashKey, beforeKey, "h1")).To(Succeed())
		g.Expect(c.Get(ctx, key, ps)).To(Succeed())
		g.Expect(ps.ResourceVersion).To(Equal(version), "an unchanged hash writes nothing")
	})

	t.Run("a missing PushSecret is a no-op", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(stampOrderPush(context.Background(), kuFakeClient(t, nil), key, hashKey, beforeKey, "h1")).To(Succeed())
	})

	t.Run("an update error is wrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		c := kuFakeClient(t, &interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return boom },
		}, &esov1alpha1.PushSecret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})

		err := stampOrderPush(context.Background(), c, key, hashKey, beforeKey, "h1")
		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("stamping order PushSecret default/backup"))
	})

	t.Run("a PushSecret that is not Ready is not fresh", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ps := &esov1alpha1.PushSecret{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{hashKey: "h1", beforeKey: "1-old"}},
			Status:     esov1alpha1.PushSecretStatus{SyncedResourceVersion: "2-pushed"},
		}
		g.Expect(orderPushFresh(ps, hashKey, beforeKey, "h1")).To(BeFalse())
	})
}
