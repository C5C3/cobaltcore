// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the RabbitMQVhost provision leg: the store gate, the Vhost, the
// user generations, the schedule and its triggers, the grace sweep and the
// stray prune, all driven by the harness clock.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// rvPasswordSecret returns generation gen's password Secret carrying password,
// labelled for the order and for the topology operator.
func rvPasswordSecret(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64, password string) *corev1.Secret {
	labels := rabbitMQVhostRef(order, cluster).childLabels()
	labels[messaging.TopologyOperatorLabel] = "true"
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: rabbitMQVhostPasswordSecretName(order, cluster, gen), Namespace: "default", Labels: labels,
		},
		Data: map[string][]byte{
			rabbitMQVhostUsernameKey: []byte(rabbitMQVhostBrokerUserName(order, cluster, gen)),
			rabbitMQVhostPasswordKey: []byte(password),
		},
	}
}

// rvGenerationChildren returns generation gen's password Secret, User and
// Permission, without a status.
func rvGenerationChildren(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64, password string) []client.Object {
	labels := rabbitMQVhostRef(order, cluster).childLabels()
	user := messaging.BuildUser(rabbitMQVhostUserName(order, cluster, gen), "default", rvBusName,
		rabbitMQVhostPasswordSecretName(order, cluster, gen))
	user.SetLabels(labels)
	permission := messaging.BuildPermission(rabbitMQVhostPermissionName(order, cluster, gen), "default", rvBusName,
		rabbitMQVhostName(order, cluster), rabbitMQVhostBrokerUserName(order, cluster, gen))
	permission.SetLabels(labels)
	return []client.Object{rvPasswordSecret(order, cluster, gen, password), user, permission}
}

// rvMinted records on order that generation gen became the delivered one at
// at, and returns the Vhost and the generation's children.
func rvMinted(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64, password string, at time.Time) []client.Object {
	interval, _ := rabbitMQVhostRotation(order)
	order.Status.Vhost = rabbitMQVhostName(order, cluster)
	order.Status.Username = rabbitMQVhostBrokerUserName(order, cluster, gen)
	order.Status.PasswordGeneration = gen
	order.Status.LastRotation = &metav1.Time{Time: at}
	order.Status.NextRotation = vhostNextRotation(order, interval)
	vhost := messaging.BuildVhost(rabbitMQVhostChildName(order, cluster), "default", rabbitMQVhostName(order, cluster),
		rvBusName, strings.ToLower(rabbitMQVhostDeletionPolicy(order)))
	vhost.SetLabels(rabbitMQVhostRef(order, cluster).childLabels())
	return append([]client.Object{vhost}, rvGenerationChildren(order, cluster, gen, password)...)
}

// rvMarkGenerationReady reports generation gen's User and Permission Ready.
func rvMarkGenerationReady(t *testing.T, h *rvHarness, gen int64) {
	t.Helper()
	order := rabbitMQVhostCR()
	rvMarkReady(t, h.mgmt, messaging.UserGVK, rabbitMQVhostUserName(order, h.cluster, gen))
	rvMarkReady(t, h.mgmt, messaging.PermissionGVK, rabbitMQVhostPermissionName(order, h.cluster, gen))
}

// newMintedRVHarness seeds an assigned order on cluster with the ready bus and
// store and generation 1 delivered at rvTestClock with the password
// "password-1", every topology child reported Ready.
func newMintedRVHarness(
	t *testing.T, cluster string, order *c5c3v1alpha1.RabbitMQVhost, mgmtFuncs *interceptor.Funcs, extra ...client.Object,
) *rvHarness {
	t.Helper()
	objs := append([]client.Object{order, rvControlPlane(cluster)}, rvReadyInfra()...)
	objs = append(objs, rvMinted(order, cluster, 1, "password-1", rvTestClock)...)
	h := newRVHarness(t, cluster, mgmtFuncs, nil, append(objs, extra...)...)
	rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, cluster))
	rvMarkGenerationReady(t, h, 1)
	return h
}

// rvGeneration returns which of generation gen's Secret, User and Permission
// exist.
func rvGeneration(t *testing.T, h *rvHarness, gen int64) (secret, user, permission bool) {
	t.Helper()
	order := rabbitMQVhostCR()
	err := h.mgmt.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: rabbitMQVhostPasswordSecretName(order, h.cluster, gen),
	}, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("reading the password Secret of generation %d: %v", gen, err)
	}
	return err == nil,
		rvTopology(t, h.mgmt, messaging.UserGVK, rabbitMQVhostUserName(order, h.cluster, gen)) != nil,
		rvTopology(t, h.mgmt, messaging.PermissionGVK, rabbitMQVhostPermissionName(order, h.cluster, gen)) != nil
}

// rvUserGenerations counts the order's User CRs.
func rvUserGenerations(t *testing.T, h *rvHarness) int {
	t.Helper()
	return labelledIn(t, h.mgmt, topologyList(messaging.UserGVK), rabbitMQVhostLabelKeys.Name, kuTestName)
}

func rvVhostReady(t *testing.T, h *rvHarness) *metav1.Condition {
	t.Helper()
	return rvCondition(rvGet(t, h), conditionTypeRabbitMQVhostVhostReady)
}

// --- the store and the topology operator ---

func TestRabbitMQVhostProvision_StoreGate(t *testing.T) {
	t.Run("a store that is not ready is a wait", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
			append([]client.Object{rabbitMQVhostCR(), rvControlPlane("")}, rvBus(true)...)...)

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(rabbitMQVhostRequeueAfter))
		cond := rvVhostReady(t, h)
		g.Expect(cond.Reason).To(Equal(reasonServiceAccountStoreNotReady))
		g.Expect(cond.Message).To(ContainSubstring("the password cannot be backed up to OpenBao"))
		delivery := rvCondition(rvGet(t, h), conditionTypeRabbitMQVhostDeliveryReady)
		g.Expect(delivery.Reason).To(Equal(reasonRabbitMQVhostWaitingForPassword))
		g.Expect(rvLabelled(t, h.mgmt)).To(BeZero())
	})

	t.Run("a failed read of the store is returned", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == esoTenantStoreName {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, append([]client.Object{rabbitMQVhostCR(), rvControlPlane("")}, rvReadyInfra()...)...)

		_, err := h.reconcile(context.Background())

		g.Expect(err).To(MatchError(boom))
		g.Expect(rvVhostReady(t, h).Reason).To(Equal(reasonRabbitMQVhostError))
	})
}

// TestRabbitMQVhostProvision_TopologyOperatorNotInstalled answers a cluster that
// serves no Vhost kind with the install it needs, and comes back on the refresh.
func TestRabbitMQVhostProvision_TopologyOperatorNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	noVhosts := &interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "rabbitmq.com", Kind: "Vhost"}, SearchedVersions: []string{"v1beta1"}}
		},
	}
	order := rabbitMQVhostCR()
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, noVhosts, nil,
		append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
	cond := rvVhostReady(t, h)
	g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostTopologyOperatorNotInstalled))
	g.Expect(cond.Message).To(Equal("the cluster serves no vhosts.rabbitmq.com; install the RabbitMQ Messaging " +
		"Topology Operator (deploy/flux-system/releases/messaging-topology-operator.yaml)"))

	ok, leg, err := h.reconciler().provisionVhost(context.Background(), order, rvControlPlane(""), "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse())
	g.Expect(leg.RequeueAfter).To(Equal(orderRefreshAfter), "the leg waits ten minutes for the install")
}

// TestRabbitMQVhostProvision_ForeignVhostIsRefused leaves a Vhost of the
// child's name the order did not create alone and asks for no requeue.
func TestRabbitMQVhostProvision_ForeignVhostIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	foreign := messaging.BuildVhost(rabbitMQVhostChildName(order, ""), "default", "somebody-else", rvBusName, "delete")
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, rvControlPlane(""), foreign}, rvReadyInfra()...)...)

	ok, leg, err := h.reconciler().provisionVhost(ctx, order, rvControlPlane(""), "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse())
	g.Expect(leg.IsZero()).To(BeTrue(), "only removing the foreign Vhost lifts the refusal")
	cond := rvCondition(order, conditionTypeRabbitMQVhostVhostReady)
	g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostError))
	g.Expect(cond.Message).To(ContainSubstring("it was not created by this order"))
	live := rvTopology(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
	g.Expect(live.Object["spec"].(map[string]any)["name"]).To(Equal("somebody-else"), "the foreign Vhost is left as it is")
}

// --- the first generation ---

// TestRabbitMQVhostProvision_FirstMint walks the first pass through the four
// waits: the Vhost, the User, the Permission, then the switch.
func TestRabbitMQVhostProvision_FirstMint(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)...)
	prefix := rabbitMQVhostChildPrefix(order, "")
	vhostName := rabbitMQVhostName(order, "")
	labels := rabbitMQVhostRef(order, "").childLabels()

	// The Vhost first, with the derived name, the policy and the bus.
	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(rabbitMQVhostRequeueAfter))
	vhost := rvTopology(t, h.mgmt, messaging.VhostGVK, prefix+"vhost")
	g.Expect(vhost).NotTo(BeNil())
	g.Expect(vhost.Object["spec"]).To(Equal(map[string]any{
		"name": vhostName, "deletionPolicy": "retain", "rabbitmqClusterReference": map[string]any{"name": rvBusName},
	}))
	g.Expect(vhost.GetLabels()).To(Equal(labels))
	g.Expect(vhost.GetOwnerReferences()).To(BeEmpty())
	g.Expect(rvVhostReady(t, h).Reason).To(Equal(reasonRabbitMQVhostWaitingForVhost))
	g.Expect(rvVhostReady(t, h).Message).To(Equal(fmt.Sprintf("Vhost default/%svhost is not Ready yet", prefix)))
	g.Expect(rvCondition(rvGet(t, h), conditionTypeRabbitMQVhostDeliveryReady).Reason).
		To(Equal(reasonRabbitMQVhostWaitingForPassword))
	g.Expect(rvUserGenerations(t, h)).To(BeZero(), "no user before the vhost is Ready")

	// The password Secret and the User once the Vhost is Ready.
	rvMarkReady(t, h.mgmt, messaging.VhostGVK, prefix+"vhost")
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret := &corev1.Secret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: prefix + "password-v1"}, secret)).To(Succeed())
	g.Expect(string(secret.Data["username"])).To(Equal(vhostName + "-v1"))
	g.Expect(secret.Data["password"]).To(HaveLen(43))
	g.Expect(secret.Labels).To(HaveKeyWithValue("rabbitmq.com/topology-operator", "true"))
	for k, v := range labels {
		g.Expect(secret.Labels).To(HaveKeyWithValue(k, v))
	}
	user := rvTopology(t, h.mgmt, messaging.UserGVK, prefix+"user-v1")
	g.Expect(user).NotTo(BeNil())
	g.Expect(user.Object["spec"]).To(Equal(map[string]any{
		"importCredentialsSecret":  map[string]any{"name": prefix + "password-v1"},
		"rabbitmqClusterReference": map[string]any{"name": rvBusName},
	}))
	g.Expect(user.GetLabels()).To(Equal(labels))
	g.Expect(rvTopology(t, h.mgmt, messaging.PermissionGVK, prefix+"permission-v1")).To(BeNil(),
		"no Permission before the broker holds the user")
	g.Expect(rvVhostReady(t, h).Reason).To(Equal(reasonRabbitMQVhostWaitingForUser))
	g.Expect(rvGet(t, h).Status.Vhost).To(Equal(vhostName))

	// The Permission once the User is Ready.
	rvMarkReady(t, h.mgmt, messaging.UserGVK, prefix+"user-v1")
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	permission := rvTopology(t, h.mgmt, messaging.PermissionGVK, prefix+"permission-v1")
	g.Expect(permission).NotTo(BeNil())
	g.Expect(permission.Object["spec"]).To(Equal(map[string]any{
		"vhost": vhostName, "user": vhostName + "-v1",
		"permissions":              map[string]any{"configure": ".*", "write": ".*", "read": ".*"},
		"rabbitmqClusterReference": map[string]any{"name": rvBusName},
	}))
	g.Expect(permission.GetLabels()).To(Equal(labels))
	cond := rvVhostReady(t, h)
	g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostWaitingForUser))
	g.Expect(cond.Message).To(Equal(fmt.Sprintf(
		"User default/%suser-v1 or Permission default/%spermission-v1 of generation 1 is not Ready yet", prefix, prefix)))
	g.Expect(rvGet(t, h).Status.PasswordGeneration).To(BeZero())

	// The switch once both are Ready.
	rvMarkReady(t, h.mgmt, messaging.PermissionGVK, prefix+"permission-v1")
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := rvGet(t, h)
	cond = rvCondition(got, conditionTypeRabbitMQVhostVhostReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostProvisioned))
	g.Expect(cond.Message).To(Equal(fmt.Sprintf(
		"vhost %q and user %q (generation 1) are provisioned on RabbitmqCluster default/%s",
		vhostName, vhostName+"-v1", rvBusName)))
	g.Expect(got.Status.Vhost).To(Equal(vhostName))
	g.Expect(got.Status.Username).To(Equal(vhostName + "-v1"))
	g.Expect(got.Status.PasswordGeneration).To(Equal(int64(1)))
	g.Expect(got.Status.LastRotation.Time).To(BeTemporally("==", rvTestClock))
	g.Expect(got.Status.NextRotation.Time).To(BeTemporally("==", rvTestClock.Add(720*time.Hour)))
	g.Expect(got.Status.PreviousPasswordGeneration).To(BeZero())
	g.Expect(got.Status.PreviousPasswordDeleteAt).To(BeNil())
}

// TestRabbitMQVhostProvision_BrokerRefusals reports a child the broker refused
// with the topology operator's own message, on the order's wait cadence.
func TestRabbitMQVhostProvision_BrokerRefusals(t *testing.T) {
	order := rabbitMQVhostCR()
	prefix := rabbitMQVhostChildPrefix(order, "")
	cases := []struct {
		name    string
		prepare func(t *testing.T, h *rvHarness)
		want    string
	}{
		{
			name: "a refused Vhost",
			prepare: func(t *testing.T, h *rvHarness) {
				rvMarkFailed(t, h.mgmt, messaging.VhostGVK, prefix+"vhost", "vhost limit reached")
			},
			want: fmt.Sprintf("the topology operator cannot create vhost %q: vhost limit reached", rabbitMQVhostName(order, "")),
		},
		{
			name: "a refused User",
			prepare: func(t *testing.T, h *rvHarness) {
				rvMarkReady(t, h.mgmt, messaging.VhostGVK, prefix+"vhost")
				_, err := h.reconcile(context.Background())
				NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
				rvMarkFailed(t, h.mgmt, messaging.UserGVK, prefix+"user-v1", "user rejected")
			},
			want: fmt.Sprintf("the topology operator cannot create User default/%suser-v1 of generation 1: user rejected", prefix),
		},
		{
			name: "a refused Permission",
			prepare: func(t *testing.T, h *rvHarness) {
				rvMarkReady(t, h.mgmt, messaging.VhostGVK, prefix+"vhost")
				_, err := h.reconcile(context.Background())
				NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
				rvMarkReady(t, h.mgmt, messaging.UserGVK, prefix+"user-v1")
				_, err = h.reconcile(context.Background())
				NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
				rvMarkFailed(t, h.mgmt, messaging.PermissionGVK, prefix+"permission-v1", "no such user")
			},
			want: fmt.Sprintf("the topology operator cannot create Permission default/%spermission-v1 of generation 1: no such user", prefix),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{rabbitMQVhostCR(), rvControlPlane("")}, rvReadyInfra()...)...)
			_, err := h.reconcile(context.Background())
			g.Expect(err).NotTo(HaveOccurred())
			tc.prepare(t, h)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(rabbitMQVhostRequeueAfter))
			cond := rvVhostReady(t, h)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostFailed))
			g.Expect(cond.Message).To(Equal(tc.want))
		})
	}
}

// TestRabbitMQVhostProvision_DeletionPolicyReachesTheVhost applies a changed
// policy to the live Vhost on the next pass.
func TestRabbitMQVhostProvision_DeletionPolicyReachesTheVhost(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	live := rvGet(t, h)
	live.Spec.DeletionPolicy = c5c3v1alpha1.RabbitMQVhostDeletionPolicyDelete
	g.Expect(h.order.Update(ctx, live)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	vhost := rvTopology(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
	g.Expect(vhost.Object["spec"].(map[string]any)["deletionPolicy"]).To(Equal("delete"))
}

// --- rotation ---

// TestRabbitMQVhostProvision_ScheduledRotation rotates on a three-minute
// schedule with a one-minute grace period: the successor is created when the
// schedule is due, the status switches once it is Ready, and the superseded
// generation goes once the grace period ends. A raise during the grace period
// waits for it to end, so no more than two generations are ever held.
func TestRabbitMQVhostProvision_ScheduledRotation(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	order.Spec.Rotation.Interval = &metav1.Duration{Duration: 3 * time.Minute}
	order.Spec.Rotation.GracePeriod = &metav1.Duration{Duration: time.Minute}
	h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	vhostName := rabbitMQVhostName(order, "")
	due := rvTestClock.Add(3 * time.Minute)

	h.now = due.Add(-time.Second)
	result, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeNumerically("<=", time.Second), "the pass wakes when the schedule is due")
	_, user2, _ := rvGeneration(t, h, 2)
	g.Expect(user2).To(BeFalse(), "nothing rotates before the due time")

	h.now = due
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret2, user2, permission2 := rvGeneration(t, h, 2)
	g.Expect(secret2).To(BeTrue())
	g.Expect(user2).To(BeTrue())
	g.Expect(permission2).To(BeFalse())
	cond := rvVhostReady(t, h)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "the delivered user stays valid")
	g.Expect(cond.Message).To(HaveSuffix("generation 2 is being created"))
	g.Expect(rvGet(t, h).Status.PasswordGeneration).To(Equal(int64(1)))
	g.Expect(rvUserGenerations(t, h)).To(Equal(2))

	rvMarkReady(t, h.mgmt, messaging.UserGVK, rabbitMQVhostUserName(order, "", 2))
	h.now = due.Add(10 * time.Second)
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rvGet(t, h).Status.PasswordGeneration).To(Equal(int64(1)), "the Permission is not Ready yet")

	rvMarkReady(t, h.mgmt, messaging.PermissionGVK, rabbitMQVhostPermissionName(order, "", 2))
	switched := due.Add(20 * time.Second)
	h.now = switched
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := rvGet(t, h)
	g.Expect(got.Status.PasswordGeneration).To(Equal(int64(2)))
	g.Expect(got.Status.Username).To(Equal(vhostName + "-v2"))
	g.Expect(got.Status.LastRotation.Time).To(BeTemporally("==", switched))
	g.Expect(got.Status.NextRotation.Time).To(BeTemporally("==", switched.Add(3*time.Minute)))
	g.Expect(got.Status.PreviousPasswordGeneration).To(Equal(int64(1)))
	g.Expect(got.Status.PreviousPasswordDeleteAt.Time).To(BeTemporally("==", switched.Add(time.Minute)))
	secret1, user1, permission1 := rvGeneration(t, h, 1)
	g.Expect(secret1 && user1 && permission1).To(BeTrue(), "the superseded generation stays during the grace period")

	// A raise during the grace period starts nothing: the order never holds a
	// third generation, and generation 1 keeps its full grace period.
	live := rvGet(t, h)
	live.Spec.PasswordGeneration = 3
	g.Expect(h.order.Update(ctx, live)).To(Succeed())
	h.now = switched.Add(30 * time.Second)
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	_, user3, _ := rvGeneration(t, h, 3)
	g.Expect(user3).To(BeFalse(), "no rotation starts while a grace period runs")
	g.Expect(rvUserGenerations(t, h)).To(Equal(2))
	g.Expect(rvGet(t, h).Status.PreviousPasswordGeneration).To(Equal(int64(1)))

	var logs []string
	logger := funcr.New(func(prefix, args string) { logs = append(logs, prefix+" "+args) }, funcr.Options{})
	h.now = got.Status.PreviousPasswordDeleteAt.Time
	_, err = h.reconcile(log.IntoContext(ctx, logger))
	g.Expect(err).NotTo(HaveOccurred())
	secret1, user1, permission1 = rvGeneration(t, h, 1)
	g.Expect(user1).To(BeFalse(), "the superseded User is deleted once the grace period ends")
	g.Expect(permission1).To(BeFalse(), "its Permission goes with it")
	g.Expect(secret1).To(BeTrue(), "its password Secret goes on the pass that finds the User gone")
	g.Expect(rvGet(t, h).Status.PreviousPasswordGeneration).To(Equal(int64(1)))

	_, err = h.reconcile(log.IntoContext(ctx, logger))
	g.Expect(err).NotTo(HaveOccurred())
	secret1, _, _ = rvGeneration(t, h, 1)
	g.Expect(secret1).To(BeFalse())
	got = rvGet(t, h)
	g.Expect(got.Status.PreviousPasswordGeneration).To(BeZero())
	g.Expect(got.Status.PreviousPasswordDeleteAt).To(BeNil())
	g.Expect(strings.Join(logs, "\n")).To(ContainSubstring(
		fmt.Sprintf("deleted superseded user generation 1 (%s-v1)", vhostName)))
	secret2, user2, permission2 = rvGeneration(t, h, 2)
	g.Expect(secret2 && user2 && permission2).To(BeTrue())

	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	_, user3, _ = rvGeneration(t, h, 3)
	g.Expect(user3).To(BeTrue(), "the deferred raise rotates once the grace period has ended")
}

func TestRabbitMQVhostProvision_ScheduleOffSetsNoNextRotation(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	order.Spec.Rotation.Interval = &metav1.Duration{}
	h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	h.now = rvTestClock.Add(100 * 24 * time.Hour)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	got := rvGet(t, h)
	g.Expect(got.Status.NextRotation).To(BeNil())
	g.Expect(got.Status.PasswordGeneration).To(Equal(int64(1)))
	g.Expect(rvUserGenerations(t, h)).To(Equal(1), "nothing rotates with the schedule off")
}

// TestRabbitMQVhostProvision_ManualRotation raises the declared generation: it
// mints that generation directly.
func TestRabbitMQVhostProvision_ManualRotation(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
	live := rvGet(t, h)
	live.Spec.PasswordGeneration = 3
	g.Expect(h.order.Update(ctx, live)).To(Succeed())

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	_, user2, _ := rvGeneration(t, h, 2)
	_, user3, _ := rvGeneration(t, h, 3)
	g.Expect(user2).To(BeFalse())
	g.Expect(user3).To(BeTrue(), "a raise mints the declared generation at once")

	rvMarkReady(t, h.mgmt, messaging.UserGVK, rabbitMQVhostUserName(order, "", 3))
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	rvMarkReady(t, h.mgmt, messaging.PermissionGVK, rabbitMQVhostPermissionName(order, "", 3))
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	got := rvGet(t, h)
	g.Expect(got.Status.PasswordGeneration).To(Equal(int64(3)))
	g.Expect(got.Status.PreviousPasswordGeneration).To(Equal(int64(1)))
}

// TestRabbitMQVhostProvision_LostPiecesRotate rotates a live generation that
// lost its password, its User or its Permission: the broker user cannot be
// trusted to match the delivered Secret any more.
func TestRabbitMQVhostProvision_LostPiecesRotate(t *testing.T) {
	order := rabbitMQVhostCR()
	cases := []struct {
		name string
		lose func(t *testing.T, h *rvHarness)
	}{
		{
			name: "a password Secret without its password",
			lose: func(t *testing.T, h *rvHarness) {
				secret := &corev1.Secret{}
				key := types.NamespacedName{Namespace: "default", Name: rabbitMQVhostPasswordSecretName(order, "", 1)}
				NewGomegaWithT(t).Expect(h.mgmt.Get(context.Background(), key, secret)).To(Succeed())
				delete(secret.Data, rabbitMQVhostPasswordKey)
				NewGomegaWithT(t).Expect(h.mgmt.Update(context.Background(), secret)).To(Succeed())
			},
		},
		{
			name: "a deleted User",
			lose: func(t *testing.T, h *rvHarness) {
				user := rvTopology(t, h.mgmt, messaging.UserGVK, rabbitMQVhostUserName(order, "", 1))
				NewGomegaWithT(t).Expect(h.mgmt.Delete(context.Background(), user)).To(Succeed())
			},
		},
		{
			name: "a deleted Permission",
			lose: func(t *testing.T, h *rvHarness) {
				permission := rvTopology(t, h.mgmt, messaging.PermissionGVK, rabbitMQVhostPermissionName(order, "", 1))
				NewGomegaWithT(t).Expect(h.mgmt.Delete(context.Background(), permission)).To(Succeed())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, rabbitMQVhostCR(), nil)
			tc.lose(t, h)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			_, user2, _ := rvGeneration(t, h, 2)
			g.Expect(user2).To(BeTrue(), "a lost piece of the live generation starts a rotation")
		})
	}
}

// TestRabbitMQVhostProvision_FailedSuccessorIsReplacedByARaise gives up a
// successor the broker refused once a higher generation is declared.
func TestRabbitMQVhostProvision_FailedSuccessorIsReplacedByARaise(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil,
		rvGenerationChildren(order, "", 2, "password-2")...)
	rvMarkFailed(t, h.mgmt, messaging.UserGVK, rabbitMQVhostUserName(order, "", 2), "user rejected")

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rvVhostReady(t, h).Reason).To(Equal(reasonRabbitMQVhostFailed), "the in-flight successor is reported")

	live := rvGet(t, h)
	live.Spec.PasswordGeneration = 3
	g.Expect(h.order.Update(ctx, live)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret2, user2, permission2 := rvGeneration(t, h, 2)
	g.Expect(secret2 || user2 || permission2).To(BeFalse(), "the failed successor is pruned")
	_, user3, _ := rvGeneration(t, h, 3)
	g.Expect(user3).To(BeTrue())
}

// TestRabbitMQVhostProvision_StraysAreDeleted prunes a generation that is
// neither live, nor wanted, nor in its grace period. A User above the live
// generation is a rotation in flight and is not a stray, so the stray here is
// below it.
func TestRabbitMQVhostProvision_StraysAreDeleted(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	order.Spec.PasswordGeneration = 3
	objs := append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)
	objs = append(objs, rvMinted(order, "", 3, "password-3", rvTestClock)...)
	objs = append(objs, rvGenerationChildren(order, "", 1, "stray-1")...)
	objs = append(objs, rvPasswordSecret(order, "", 7, "stray-7"))
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)
	rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
	rvMarkGenerationReady(t, h, 3)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	secret1, user1, permission1 := rvGeneration(t, h, 1)
	g.Expect(secret1 || user1 || permission1).To(BeFalse(), "every piece of a stray generation is deleted")
	secret7, _, _ := rvGeneration(t, h, 7)
	g.Expect(secret7).To(BeFalse(), "a stray password Secret is deleted")
	secret3, user3, permission3 := rvGeneration(t, h, 3)
	g.Expect(secret3 && user3 && permission3).To(BeTrue(), "the live generation stays")
}

// TestRabbitMQVhostProvision_UserListWithoutTheKind answers a cluster that
// serves the Vhost kind but not the User kind with the install it needs.
func TestRabbitMQVhostProvision_UserListWithoutTheKind(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	h := newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, &interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "UserList" {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "rabbitmq.com", Kind: "User"}}
			}
			return cl.List(ctx, list, opts...)
		},
	})

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	cond := rvVhostReady(t, h)
	g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostTopologyOperatorNotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("serves no users.rabbitmq.com"))
}

// TestRabbitMQVhostProvision_KubernetesErrors returns a failed read or write of
// the order's children and reports it as VhostError, so a failure never leaves
// VhostReady at an earlier True.
func TestRabbitMQVhostProvision_KubernetesErrors(t *testing.T) {
	boom := errors.New("boom")
	failApply := &interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			return boom
		},
	}
	failUserList := &interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "UserList" {
				return boom
			}
			return cl.List(ctx, list, opts...)
		},
	}
	failSecretCreate := &interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return boom
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
	failDelete := &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return boom
		},
	}
	cases := []struct {
		name    string
		harness func(t *testing.T) *rvHarness
		wantSub string
	}{
		{
			name: "a failed Vhost apply",
			harness: func(t *testing.T) *rvHarness {
				return newRVHarness(t, c5c3v1alpha1.ManagementCluster, failApply, nil,
					append([]client.Object{rabbitMQVhostCR(), rvControlPlane("")}, rvReadyInfra()...)...)
			},
			wantSub: "ensuring Vhost default/",
		},
		{
			name: "a failed User list",
			harness: func(t *testing.T) *rvHarness {
				return newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, rabbitMQVhostCR(), failUserList)
			},
			wantSub: "listing the order's UserList",
		},
		{
			name: "a failed write of the first password Secret",
			harness: func(t *testing.T) *rvHarness {
				order := rabbitMQVhostCR()
				vhost := messaging.BuildVhost(rabbitMQVhostChildName(order, ""), "default", rabbitMQVhostName(order, ""),
					rvBusName, "retain")
				vhost.SetLabels(rabbitMQVhostRef(order, "").childLabels())
				h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, failSecretCreate, nil,
					append([]client.Object{order, rvControlPlane(""), vhost}, rvReadyInfra()...)...)
				rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
				return h
			},
			wantSub: "ensuring the password Secret of generation 1",
		},
		{
			name: "a failed delete of a stray generation",
			harness: func(t *testing.T) *rvHarness {
				order := rabbitMQVhostCR()
				return newMintedRVHarness(t, c5c3v1alpha1.ManagementCluster, order, failDelete,
					rvPasswordSecret(order, "", 7, "stray-7"))
			},
			wantSub: "deleting stray vhost user generation",
		},
		{
			name: "a failed delete at the end of the grace period",
			harness: func(t *testing.T) *rvHarness {
				order := rabbitMQVhostCR()
				objs := append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)
				objs = append(objs, rvMinted(order, "", 2, "password-2", rvTestClock)...)
				objs = append(objs, rvGenerationChildren(order, "", 1, "password-1")...)
				order.Status.PreviousPasswordGeneration = 1
				order.Status.PreviousPasswordDeleteAt = &metav1.Time{Time: rvTestClock}
				h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, failDelete, nil, objs...)
				rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
				rvMarkGenerationReady(t, h, 1)
				rvMarkGenerationReady(t, h, 2)
				return h
			},
			wantSub: "of superseded generation 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := tc.harness(t)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(boom))
			cond := rvVhostReady(t, h)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostError))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSub))
		})
	}
}

func TestVhostNextRotation(t *testing.T) {
	g := NewGomegaWithT(t)

	order := rabbitMQVhostCR()
	g.Expect(vhostNextRotation(order, time.Hour)).To(BeNil(), "nothing is scheduled before the first generation")
	order.Status.LastRotation = &metav1.Time{Time: rvTestClock}
	g.Expect(vhostNextRotation(order, time.Hour).Time).To(BeTemporally("==", rvTestClock.Add(time.Hour)))
	g.Expect(vhostNextRotation(order, 0)).To(BeNil(), "an interval of 0s schedules nothing")
}

func TestGeneratedVhostPasswordMutator(t *testing.T) {
	g := NewGomegaWithT(t)

	secret := &corev1.Secret{Data: map[string][]byte{}}
	g.Expect(generatedVhostPasswordMutator(secret)).To(Succeed())
	first := string(secret.Data[rabbitMQVhostPasswordKey])
	g.Expect(first).To(HaveLen(43))
	g.Expect(generatedVhostPasswordMutator(secret)).To(Succeed())
	g.Expect(string(secret.Data[rabbitMQVhostPasswordKey])).To(Equal(first), "a password present is preserved")
}
