// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// TestReconcileTransportURLSecret_WaitsForBrokerCredentials covers the managed
// cluster that has not published its default user yet: the step reports
// SecretsReady=False / WaitingForMessagingCredentials, polls, and returns
// neither a digest nor an egress port, so nothing downstream stamps a
// pod-template annotation or opens a NetworkPolicy peer against a URL that does
// not exist.
func TestReconcileTransportURLSecret_WaitsForBrokerCredentials(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron, rabbitmqCluster(testRabbitmqClusterName, testNamespace))

	res, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	g.Expect(port).To(Equal(int32(0)))

	cond := neutronCondition(neutron, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(neutron.Name)}
	g.Expect(r.Get(context.Background(), key, &derived)).NotTo(Succeed(),
		"no derived Secret is written while the broker credentials are missing")
}

// TestReconcileTransportURLSecret_ManagedDerivesURLAndPort covers the ready
// cluster: the derived Secret is materialised, the digest is non-empty, and the
// egress port is the one the broker published rather than the AMQP default.
func TestReconcileTransportURLSecret_ManagedDerivesURLAndPort(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	neutron := validNeutron()
	cluster := rabbitmqCluster(testRabbitmqClusterName, testNamespace)
	c := neutronFakeClientBuilder(neutron, cluster, rabbitmqDefaultUserSecret("5671")).
		WithStatusSubresource(cluster).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c,
		client.ObjectKeyFromObject(cluster), testRabbitmqUserSecret)).To(Succeed())

	res, digest, port, err := r.reconcileTransportURLSecret(ctx, r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())
	g.Expect(port).To(Equal(int32(5671)), "the egress port is the transport URL's port")

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(neutron.Name)}
	g.Expect(r.Get(ctx, key, &derived)).To(Succeed())
	g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(HavePrefix("rabbit://"))

	// A second pass returns the identical digest: it drives the pod roll, so a
	// value that moved without the credentials moving would roll every pass.
	_, digest2, port2, err := r.reconcileTransportURLSecret(ctx, r.Client, neutron)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
	g.Expect(port2).To(Equal(port))
}

// TestReconcileTransportURLSecret_BrownfieldKeepsURLPort covers the brownfield
// bus: the URL is copied verbatim, so the port the broker's administrator chose
// is the one the NetworkPolicy has to open.
func TestReconcileTransportURLSecret_BrownfieldKeepsURLPort(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "external-bus", Key: commonv1.DefaultTransportURLSecretKey},
	}
	upstream := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: testNamespace},
		Data: map[string][]byte{
			commonv1.DefaultTransportURLSecretKey: []byte("rabbit://neutron:pw@bus.example.com:5673/"),
		},
	}
	r := newNeutronTestReconciler(neutron, upstream)

	_, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).NotTo(BeEmpty())
	g.Expect(port).To(Equal(int32(5673)))
}
