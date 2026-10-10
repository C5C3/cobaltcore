// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the RabbitMQVhost reconciler's lifecycle: the consent gate and the
// freeze, the messaging gate, the naming contract, the watch mappers and the
// teardown.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// rvTestClock is the instant the harness clock starts at, on a whole second
// the way a status time keeps it.
var rvTestClock = time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)

// The managed bus the fixtures declare, in the ControlPlane's namespace
// "default".
const (
	rvBusName       = "cp-rabbitmq"
	rvBusUserSecret = "cp-rabbitmq-default-user"
	rvBusHost       = "cp-rabbitmq.default.svc"
	rvPublished     = "broker.example.test:5672"
)

// rabbitMQVhostCR returns the order "workflow" in kuTestNamespace with the
// schema's defaults spelled out, already carrying the teardown finalizer.
func rabbitMQVhostCR() *c5c3v1alpha1.RabbitMQVhost {
	return &c5c3v1alpha1.RabbitMQVhost{
		ObjectMeta: metav1.ObjectMeta{
			Name:       kuTestName,
			Namespace:  kuTestNamespace,
			Generation: 1,
			UID:        types.UID("rv-uid"),
			Finalizers: []string{rabbitMQVhostFinalizerName},
		},
		Spec: c5c3v1alpha1.RabbitMQVhostSpec{
			ControlPlaneRef:    c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
			PasswordGeneration: 1,
			Rotation: c5c3v1alpha1.RabbitMQVhostRotationSpec{
				Interval:    &metav1.Duration{Duration: 720 * time.Hour},
				GracePeriod: &metav1.Duration{Duration: 24 * time.Hour},
			},
			DeletionPolicy: c5c3v1alpha1.RabbitMQVhostDeletionPolicyRetain,
		},
	}
}

// rvControlPlane assigns kuTestNamespace on cluster and declares the managed
// bus; for an order on a target cluster it publishes the broker as well.
func rvControlPlane(cluster string) *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	cp.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{assignOn(kuTestNamespace, cluster)}
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
		Messaging: &commonv1.MessagingSpec{ClusterRef: &corev1.LocalObjectReference{Name: rvBusName}},
	}
	if cluster != c5c3v1alpha1.ManagementCluster {
		cp.Spec.Infrastructure.PublishedMessagingEndpoint = rvPublished
	}
	return cp
}

// rvBus returns the managed bus as the RabbitMQ Cluster Operator reports it:
// the RabbitmqCluster naming its default-user Secret, AllReplicasReady when
// ready, and that Secret with the in-cluster address.
func rvBus(ready bool) []client.Object {
	bus := &unstructured.Unstructured{}
	bus.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	bus.SetName(rvBusName)
	bus.SetNamespace("default")
	status := "False"
	if ready {
		status = "True"
	}
	bus.Object["status"] = map[string]any{
		"defaultUser": map[string]any{"secretReference": map[string]any{"name": rvBusUserSecret}},
		"conditions":  []any{map[string]any{"type": "AllReplicasReady", "status": status}},
	}
	return []client.Object{bus, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: rvBusUserSecret, Namespace: "default"},
		Data: map[string][]byte{
			"username": []byte("default_user_abc"), "password": []byte("admin-password"),
			"host": []byte(rvBusHost), "port": []byte("5672"),
		},
	}}
}

// rvReadyInfra returns the ready bus and the ready tenant store of the
// ControlPlane's namespace.
func rvReadyInfra() []client.Object {
	return append(rvBus(true), readyTenantStoreFor(korcControlPlane()))
}

// rvHarness drives the reconciler with a clock the test moves.
type rvHarness struct {
	*orderHarness
	now time.Time
}

func newRVHarness(t *testing.T, cluster string, mgmtFuncs, orderFuncs *interceptor.Funcs, objs ...client.Object) *rvHarness {
	t.Helper()
	h := &rvHarness{now: rvTestClock}
	h.orderHarness = newOrderHarness(t, cluster, types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName},
		mgmtFuncs, orderFuncs, objs, func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler {
			return &RabbitMQVhostReconciler{
				Client: mgmt, Scheme: mgmt.Scheme(), Resolver: resolver,
				Now: func() time.Time { return h.now },
			}
		})
	return h
}

// reconciler returns the harness's reconciler, for a call into one leg.
func (h *rvHarness) reconciler() *RabbitMQVhostReconciler {
	return h.r.(*RabbitMQVhostReconciler)
}

func rvGet(t *testing.T, h *rvHarness) *c5c3v1alpha1.RabbitMQVhost {
	t.Helper()
	return reloadOrder[c5c3v1alpha1.RabbitMQVhost](t, h.orderHarness)
}

func rvCondition(order *c5c3v1alpha1.RabbitMQVhost, condType string) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, condType)
}

// expectRVConditions asserts both sub-conditions read False with reason.
func expectRVConditions(t *testing.T, order *c5c3v1alpha1.RabbitMQVhost, reason string) {
	t.Helper()
	g := NewGomegaWithT(t)
	for _, condType := range rabbitMQVhostSubConditionTypes {
		cond := rvCondition(order, condType)
		g.Expect(cond).NotTo(BeNil(), condType)
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), condType)
		g.Expect(cond.Reason).To(Equal(reason), condType)
	}
}

// rvLabelled counts the Vhosts, Users, Permissions, Secrets and PushSecrets
// in the ControlPlane's namespace that carry the order's name label.
func rvLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	key := rabbitMQVhostLabelKeys.Name
	return labelledIn(t, c, topologyList(messaging.VhostGVK), key, kuTestName) +
		labelledIn(t, c, topologyList(messaging.UserGVK), key, kuTestName) +
		labelledIn(t, c, topologyList(messaging.PermissionGVK), key, kuTestName) +
		labelledIn(t, c, &corev1.SecretList{}, key, kuTestName) +
		labelledIn(t, c, &esov1alpha1.PushSecretList{}, key, kuTestName)
}

// rvTopology reads the topology child of gvk named name, or nil once it is
// gone.
func rvTopology(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("reading %s %s: %v", gvk.Kind, name, err)
	}
	return u
}

// rvSetTopologyStatus writes the Ready condition the topology operator reports
// onto the child of gvk named name, at the child's current generation.
func rvSetTopologyStatus(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name, status, reason, message string) {
	t.Helper()
	u := rvTopology(t, c, gvk, name)
	if u == nil {
		t.Fatalf("%s %s does not exist", gvk.Kind, name)
	}
	u.Object["status"] = map[string]any{
		"observedGeneration": u.GetGeneration(),
		"conditions": []any{map[string]any{
			"type": "Ready", "status": status, "reason": reason, "message": message,
		}},
	}
	if err := c.Update(context.Background(), u); err != nil {
		t.Fatalf("writing the status of %s %s: %v", gvk.Kind, name, err)
	}
}

// rvMarkReady reports the child of gvk named name Ready.
func rvMarkReady(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) {
	t.Helper()
	rvSetTopologyStatus(t, c, gvk, name, "True", "SuccessfulCreateOrUpdate", "")
}

// rvMarkFailed reports the child of gvk named name refused by the broker.
func rvMarkFailed(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name, message string) {
	t.Helper()
	rvSetTopologyStatus(t, c, gvk, name, "False", "FailedCreateOrUpdate", message)
}

// --- lifecycle and gates ---

func TestRabbitMQVhost_InstallsTheFinalizerOnceAssigned(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	order.Finalizers = nil
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).NotTo(BeZero(), "the finalizer pass requeues")
	got := rvGet(t, h)
	g.Expect(got.Finalizers).To(ContainElement(rabbitMQVhostFinalizerName))
	for _, condType := range rabbitMQVhostSubConditionTypes {
		g.Expect(rvCondition(got, condType)).To(BeNil(), "the finalizer pass writes no %s", condType)
	}
	g.Expect(rvLabelled(t, h.mgmt)).To(BeZero())
}

// TestRabbitMQVhost_AdmissionGates covers the scaffold's gates: both
// sub-conditions take the refusal, nothing is created in the ControlPlane's
// namespace, and an order the operator does not serve gets no finalizer.
func TestRabbitMQVhost_AdmissionGates(t *testing.T) {
	longCluster := strings.Repeat("c", 64)
	unassigned := func() *c5c3v1alpha1.ControlPlane {
		cp := rvControlPlane("")
		cp.Spec.NamespaceAssignments = nil
		return cp
	}
	cases := []struct {
		name       string
		cluster    string
		cp         *c5c3v1alpha1.ControlPlane
		wantReason string
	}{
		{name: "the namespace is not assigned on the management cluster", cp: unassigned(), wantReason: reasonOrderNamespaceNotAssigned},
		{
			name: "the namespace is not assigned on a target cluster", cluster: kuTestCluster, cp: rvControlPlane(""),
			wantReason: reasonOrderNamespaceNotAssigned,
		},
		{
			name: "a cluster name no label value can carry", cluster: longCluster, cp: rvControlPlane(longCluster),
			wantReason: reasonOrderClusterNameTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := rabbitMQVhostCR()
			order.Finalizers = nil
			h := newRVHarness(t, tc.cluster, nil, nil, append([]client.Object{order, tc.cp}, rvReadyInfra()...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			got := rvGet(t, h)
			expectRVConditions(t, got, tc.wantReason)
			g.Expect(got.Finalizers).To(BeEmpty())
			g.Expect(rvLabelled(t, h.mgmt)).To(BeZero(), "nothing is created in the ControlPlane's namespace")
		})
	}
}

// TestRabbitMQVhost_MessagingGate covers the bus the order needs: a refusal only
// a ControlPlane edit lifts asks for no requeue of its own, a wait comes back on
// the K-ORC cadence, and no gate creates a child.
func TestRabbitMQVhost_MessagingGate(t *testing.T) {
	withInfra := func(mutate func(*c5c3v1alpha1.InfrastructureSpec)) *c5c3v1alpha1.ControlPlane {
		cp := rvControlPlane("")
		mutate(cp.Spec.Infrastructure)
		return cp
	}
	noInfrastructure := rvControlPlane("")
	noInfrastructure.Spec.Infrastructure = nil
	pendingBus := rvBus(true)
	delete(pendingBus[0].(*unstructured.Unstructured).Object, "status")

	cases := []struct {
		name       string
		cp         *c5c3v1alpha1.ControlPlane
		infra      []client.Object
		wantReason string
		wantGate   time.Duration
		wantPass   time.Duration
		wantText   string
	}{
		{
			name: "a ControlPlane without an infrastructure block", cp: noInfrastructure, infra: rvReadyInfra(),
			wantReason: reasonRabbitMQVhostMessagingNotDeclared, wantPass: orderRefreshAfter,
			wantText: "declares no spec.infrastructure.messaging",
		},
		{
			name: "a ControlPlane without a message bus",
			cp:   withInfra(func(infra *c5c3v1alpha1.InfrastructureSpec) { infra.Messaging = nil }), infra: rvReadyInfra(),
			wantReason: reasonRabbitMQVhostMessagingNotDeclared, wantPass: orderRefreshAfter,
			wantText: "needs the managed message bus",
		},
		{
			name: "a brownfield bus",
			cp: withInfra(func(infra *c5c3v1alpha1.InfrastructureSpec) {
				infra.Messaging = &commonv1.MessagingSpec{SecretRef: &commonv1.SecretRefSpec{Name: "bus-url"}}
			}),
			infra: rvReadyInfra(), wantReason: reasonRabbitMQVhostMessagingNotManaged, wantPass: orderRefreshAfter,
			wantText: "brownfield broker",
		},
		{
			name: "a bus without its default user", cp: rvControlPlane(""), infra: pendingBus,
			wantReason: messaging.ReasonWaitingForMessagingCredentials, wantGate: rabbitMQVhostRequeueAfter, wantPass: rabbitMQVhostRequeueAfter,
			wantText: "has no status.defaultUser.secretReference.name yet",
		},
		{
			name: "a bus whose default-user Secret is missing", cp: rvControlPlane(""), infra: rvBus(true)[:1],
			wantReason: messaging.ReasonWaitingForMessagingCredentials, wantGate: rabbitMQVhostRequeueAfter, wantPass: rabbitMQVhostRequeueAfter,
			wantText: "default-user Secret default/" + rvBusUserSecret + " not found",
		},
		{
			name: "a bus that is not AllReplicasReady", cp: rvControlPlane(""), infra: rvBus(false),
			wantReason: reasonRabbitMQVhostWaitingForMessaging, wantGate: rabbitMQVhostRequeueAfter, wantPass: rabbitMQVhostRequeueAfter,
			wantText: "RabbitmqCluster default/" + rvBusName + " is not AllReplicasReady",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{rabbitMQVhostCR(), tc.cp}, tc.infra...)...)

			result, err := h.reconcile(ctx)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(tc.wantPass))
			got := rvGet(t, h)
			expectRVConditions(t, got, tc.wantReason)
			g.Expect(rvCondition(got, conditionTypeRabbitMQVhostVhostReady).Message).To(ContainSubstring(tc.wantText))
			g.Expect(rvLabelled(t, h.mgmt)).To(BeZero(), "no gate creates a child")

			_, _, gate, ok, err := h.reconciler().rabbitMQVhostMessagingGate(ctx, tc.cp, func(string, string) {})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ok).To(BeFalse())
			g.Expect(gate.RequeueAfter).To(Equal(tc.wantGate))
		})
	}

	t.Run("a ready bus answers its in-cluster address", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := rvControlPlane("")
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{cp}, rvReadyInfra()...)...)

		host, port, result, ok, err := h.reconciler().rabbitMQVhostMessagingGate(context.Background(), cp,
			func(reason, message string) { t.Fatalf("unexpected refusal %s: %s", reason, message) })

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(host).To(Equal(rvBusHost))
		g.Expect(port).To(Equal(int32(5672)))
	})

	t.Run("a failed read of the bus is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == messaging.RabbitmqClusterGVK {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, append([]client.Object{rabbitMQVhostCR(), rvControlPlane("")}, rvReadyInfra()...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("resolving the managed bus of ControlPlane default/cp"))
		expectRVConditions(t, rvGet(t, h), reasonRabbitMQVhostError)
		g.Expect(rvLabelled(t, h.mgmt)).To(BeZero())
	})

	t.Run("a default-user port that is no number is an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		infra := rvReadyInfra()
		infra[1].(*corev1.Secret).Data["port"] = []byte("amqp")
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{rabbitMQVhostCR(), rvControlPlane("")}, infra...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(HaveOccurred())
		expectRVConditions(t, rvGet(t, h), reasonRabbitMQVhostError)
		g.Expect(rvCondition(rvGet(t, h), conditionTypeRabbitMQVhostVhostReady).Message).NotTo(ContainSubstring("admin-password"),
			"a condition never quotes the broker's admin password")
	})
}

func TestRabbitMQVhost_PassResult(t *testing.T) {
	timer := ctrl.Result{RequeueAfter: 30 * time.Hour}
	short := ctrl.Result{RequeueAfter: 3 * time.Second}
	refresh := ctrl.Result{RequeueAfter: orderRefreshAfter}
	cases := []struct {
		name      string
		cluster   string
		converged bool
		result    ctrl.Result
		want      ctrl.Result
	}{
		{name: "a converged order on the management cluster waits for its timer", converged: true, result: timer, want: timer},
		{name: "a converged order on the management cluster without a timer waits for an event", converged: true},
		{name: "a converged order on a target cluster refreshes first", cluster: kuTestCluster, converged: true, result: timer, want: refresh},
		{name: "a shorter requeue stands", cluster: kuTestCluster, result: short, want: short},
		{name: "an unconverged order refreshes", result: timer, want: refresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := rabbitMQVhostCR()
			if tc.converged {
				order.Status.Conditions = []metav1.Condition{{
					Type: conditionTypeRabbitMQVhostDeliveryReady, Status: metav1.ConditionTrue, Reason: reasonKeystoneUserDelivered,
				}}
			}
			g.Expect(rabbitMQVhostPassResult(order, tc.cluster, tc.result)).To(Equal(tc.want))
		})
	}
}

// --- naming ---

func TestRabbitMQVhost_ChildNames(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	local := rabbitMQVhostChildPrefix(order, c5c3v1alpha1.ManagementCluster)
	remote := rabbitMQVhostChildPrefix(order, kuTestCluster)
	g.Expect(local).To(MatchRegexp(`^workflow-[0-9a-f]{8}-vhost-$`))
	g.Expect(remote).NotTo(Equal(local), "the same order on another cluster is another order")

	vhost := rabbitMQVhostName(order, "")
	g.Expect(vhost).To(MatchRegexp(`^workflow-[0-9a-f]{8}$`))
	g.Expect(local).To(Equal(vhost + "-vhost-"))
	g.Expect(rabbitMQVhostName(order, kuTestCluster)).NotTo(Equal(vhost), "two orders never share a vhost")

	g.Expect(rabbitMQVhostChildName(order, "")).To(Equal(local + "vhost"))
	g.Expect(rabbitMQVhostPasswordSecretName(order, "", 3)).To(Equal(local + "password-v3"))
	g.Expect(rabbitMQVhostUserName(order, "", 3)).To(Equal(local + "user-v3"))
	g.Expect(rabbitMQVhostPermissionName(order, "", 3)).To(Equal(local + "permission-v3"))
	g.Expect(rabbitMQVhostBrokerUserName(order, "", 3)).To(Equal(vhost + "-v3"))
	g.Expect(rabbitMQVhostSourceSecretName(order, "")).To(Equal(local + "source"))
	g.Expect(rabbitMQVhostPushSecretName(order, "")).To(Equal(local + "backup"))
	g.Expect(rabbitMQVhostCredentialsSecretName(order)).To(Equal("workflow-credentials"))
	g.Expect(rabbitMQVhostRemoteKeyFor(korcControlPlane(), local)).To(MatchRegexp(
		`^openstack/rabbitmq/default/workflow-[0-9a-f]{8}-vhost/credentials$`))

	// Every child name fits the 253-byte object name budget for the longest
	// order name and generation.
	order.Name = strings.Repeat("a", 63)
	for _, name := range []string{
		rabbitMQVhostPermissionName(order, kuTestCluster, 1<<62),
		rabbitMQVhostPasswordSecretName(order, kuTestCluster, 1<<62),
		rabbitMQVhostBrokerUserName(order, kuTestCluster, 1<<62),
	} {
		g.Expect(len(name)).To(BeNumerically("<=", 253), name)
	}
}

// TestRabbitMQVhost_StoredDefaults resolves the defaults the reconciler applies
// to an object stored without the schema's.
func TestRabbitMQVhost_StoredDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	order.Spec.PasswordGeneration = 0
	order.Spec.Rotation = c5c3v1alpha1.RabbitMQVhostRotationSpec{}
	order.Spec.DeletionPolicy = ""
	g.Expect(rabbitMQVhostGeneration(order)).To(Equal(int64(1)))
	interval, grace := rabbitMQVhostRotation(order)
	g.Expect(interval).To(Equal(720 * time.Hour))
	g.Expect(grace).To(Equal(24 * time.Hour))
	g.Expect(rabbitMQVhostDeletionPolicy(order)).To(Equal(c5c3v1alpha1.RabbitMQVhostDeletionPolicyRetain))

	order.Spec.Rotation.Interval = &metav1.Duration{}
	interval, _ = rabbitMQVhostRotation(order)
	g.Expect(interval).To(BeZero(), "an explicit 0s turns the schedule off")
	order.Spec.DeletionPolicy = c5c3v1alpha1.RabbitMQVhostDeletionPolicyDelete
	g.Expect(rabbitMQVhostDeletionPolicy(order)).To(Equal(c5c3v1alpha1.RabbitMQVhostDeletionPolicyDelete))
}

// --- watch mappers ---

func TestControlPlaneToRabbitMQVhostsMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := rabbitMQVhostCR()
	elsewhere := rabbitMQVhostCR()
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(explicit, elsewhere).
		WithIndex(&c5c3v1alpha1.RabbitMQVhost{}, RabbitMQVhostControlPlaneRefIndexKey, rabbitMQVhostControlPlaneRefExtractor).
		Build()

	g.Expect(controlPlaneToRabbitMQVhostsMapper(c)(context.Background(), korcControlPlane())).To(Equal(
		[]reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(explicit)}}))

	failing := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}).Build()
	g.Expect(controlPlaneToRabbitMQVhostsMapper(failing)(context.Background(), korcControlPlane())).To(BeNil())
}

func TestRabbitMQVhostControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	g.Expect(rabbitMQVhostControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(rabbitMQVhostControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}))
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(rabbitMQVhostControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(rabbitMQVhostControlPlaneRefExtractor(korcControlPlane())).To(BeNil())
}

// TestRabbitMQVhost_ChildEventsMapBack maps a labelled topology child to the
// order on the cluster its label names.
func TestRabbitMQVhost_ChildEventsMapBack(t *testing.T) {
	g := NewGomegaWithT(t)

	user := messaging.BuildUser("u", "default", rvBusName, "s")
	user.SetLabels(rabbitMQVhostRef(rabbitMQVhostCR(), kuTestCluster).childLabels())
	requests := orderChildToRequests(rabbitMQVhostLabelKeys)(context.Background(), user)
	g.Expect(requests).To(HaveLen(1))
	g.Expect(requests[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: kuTestNamespace, Name: kuTestName}))
	g.Expect(requests[0].ClusterName).To(Equal(mcruntime.ClusterName(kuTestCluster)))
}

// TestRabbitMQVhost_ReconcileEntry covers the branches Reconcile answers
// itself.
func TestRabbitMQVhost_ReconcileEntry(t *testing.T) {
	t.Run("a missing order is not an error", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil)
		result, err := h.reconcile(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
	})

	t.Run("an unresolvable cluster is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newRVHarness(t, kuTestCluster, nil, nil, rabbitMQVhostCR(), rvControlPlane(kuTestCluster))
		h.reconciler().Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(rvGet(t, h).Status.Conditions).To(BeEmpty())
	})

	t.Run("a failed read of the order is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*c5c3v1alpha1.RabbitMQVhost); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, rabbitMQVhostCR(), rvControlPlane(""))

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring("fetching RabbitMQVhost"))
	})
}

// --- teardown ---

// rvDeletingOrder returns an order marked for deletion with every child
// seeded: the Vhost, generation 1's Permission, User and password Secret, the
// User held by the topology operator's finalizer, the source Secret, the
// PushSecret and the delivered Secret.
func rvDeletingOrder() (*c5c3v1alpha1.RabbitMQVhost, []client.Object) {
	order := rabbitMQVhostCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	labels := rabbitMQVhostRef(order, "").childLabels()
	labelled := func(obj client.Object) client.Object {
		obj.SetLabels(labels)
		return obj
	}
	child := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels}
	}
	vhost := rabbitMQVhostName(order, "")
	user := messaging.BuildUser(rabbitMQVhostUserName(order, "", 1), "default", rvBusName,
		rabbitMQVhostPasswordSecretName(order, "", 1))
	user.SetFinalizers([]string{"deletion.finalizers.users.rabbitmq.com"})
	return order, []client.Object{
		labelled(messaging.BuildVhost(rabbitMQVhostChildName(order, ""), "default", vhost, rvBusName, "retain")),
		labelled(messaging.BuildPermission(rabbitMQVhostPermissionName(order, "", 1), "default", rvBusName, vhost,
			rabbitMQVhostBrokerUserName(order, "", 1))),
		labelled(user),
		&corev1.Secret{ObjectMeta: child(rabbitMQVhostPasswordSecretName(order, "", 1))},
		&corev1.Secret{ObjectMeta: child(rabbitMQVhostSourceSecretName(order, ""))},
		&esov1alpha1.PushSecret{ObjectMeta: child(rabbitMQVhostPushSecretName(order, ""))},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "workflow-credentials", Namespace: kuTestNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: c5c3v1alpha1.GroupVersion.String(), Kind: "RabbitMQVhost",
				Name: order.Name, UID: order.UID, Controller: ptr.To(true),
			}},
		}},
	}
}

// rvDeleteRecorder records the names of the objects deleted through it.
func rvDeleteRecorder(deleted *[]string) *interceptor.Funcs {
	return &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*deleted = append(*deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}
}

// TestRabbitMQVhost_DeleteSweepsInOrderAndHolds issues every delete in the
// documented order, holds the finalizer while the topology operator still
// holds the User, and releases it once nothing the order owns is listed.
func TestRabbitMQVhost_DeleteSweepsInOrderAndHolds(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := rvDeletingOrder()
	var deleted []string
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, rvDeleteRecorder(&deleted), nil,
		append([]client.Object{order, rvControlPlane("")}, seeded...)...)
	prefix := rabbitMQVhostChildPrefix(order, "")

	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter))
	g.Expect(deleted).To(Equal([]string{
		prefix + "backup", prefix + "permission-v1", prefix + "user-v1", prefix + "vhost",
		prefix + "password-v1", prefix + "source", "workflow-credentials",
	}))
	g.Expect(rvGet(t, h).Finalizers).To(ContainElement(rabbitMQVhostFinalizerName),
		"the finalizer holds while the topology operator still holds the User")

	user := rvTopology(t, h.mgmt, messaging.UserGVK, prefix+"user-v1")
	g.Expect(user).NotTo(BeNil())
	user.SetFinalizers(nil)
	g.Expect(h.mgmt.Update(ctx, user)).To(Succeed())

	deleted = nil
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(BeEmpty())
	g.Expect(rvGet(t, h)).To(BeNil(), "the order is gone once nothing it owns is listed")
	g.Expect(rvLabelled(t, h.mgmt)).To(BeZero())
}

// TestRabbitMQVhost_DeleteRemovesTheVhostCRUnderEitherPolicy deletes the Vhost
// CR under both policies: the policy lives in its spec, where the topology
// operator reads it.
func TestRabbitMQVhost_DeleteRemovesTheVhostCRUnderEitherPolicy(t *testing.T) {
	for _, policy := range []string{"retain", "delete"} {
		t.Run(policy, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order, seeded := rvDeletingOrder()
			seeded[0].(*unstructured.Unstructured).Object["spec"].(map[string]any)["deletionPolicy"] = policy
			seeded[2].SetFinalizers(nil)
			h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{order, rvControlPlane("")}, seeded...)...)

			for range 2 {
				_, err := h.reconcile(context.Background())
				g.Expect(err).NotTo(HaveOccurred())
			}
			g.Expect(rvTopology(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))).To(BeNil())
			g.Expect(rvGet(t, h)).To(BeNil())
		})
	}
}

// TestRabbitMQVhost_DeleteAppliesTheOrdersCurrentPolicy deletes the Vhost CR
// under the policy the order holds when it is deleted. A policy changed right
// before the delete never reached the CR through a provisioning pass, and the
// topology operator reads the policy off the CR it deletes. The topology
// operator's finalizer keeps the deleted Vhost readable.
func TestRabbitMQVhost_DeleteAppliesTheOrdersCurrentPolicy(t *testing.T) {
	deletingVhost := func(seeded []client.Object, stored string) {
		vhost := seeded[0].(*unstructured.Unstructured)
		vhost.Object["spec"].(map[string]any)["deletionPolicy"] = stored
		vhost.SetFinalizers([]string{"deletion.finalizers.vhosts.rabbitmq.com"})
	}
	cases := []struct {
		policy, stored, want string
	}{
		{policy: c5c3v1alpha1.RabbitMQVhostDeletionPolicyRetain, stored: "delete", want: "retain"},
		{policy: c5c3v1alpha1.RabbitMQVhostDeletionPolicyDelete, stored: "retain", want: "delete"},
	}
	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order, seeded := rvDeletingOrder()
			order.Spec.DeletionPolicy = tc.policy
			deletingVhost(seeded, tc.stored)
			h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{order, rvControlPlane("")}, seeded...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			live := rvTopology(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
			g.Expect(live.GetDeletionTimestamp()).NotTo(BeNil(), "the Vhost is deleted in the same pass")
			g.Expect(live.Object["spec"].(map[string]any)["deletionPolicy"]).To(Equal(tc.want))
		})
	}

	boom := errors.New("boom")
	prefix := rabbitMQVhostChildPrefix(rabbitMQVhostCR(), "")
	// failedWrite deletes an order whose policy cannot be written onto its
	// Vhost and returns the names deleted and the reconcile's error.
	failedWrite := func(t *testing.T, plane ...client.Object) (*rvHarness, []string, error) {
		order, seeded := rvDeletingOrder()
		deletingVhost(seeded, "delete")
		var deleted []string
		funcs := rvDeleteRecorder(&deleted)
		funcs.Patch = func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return boom
		}
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, funcs, nil,
			append(append([]client.Object{order}, plane...), seeded...)...)
		_, err := h.reconcile(context.Background())
		return h, deleted, err
	}
	// everyChildButTheVhost is what a pass deletes when the Vhost keeps its CR.
	everyChildButTheVhost := []string{
		prefix + "backup", prefix + "permission-v1", prefix + "user-v1",
		prefix + "password-v1", prefix + "source", "workflow-credentials",
	}

	t.Run("a failed write of the policy holds the finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)

		h, deleted, err := failedWrite(t, rvControlPlane(""))

		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(ContainSubstring(`writing deletionPolicy "retain" onto Vhost`))
		g.Expect(deleted).To(Equal(everyChildButTheVhost), "the other children are deleted in the same pass")
		live := rvTopology(t, h.mgmt, messaging.VhostGVK, prefix+"vhost")
		g.Expect(live.GetDeletionTimestamp()).To(BeNil(), "a Vhost whose policy could not be written is not deleted")
		g.Expect(rvGet(t, h).Finalizers).To(ContainElement(rabbitMQVhostFinalizerName))
	})

	t.Run("a failed write of the policy without the plane still deletes the credentials", func(t *testing.T) {
		g := NewGomegaWithT(t)

		h, deleted, err := failedWrite(t)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(deleted).To(Equal(everyChildButTheVhost),
			"the password Secrets, the User and its Permission do not outlive the released finalizer")
		live := rvTopology(t, h.mgmt, messaging.VhostGVK, prefix+"vhost")
		g.Expect(live.GetDeletionTimestamp()).To(BeNil(), "a Vhost whose policy could not be written is not deleted")
		g.Expect(rvGet(t, h)).To(BeNil(), "without a plane the finalizer is released at once")
	})

	// A later delete that fails does not hide the failed write: without the
	// plane the teardown logs the one error the sweep returns and releases the
	// finalizer, and that line is the only trace of the Vhost left behind.
	for _, name := range []string{prefix + "user-v1", prefix + "password-v1", "workflow-credentials"} {
		t.Run("a failed write of the policy is reported beside a failed delete of "+name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order, seeded := rvDeletingOrder()
			deletingVhost(seeded, "delete")
			refused := errors.New("refused")
			h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
				Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					return boom
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if obj.GetName() == name {
						return refused
					}
					return cl.Delete(ctx, obj, opts...)
				},
			}, nil, append([]client.Object{order, rvControlPlane("")}, seeded...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(refused))
			g.Expect(err).To(MatchError(boom))
			g.Expect(err.Error()).To(ContainSubstring(`writing deletionPolicy "retain" onto Vhost`))
		})
	}
}

func TestRabbitMQVhost_DeleteFailsOpenWithoutTheControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	order, seeded := rvDeletingOrder()
	var deleted []string
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, rvDeleteRecorder(&deleted), nil,
		append([]client.Object{order}, seeded...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(HaveLen(7), "without a plane one pass issues every delete")
	g.Expect(rvGet(t, h)).To(BeNil(), "without a plane the finalizer is released at once")
}

func TestRabbitMQVhost_DeleteOfAFrozenOrderTearsDown(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order, seeded := rvDeletingOrder()
	seeded[2].SetFinalizers(nil)
	cp := rvControlPlane("")
	cp.Spec.NamespaceAssignments = nil
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order, cp}, seeded...)...)

	for range 2 {
		_, err := h.reconcile(ctx)
		g.Expect(err).NotTo(HaveOccurred())
	}
	g.Expect(rvGet(t, h)).To(BeNil(), "a withdrawn assignment does not stop the teardown")
	g.Expect(rvLabelled(t, h.mgmt)).To(BeZero())
}

// TestRabbitMQVhost_DeleteWithoutTheTopologyKinds tears down on a cluster that
// serves none of the topology kinds: there is none of them to sweep.
func TestRabbitMQVhost_DeleteWithoutTheTopologyKinds(t *testing.T) {
	g := NewGomegaWithT(t)

	order, seeded := rvDeletingOrder()
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok {
				return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}
			}
			return cl.List(ctx, list, opts...)
		},
	}, nil, append([]client.Object{order, rvControlPlane("")}, seeded[3:]...)...)

	for range 2 {
		_, err := h.reconcile(context.Background())
		g.Expect(err).NotTo(HaveOccurred())
	}
	g.Expect(rvGet(t, h)).To(BeNil())
}

func TestRabbitMQVhost_TeardownFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		funcs   *interceptor.Funcs
		wantErr string
	}{
		{
			name: "a failed User list",
			funcs: &interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "UserList" {
						return boom
					}
					return cl.List(ctx, list, opts...)
				},
			},
			wantErr: "listing order *unstructured.UnstructuredList",
		},
		{
			name: "a failed delete",
			funcs: &interceptor.Funcs{
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*unstructured.Unstructured); ok {
						return boom
					}
					return cl.Delete(ctx, obj, opts...)
				},
			},
			wantErr: "deleting RabbitMQVhost child",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order, seeded := rvDeletingOrder()
			h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, tc.funcs, nil,
				append([]client.Object{order, rvControlPlane("")}, seeded...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(boom))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			g.Expect(rvGet(t, h).Finalizers).To(ContainElement(rabbitMQVhostFinalizerName))
		})
	}

	t.Run("a delivered Secret the order does not control stays", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		order, seeded := rvDeletingOrder()
		foreign := seeded[len(seeded)-1].(*corev1.Secret)
		foreign.OwnerReferences = nil
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order}, seeded...)...)

		_, err := h.reconcile(ctx)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rvGet(t, h)).To(BeNil())
		err = h.mgmt.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{})
		g.Expect(apierrors.IsNotFound(err)).To(BeFalse(), "a Secret of the delivered name the order does not own stays")
	})
}
