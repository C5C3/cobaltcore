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
)

// TestReconcileSecrets_StoreGate covers both shapes of an unusable secret store:
// a store that reports Ready=False and one that does not exist at all. Both must
// requeue with SecretsReady=False / SecretStoreNotReady and NO error — an absent
// store is an upstream state the operator polls for, not a reconcile failure.
func TestReconcileSecrets_StoreGate(t *testing.T) {
	tests := []struct {
		name  string
		store client.Object
	}{
		{name: "store reports not ready", store: notReadyClusterSecretStore(openBaoClusterStoreName)},
		{name: "store does not exist", store: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			objs := []client.Object{neutron}
			if tc.store != nil {
				objs = append(objs, tc.store)
			}
			r := newNeutronTestReconciler(objs...)

			res, digest, _, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(digest).To(BeEmpty())
			cond := neutronCondition(neutron, "SecretsReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
		})
	}
}

// TestReconcileSecrets_CredentialGates covers the two credential Secrets in both
// unusable shapes: absent entirely, and present but missing the key the gate
// expects. Each must requeue with the gate's own reason and no digest.
func TestReconcileSecrets_CredentialGates(t *testing.T) {
	dbSecretWrongKeys := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "neutron-db", Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("neutron")}, // password missing
	}
	serviceUserWrongKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "neutron-service-user", Namespace: testNamespace},
		Data:       map[string][]byte{"not-password": []byte("svc-pw")},
	}

	tests := []struct {
		name       string
		secrets    []client.Object
		wantReason string
	}{
		{
			name:       "database Secret absent",
			secrets:    []client.Object{neutronServiceUserSecret("svc-pw")},
			wantReason: "WaitingForDBCredentials",
		},
		{
			name:       "database Secret missing the password key",
			secrets:    []client.Object{dbSecretWrongKeys, neutronServiceUserSecret("svc-pw")},
			wantReason: "WaitingForDBCredentials",
		},
		{
			name:       "service-user Secret absent",
			secrets:    []client.Object{neutronDBSecret()},
			wantReason: "WaitingForServiceUserCredentials",
		},
		{
			name:       "service-user Secret missing the password key",
			secrets:    []client.Object{neutronDBSecret(), serviceUserWrongKey},
			wantReason: "WaitingForServiceUserCredentials",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			objs := append([]client.Object{neutron, readyClusterSecretStore(openBaoClusterStoreName)}, tc.secrets...)
			r := newNeutronTestReconciler(objs...)

			res, digest, _, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(digest).To(BeEmpty())
			cond := neutronCondition(neutron, "SecretsReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

func TestReconcileSecrets_AllPresentReturnsDigest(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron,
		readyClusterSecretStore(openBaoClusterStoreName),
		neutronDBSecret(), neutronServiceUserSecret("svc-pw"))

	res, digest, _, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	// The digest is the SHA-256 of the service-user password and is stable.
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("svc-pw")))

	cond := neutronCondition(neutron, "SecretsReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("SecretsAvailable"))

	// A rotated password at the OpenBao source changes the digest, which is what
	// rolls the pods once the deployment step stamps it.
	rotated := newNeutronTestReconciler(neutron,
		readyClusterSecretStore(openBaoClusterStoreName),
		neutronDBSecret(), neutronServiceUserSecret("rotated-pw"))
	_, rotatedDigest, _, err := rotated.reconcileSecrets(context.Background(), rotated.Client, neutron)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rotatedDigest).NotTo(Equal(digest))
}

// TestReconcileSecrets_NonDefaultServiceUserKey covers the CR that names its own
// data key: the gate and the digest read must both follow spec.serviceUser
// .secretRef.key rather than the "password" default.
func TestReconcileSecrets_NonDefaultServiceUserKey(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.ServiceUser.SecretRef.Key = "svc-password"
	serviceUser := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "neutron-service-user", Namespace: testNamespace},
		Data:       map[string][]byte{"svc-password": []byte("keyed-pw")},
	}
	r := newNeutronTestReconciler(neutron,
		readyClusterSecretStore(openBaoClusterStoreName), neutronDBSecret(), serviceUser)

	_, digest, _, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("keyed-pw")))

	// A CR that bypassed the defaulting webhook carries no key and falls back to
	// "password".
	neutron.Spec.ServiceUser.SecretRef.Key = ""
	g.Expect(effectiveServiceUserKey(neutron)).To(Equal("password"))
}

// TestReconcileSecrets_NovaNotifierGate covers the third credential gate, which
// exists only while spec.nova is set: the notifier Secret absent, present but
// carrying another key, and usable. The first two must requeue under the gate's
// own reason, so an operator reading the condition sees which Secret is missing
// rather than a generic wait.
func TestReconcileSecrets_NovaNotifierGate(t *testing.T) {
	wrongKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testNovaNotifierSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"not-password": []byte("nova-pw")},
	}

	tests := []struct {
		name       string
		notifier   client.Object
		wantReason string
	}{
		{name: "notifier Secret absent", notifier: nil, wantReason: "WaitingForNovaNotifierCredentials"},
		{name: "notifier Secret missing the password key", notifier: wrongKey, wantReason: "WaitingForNovaNotifierCredentials"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			neutron.Spec.Nova = novaNotifierSpec()
			objs := []client.Object{
				neutron, readyClusterSecretStore(openBaoClusterStoreName),
				neutronDBSecret(), neutronServiceUserSecret("svc-pw"),
			}
			if tc.notifier != nil {
				objs = append(objs, tc.notifier)
			}
			r := newNeutronTestReconciler(objs...)

			res, digest, notifierDigest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(digest).To(BeEmpty())
			g.Expect(notifierDigest).To(BeEmpty())
			cond := neutronCondition(neutron, "SecretsReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}

	t.Run("notifier Secret present", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := validNeutron()
		neutron.Spec.Nova = novaNotifierSpec()
		r := newNeutronTestReconciler(neutron, readyClusterSecretStore(openBaoClusterStoreName),
			neutronDBSecret(), neutronServiceUserSecret("svc-pw"), neutronNovaNotifierSecret("nova-pw"))

		res, digest, notifierDigest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("svc-pw")))
		g.Expect(notifierDigest).To(Equal(secrets.AdminPasswordDigest("nova-pw")))
		cond := neutronCondition(neutron, "SecretsReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal("SecretsAvailable"))
	})
}

// TestReconcileSecrets_EachPasswordRollsUnderItsOwnDigest pins what the two
// digests cover once spec.nova is set: rotating the service-user password moves
// the authtoken digest alone, and rotating the notifier password moves the
// notifier digest alone. Both passwords reach the processes as env overrides and
// only take effect on a restart, so each has to roll the pods, and the
// annotation that changed names the password that rotated.
func TestReconcileSecrets_EachPasswordRollsUnderItsOwnDigest(t *testing.T) {
	g := NewGomegaWithT(t)

	digestsFor := func(servicePassword, notifierPassword string) (string, string) {
		t.Helper()
		neutron := validNeutron()
		neutron.Spec.Nova = novaNotifierSpec()
		r := newNeutronTestReconciler(neutron, readyClusterSecretStore(openBaoClusterStoreName),
			neutronDBSecret(), neutronServiceUserSecret(servicePassword),
			neutronNovaNotifierSecret(notifierPassword))
		_, authtoken, notifier, err := r.reconcileSecrets(context.Background(), r.Client, neutron)
		g.Expect(err).NotTo(HaveOccurred())
		return authtoken, notifier
	}

	baseAuth, baseNotifier := digestsFor("svc-pw", "nova-pw")
	g.Expect(baseAuth).To(Equal(secrets.AdminPasswordDigest("svc-pw")))
	g.Expect(baseNotifier).To(Equal(secrets.AdminPasswordDigest("nova-pw")))

	auth, notifier := digestsFor("rotated-svc-pw", "nova-pw")
	g.Expect(auth).NotTo(Equal(baseAuth))
	g.Expect(notifier).To(Equal(baseNotifier))

	auth, notifier = digestsFor("svc-pw", "rotated-nova-pw")
	g.Expect(auth).To(Equal(baseAuth))
	g.Expect(notifier).NotTo(Equal(baseNotifier))

	// A Neutron without the block carries no notifier digest, and its authtoken
	// digest is the one it carried before spec.nova existed.
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron, readyClusterSecretStore(openBaoClusterStoreName),
		neutronDBSecret(), neutronServiceUserSecret("svc-pw"))
	_, withoutNova, notifierWithoutNova, err := r.reconcileSecrets(context.Background(), r.Client, neutron)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(withoutNova).To(Equal(baseAuth))
	g.Expect(notifierWithoutNova).To(BeEmpty())
}

// TestReconcileSecrets_NonDefaultNovaNotifierKey covers a notifier Secret that
// carries its password under a key of its own. Both the gate and the digest read
// have to follow spec.nova.serviceUser.secretRef.key: a hard-coded "password"
// would park SecretsReady on WaitingForNovaNotifierCredentials for good.
func TestReconcileSecrets_NonDefaultNovaNotifierKey(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Nova = novaNotifierSpec()
	neutron.Spec.Nova.ServiceUser.SecretRef.Key = "notifier-password"
	notifier := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testNovaNotifierSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"notifier-password": []byte("keyed-nova-pw")},
	}
	r := newNeutronTestReconciler(neutron, readyClusterSecretStore(openBaoClusterStoreName),
		neutronDBSecret(), neutronServiceUserSecret("svc-pw"), notifier)

	res, digest, notifierDigest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("svc-pw")))
	g.Expect(notifierDigest).To(Equal(secrets.AdminPasswordDigest("keyed-nova-pw")))
	cond := neutronCondition(neutron, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// TestReconcileSecrets_NovaNotifierReadErrorPropagatesWrapped covers the second
// digest read: a backend failure on the notifier Secret must surface wrapped
// rather than as a silent empty digest, which would clear the pod-template
// annotation and roll every pod.
func TestReconcileSecrets_NovaNotifierReadErrorPropagatesWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.Nova = novaNotifierSpec()

	notifierKey := client.ObjectKey{Namespace: testNamespace, Name: testNovaNotifierSecretName}
	boom := errors.New("etcd unavailable")
	// The gate reads the notifier Secret once; the digest read is the second Get
	// for that key, and only that one fails.
	var notifierGets int
	c := neutronFakeClientBuilder(neutron,
		readyClusterSecretStore(openBaoClusterStoreName),
		neutronDBSecret(), neutronServiceUserSecret("svc-pw"), neutronNovaNotifierSecret("nova-pw")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret && key == notifierKey {
					notifierGets++
					if notifierGets > 1 {
						return boom
					}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, digest, notifierDigest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading the nova notifier password value:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())
	g.Expect(notifierDigest).To(BeEmpty())
}

// TestReconcileSecrets_ReadErrorPropagatesWrapped covers the read that happens
// AFTER both gates passed: the digest read of the service-user password. A
// backend failure there must surface as a wrapped error rather than a silent
// empty digest, because an empty digest would clear the pod-template annotation
// and roll every pod.
func TestReconcileSecrets_ReadErrorPropagatesWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	serviceUserKey := client.ObjectKey{Namespace: testNamespace, Name: "neutron-service-user"}
	boom := errors.New("etcd unavailable")
	// The gate reads the service-user Secret once; the digest read is the second
	// Get for that key, and only that one fails.
	var serviceUserGets int
	c := neutronFakeClientBuilder(neutron,
		readyClusterSecretStore(openBaoClusterStoreName),
		neutronDBSecret(), neutronServiceUserSecret("svc-pw")).
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
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, digest, _, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading service-user password value:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())
}
