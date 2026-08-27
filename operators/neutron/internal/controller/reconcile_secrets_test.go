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

			res, digest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

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

			res, digest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

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

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

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
	_, rotatedDigest, err := rotated.reconcileSecrets(context.Background(), rotated.Client, neutron)
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

	_, digest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest).To(Equal(secrets.AdminPasswordDigest("keyed-pw")))

	// A CR that bypassed the defaulting webhook carries no key and falls back to
	// "password".
	neutron.Spec.ServiceUser.SecretRef.Key = ""
	g.Expect(effectiveServiceUserKey(neutron)).To(Equal("password"))
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

	res, digest, err := r.reconcileSecrets(context.Background(), r.Client, neutron)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading service-user password value:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())
}
