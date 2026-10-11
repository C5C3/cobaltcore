// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the RabbitMQVhost delivery leg: the broker address per cluster,
// the OpenBao backup, the Secret beside the order, its repair, the switch to a
// rotated user, and the result of a converged pass.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// rvDeliveredAddress is the host and port an order on cluster is delivered:
// the in-cluster Service on the management cluster, the published endpoint
// elsewhere.
func rvDeliveredAddress(cluster string) (string, int32) {
	if cluster == c5c3v1alpha1.ManagementCluster {
		return rvBusHost, 5672
	}
	host, port, _ := net.SplitHostPort(rvPublished)
	p, _ := strconv.ParseInt(port, 10, 32)
	return host, int32(p)
}

// rvTransportURL is the transport URL generation gen with password is
// delivered under on cluster.
func rvTransportURL(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64, password string) string {
	host, port := rvDeliveredAddress(cluster)
	url, _ := messaging.BuildTransportURLForVhost(rabbitMQVhostBrokerUserName(order, cluster, gen), password, host, port,
		rabbitMQVhostName(order, cluster))
	return url
}

// rvPushed returns the order's PushSecret as ESO leaves it once it has pushed
// the transport URL of generation gen with password: the hash, the
// syncedResourceVersion recorded when it was stamped, and a status that has
// moved past it.
func rvPushed(order *c5c3v1alpha1.RabbitMQVhost, cluster string, gen int64, password string) *esov1alpha1.PushSecret {
	sum := sha256.Sum256([]byte(rvTransportURL(order, cluster, gen, password)))
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name: rabbitMQVhostPushSecretName(order, cluster), Namespace: "default",
			Labels: rabbitMQVhostRef(order, cluster).childLabels(),
			Annotations: map[string]string{
				rabbitMQVhostPushContentHashAnnotation:  hex.EncodeToString(sum[:]),
				rabbitMQVhostPushSyncedBeforeAnnotation: "1-before-push",
			},
		},
		Status: esov1alpha1.PushSecretStatus{
			Conditions:            []esov1alpha1.PushSecretStatusCondition{{Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue}},
			SyncedResourceVersion: "1-pushed",
		},
	}
}

// newDeliveredRVHarness seeds an order on cluster with generation 1 delivered
// and its transport URL pushed to OpenBao.
func newDeliveredRVHarness(
	t *testing.T, cluster string, order *c5c3v1alpha1.RabbitMQVhost, orderFuncs *interceptor.Funcs, extra ...client.Object,
) *rvHarness {
	t.Helper()
	objs := append([]client.Object{order, rvControlPlane(cluster)}, rvReadyInfra()...)
	objs = append(objs, rvMinted(order, cluster, 1, "password-1", rvTestClock)...)
	objs = append(objs, rvPushed(order, cluster, 1, "password-1"))
	h := newRVHarness(t, cluster, nil, orderFuncs, append(objs, extra...)...)
	rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, cluster))
	rvMarkGenerationReady(t, h, 1)
	return h
}

// rvDelivered reads the delivered Secret off the order's cluster.
func rvDelivered(t *testing.T, h *rvHarness) (*corev1.Secret, bool) {
	t.Helper()
	secret := &corev1.Secret{}
	err := h.order.Get(context.Background(),
		types.NamespacedName{Namespace: kuTestNamespace, Name: "workflow-credentials"}, secret)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("reading the delivered Secret: %v", err)
	}
	return secret, true
}

func rvDeliveryReady(t *testing.T, h *rvHarness) *metav1.Condition {
	t.Helper()
	return rvCondition(rvGet(t, h), conditionTypeRabbitMQVhostDeliveryReady)
}

// TestRabbitMQVhostDelivery_DeliversOnEachCluster is the contract on both
// clusters: the backup in the ControlPlane's namespace, the Secret beside the
// order with its six keys and owner, the status, and the result of a
// converged pass.
func TestRabbitMQVhostDelivery_DeliversOnEachCluster(t *testing.T) {
	cases := []struct {
		cluster    string
		wantHost   string
		wantResult time.Duration
	}{
		{cluster: c5c3v1alpha1.ManagementCluster, wantHost: rvBusHost, wantResult: 720 * time.Hour},
		{cluster: kuTestCluster, wantHost: "broker.example.test", wantResult: orderRefreshAfter},
	}
	for _, tc := range cases {
		t.Run(orderRef{Cluster: tc.cluster}.location(), func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			order := rabbitMQVhostCR()
			h := newDeliveredRVHarness(t, tc.cluster, order, nil)
			vhost := rabbitMQVhostName(order, tc.cluster)
			username := vhost + "-v1"

			result, err := h.reconcile(ctx)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(tc.wantResult))

			secret, found := rvDelivered(t, h)
			g.Expect(found).To(BeTrue())
			g.Expect(secret.Data).To(HaveLen(6))
			g.Expect(string(secret.Data["transport_url"])).To(Equal(
				"rabbit://" + username + ":password-1@" + tc.wantHost + ":5672/" + vhost))
			g.Expect(string(secret.Data["host"])).To(Equal(tc.wantHost))
			g.Expect(string(secret.Data["port"])).To(Equal("5672"))
			g.Expect(string(secret.Data["username"])).To(Equal(username))
			g.Expect(string(secret.Data["password"])).To(Equal("password-1"))
			g.Expect(string(secret.Data["vhost"])).To(Equal(vhost))
			g.Expect(secret.OwnerReferences).To(HaveLen(1))
			g.Expect(secret.OwnerReferences[0].Kind).To(Equal("RabbitMQVhost"))
			g.Expect(*secret.OwnerReferences[0].Controller).To(BeTrue())
			g.Expect(secret.Labels).To(Equal(map[string]string{
				rabbitMQVhostLabelKeys.Name: kuTestName, rabbitMQVhostLabelKeys.Namespace: kuTestNamespace,
			}), "the delivered Secret carries no cluster label")

			push := &esov1alpha1.PushSecret{}
			g.Expect(h.mgmt.Get(ctx, types.NamespacedName{
				Namespace: "default", Name: rabbitMQVhostPushSecretName(order, tc.cluster),
			}, push)).To(Succeed())
			g.Expect(push.Spec.Data).To(HaveLen(1))
			g.Expect(push.Spec.Data[0].Match.RemoteRef.RemoteKey).To(MatchRegexp(
				`^openstack/rabbitmq/default/workflow-[0-9a-f]{8}-vhost/credentials$`))
			source := &corev1.Secret{}
			g.Expect(h.mgmt.Get(ctx, types.NamespacedName{
				Namespace: "default", Name: rabbitMQVhostSourceSecretName(order, tc.cluster),
			}, source)).To(Succeed())
			g.Expect(source.Data).To(Equal(secret.Data), "the backup carries what is delivered")

			got := rvGet(t, h)
			g.Expect(got.Status.Host).To(Equal(tc.wantHost))
			g.Expect(got.Status.Port).To(Equal(int32(5672)))
			g.Expect(got.Status.SecretName).To(Equal("workflow-credentials"))
			g.Expect(got.Status.SecretKeys).To(Equal([]string{"transport_url", "host", "port", "username", "password", "vhost"}))
			cond := rvCondition(got, conditionTypeRabbitMQVhostDeliveryReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDelivered))
			g.Expect(cond.Message).To(ContainSubstring(
				`credentials are delivered in Secret "workflow-credentials" (keys transport_url, host, port, username, password, vhost)`))
			g.Expect(rvCondition(got, conditionTypeReady).Status).To(Equal(metav1.ConditionTrue))
		})
	}
}

// TestRabbitMQVhostDelivery_WaitsForTheBackup writes the source Secret and the
// PushSecret but delivers nothing before ESO has pushed the current document.
func TestRabbitMQVhostDelivery_WaitsForTheBackup(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	objs := append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)
	objs = append(objs, rvMinted(order, "", 1, "password-1", rvTestClock)...)
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)
	rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
	rvMarkGenerationReady(t, h, 1)

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(rabbitMQVhostRequeueAfter))
	cond := rvDeliveryReady(t, h)
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserBackupNotSynced))
	g.Expect(cond.Message).To(ContainSubstring("the credentials are not backed up to OpenBao yet"))
	_, found := rvDelivered(t, h)
	g.Expect(found).To(BeFalse(), "nothing is delivered before the backup")
	push := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: rabbitMQVhostPushSecretName(order, "")},
		push)).To(Succeed())
	g.Expect(push.Annotations).To(HaveKey(rabbitMQVhostPushContentHashAnnotation))
	g.Expect(push.Spec.DeletionPolicy).To(Equal(esov1alpha1.PushSecretDeletionPolicyDelete))
}

// TestRabbitMQVhostDelivery_UnpublishedMessaging covers an order on a target
// cluster while the ControlPlane publishes no broker: the leg refuses with a
// zero result, the pass turns that into the refresh, and the vhost stays
// provisioned.
func TestRabbitMQVhostDelivery_UnpublishedMessaging(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newDeliveredRVHarness(t, kuTestCluster, order, nil)
	cp := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: "cp"}, cp)).To(Succeed())
	cp.Spec.Infrastructure.PublishedMessagingEndpoint = ""
	g.Expect(h.mgmt.Update(ctx, cp)).To(Succeed())

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
	cond := rvDeliveryReady(t, h)
	g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostMessagingNotPublished))
	g.Expect(cond.Message).To(ContainSubstring(`the order lives on target cluster "edge-1"`))
	g.Expect(cond.Message).To(ContainSubstring("spec.infrastructure.publishedMessagingEndpoint on ControlPlane default/cp"))
	g.Expect(rvVhostReady(t, h).Status).To(Equal(metav1.ConditionTrue))
	_, found := rvDelivered(t, h)
	g.Expect(found).To(BeFalse())

	live := rvGet(t, h)
	leg, err := h.reconciler().deliverCredentials(ctx, h.order, live, cp, kuTestCluster, rvBusHost, 5672)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(leg.IsZero()).To(BeTrue(), "the leg itself asks for no requeue")
}

// TestRabbitMQVhostDelivery_InvalidPublishedPort refuses a published port the
// schema admits but no broker listens on.
func TestRabbitMQVhostDelivery_InvalidPublishedPort(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newDeliveredRVHarness(t, kuTestCluster, order, nil)
	cp := rvControlPlane(kuTestCluster)
	cp.Spec.Infrastructure.PublishedMessagingEndpoint = "broker.example.test:99999"

	live := rvGet(t, h)
	leg, err := h.reconciler().deliverCredentials(ctx, h.order, live, cp, kuTestCluster, rvBusHost, 5672)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(leg.IsZero()).To(BeTrue())
	cond := rvCondition(live, conditionTypeRabbitMQVhostDeliveryReady)
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDeliveryError))
	g.Expect(cond.Message).To(ContainSubstring(`publishedMessagingEndpoint "broker.example.test:99999"`))
	g.Expect(cond.Message).To(ContainSubstring("between 1 and 65535"))
	_, found := rvDelivered(t, h)
	g.Expect(found).To(BeFalse())
}

// TestRabbitMQVhostDelivery_IPv6PublishedEndpoint delivers a bracketed IPv6
// endpoint as the bare host and the bracketed transport URL.
func TestRabbitMQVhostDelivery_IPv6PublishedEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	h := newDeliveredRVHarness(t, kuTestCluster, order, nil)
	cp := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(ctx, types.NamespacedName{Namespace: "default", Name: "cp"}, cp)).To(Succeed())
	cp.Spec.Infrastructure.PublishedMessagingEndpoint = "[fd00::1]:5672"
	g.Expect(h.mgmt.Update(ctx, cp)).To(Succeed())

	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rvDeliveryReady(t, h).Reason).To(Equal(reasonKeystoneUserBackupNotSynced))
	// ESO pushes the document stamped for the new address.
	push := &esov1alpha1.PushSecret{}
	pushKey := types.NamespacedName{Namespace: "default", Name: rabbitMQVhostPushSecretName(order, kuTestCluster)}
	g.Expect(h.mgmt.Get(ctx, pushKey, push)).To(Succeed())
	push.Status.SyncedResourceVersion = "2-pushed"
	g.Expect(h.mgmt.Update(ctx, push)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	secret, found := rvDelivered(t, h)
	g.Expect(found).To(BeTrue())
	g.Expect(string(secret.Data["host"])).To(Equal("fd00::1"))
	g.Expect(string(secret.Data["port"])).To(Equal("5672"))
	g.Expect(string(secret.Data["transport_url"])).To(ContainSubstring("@[fd00::1]:5672/"))
	g.Expect(rvGet(t, h).Status.Host).To(Equal("fd00::1"))
}

func TestSplitBrokerEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		wantHost string
		wantPort int32
		wantOK   bool
	}{
		{endpoint: "broker.example.test:5672", wantHost: "broker.example.test", wantPort: 5672, wantOK: true},
		{endpoint: "broker.example.test:1", wantHost: "broker.example.test", wantPort: 1, wantOK: true},
		{endpoint: "broker.example.test:65535", wantHost: "broker.example.test", wantPort: 65535, wantOK: true},
		{endpoint: "[::1]:5672", wantHost: "::1", wantPort: 5672, wantOK: true},
		{endpoint: "broker.example.test:65536"},
		{endpoint: "broker.example.test:0"},
		{endpoint: "broker.example.test:00000"},
		{endpoint: "broker.example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			g := NewGomegaWithT(t)

			host, port, ok := splitBrokerEndpoint(tc.endpoint)

			g.Expect(ok).To(Equal(tc.wantOK))
			g.Expect(host).To(Equal(tc.wantHost))
			g.Expect(port).To(Equal(tc.wantPort))
		})
	}
}

func TestRabbitMQVhostDelivery_Refusals(t *testing.T) {
	t.Run("a Secret of the delivered name the order does not own", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "workflow-credentials", Namespace: kuTestNamespace},
			Data:       map[string][]byte{"password": []byte("theirs")},
		}
		h := newDeliveredRVHarness(t, c5c3v1alpha1.ManagementCluster, rabbitMQVhostCR(), nil, foreign)

		_, err := h.reconcile(ctx)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rvDeliveryReady(t, h).Reason).To(Equal(reasonKeystoneUserDeliveryRefused))
		secret, _ := rvDelivered(t, h)
		g.Expect(string(secret.Data["password"])).To(Equal("theirs"), "a foreign Secret is never overwritten")
	})

	t.Run("a password Secret without its password", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := rabbitMQVhostCR()
		h := newDeliveredRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil)
		live := rvGet(t, h)
		live.Status.PasswordGeneration = 4

		result, err := h.reconciler().deliverCredentials(context.Background(), h.order, live, rvControlPlane(""), "",
			rvBusHost, 5672)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(rabbitMQVhostRequeueAfter))
		cond := rvCondition(live, conditionTypeRabbitMQVhostDeliveryReady)
		g.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostWaitingForPassword))
		g.Expect(cond.Message).To(Equal("the password of generation 4 is not available yet"))
	})

	t.Run("nothing provisioned yet", func(t *testing.T) {
		g := NewGomegaWithT(t)
		h := newDeliveredRVHarness(t, c5c3v1alpha1.ManagementCluster, rabbitMQVhostCR(), nil)
		live := rvGet(t, h)
		live.Status.PasswordGeneration = 0

		result, err := h.reconciler().deliverCredentials(context.Background(), h.order, live, rvControlPlane(""), "",
			rvBusHost, 5672)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.IsZero()).To(BeTrue())
		g.Expect(rvCondition(live, conditionTypeRabbitMQVhostDeliveryReady).Reason).
			To(Equal(reasonRabbitMQVhostWaitingForPassword))
	})

	t.Run("a failed read of the password Secret", func(t *testing.T) {
		g := NewGomegaWithT(t)
		boom := errors.New("boom")
		order := rabbitMQVhostCR()
		objs := append([]client.Object{order, rvControlPlane("")}, rvReadyInfra()...)
		objs = append(objs, rvMinted(order, "", 1, "password-1", rvTestClock)...)
		h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == rabbitMQVhostPasswordSecretName(order, "", 1) {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, nil, objs...)

		_, err := h.reconciler().deliverCredentials(context.Background(), h.order, rvGet(t, h), rvControlPlane(""), "",
			rvBusHost, 5672)

		g.Expect(err).To(MatchError(boom))
	})
}

// TestRabbitMQVhostDelivery_ForeignPushSecretIsRefused seeds a PushSecret at
// the order's child name that carries the same order's labels for another
// cluster: the delivery refuses to adopt it, leaves it as it is and delivers
// nothing.
func TestRabbitMQVhostDelivery_ForeignPushSecretIsRefused(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	foreignLabels := rabbitMQVhostRef(order, kuTestCluster).childLabels()
	push := rvPushed(order, "", 1, "password-1")
	push.SetLabels(foreignLabels)
	objs := append([]client.Object{order, rvControlPlane(""), push}, rvReadyInfra()...)
	objs = append(objs, rvMinted(order, "", 1, "password-1", rvTestClock)...)
	h := newRVHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)
	rvMarkReady(t, h.mgmt, messaging.VhostGVK, rabbitMQVhostChildName(order, ""))
	rvMarkGenerationReady(t, h, 1)

	_, err := h.reconcile(ctx)

	g.Expect(err).To(MatchError(errOrderChildForeign))
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))
	g.Expect(rvDeliveryReady(t, h).Reason).To(Equal(reasonKeystoneUserDeliveryError))
	live := &esov1alpha1.PushSecret{}
	g.Expect(h.mgmt.Get(ctx, client.ObjectKeyFromObject(push), live)).To(Succeed())
	g.Expect(live.Labels).To(Equal(foreignLabels), "the foreign PushSecret is not claimed")
	_, found := rvDelivered(t, h)
	g.Expect(found).To(BeFalse())
}

// TestRabbitMQVhostDelivery_ApplyErrorIsReported fails the apply on the
// order's cluster only, which for an order on a target cluster is the
// delivered Secret alone.
func TestRabbitMQVhostDelivery_ApplyErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	h := newDeliveredRVHarness(t, kuTestCluster, rabbitMQVhostCR(), &interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errors.New("admission webhook denied the request")
		},
	})

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("delivering Secret"))
	cond := rvDeliveryReady(t, h)
	g.Expect(cond.Reason).To(Equal(reasonKeystoneUserDeliveryError))
	g.Expect(cond.Message).To(ContainSubstring("admission webhook denied the request"))
}

// TestRabbitMQVhostDelivery_RepairsTheSecret rewrites an edited password and
// re-creates a deleted Secret.
func TestRabbitMQVhostDelivery_RepairsTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	h := newDeliveredRVHarness(t, c5c3v1alpha1.ManagementCluster, rabbitMQVhostCR(), nil)
	_, err := h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	secret, _ := rvDelivered(t, h)
	secret.Data["password"] = []byte("Zm9v")
	g.Expect(h.order.Update(ctx, secret)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret, _ = rvDelivered(t, h)
	g.Expect(string(secret.Data["password"])).To(Equal("password-1"), "an edited password is rewritten")

	g.Expect(h.order.Delete(ctx, secret)).To(Succeed())
	_, err = h.reconcile(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	secret, found := rvDelivered(t, h)
	g.Expect(found).To(BeTrue(), "a deleted Secret is re-created")
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
}

// TestRabbitMQVhostDelivery_RotationSwitchesTheSecret delivers the successor
// in the pass the status switches to it, once its document is pushed.
func TestRabbitMQVhostDelivery_RotationSwitchesTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	order := rabbitMQVhostCR()
	due := rvTestClock.Add(720 * time.Hour)
	h := newDeliveredRVHarness(t, c5c3v1alpha1.ManagementCluster, order, nil,
		rvGenerationChildren(order, "", 2, "password-2")...)
	rvMarkGenerationReady(t, h, 2)
	push := &esov1alpha1.PushSecret{}
	pushKey := types.NamespacedName{Namespace: "default", Name: rabbitMQVhostPushSecretName(order, "")}
	g.Expect(h.mgmt.Get(ctx, pushKey, push)).To(Succeed())
	push.Annotations = rvPushed(order, "", 2, "password-2").Annotations
	push.Status.SyncedResourceVersion = "2-pushed"
	g.Expect(h.mgmt.Update(ctx, push)).To(Succeed())
	h.now = due

	result, err := h.reconcile(ctx)

	g.Expect(err).NotTo(HaveOccurred())
	got := rvGet(t, h)
	g.Expect(got.Status.PasswordGeneration).To(Equal(int64(2)))
	secret, _ := rvDelivered(t, h)
	g.Expect(string(secret.Data["username"])).To(Equal(rabbitMQVhostName(order, "") + "-v2"))
	g.Expect(string(secret.Data["password"])).To(Equal("password-2"))
	g.Expect(rvDeliveryReady(t, h).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(result.RequeueAfter).To(Equal(24*time.Hour), "the grace period ends before the next rotation is due")
}
