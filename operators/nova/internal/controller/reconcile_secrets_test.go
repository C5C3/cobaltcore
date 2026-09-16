// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// allSecrets returns the four Secrets a plaintext-bus Nova gates on, so a test
// that breaks one of them carries the other three intact.
func allSecrets() []client.Object {
	return []client.Object{
		novaAPIDBSecret(), novaCellDBSecret(),
		novaServiceUserSecret("svc-pw"), novaSharedSecret("meta-secret"),
	}
}

// assertWaitingOnGate asserts the shape every unusable credential Secret
// produces: a poll, no values at all, and SecretsReady=False carrying the gate's
// own reason and the noun that names which Secret is missing.
func assertWaitingOnGate(t *testing.T, nova *novav1alpha1.Nova, objs []client.Object,
	wantReason, wantNoun string,
) {
	t.Helper()
	g := NewGomegaWithT(t)
	r := newNovaTestReconciler(append([]client.Object{
		nova,
		readyClusterSecretStore(openBaoClusterStoreName),
	}, objs...)...)

	res, values, err := r.reconcileSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(values).To(Equal(secretValues{}))

	cond := novaCondition(nova, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(wantReason))
	g.Expect(cond.Message).To(ContainSubstring(wantNoun))
}

func TestReconcileSecrets_StoreNotReady(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, notReadyClusterSecretStore(openBaoClusterStoreName))

	res, values, err := r.reconcileSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(values).To(Equal(secretValues{}))

	cond := novaCondition(nova, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
}

// TestReconcileSecrets_APIDBSecretMissingKeyWaits covers the half-synced
// nova_api credentials Secret: it exists, so the fast path finds it, but it
// carries only the username ESO wrote first. A DSN assembled from it would name
// an empty password.
func TestReconcileSecrets_APIDBSecretMissingKeyWaits(t *testing.T) {
	halfSynced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAPIDBSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("nova-api")},
	}
	assertWaitingOnGate(t, validNova(),
		[]client.Object{halfSynced, novaCellDBSecret(), novaServiceUserSecret("svc-pw"), novaSharedSecret("meta")},
		"WaitingForDBCredentials", "API database credentials")
}

// TestReconcileSecrets_CellDBSecretMissingKeyWaits covers the same half-synced
// shape on the cell schema. It shares the reason with the nova_api gate, so the
// message noun is what tells the two apart on the CR.
func TestReconcileSecrets_CellDBSecretMissingKeyWaits(t *testing.T) {
	halfSynced := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCellDBSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("nova")},
	}
	assertWaitingOnGate(t, validNova(),
		[]client.Object{novaAPIDBSecret(), halfSynced, novaServiceUserSecret("svc-pw"), novaSharedSecret("meta")},
		"WaitingForDBCredentials", "Cell database credentials")
}

// TestReconcileSecrets_ServiceUserKeyMissingWaits proves the service-user gate
// checks the key spec.serviceUser.secretRef.key names rather than the Secret's
// mere existence.
func TestReconcileSecrets_ServiceUserKeyMissingWaits(t *testing.T) {
	wrongKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceUserSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"not-password": []byte("svc-pw")},
	}
	assertWaitingOnGate(t, validNova(),
		[]client.Object{novaAPIDBSecret(), novaCellDBSecret(), wrongKey, novaSharedSecret("meta")},
		"WaitingForServiceUserCredentials", "Service-user credentials")
}

// TestReconcileSecrets_SharedSecretKeyMissingWaits covers the metadata shared
// secret the operator reads rather than generates: the same value has to reach
// the Neutron metadata agent, so a Secret without the key leaves every metadata
// request rejected and the gate has to hold.
func TestReconcileSecrets_SharedSecretKeyMissingWaits(t *testing.T) {
	wrongKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSharedSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"not-shared_secret": []byte("meta")},
	}
	assertWaitingOnGate(t, validNova(),
		[]client.Object{novaAPIDBSecret(), novaCellDBSecret(), novaServiceUserSecret("svc-pw"), wrongKey},
		"WaitingForMetadataSharedSecret", "Nova metadata shared secret")
}

// TestReconcileSecrets_EmptySharedSecretWaits covers the shared secret whose key
// exists but carries no value, which the key gate lets through. nova checks the
// instance signature with an HMAC keyed on this value and does not refuse an
// empty key, so anyone reaching the metadata API could forge a request for any
// instance: the step must hold instead of publishing the empty value.
func TestReconcileSecrets_EmptySharedSecretWaits(t *testing.T) {
	assertWaitingOnGate(t, validNova(),
		[]client.Object{novaAPIDBSecret(), novaCellDBSecret(), novaServiceUserSecret("svc-pw"), novaSharedSecret("")},
		"MetadataSharedSecretEmpty", "would accept a forged instance signature")
}

// TestReconcileSecrets_MessagingCAMissingWaits covers the fifth gate, which only
// a Nova with spec.messaging.tls carries: the CA Secret exists but lacks the
// bundle, so the broker's certificate could not be verified.
func TestReconcileSecrets_MessagingCAMissingWaits(t *testing.T) {
	emptyCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testMessagingCASecret, Namespace: testNamespace},
		Data:       map[string][]byte{"not-ca.crt": []byte("-----BEGIN CERTIFICATE-----")},
	}
	assertWaitingOnGate(t, novaWithMessagingTLS(),
		append(allSecrets(), emptyCA),
		"WaitingForMessagingCA", "Messaging CA bundle")
}

// TestReconcileSecrets_MessagingCAIsNotGatedOnAPlaintextBus proves the fifth
// gate is conditional: a Nova without spec.messaging.tls references no CA Secret
// at all, and gating on one would leave it waiting for a Secret nothing creates.
func TestReconcileSecrets_MessagingCAIsNotGatedOnAPlaintextBus(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(append([]client.Object{
		nova,
		readyClusterSecretStore(openBaoClusterStoreName),
	}, allSecrets()...)...)

	res, values, err := r.reconcileSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(values.messagingCA).To(BeEmpty())
}

// TestReconcileSecrets_ReadErrorAfterGateIsWrapped covers the read that happens
// AFTER every gate passed: the service-user password read. A backend failure
// there must surface as a wrapped error rather than a silent empty value,
// because an empty password would reach the compute contract and clear the
// rollout annotation derived from it.
func TestReconcileSecrets_ReadErrorAfterGateIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	serviceUserKey := client.ObjectKey{Namespace: testNamespace, Name: testServiceUserSecret}
	boom := errors.New("etcd unavailable")
	// The gate reads the service-user Secret once; the value read is the second
	// Get for that key, and only that one fails.
	var serviceUserGets int
	c := novaFakeClientBuilder(append([]client.Object{
		nova,
		readyClusterSecretStore(openBaoClusterStoreName),
	}, allSecrets()...)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret && key == serviceUserKey {
					serviceUserGets++
					if serviceUserGets > 1 {
						return boom
					}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, values, err := r.reconcileSecrets(context.Background(), r.Client, nova)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading the service-user password:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(values).To(Equal(secretValues{}))
}

// TestReconcileSecrets_AllPresentReturnsValuesAndStableDigests covers the ready
// path on both bus postures: the values are the Secret contents, the digests the
// pipeline derives from them are non-empty and stable across passes (they drive
// the pod roll, so a value that moved on its own would roll every pass), and the
// CA bundle is carried only for a bus the CR asked to verify.
func TestReconcileSecrets_AllPresentReturnsValuesAndStableDigests(t *testing.T) {
	tests := []struct {
		name   string
		nova   *novav1alpha1.Nova
		extra  []client.Object
		wantCA string
	}{
		{name: "plaintext bus", nova: validNova()},
		{
			name:   "verified bus",
			nova:   novaWithMessagingTLS(),
			extra:  []client.Object{novaMessagingCASecret()},
			wantCA: "-----BEGIN CERTIFICATE-----",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			objs := append([]client.Object{tc.nova, readyClusterSecretStore(openBaoClusterStoreName)}, allSecrets()...)
			r := newNovaTestReconciler(append(objs, tc.extra...)...)

			res, values, err := r.reconcileSecrets(context.Background(), r.Client, tc.nova)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(values.serviceUserPassword).To(Equal("svc-pw"))
			g.Expect(values.metadataSharedSecret).To(Equal("meta-secret"))
			g.Expect(values.messagingCA).To(Equal(tc.wantCA))

			cond := novaCondition(tc.nova, "SecretsReady")
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("SecretsAvailable"))

			passwordDigest := secrets.AdminPasswordDigest(values.serviceUserPassword)
			sharedDigest := secrets.AdminPasswordDigest(values.metadataSharedSecret)
			g.Expect(passwordDigest).NotTo(BeEmpty())
			g.Expect(sharedDigest).NotTo(BeEmpty())
			g.Expect(passwordDigest).NotTo(Equal(sharedDigest))

			_, second, err := r.reconcileSecrets(context.Background(), r.Client, tc.nova)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(secrets.AdminPasswordDigest(second.serviceUserPassword)).To(Equal(passwordDigest))
			g.Expect(secrets.AdminPasswordDigest(second.metadataSharedSecret)).To(Equal(sharedDigest))
		})
	}
}

// TestEffectiveKeys covers the three data-key resolvers on both inputs: a CR
// that carries the explicit key and one that bypassed the defaulting webhook and
// left it empty.
func TestEffectiveKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	// novaMinimal skipped admission, so all three keys are empty and resolve to
	// their fallbacks; validNova went through the defaulter.
	bypassed := novaMinimal()
	bypassed.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: testMessagingCASecret},
	}
	g.Expect(effectiveServiceUserKey(bypassed)).To(Equal("password"))
	g.Expect(effectiveSharedSecretKey(bypassed)).To(Equal(novav1alpha1.DefaultSharedSecretKey))
	g.Expect(effectiveMessagingCAKey(bypassed)).To(Equal("ca.crt"))

	explicit := novaWithMessagingTLS()
	explicit.Spec.ServiceUser.SecretRef.Key = "nova-password"
	explicit.Spec.Metadata.SharedSecretRef.Key = "metadata-proxy"
	explicit.Spec.Messaging.TLS.CABundleSecretRef.Key = "bundle.pem"
	g.Expect(effectiveServiceUserKey(explicit)).To(Equal("nova-password"))
	g.Expect(effectiveSharedSecretKey(explicit)).To(Equal("metadata-proxy"))
	g.Expect(effectiveMessagingCAKey(explicit)).To(Equal("bundle.pem"))
}
