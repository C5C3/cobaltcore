// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
)

func TestGateStoreReady_readyCluster(t *testing.T) {
	g := gomega.NewWithT(t)
	s := gateTestScheme(t)
	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-cluster-store"},
		Status: esov1.SecretStoreStatus{Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(store).WithStatusSubresource(store).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindCluster, Name: "openbao-cluster-store"}
	var conds []metav1.Condition
	ready, err := GateStoreReady(context.Background(), c, ref, "ns", &conds, 2, "SecretsReady")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeTrue())
	g.Expect(conds).To(gomega.BeEmpty(), "a ready store writes no condition")
}

func TestGateStoreReady_readyNamespaced(t *testing.T) {
	g := gomega.NewWithT(t)
	s := gateTestScheme(t)
	store := &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: "openbao-tenant-store", Namespace: "tenant-a"},
		Status: esov1.SecretStoreStatus{Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(store).WithStatusSubresource(store).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store"}
	var conds []metav1.Condition
	ready, err := GateStoreReady(context.Background(), c, ref, "tenant-a", &conds, 2, "SecretsReady")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeTrue())
	g.Expect(conds).To(gomega.BeEmpty())
}

func TestGateStoreReady_notReadyNamespacedSetsCondition(t *testing.T) {
	g := gomega.NewWithT(t)
	s := gateTestScheme(t)
	// The namespaced store does not exist, so it is treated as not-ready and the
	// condition message names the namespaced kind and the store name.
	c := fake.NewClientBuilder().WithScheme(s).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreKindNamespaced, Name: "openbao-tenant-store"}
	var conds []metav1.Condition
	ready, err := GateStoreReady(context.Background(), c, ref, "tenant-a", &conds, 2, "SecretsReady")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeFalse())
	cond := conditions.GetCondition(conds, "SecretsReady")
	g.Expect(cond.Status).To(gomega.Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(gomega.Equal("SecretStoreNotReady"))
	g.Expect(cond.Message).To(gomega.ContainSubstring("SecretStore"))
	g.Expect(cond.Message).To(gomega.ContainSubstring("openbao-tenant-store"))
	g.Expect(cond.Message).To(gomega.ContainSubstring("upstream secret backend unreachable"))
}

func TestGateStoreReady_unknownKindErrors(t *testing.T) {
	g := gomega.NewWithT(t)
	s := gateTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	ref := commonv1.SecretStoreRefSpec{Kind: commonv1.SecretStoreRefKind("Bogus"), Name: "x"}
	var conds []metav1.Condition
	ready, err := GateStoreReady(context.Background(), c, ref, "ns", &conds, 2, "SecretsReady")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeFalse())
	g.Expect(conds).To(gomega.BeEmpty(), "an errored gate writes no condition")
}

func TestGateCredential_states(t *testing.T) {
	key := client.ObjectKey{Namespace: "ns", Name: "cred"}
	spec := CredentialGateSpec{
		Key: key, Reason: "WaitingForDBCredentials", Noun: "Database credentials",
		WaitingMsg: "Waiting for ESO to sync", ExpectedKeys: []string{"username", "password"},
	}
	full := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cred", Namespace: "ns"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	partial := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cred", Namespace: "ns"},
		Data:       map[string][]byte{"username": []byte("u")},
	}

	for _, tc := range []struct {
		name       string
		objs       []client.Object
		wantReady  bool
		wantMsgSub string
	}{
		{name: "ready", objs: []client.Object{full}, wantReady: true},
		{name: "es missing", objs: nil, wantReady: false, wantMsgSub: "ExternalSecret ns/cred not found yet"},
		{name: "es not synced", objs: []client.Object{gateExternalSecret("cred", false)}, wantReady: false, wantMsgSub: "Waiting for ESO to sync"},
		{name: "keys missing", objs: []client.Object{partial, gateExternalSecret("cred", true)}, wantReady: false, wantMsgSub: "missing expected keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			s := gateTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
			var conds []metav1.Condition
			ready, err := GateCredential(context.Background(), c, spec, &conds, 3, "SecretsReady")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ready).To(gomega.Equal(tc.wantReady))
			if tc.wantReady {
				g.Expect(conds).To(gomega.BeEmpty())
				return
			}
			cond := conditions.GetCondition(conds, "SecretsReady")
			g.Expect(cond.Reason).To(gomega.Equal("WaitingForDBCredentials"))
			g.Expect(cond.Message).To(gomega.ContainSubstring(tc.wantMsgSub))
		})
	}
}

func TestGateCredentials_shortCircuitsOnFirstNotReady(t *testing.T) {
	g := gomega.NewWithT(t)
	s := gateTestScheme(t)
	// Only the second credential's Secret exists; the first is missing so the
	// loop short-circuits with the first spec's reason.
	second := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("p")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(second).Build()
	specs := []CredentialGateSpec{
		{Key: client.ObjectKey{Namespace: "ns", Name: "a"}, Reason: "WaitingForA", Noun: "A", ExpectedKeys: []string{"password"}},
		{Key: client.ObjectKey{Namespace: "ns", Name: "b"}, Reason: "WaitingForB", Noun: "B", ExpectedKeys: []string{"password"}},
	}
	var conds []metav1.Condition
	ready, err := GateCredentials(context.Background(), c, specs, &conds, 3, "SecretsReady")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.BeFalse())
	g.Expect(conditions.GetCondition(conds, "SecretsReady").Reason).To(gomega.Equal("WaitingForA"))
}
