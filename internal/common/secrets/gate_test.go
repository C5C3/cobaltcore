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
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func gateTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := esov1.AddToScheme(s); err != nil {
		t.Fatalf("adding ESO scheme: %v", err)
	}
	return s
}

func gateExternalSecret(name string, ready bool) *esov1.ExternalSecret {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: esov1.ExternalSecretStatus{
			Conditions: []esov1.ExternalSecretStatusCondition{
				{Type: esov1.ExternalSecretReady, Status: corev1.ConditionStatus(status)},
			},
		},
	}
}

// TestGateSyncedSecret walks the full ladder-state table, including the
// error/edge rungs: nothing exists, ExternalSecret pending, ESO Ready but the
// Secret missing a key, and the empty-expectedKeys existence-only shape.
func TestGateSyncedSecret(t *testing.T) {
	key := client.ObjectKey{Namespace: "ns", Name: "cred"}
	fullSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cred", Namespace: "ns"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	partialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cred", Namespace: "ns"},
		Data:       map[string][]byte{"username": []byte("u")},
	}

	cases := []struct {
		name         string
		objects      []client.Object
		expectedKeys []string
		want         GateState
	}{
		{
			name:         "secret with all keys is ready",
			objects:      []client.Object{fullSecret},
			expectedKeys: []string{"username", "password"},
			want:         GateReady,
		},
		{
			name:         "nothing exists yet",
			objects:      nil,
			expectedKeys: []string{"password"},
			want:         GateExternalSecretMissing,
		},
		{
			name:         "externalsecret exists but has not synced",
			objects:      []client.Object{gateExternalSecret("cred", false)},
			expectedKeys: []string{"password"},
			want:         GateExternalSecretNotSynced,
		},
		{
			name:         "eso ready but secret missing a key",
			objects:      []client.Object{partialSecret, gateExternalSecret("cred", true)},
			expectedKeys: []string{"username", "password"},
			want:         GateSecretKeysMissing,
		},
		{
			// A momentarily not-Ready ExternalSecret must NOT gate while the
			// materialized Secret still holds valid keys — pods consume the
			// Secret directly.
			name:         "secret usable despite eso not ready",
			objects:      []client.Object{fullSecret, gateExternalSecret("cred", false)},
			expectedKeys: []string{"username", "password"},
			want:         GateReady,
		},
		{
			// Empty expectedKeys skips the keys rung: bare existence is enough.
			name:         "no expected keys requires existence only",
			objects:      []client.Object{partialSecret},
			expectedKeys: nil,
			want:         GateReady,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(gateTestScheme(t)).WithObjects(tc.objects...).Build()

			state, err := GateSyncedSecret(context.Background(), c, key, tc.expectedKeys...)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(state).To(gomega.Equal(tc.want))
		})
	}
}
