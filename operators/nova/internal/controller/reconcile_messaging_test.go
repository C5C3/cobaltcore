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
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// brownfieldNova returns the fixture pointed at an external broker through
// spec.messaging.secretRef, the mode the upstream Secret cases exercise.
func brownfieldNova(secretName string) *novav1alpha1.Nova {
	nova := validNova()
	nova.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: secretName, Key: commonv1.DefaultTransportURLSecretKey},
	}
	return nova
}

// brownfieldBusSecret returns the external broker's Secret carrying url under
// the default transport_url key.
func brownfieldBusSecret(name, url string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(url)},
	}
}

// assertWaitingForCredentials asserts the shape every incomplete upstream
// produces: a poll, no URL, no digest, no egress port, SecretsReady=False, and
// no derived Secret, so nothing downstream writes a compute contract, stamps a
// pod-template annotation or opens a NetworkPolicy peer against a URL that does
// not exist.
func assertWaitingForCredentials(t *testing.T, r *NovaReconciler, nova *novav1alpha1.Nova) {
	t.Helper()
	g := NewGomegaWithT(t)

	res, transportURL, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(transportURL).To(BeEmpty())
	g.Expect(digest).To(BeEmpty())
	g.Expect(port).To(Equal(int32(0)))

	cond := novaCondition(nova, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(nova.Name)}
	g.Expect(r.Get(context.Background(), key, &derived)).NotTo(Succeed(),
		"no derived Secret is written while the broker credentials are missing")
}

// readyBrokerReconciler builds a reconciler whose managed RabbitmqCluster has
// published its default-user Secret reference, the state the flow reads the four
// halves of the URL from.
func readyBrokerReconciler(t *testing.T, nova *novav1alpha1.Nova, userSecret *corev1.Secret) *NovaReconciler {
	t.Helper()
	g := NewGomegaWithT(t)
	cluster := rabbitmqCluster(testRabbitmqClusterName, testNamespace)
	c := novaFakeClientBuilder(nova, cluster, userSecret).
		WithStatusSubresource(cluster).
		Build()
	g.Expect(simulators.SimulateRabbitmqClusterReady(context.Background(), c,
		client.ObjectKeyFromObject(cluster), testRabbitmqUserSecret)).To(Succeed())
	return &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}
}

// TestReconcileTransportURLSecret_MissingClusterWaits covers the GitOps ordering
// in which the Nova is applied before its broker exists.
func TestReconcileTransportURLSecret_MissingClusterWaits(t *testing.T) {
	nova := validNova()
	assertWaitingForCredentials(t, newNovaTestReconciler(nova), nova)
}

// TestReconcileTransportURLSecret_ClusterWithoutDefaultUserWaits covers the
// broker that exists but has not published status.defaultUser.secretReference
// yet.
func TestReconcileTransportURLSecret_ClusterWithoutDefaultUserWaits(t *testing.T) {
	nova := validNova()
	r := newNovaTestReconciler(nova, rabbitmqCluster(testRabbitmqClusterName, testNamespace))
	assertWaitingForCredentials(t, r, nova)
}

// TestReconcileTransportURLSecret_DefaultUserSecretMissingKeyWaits covers the
// default-user Secret that exists but is missing one of the four halves of the
// URL: a URL assembled from it would name an empty host.
func TestReconcileTransportURLSecret_DefaultUserSecretMissingKeyWaits(t *testing.T) {
	nova := validNova()
	assertWaitingForCredentials(t, readyBrokerReconciler(t, nova, rabbitmqDefaultUserSecret("5672", "host")), nova)
}

// TestReconcileTransportURLSecret_MissingBrownfieldSecretWaits covers the
// external broker whose credentials Secret has not been created yet.
func TestReconcileTransportURLSecret_MissingBrownfieldSecretWaits(t *testing.T) {
	nova := brownfieldNova("external-bus")
	assertWaitingForCredentials(t, newNovaTestReconciler(nova), nova)
}

// TestReconcileTransportURLSecret_EmptyBrownfieldKeyWaits covers the Secret that
// exists but carries an empty transport_url, the shape a half-written external
// Secret has.
func TestReconcileTransportURLSecret_EmptyBrownfieldKeyWaits(t *testing.T) {
	nova := brownfieldNova("external-bus")
	r := newNovaTestReconciler(nova, brownfieldBusSecret("external-bus", ""))
	assertWaitingForCredentials(t, r, nova)
}

// TestReconcileTransportURLSecret_NonRabbitSchemeErrors covers the external URL
// naming a driver Nova is not configured for. It is a returned error rather than
// a wait, and the error names the scheme it found: no amount of polling turns
// amqp:// into rabbit://.
func TestReconcileTransportURLSecret_NonRabbitSchemeErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := brownfieldNova("external-bus")
	r := newNovaTestReconciler(nova,
		brownfieldBusSecret("external-bus", "amqp://nova:pw@bus.example.com:5672/"))

	_, transportURL, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, nova)

	g.Expect(err).To(MatchError(ContainSubstring("scheme must be rabbit")))
	g.Expect(err).To(MatchError(ContainSubstring(`got "amqp"`)))
	g.Expect(transportURL).To(BeEmpty())
	g.Expect(digest).To(BeEmpty())
	g.Expect(port).To(Equal(int32(0)))
}

// TestReconcileTransportURLSecret_ManagedDerivesURLAndPort covers the ready
// cluster: the derived Secret is materialised, the returned URL is the one it
// carries (the compute contract hands that same URL to a nova-compute outside
// this cluster), the digest is non-empty, and the egress port is the one the
// broker published rather than the AMQP default.
func TestReconcileTransportURLSecret_ManagedDerivesURLAndPort(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	nova := validNova()
	r := readyBrokerReconciler(t, nova, rabbitmqDefaultUserSecret("5671"))

	res, transportURL, digest, port, err := r.reconcileTransportURLSecret(ctx, r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(transportURL).To(HavePrefix("rabbit://"))
	g.Expect(digest).NotTo(BeEmpty())
	g.Expect(port).To(Equal(int32(5671)), "the egress port is the transport URL's port")

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(nova.Name)}
	g.Expect(r.Get(ctx, key, &derived)).To(Succeed())
	g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(transportURL),
		"the returned URL is the one the derived Secret carries")

	// A second pass returns the identical digest: it drives the pod roll, so a
	// value that moved without the credentials moving would roll every pass.
	_, url2, digest2, port2, err := r.reconcileTransportURLSecret(ctx, r.Client, nova)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(url2).To(Equal(transportURL))
	g.Expect(digest2).To(Equal(digest))
	g.Expect(port2).To(Equal(port))
}

// TestReconcileTransportURLSecret_BrownfieldKeepsURLPort covers the brownfield
// bus: the URL is copied verbatim, so the port the broker's administrator chose
// is the one the NetworkPolicy has to open.
func TestReconcileTransportURLSecret_BrownfieldKeepsURLPort(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := brownfieldNova("external-bus")
	external := "rabbit://nova:pw@bus.example.com:5673/"
	r := newNovaTestReconciler(nova, brownfieldBusSecret("external-bus", external))

	_, transportURL, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(transportURL).To(Equal(external))
	g.Expect(digest).NotTo(BeEmpty())
	g.Expect(port).To(Equal(int32(5673)))
}

// TestReconcileTransportURLSecret_BrownfieldURLTheCellTemplateCannotExpandRejected
// covers the brownfield URLs the cell mapping's {hostname}:{port} template
// breaks: nova fills a missing port with "None" and strips an IPv6 literal of its
// brackets, so cell1 would carry a transport URL nothing can connect with. Both
// set SecretsReady=False with reason TransportURLRejected and poll, and neither
// message quotes the URL, which carries the broker password. The refusal comes
// before the write: a derived Secret that already carries an accepted URL keeps
// it, because every workload reads that Secret on its next restart. A multi-host
// URL expands intact and passes.
func TestReconcileTransportURLSecret_BrownfieldURLTheCellTemplateCannotExpandRejected(t *testing.T) {
	const accepted = "rabbit://nova:pw@bus.example.com:5672/"
	for _, tc := range []struct {
		name         string
		external     string
		wantRejected bool
	}{
		{name: "no explicit port", external: "rabbit://nova:pw@bus.example.com/", wantRejected: true},
		{name: "IPv6 literal host", external: "rabbit://nova:pw@[fd00::1]:5672/", wantRejected: true},
		{name: "multi-host URL", external: "rabbit://nova:pw@bus-0:5672,nova:pw@bus-1:5672/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			nova := brownfieldNova("external-bus")
			r := newNovaTestReconciler(nova, brownfieldBusSecret("external-bus", accepted))
			_, _, _, _, err := r.reconcileTransportURLSecret(ctx, r.Client, nova)
			g.Expect(err).NotTo(HaveOccurred())

			// The broker Secret is rewritten after the derived Secret was
			// materialised from the accepted URL.
			var bus corev1.Secret
			g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "external-bus"}, &bus)).To(Succeed())
			bus.Data[commonv1.DefaultTransportURLSecretKey] = []byte(tc.external)
			g.Expect(r.Update(ctx, &bus)).To(Succeed())

			res, transportURL, digest, port, err := r.reconcileTransportURLSecret(ctx, r.Client, nova)
			g.Expect(err).NotTo(HaveOccurred())

			var derived corev1.Secret
			key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(nova.Name)}
			g.Expect(r.Get(ctx, key, &derived)).To(Succeed())

			if !tc.wantRejected {
				g.Expect(transportURL).To(Equal(tc.external))
				g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(tc.external))
				return
			}
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(transportURL).To(BeEmpty())
			g.Expect(digest).To(BeEmpty())
			g.Expect(port).To(Equal(int32(0)))

			cond := novaCondition(nova, "SecretsReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(messaging.ReasonTransportURLRejected))
			g.Expect(cond.Message).To(ContainSubstring("must name an explicit port and must not use an IPv6 literal host"))
			g.Expect(cond.Message).NotTo(ContainSubstring("pw"), "the condition must not quote the password")

			g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(accepted),
				"a restarting pod must keep reading the last accepted URL")
		})
	}
}
