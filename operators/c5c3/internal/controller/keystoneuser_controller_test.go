// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneUser reconciler's lifecycle: the consent gate and the
// freeze, the shared gates, the naming contract, the watch mappers and the
// teardown.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	"github.com/go-logr/logr/funcr"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The fixtures share one order identity, in a namespace apart from the
// ControlPlane's "default", so a test can tell the order's cluster from the
// management cluster by namespace alone.
const (
	kuTestName      = "workflow"
	kuTestNamespace = "tenant-a"
	kuTestCluster   = "edge-1"
)

// keystoneUserCR returns the order "workflow" in namespace, referencing the
// ControlPlane cp in "default" and already carrying the teardown finalizer, so
// a reconcile runs the gates instead of returning on the finalizer pass.
func keystoneUserCR(namespace string) *c5c3v1alpha1.KeystoneUser {
	return &c5c3v1alpha1.KeystoneUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:       kuTestName,
			Namespace:  namespace,
			Generation: 1,
			UID:        types.UID("ku-uid"),
			Finalizers: []string{keystoneUserFinalizerName},
		},
		Spec: c5c3v1alpha1.KeystoneUserSpec{
			ControlPlaneRef:    c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
			PasswordGeneration: 1,
		},
	}
}

// keystoneUserControlPlane returns a ControlPlane past the admin-credential gate
// carrying the given namespace assignments.
func keystoneUserControlPlane(entries ...c5c3v1alpha1.NamespaceAssignmentSpec) *c5c3v1alpha1.ControlPlane {
	cp := ksControlPlane()
	cp.Spec.NamespaceAssignments = entries
	return cp
}

// assignOn returns the assignment of namespace on cluster
// (c5c3v1alpha1.ManagementCluster for the management cluster).
func assignOn(namespace, cluster string) c5c3v1alpha1.NamespaceAssignmentSpec {
	entry := c5c3v1alpha1.NamespaceAssignmentSpec{Namespace: namespace}
	if cluster != c5c3v1alpha1.ManagementCluster {
		entry.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: cluster}
	}
	return entry
}

// kuHarness is one reconciler with the clients of the two clusters an order's
// objects are spread over. order is the management client for an order on the
// management cluster.
type kuHarness struct {
	r       *KeystoneUserReconciler
	mgmt    client.Client
	order   client.Client
	cluster string
	key     types.NamespacedName
}

// kuFakeClient builds a fake client with the status subresources the reconciler
// and the tests write.
func kuFakeClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneUser{}, &orcv1alpha1.User{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

// newKUHarness seeds the clients and the reconciler. For an order on a target
// cluster every object in kuTestNamespace (the order, its delivered Secret) goes
// to that cluster's client and everything else to the management client; for the
// management cluster both are one client. The first KeystoneUser in objs keys
// the requests.
func newKUHarness(t *testing.T, cluster string, mgmtFuncs, orderFuncs *interceptor.Funcs, objs ...client.Object) *kuHarness {
	t.Helper()
	h := &kuHarness{cluster: cluster}
	var mgmtObjs, orderObjs []client.Object
	for _, obj := range objs {
		if order, ok := obj.(*c5c3v1alpha1.KeystoneUser); ok && h.key.Name == "" {
			h.key = client.ObjectKeyFromObject(order)
		}
		if cluster != c5c3v1alpha1.ManagementCluster && obj.GetNamespace() == kuTestNamespace {
			orderObjs = append(orderObjs, obj)
			continue
		}
		mgmtObjs = append(mgmtObjs, obj)
	}
	if h.key.Name == "" {
		h.key = types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}
	}
	h.mgmt = kuFakeClient(t, mgmtFuncs, mgmtObjs...)
	h.r = &KeystoneUserReconciler{Client: h.mgmt, Scheme: h.mgmt.Scheme()}
	h.order = h.mgmt
	if cluster != c5c3v1alpha1.ManagementCluster {
		h.order = kuFakeClient(t, orderFuncs, orderObjs...)
		h.r.Resolver = &childrenResolver{children: h.order}
	}
	return h
}

func (h *kuHarness) reconcile(ctx context.Context) (ctrl.Result, error) {
	return h.r.Reconcile(ctx, mcreconcile.Request{
		Request:     reconcile.Request{NamespacedName: h.key},
		ClusterName: mcruntime.ClusterName(h.cluster),
	})
}

// get reloads the order from its cluster, or returns nil once it is gone.
func (h *kuHarness) get(t *testing.T) *c5c3v1alpha1.KeystoneUser {
	t.Helper()
	got := &c5c3v1alpha1.KeystoneUser{}
	if err := h.order.Get(context.Background(), h.key, got); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("reloading the KeystoneUser: %v", err)
	}
	return got
}

// reconcileKeystoneUser runs one Reconcile for an order on cluster and returns
// the reloaded order, the harness, and the raw result and error.
func reconcileKeystoneUser(
	t *testing.T, cluster string, objs ...client.Object,
) (*c5c3v1alpha1.KeystoneUser, *kuHarness, ctrl.Result, error) {
	t.Helper()
	h := newKUHarness(t, cluster, nil, nil, objs...)
	result, err := h.reconcile(context.Background())
	return h.get(t), h, result, err
}

func kuCondition(order *c5c3v1alpha1.KeystoneUser, condType string) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, condType)
}

// expectBothConditions asserts both sub-conditions read False with reason.
func expectBothConditions(t *testing.T, order *c5c3v1alpha1.KeystoneUser, reason string) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, condType := range keystoneUserSubConditionTypes {
		cond := kuCondition(order, condType)
		g.Expect(cond).NotTo(BeNil(), condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), condType)
		g.Expect(cond.Reason).To(Equal(reason), condType)
	}
}

// kuLabelled counts the objects of the three child kinds in the ControlPlane's
// namespace that carry the order's name label.
func kuLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	g := NewGomegaWithT(t)
	selector := client.MatchingLabels{keystoneUserNameLabel: kuTestName}
	var users orcv1alpha1.UserList
	g.Expect(c.List(context.Background(), &users, client.InNamespace("default"), selector)).To(Succeed())
	var secrets corev1.SecretList
	g.Expect(c.List(context.Background(), &secrets, client.InNamespace("default"), selector)).To(Succeed())
	var pushSecrets esov1alpha1.PushSecretList
	g.Expect(c.List(context.Background(), &pushSecrets, client.InNamespace("default"), selector)).To(Succeed())
	return len(users.Items) + len(secrets.Items) + len(pushSecrets.Items)
}

// kuSeededPushAnnotations is what the order's PushSecret carries once ESO has
// pushed the document the seeded password renders: its content hash, and the
// syncedResourceVersion recorded when that hash was stamped, which the seeded
// status has moved past.
func kuSeededPushAnnotations(order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane, cluster string) map[string]string {
	sum := sha256.Sum256([]byte(buildUserCloudsYAML(cp, keystoneUserName(order), adminDomainName(cp),
		"seeded-password", keystoneUserClusterRef(cluster))))
	return map[string]string{
		keystoneUserPushContentHashAnnotation:  hex.EncodeToString(sum[:]),
		keystoneUserPushSyncedBeforeAnnotation: "1-before-push",
	}
}

// kuConvergedChildren seeds what an order on cluster has in the ControlPlane's
// namespace once K-ORC and ESO have converged on generation gen: the managed
// User with gen applied, its password Secret, the PushSecret ESO has pushed the
// seeded document with, and the ready store the provision gates on.
func kuConvergedChildren(order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane, cluster string, gen int64) []client.Object {
	labels := keystoneUserChildLabels(order, cluster)
	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneUserUserRef(order, cluster),
			Namespace: cp.Namespace,
			Labels:    labels,
			// The generation stamp reads a live object's creation timestamp, which
			// the fake client does not set.
			CreationTimestamp: metav1.Now(),
		},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			Resource: &orcv1alpha1.UserResourceSpec{
				PasswordRef: ptr.To(orcv1alpha1.KubernetesNameRef(keystoneUserPasswordSecretName(order, cluster, gen))),
			},
		},
		Status: orcv1alpha1.UserStatus{
			Conditions: availableImportConditions(),
			ID:         ptr.To("ku-user-id"),
			Resource:   &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: keystoneUserPasswordSecretName(order, cluster, gen)},
		},
	}
	pw := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneUserPasswordSecretName(order, cluster, gen),
			Namespace: cp.Namespace,
			Labels:    labels,
		},
		Data: map[string][]byte{serviceAccountPasswordKey: []byte("seeded-password")},
	}
	push := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystoneUserPushSecretName(order, cluster), Namespace: cp.Namespace, Labels: labels,
			Annotations: kuSeededPushAnnotations(order, cp, cluster),
		},
		Status: esov1alpha1.PushSecretStatus{
			Conditions:            []esov1alpha1.PushSecretStatusCondition{{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue}},
			SyncedResourceVersion: "1-pushed",
		},
	}
	return []client.Object{user, pw, push, readyTenantStoreFor(cp)}
}

// --- lifecycle ---

func TestKeystoneUser_MissingOrderIsNotAnError(t *testing.T) {
	g := NewGomegaWithT(t)

	got, _, result, err := reconcileKeystoneUser(t, c5c3v1alpha1.ManagementCluster)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(got).To(BeNil())
}

func TestKeystoneUser_InstallsTheFinalizerOnceAssigned(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneUserCR(kuTestNamespace)
	order.Finalizers = nil

	got, h, result, err := reconcileKeystoneUser(t, c5c3v1alpha1.ManagementCluster,
		order, keystoneUserControlPlane(assignOn(kuTestNamespace, c5c3v1alpha1.ManagementCluster)))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).NotTo(BeZero(), "the finalizer pass requeues")
	g.Expect(got.Finalizers).To(ContainElement(keystoneUserFinalizerName))
	for _, condType := range keystoneUserSubConditionTypes {
		g.Expect(kuCondition(got, condType)).To(BeNil(), "the finalizer pass writes no %s", condType)
	}
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero())
}

// TestKeystoneUser_NoFinalizerOnAnOrderThatIsNotServed covers the orders the
// gates stop before the finalizer: nothing is created for them, so nothing may
// hold their deletion.
func TestKeystoneUser_NoFinalizerOnAnOrderThatIsNotServed(t *testing.T) {
	cases := []struct {
		name   string
		objs   func() []client.Object
		reason string
	}{
		{
			name:   "the namespace is not assigned",
			objs:   func() []client.Object { return []client.Object{keystoneUserControlPlane()} },
			reason: reasonOrderNamespaceNotAssigned,
		},
		{
			name:   "the ControlPlane does not exist",
			objs:   func() []client.Object { return nil },
			reason: reasonKeystoneServiceControlPlaneNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := keystoneUserCR(kuTestNamespace)
			order.Finalizers = nil

			got, _, _, err := reconcileKeystoneUser(t, c5c3v1alpha1.ManagementCluster,
				append([]client.Object{order}, tc.objs()...)...)

			g.Expect(err).NotTo(HaveOccurred())
			expectBothConditions(t, got, tc.reason)
			g.Expect(got.Finalizers).To(BeEmpty())
		})
	}
}

// TestKeystoneUser_OverlongClusterNameIsRefused covers a target cluster
// registered under a name no label value can carry: the order is refused before
// anything is read or written, and no requeue repeats it.
func TestKeystoneUser_OverlongClusterNameIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)

	cluster := strings.Repeat("c", 64)
	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, cluster))
	order := keystoneUserCR(kuTestNamespace)
	order.Finalizers = nil

	got, h, result, err := reconcileKeystoneUser(t, cluster, order, cp, readyTenantStoreFor(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	expectBothConditions(t, got, reasonOrderClusterNameTooLong)
	g.Expect(kuCondition(got, conditionTypeKeystoneUserUserReady).Message).To(ContainSubstring("63 characters"))
	g.Expect(got.Finalizers).To(BeEmpty())
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero())
}

// --- consent and freeze ---

// TestKeystoneUser_NamespaceNotAssigned covers every way an order meets no
// assignment entry. allowedNamespaces and the ControlPlane's own namespace admit
// KeystoneService registrations and admit no order; a management-cluster entry
// does not cover the same namespace on a target cluster.
func TestKeystoneUser_NamespaceNotAssigned(t *testing.T) {
	cases := []struct {
		name      string
		cluster   string
		namespace string
		cp        func() *c5c3v1alpha1.ControlPlane
		location  string
	}{
		{
			name: "no entry", namespace: kuTestNamespace,
			cp:       func() *c5c3v1alpha1.ControlPlane { return keystoneUserControlPlane() },
			location: "the management cluster",
		},
		{
			name: "allowedNamespaces lists the namespace", namespace: kuTestNamespace,
			cp: func() *c5c3v1alpha1.ControlPlane {
				cp := keystoneUserControlPlane(assignOn("tenant-b", c5c3v1alpha1.ManagementCluster))
				cp.Spec.KORC.ServiceRegistrations = &c5c3v1alpha1.ServiceRegistrationsSpec{
					AllowedNamespaces: []string{kuTestNamespace},
				}
				return cp
			},
			location: "the management cluster",
		},
		{
			name: "the ControlPlane's own namespace", namespace: "default",
			cp:       func() *c5c3v1alpha1.ControlPlane { return keystoneUserControlPlane() },
			location: "the management cluster",
		},
		{
			name: "a management entry on a target cluster", cluster: kuTestCluster, namespace: kuTestNamespace,
			cp: func() *c5c3v1alpha1.ControlPlane {
				return keystoneUserControlPlane(assignOn(kuTestNamespace, c5c3v1alpha1.ManagementCluster))
			},
			location: `target cluster "edge-1"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := tc.cp()
			order := keystoneUserCR(tc.namespace)

			got, h, result, err := reconcileKeystoneUser(t, tc.cluster, order, cp, readyTenantStoreFor(cp))

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(namespaceAssignmentRequeueAfter))
			expectBothConditions(t, got, reasonOrderNamespaceNotAssigned)
			message := kuCondition(got, conditionTypeKeystoneUserUserReady).Message
			g.Expect(message).To(ContainSubstring("spec.namespaceAssignments"))
			g.Expect(message).To(ContainSubstring(tc.location))
			g.Expect(message).To(ContainSubstring(tc.namespace))
			g.Expect(kuLabelled(t, h.mgmt)).To(BeZero(), "an unassigned order projects nothing")
		})
	}
}

func TestKeystoneUser_TargetClusterEntryAdmitsTheOrder(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster))
	order := keystoneUserCR(kuTestNamespace)

	got, h, _, err := reconcileKeystoneUser(t, kuTestCluster, order, cp, readyTenantStoreFor(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kuCondition(got, conditionTypeKeystoneUserUserReady).Reason).To(Equal(reasonProbingForCollision))
	probe := &orcv1alpha1.User{}
	g.Expect(h.mgmt.Get(context.Background(), types.NamespacedName{
		Namespace: cp.Namespace, Name: keystoneUserUserProbeRef(order, kuTestCluster),
	}, probe)).To(Succeed(), "an assigned order probes for its user on the management cluster")
	g.Expect(probe.Labels).To(HaveKeyWithValue(keystoneUserClusterLabel, kuTestCluster))
}

// TestKeystoneUser_WithdrawnAssignmentFreezes pins #1327 D2 for orders: a
// withdrawn entry refuses the order and leaves everything it created in place,
// and the delivered Secret is not repaired while the order is frozen.
func TestKeystoneUser_WithdrawnAssignmentFreezes(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, c5c3v1alpha1.ManagementCluster))
	order := keystoneUserCR(kuTestNamespace)
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, cp}, kuConvergedChildren(order, cp, c5c3v1alpha1.ManagementCluster, 1)...)...)

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(conditions.AllTrue(h.get(t).Status.Conditions, "Ready")).To(BeTrue(), "the order converges first")

	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(cp), live)).To(Succeed())
	live.Spec.NamespaceAssignments = nil
	g.Expect(h.mgmt.Update(ctx, live)).To(Succeed())
	secretKey := types.NamespacedName{Namespace: kuTestNamespace, Name: "workflow-credentials"}
	delivered := &corev1.Secret{}
	g.Expect(h.mgmt.Get(ctx, secretKey, delivered)).To(Succeed())
	delivered.Data[serviceAccountPasswordKey] = []byte("foo")
	g.Expect(h.mgmt.Update(ctx, delivered)).To(Succeed())

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(namespaceAssignmentRequeueAfter))
	got := h.get(t)
	expectBothConditions(t, got, reasonOrderNamespaceNotAssigned)
	g.Expect(got.Status.SecretName).To(Equal("workflow-credentials"), "the frozen order keeps its status")

	for _, obj := range []client.Object{
		&orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: keystoneUserUserRef(order, ""), Namespace: "default"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: keystoneUserPasswordSecretName(order, "", 1), Namespace: "default"}},
		&esov1alpha1.PushSecret{ObjectMeta: metav1.ObjectMeta{Name: keystoneUserPushSecretName(order, ""), Namespace: "default"}},
	} {
		g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed(),
			"a frozen order revokes nothing: %T %s stays", obj, obj.GetName())
	}
	g.Expect(h.mgmt.Get(ctx, secretKey, delivered)).To(Succeed())
	g.Expect(string(delivered.Data[serviceAccountPasswordKey])).To(Equal("foo"),
		"a frozen order's Secret is not repaired")
}

// --- shared gates ---

func TestKeystoneUser_ControlPlaneNotFound(t *testing.T) {
	g := NewGomegaWithT(t)

	got, h, result, err := reconcileKeystoneUser(t, c5c3v1alpha1.ManagementCluster, keystoneUserCR(kuTestNamespace))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	expectBothConditions(t, got, reasonKeystoneServiceControlPlaneNotFound)
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero())
}

// TestKeystoneUser_MissingControlPlaneOnATargetReadsAsNotAssigned pins that an
// owner on a target cluster cannot tell a missing ControlPlane from one that
// does not assign the namespace: both write the same reason and message.
func TestKeystoneUser_MissingControlPlaneOnATargetReadsAsNotAssigned(t *testing.T) {
	g := NewGomegaWithT(t)

	missing, _, missingResult, err := reconcileKeystoneUser(t, kuTestCluster, keystoneUserCR(kuTestNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	cp := keystoneUserControlPlane()
	unassigned, _, unassignedResult, err := reconcileKeystoneUser(t, kuTestCluster, keystoneUserCR(kuTestNamespace), cp)
	g.Expect(err).NotTo(HaveOccurred())

	expectBothConditions(t, missing, reasonOrderNamespaceNotAssigned)
	for _, condType := range keystoneUserSubConditionTypes {
		g.Expect(kuCondition(missing, condType).Message).To(Equal(kuCondition(unassigned, condType).Message), condType)
	}
	g.Expect(missingResult).To(Equal(unassignedResult))
}

func TestKeystoneUser_WaitsForAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcControlPlane()
	cp.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{
		assignOn(kuTestNamespace, c5c3v1alpha1.ManagementCluster),
	}

	got, h, result, err := reconcileKeystoneUser(t, c5c3v1alpha1.ManagementCluster, keystoneUserCR(kuTestNamespace), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	expectBothConditions(t, got, reasonWaitingForServiceAccountAdmin)
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneUser_ControlPlaneReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)

	boom := errors.New("boom")
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c5c3v1alpha1.ControlPlane); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, nil, keystoneUserCR(kuTestNamespace), keystoneUserControlPlane(assignOn(kuTestNamespace, "")))

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	g.Expect(err.Error()).To(ContainSubstring("fetching ControlPlane"))
	got := h.get(t)
	for _, condType := range keystoneUserSubConditionTypes {
		g.Expect(kuCondition(got, condType)).To(BeNil(), "a read error writes no %s", condType)
	}
}

// TestKeystoneUser_UnresolvableClusterIsSkipped covers a request whose cluster
// was deregistered between the event and the pass: nothing can be read there,
// so the pass logs the cluster and ends without an error or a requeue.
func TestKeystoneUser_UnresolvableClusterIsSkipped(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, kuTestCluster))
	h := newKUHarness(t, kuTestCluster, nil, nil, keystoneUserCR(kuTestNamespace), cp, readyTenantStoreFor(cp))
	h.r.Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

	var logs []string
	logger := funcr.New(func(prefix, args string) { logs = append(logs, prefix+" "+args) }, funcr.Options{})
	result, err := h.reconcile(log.IntoContext(context.Background(), logger))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(strings.Join(logs, "\n")).To(ContainSubstring(`"cluster"="edge-1"`))
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero(), "nothing is written for an unresolvable cluster")
	g.Expect(h.get(t).Status.Conditions).To(BeEmpty())
}

// TestKeystoneUser_PushSecretReadErrorIsReported fails the read-back of the
// PushSecret only, after the order's push-hash annotation is on it, so the
// pre-apply check and the re-push nudge read it unharmed. The PushSecret is
// seeded without the stamp, so the nudge is what writes it.
func TestKeystoneUser_PushSecretReadErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, ""))
	order := keystoneUserCR(kuTestNamespace)
	children := kuConvergedChildren(order, cp, "", 1)
	for _, obj := range children {
		if _, ok := obj.(*esov1alpha1.PushSecret); ok {
			obj.SetAnnotations(nil)
		}
	}
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if ps, ok := obj.(*esov1alpha1.PushSecret); ok && ps.Annotations[keystoneUserPushContentHashAnnotation] != "" {
				return errors.New("boom")
			}
			return nil
		},
	}, nil, append([]client.Object{order, cp}, children...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("reading order PushSecret"))
	g.Expect(kuCondition(h.get(t), conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonKeystoneUserDeliveryError))
}

// --- naming ---

func TestKeystoneUser_ChildNames(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneUserCR(kuTestNamespace)
	cp := ksControlPlane()

	local := keystoneUserChildPrefix(order, c5c3v1alpha1.ManagementCluster)
	remote := keystoneUserChildPrefix(order, kuTestCluster)
	g.Expect(local).To(MatchRegexp(`^workflow-[0-9a-f]{8}-user-$`))
	g.Expect(remote).To(MatchRegexp(`^workflow-[0-9a-f]{8}-user-$`))
	g.Expect(remote).NotTo(Equal(local), "the same order on another cluster is another order")
	g.Expect(keystoneUserChildPrefix(keystoneUserCR("tenant-b"), "")).NotTo(Equal(local),
		"the same name in another namespace is another order")

	g.Expect(keystoneUserUserRef(order, "")).To(Equal(local + "user"))
	g.Expect(keystoneUserPasswordSecretName(order, "", 3)).To(Equal(local + "password-v3"))
	g.Expect(keystoneUserCredentialsSecretName(order)).To(Equal("workflow-credentials"))
	g.Expect(keystoneUserRemoteKeyFor(cp, local)).To(Equal(
		"openstack/keystone/default/" + strings.TrimSuffix(local, "-") + "/service-accounts/credentials"))

	g.Expect(keystoneUserName(order)).To(Equal(kuTestName), "an empty userName means metadata.name")
	order.Spec.UserName = "svc-user"
	g.Expect(keystoneUserName(order)).To(Equal("svc-user"))
	order.Spec.PasswordGeneration = 0
	g.Expect(keystoneUserPasswordGeneration(order)).To(Equal(int64(1)), "a stored zero reads as 1")
}

// TestKeystoneUser_ChildNamesFitTheNameBudget pins the 63-byte metadata.name
// rule against the longest child name: a password Secret at the largest
// generation an int64 carries stays inside the 253-byte object name limit.
func TestKeystoneUser_ChildNamesFitTheNameBudget(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneUserCR(kuTestNamespace)
	order.Name = strings.Repeat("a", 63)

	longest := keystoneUserPasswordSecretName(order, kuTestCluster, 1<<62)
	g.Expect(len(longest)).To(BeNumerically("<=", 253))
}

// --- watch mappers ---

func TestKeystoneUserChildToRequests(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   []mcreconcile.Request
	}{
		{
			name: "target cluster child",
			labels: map[string]string{
				keystoneUserNameLabel: kuTestName, keystoneUserNamespaceLabel: kuTestNamespace,
				keystoneUserClusterLabel: kuTestCluster,
			},
			want: []mcreconcile.Request{{
				Request:     reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}},
				ClusterName: kuTestCluster,
			}},
		},
		{
			name: "management cluster child",
			labels: map[string]string{
				keystoneUserNameLabel: kuTestName, keystoneUserNamespaceLabel: kuTestNamespace,
				keystoneUserClusterLabel: "",
			},
			want: []mcreconcile.Request{{
				Request: reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}},
			}},
		},
		{
			name: "no name label",
			labels: map[string]string{
				keystoneUserNamespaceLabel: kuTestNamespace, keystoneUserClusterLabel: kuTestCluster,
			},
		},
		{name: "no labels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			user := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default", Labels: tc.labels}}
			g.Expect(orderChildToRequests(keystoneUserLabelKeys)(context.Background(), user)).To(Equal(tc.want))
		})
	}
}

func newKeystoneUserMapperClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.KeystoneUser{}, KeystoneUserControlPlaneRefIndexKey, keystoneUserControlPlaneRefExtractor)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func TestControlPlaneToKeystoneUsersMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := keystoneUserCR(kuTestNamespace)
	defaulted := keystoneUserCR("default")
	defaulted.Name = "same-namespace"
	defaulted.Spec.ControlPlaneRef.Namespace = ""
	elsewhere := keystoneUserCR(kuTestNamespace)
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef = c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "other"}

	c := newKeystoneUserMapperClient(t, nil, explicit, defaulted, elsewhere)
	mapper := controlPlaneToKeystoneUsersMapper(c)

	got := mapper(context.Background(), ksControlPlane())
	g.Expect(got).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(explicit)},
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(defaulted)},
	))

	unrelated := ksControlPlane()
	unrelated.Name = "another"
	g.Expect(mapper(context.Background(), unrelated)).To(BeEmpty())
}

func TestControlPlaneToKeystoneUsersMapper_ReturnsNilOnListError(t *testing.T) {
	g := NewGomegaWithT(t)

	c := newKeystoneUserMapperClient(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}, keystoneUserCR(kuTestNamespace))

	g.Expect(controlPlaneToKeystoneUsersMapper(c)(context.Background(), ksControlPlane())).To(BeNil())
}

// TestOrderControlPlanePredicate pins which ControlPlane updates reach the
// orders: what they read, and none of the plane's other status writes.
func TestOrderControlPlanePredicate(t *testing.T) {
	base := ksControlPlane()
	base.Generation = 3
	cases := []struct {
		name   string
		mutate func(cp *c5c3v1alpha1.ControlPlane)
		want   bool
	}{
		{name: "a status write that changes nothing an order reads", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Status.ObservedGeneration = 3
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type: "KeystoneReady", Status: metav1.ConditionFalse, Reason: "Rolling",
			})
		}, want: false},
		{name: "a spec change", mutate: func(cp *c5c3v1alpha1.ControlPlane) { cp.Generation = 4 }, want: true},
		{name: "AdminCredentialReady flips", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type: conditionTypeAdminCredentialReady, Status: metav1.ConditionFalse, Reason: "Rotating",
			})
		}, want: true},
		{name: "a deletion", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.DeletionTimestamp = ptr.To(metav1.Now())
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			updated := base.DeepCopy()
			tc.mutate(updated)

			g.Expect(orderControlPlanePredicate().Update(event.UpdateEvent{
				ObjectOld: base, ObjectNew: updated,
			})).To(Equal(tc.want))
		})
	}
}

func TestKeystoneUserControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := keystoneUserCR(kuTestNamespace)
	g.Expect(keystoneUserControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(keystoneUserControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}),
		"an empty namespace defaults to the order's own")
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(keystoneUserControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(keystoneUserControlPlaneRefExtractor(ksControlPlane())).To(BeNil())
}

// --- refresh ---

// TestKeystoneUser_RefusalComesBackOnTheRefresh pins the pass rule for a refusal
// no watch lifts: a Secret of the delivered name that somebody else owns leaves
// the delivery leg with a zero result, and the pass turns that into the refresh.
func TestKeystoneUser_RefusalComesBackOnTheRefresh(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, ""))
	order := keystoneUserCR(kuTestNamespace)
	stranger := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "workflow-credentials", Namespace: kuTestNamespace}}

	got, _, result, err := reconcileKeystoneUser(t, c5c3v1alpha1.ManagementCluster,
		append([]client.Object{order, cp, stranger}, kuConvergedChildren(order, cp, "", 1)...)...)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kuCondition(got, conditionTypeKeystoneUserDeliveryReady).Reason).To(Equal(reasonKeystoneUserDeliveryRefused))
	g.Expect(result.RequeueAfter).To(Equal(keystoneUserRefreshAfter))
}

// --- teardown ---

// kuDeletingOrder returns a converged order marked for deletion, with every
// child seeded: the K-ORC User held by its finalizer, the probe, two password
// generations, the source Secret, the PushSecret and the delivered Secret.
func kuDeletingOrder(cp *c5c3v1alpha1.ControlPlane) (*c5c3v1alpha1.KeystoneUser, []client.Object) {
	order := keystoneUserCR(kuTestNamespace)
	order.DeletionTimestamp = ptr.To(metav1.Now())
	labels := keystoneUserChildLabels(order, "")
	seeded := kuConvergedChildren(order, cp, "", 2)
	seeded[0].SetFinalizers([]string{"openstack.k-orc.cloud/user"})
	seeded = append(seeded,
		&orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneUserUserProbeRef(order, ""), Namespace: cp.Namespace, Labels: labels,
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneUserPasswordSecretName(order, "", 1), Namespace: cp.Namespace, Labels: labels,
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: keystoneUserSourceSecretName(order, ""), Namespace: cp.Namespace, Labels: labels,
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "workflow-credentials", Namespace: kuTestNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: c5c3v1alpha1.GroupVersion.String(), Kind: "KeystoneUser",
				Name: order.Name, UID: order.UID, Controller: ptr.To(true),
			}},
		}},
	)
	return order, seeded
}

// TestKeystoneUser_DeleteRemovesChildrenThenReleasesTheFinalizer issues every
// delete in the documented order on the first pass, holds the finalizer while
// K-ORC still holds the User, and releases it once nothing is left.
func TestKeystoneUser_DeleteRemovesChildrenThenReleasesTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, ""))
	order, seeded := kuDeletingOrder(cp)
	var deleted []string
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}, nil, append([]client.Object{order, cp}, seeded...)...)

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter), "the pass that issued the deletes requeues")
	prefix := keystoneUserChildPrefix(order, "")
	g.Expect(deleted).To(Equal([]string{
		prefix + "backup", prefix + "user", prefix + "user-probe",
		prefix + "password-v1", prefix + "password-v2", prefix + "source", "workflow-credentials",
	}))
	g.Expect(h.get(t).Finalizers).To(ContainElement(keystoneUserFinalizerName),
		"the finalizer is held while K-ORC still holds the User")

	user := &orcv1alpha1.User{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "user"}, user)).To(Succeed())
	user.Finalizers = nil
	g.Expect(h.mgmt.Update(ctx, user)).To(Succeed())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.get(t)).To(BeNil(), "the order is gone once its finalizer is released")
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero())
}

func TestKeystoneUser_DeleteFailsOpenWithoutTheControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, ""))
	order, seeded := kuDeletingOrder(cp)
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order}, seeded...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.get(t)).To(BeNil(), "without a plane the finalizer is released at once")
	g.Expect(kuLabelled(t, h.mgmt)).To(Equal(1), "only the User K-ORC still holds is left")
	err = h.mgmt.Get(context.Background(),
		types.NamespacedName{Namespace: kuTestNamespace, Name: "workflow-credentials"}, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestKeystoneUser_DeleteOfAFrozenOrderTearsDown(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := keystoneUserControlPlane() // no entry: the order is frozen
	order, seeded := kuDeletingOrder(cp)
	seeded[0].SetFinalizers(nil)
	h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order, cp}, seeded...)...)

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(kuLabelled(t, h.mgmt)).To(BeZero(), "a withdrawn assignment does not stop the teardown")
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.get(t)).To(BeNil())
}

// TestKeystoneUser_SweepLeavesForeignObjectsAlone seeds three near misses in
// the ControlPlane's namespace: an object with the order's labels outside its
// prefix, one inside the prefix without the labels, and one inside the prefix
// labelled for the same order on the other cluster. Beside the order it seeds a
// Secret of the delivered name that the owner created and the order never
// delivered.
func TestKeystoneUser_SweepLeavesForeignObjectsAlone(t *testing.T) {
	cases := []struct {
		name           string
		cluster, other string
	}{
		{name: "an order on the management cluster", cluster: c5c3v1alpha1.ManagementCluster, other: kuTestCluster},
		{name: "an order on a target cluster", cluster: kuTestCluster, other: c5c3v1alpha1.ManagementCluster},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()

			cp := keystoneUserControlPlane(assignOn(kuTestNamespace, tc.cluster))
			order := keystoneUserCR(kuTestNamespace)
			order.DeletionTimestamp = ptr.To(metav1.Now())
			labelledOnly := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "someone-elses", Namespace: "default", Labels: keystoneUserChildLabels(order, tc.cluster),
			}}
			prefixedOnly := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{
				Name: keystoneUserUserRef(order, tc.cluster), Namespace: "default",
			}}
			otherCluster := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: keystoneUserSourceSecretName(order, tc.cluster), Namespace: "default",
				Labels: keystoneUserChildLabels(order, tc.other),
			}}
			strangerCreds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "workflow-credentials", Namespace: kuTestNamespace},
				Data:       map[string][]byte{"token": []byte("theirs")},
			}
			h := newKUHarness(t, tc.cluster, nil, nil,
				order, cp, labelledOnly, prefixedOnly, otherCluster, strangerCreds)

			_, err := h.reconcile(ctx)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(h.get(t)).To(BeNil())
			for _, obj := range []client.Object{labelledOnly, prefixedOnly, otherCluster} {
				g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed(),
					"%T %s is not the order's child", obj, obj.GetName())
			}
			g.Expect(h.order.Get(ctx, client.ObjectKeyFromObject(strangerCreds), strangerCreds)).To(Succeed(),
				"a Secret of the delivered name the order does not own stays")
			g.Expect(strangerCreds.Data).To(Equal(map[string][]byte{"token": []byte("theirs")}))
		})
	}
}

// TestKeystoneUser_TeardownFailures pins when a failing teardown keeps the
// finalizer: a ControlPlane read error, and a failed delete while the plane
// exists. Only a failed delete with the plane gone releases it, leaving the
// child behind.
func TestKeystoneUser_TeardownFailures(t *testing.T) {
	boom := errors.New("boom")
	failControlPlaneRead := &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c5c3v1alpha1.ControlPlane); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
	failPushSecretDelete := &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*esov1alpha1.PushSecret); ok {
				return boom
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}
	cases := []struct {
		name      string
		funcs     *interceptor.Funcs
		withPlane bool
		wantErr   string
		// wantSwept is false when the pass fails before it deletes anything.
		wantSwept    bool
		wantReleased bool
	}{
		{name: "the ControlPlane read fails", funcs: failControlPlaneRead, withPlane: true, wantErr: "fetching ControlPlane"},
		{
			name: "a delete fails with the plane present", funcs: failPushSecretDelete, withPlane: true,
			wantErr: "deleting KeystoneUser child", wantSwept: true,
		},
		{name: "a delete fails with the plane gone", funcs: failPushSecretDelete, wantSwept: true, wantReleased: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()

			cp := keystoneUserControlPlane(assignOn(kuTestNamespace, ""))
			order, seeded := kuDeletingOrder(cp)
			objs := append([]client.Object{order}, seeded...)
			if tc.withPlane {
				objs = append(objs, cp)
			}
			h := newKUHarness(t, c5c3v1alpha1.ManagementCluster, tc.funcs, nil, objs...)
			before := kuLabelled(t, h.mgmt)

			_, err := h.reconcile(ctx)

			if tc.wantErr != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(errors.Is(err, boom)).To(BeTrue())
				g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
			got := h.get(t)
			if tc.wantReleased {
				g.Expect(got).To(BeNil(), "with the plane gone the finalizer is released")
			} else {
				g.Expect(got.Finalizers).To(ContainElement(keystoneUserFinalizerName))
			}
			if !tc.wantSwept {
				g.Expect(kuLabelled(t, h.mgmt)).To(Equal(before), "nothing is deleted without the plane read")
			} else {
				var pushSecrets esov1alpha1.PushSecretList
				g.Expect(h.mgmt.List(ctx, &pushSecrets, client.InNamespace("default"))).To(Succeed())
				g.Expect(pushSecrets.Items).To(HaveLen(1), "the PushSecret whose delete failed stays")
			}
		})
	}
}
