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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// brownfieldCinder returns the fixture pointed at an external broker through
// spec.messaging.secretRef, the mode the upstream Secret cases exercise.
func brownfieldCinder(secretName string) *cinderv1alpha1.Cinder {
	cinder := validCinder()
	cinder.Spec.Messaging = commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: secretName, Key: commonv1.DefaultTransportURLSecretKey},
	}
	return cinder
}

// assertWaitingForCredentials asserts the shape every incomplete upstream
// produces: a poll, no digest, no egress port, SecretsReady=False, and no
// derived Secret, so nothing downstream stamps a pod-template annotation or
// opens a NetworkPolicy peer against a URL that does not exist.
func assertWaitingForCredentials(t *testing.T, r *CinderReconciler, cinder *cinderv1alpha1.Cinder) {
	t.Helper()
	g := NewGomegaWithT(t)

	res, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())
	g.Expect(port).To(Equal(int32(0)))

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(cinder.Name)}
	g.Expect(r.Get(context.Background(), key, &derived)).NotTo(Succeed(),
		"no derived Secret is written while the broker credentials are missing")
}

// TestReconcileTransportURLSecret_MissingClusterWaits covers the GitOps ordering
// in which the Cinder is applied before its broker exists.
func TestReconcileTransportURLSecret_MissingClusterWaits(t *testing.T) {
	cinder := validCinder()
	assertWaitingForCredentials(t, newCinderTestReconciler(cinder), cinder)
}

// TestReconcileTransportURLSecret_ClusterWithoutDefaultUserWaits covers the
// broker that exists but has not published status.defaultUser.secretReference
// yet.
func TestReconcileTransportURLSecret_ClusterWithoutDefaultUserWaits(t *testing.T) {
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, rabbitmqCluster(testRabbitmqClusterName, testNamespace))
	assertWaitingForCredentials(t, r, cinder)
}

// TestReconcileTransportURLSecret_DefaultUserSecretMissingKeyWaits covers the
// default-user Secret that exists but is missing one of the four halves of the
// URL: a URL assembled from it would name an empty host.
func TestReconcileTransportURLSecret_DefaultUserSecretMissingKeyWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := validCinder()
	cluster := rabbitmqCluster(testRabbitmqClusterName, testNamespace)
	c := cinderFakeClientBuilder(cinder, cluster, rabbitmqDefaultUserSecret("5672", "host")).
		WithStatusSubresource(cluster).
		Build()
	r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}
	g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c,
		client.ObjectKeyFromObject(cluster), testRabbitmqUserSecret)).To(Succeed())

	assertWaitingForCredentials(t, r, cinder)
}

// TestReconcileTransportURLSecret_MissingBrownfieldSecretWaits covers the
// external broker whose credentials Secret has not been created yet.
func TestReconcileTransportURLSecret_MissingBrownfieldSecretWaits(t *testing.T) {
	cinder := brownfieldCinder("external-bus")
	assertWaitingForCredentials(t, newCinderTestReconciler(cinder), cinder)
}

// TestReconcileTransportURLSecret_EmptyBrownfieldKeyWaits covers the Secret that
// exists but carries an empty transport_url, the shape a half-written external
// Secret has.
func TestReconcileTransportURLSecret_EmptyBrownfieldKeyWaits(t *testing.T) {
	cinder := brownfieldCinder("external-bus")
	upstream := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: testNamespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte("")},
	}
	assertWaitingForCredentials(t, newCinderTestReconciler(cinder, upstream), cinder)
}

// TestReconcileTransportURLSecret_NonRabbitSchemeErrors covers the external URL
// naming a driver the service is not configured for. It is a returned error
// rather than a wait: no amount of polling turns amqp:// into rabbit://.
func TestReconcileTransportURLSecret_NonRabbitSchemeErrors(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := brownfieldCinder("external-bus")
	upstream := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: testNamespace},
		Data: map[string][]byte{
			commonv1.DefaultTransportURLSecretKey: []byte("mysql://cinder:pw@bus.example.com:3306/"),
		},
	}
	r := newCinderTestReconciler(cinder, upstream)

	_, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, cinder)

	g.Expect(err).To(MatchError(ContainSubstring("scheme must be rabbit")))
	g.Expect(digest).To(BeEmpty())
	g.Expect(port).To(Equal(int32(0)))
}

// TestReconcileTransportURLSecret_ManagedDerivesURLAndPort covers the ready
// cluster: the derived Secret is materialised, the digest is non-empty, and the
// egress port is the one the broker published rather than the AMQP default.
func TestReconcileTransportURLSecret_ManagedDerivesURLAndPort(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := validCinder()
	cluster := rabbitmqCluster(testRabbitmqClusterName, testNamespace)
	c := cinderFakeClientBuilder(cinder, cluster, rabbitmqDefaultUserSecret("5671")).
		WithStatusSubresource(cluster).
		Build()
	r := &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c,
		client.ObjectKeyFromObject(cluster), testRabbitmqUserSecret)).To(Succeed())

	res, digest, port, err := r.reconcileTransportURLSecret(ctx, r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())
	g.Expect(port).To(Equal(int32(5671)), "the egress port is the transport URL's port")

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(cinder.Name)}
	g.Expect(r.Get(ctx, key, &derived)).To(Succeed())
	g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(HavePrefix("rabbit://"))

	// A second pass returns the identical digest: it drives the pod roll, so a
	// value that moved without the credentials moving would roll every pass.
	_, digest2, port2, err := r.reconcileTransportURLSecret(ctx, r.Client, cinder)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
	g.Expect(port2).To(Equal(port))
}

// TestReconcileTransportURLSecret_BrownfieldKeepsURLPort covers the brownfield
// bus: the URL is copied verbatim, so the port the broker's administrator chose
// is the one the NetworkPolicy has to open.
func TestReconcileTransportURLSecret_BrownfieldKeepsURLPort(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := brownfieldCinder("external-bus")
	upstream := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: testNamespace},
		Data: map[string][]byte{
			commonv1.DefaultTransportURLSecretKey: []byte("rabbit://cinder:pw@bus.example.com:5673/"),
		},
	}
	r := newCinderTestReconciler(cinder, upstream)

	_, digest, port, err := r.reconcileTransportURLSecret(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).NotTo(BeEmpty())
	g.Expect(port).To(Equal(int32(5673)))
}
