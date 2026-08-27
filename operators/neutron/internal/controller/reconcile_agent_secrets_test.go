// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// testAgentSharedSecretName is the Secret holding the value the agent signs
// forwarded metadata requests with.
const testAgentSharedSecretName = "nova-metadata-secret"

// withNovaMetadata returns the agent fixture pointed at a Nova metadata API,
// signing its forwarded requests with the named Secret key.
func withNovaMetadata(key string) *neutronv1alpha1.NeutronMetadataAgent {
	cr := validAgent()
	cr.Spec.NovaMetadata = &neutronv1alpha1.NovaMetadataSpec{
		Host:            "nova-metadata.openstack.svc",
		Port:            8775,
		SharedSecretRef: &commonv1.SecretRefSpec{Name: testAgentSharedSecretName, Key: key},
	}
	return cr
}

// withManagedMessaging returns the agent fixture attached to the shared
// RabbitmqCluster, the mode in which the operator derives the transport URL
// itself.
func withManagedMessaging() *neutronv1alpha1.NeutronMetadataAgent {
	cr := validAgent()
	cr.Spec.Messaging = &commonv1.MessagingSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
		Replicas:   3,
	}
	return cr
}

// An agent that names neither block reads nothing and reports available: both
// spec blocks are optional, and a CR that sets neither is the smallest one the
// operator projects.
func TestReconcileAgentSecrets_NoOptionalBlocksIsAvailable(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()
	r := newAgentTestReconciler(cr)

	res, digest, err := r.reconcileAgentSecrets(context.Background(), r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())

	cond := agentCondition(cr, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))
}

// The shared secret has to exist and carry the key the pod sources, because Nova
// rejects a request signed with nothing when it carries a secret of its own. A
// Secret that exists but lacks the key is the same wait as one that does not
// exist yet: the pod would start with an env var sourced from a key nobody
// wrote.
func TestReconcileAgentSecrets_WaitsForTheNovaSharedSecret(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		secret *corev1.Secret
	}{
		{
			name: "the Secret has not been created yet",
			key:  "shared_secret",
		},
		{
			name: "the Secret exists without the referenced key",
			key:  "shared_secret",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testAgentSharedSecretName, Namespace: testNamespace},
				Data:       map[string][]byte{"other": []byte("value")},
			},
		},
		{
			name: "the Secret carries the default key while the ref names another",
			key:  "metadata_proxy_shared_secret",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testAgentSharedSecretName, Namespace: testNamespace},
				Data:       map[string][]byte{"shared_secret": []byte("value")},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cr := withNovaMetadata(tc.key)
			objs := []client.Object{cr}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			r := newAgentTestReconciler(objs...)

			res, digest, err := r.reconcileAgentSecrets(context.Background(), r.Client, cr)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(digest).To(BeEmpty())

			cond := agentCondition(cr, "SecretsReady")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("WaitingForNovaSharedSecret"))
			g.Expect(cond.Message).To(ContainSubstring("Nova metadata shared secret"))
		})
	}
}

// An empty key on the ref falls back to the webhook's default rather than
// gating on "", which no Secret carries.
func TestReconcileAgentSecrets_EmptyKeyFallsBackToTheDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := withNovaMetadata("")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentSharedSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{agentSharedSecretDefaultKey: []byte("s3cr3t")},
	}
	r := newAgentTestReconciler(cr, secret)

	res, _, err := r.reconcileAgentSecrets(context.Background(), r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(agentCondition(cr, "SecretsReady").Reason).To(Equal("SecretsAvailable"))
	g.Expect(agentSharedSecretKey(cr)).To(Equal(agentSharedSecretDefaultKey))
}

// A broker that has not published its default user yet holds the pipeline under
// the shared messaging reason, so no derived Secret is materialised with partial
// credentials.
func TestReconcileAgentSecrets_WaitsForTheTransportURL(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := withManagedMessaging()
	r := newAgentTestReconciler(cr, rabbitmqCluster(testRabbitmqClusterName, testNamespace))

	res, digest, err := r.reconcileAgentSecrets(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := agentCondition(cr, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))

	key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(cr.Name)}
	g.Expect(r.Get(ctx, key, &corev1.Secret{})).NotTo(Succeed(),
		"no derived Secret is written while the broker credentials are missing")
}

// The digest is what rolls the pods when the broker credential rotates, so it
// has to come back non-empty from both modes and stay stable across passes.
func TestReconcileAgentSecrets_DigestFromBothMessagingModes(t *testing.T) {
	t.Run("managed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cr := withManagedMessaging()
		cluster := rabbitmqCluster(testRabbitmqClusterName, testNamespace)
		c := neutronFakeClientBuilder(cr, cluster, rabbitmqDefaultUserSecret("5672")).
			WithStatusSubresource(cluster).
			Build()
		r := &NeutronMetadataAgentReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}
		g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c,
			client.ObjectKeyFromObject(cluster), testRabbitmqUserSecret)).To(Succeed())

		res, digest, err := r.reconcileAgentSecrets(ctx, r.Client, cr)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(digest).NotTo(BeEmpty())
		g.Expect(agentCondition(cr, "SecretsReady").Reason).To(Equal("SecretsAvailable"))

		var derived corev1.Secret
		key := client.ObjectKey{Namespace: testNamespace, Name: messaging.TransportURLSecretName(cr.Name)}
		g.Expect(r.Get(ctx, key, &derived)).To(Succeed())
		g.Expect(string(derived.Data[commonv1.DefaultTransportURLSecretKey])).To(HavePrefix("rabbit://"))

		_, second, err := r.reconcileAgentSecrets(ctx, r.Client, cr)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(second).To(Equal(digest), "an unchanged credential must not roll the pods")
	})

	t.Run("brownfield", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validAgent()
		cr.Spec.Messaging = &commonv1.MessagingSpec{
			SecretRef: &commonv1.SecretRefSpec{Name: "external-bus", Key: commonv1.DefaultTransportURLSecretKey},
		}
		upstream := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: testNamespace},
			Data: map[string][]byte{
				commonv1.DefaultTransportURLSecretKey: []byte("rabbit://neutron:pw@bus.example.com:5673/"),
			},
		}
		r := newAgentTestReconciler(cr, upstream)

		res, digest, err := r.reconcileAgentSecrets(context.Background(), r.Client, cr)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(digest).NotTo(BeEmpty())
	})
}

// A backend failure inside the shared flow surfaces as the error the flow
// wrapped it in, so the reconcile fails and the workqueue retries with backoff
// rather than reporting the agent as ready without a bus.
func TestReconcileAgentSecrets_FlowErrorPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := withManagedMessaging()
	c := neutronFakeClientBuilder(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, isUnstructured := obj.(*unstructured.Unstructured); isUnstructured {
					return apierrors.NewServiceUnavailable("cache not started")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &NeutronMetadataAgentReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, digest, err := r.reconcileAgentSecrets(context.Background(), r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("reading RabbitmqCluster " + testNamespace + "/" + testRabbitmqClusterName)))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())
	g.Expect(agentCondition(cr, "SecretsReady")).To(BeNil(),
		"the shared flow reports its waits, not its backend failures")
}

// A backend failure on the shared-secret gate fails the pass the same way: the
// gate returns the error rather than a wait, so nothing downstream renders.
func TestReconcileAgentSecrets_SharedSecretGateErrorPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := withNovaMetadata("shared_secret")
	c := neutronFakeClientBuilder(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return apierrors.NewServiceUnavailable("cache not started")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &NeutronMetadataAgentReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, digest, err := r.reconcileAgentSecrets(context.Background(), r.Client, cr)

	g.Expect(apierrors.IsServiceUnavailable(err)).To(BeTrue())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())
}
