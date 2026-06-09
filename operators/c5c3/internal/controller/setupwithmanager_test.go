// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane SetupWithManager wiring: the secret-name field
// indexer extractor and the Secret -> ControlPlane watch mapper (CC-0110,
// REQ-012).
package controller

import (
	"context"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// newControlPlaneMapperClient returns a fake client pre-registered with the
// ControlPlaneSecretNameIndexKey field indexer so secretToControlPlaneMapper can
// resolve its MatchingFields lookups, mirroring keystone's
// newMapperFakeClientBuilder (CC-0110, REQ-012).
func newControlPlaneMapperClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(controllerTestScheme(t)).
		WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.ControlPlane{}, ControlPlaneSecretNameIndexKey, controlPlaneSecretNameExtractor).
		Build()
}

// mapperControlPlane builds a minimal ControlPlane whose admin passwordSecretRef
// points at the named Secret.
func mapperControlPlane(name, namespace, secretName string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			KORC: c5c3v1alpha1.KORCSpec{
				AdminCredential: c5c3v1alpha1.AdminCredentialSpec{
					PasswordSecretRef: commonv1.SecretRefSpec{Name: secretName, Key: "password"},
				},
			},
		},
	}
}

// --- controlPlaneSecretNameExtractor ---

func TestControlPlaneSecretNameExtractor_ReturnsPasswordSecretRefName(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(ConsistOf("keystone-admin"),
		"extractor must return the admin passwordSecretRef name")
}

func TestControlPlaneSecretNameExtractor_EmptyWhenUnset(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "")
	got := controlPlaneSecretNameExtractor(cp)

	g.Expect(got).To(BeEmpty(),
		"extractor must return an empty slice when passwordSecretRef.name is unset")
}

func TestControlPlaneSecretNameExtractor_WrongTypeReturnsNil(t *testing.T) {
	g := NewGomegaWithT(t)

	// A non-ControlPlane object must not panic; a nil return is the contract.
	got := controlPlaneSecretNameExtractor(&corev1.Secret{})

	g.Expect(got).To(BeNil(),
		"extractor must return nil for a non-ControlPlane object")
}

// --- secretToControlPlaneMapper ---

func TestSecretToControlPlaneMapper_EnqueuesMatchingAdminSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	c := newControlPlaneMapperClient(t, cp)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-admin", Namespace: "default"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"a Secret matching the admin passwordSecretRef must enqueue its ControlPlane")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "default", Name: "cp"}))
}

func TestSecretToControlPlaneMapper_IgnoresNonMatchingSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mapperControlPlane("cp", "default", "keystone-admin")
	c := newControlPlaneMapperClient(t, cp)
	mapper := secretToControlPlaneMapper(c)

	// A Secret whose name does not match the admin passwordSecretRef must yield
	// no reconcile requests.
	other := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-secret", Namespace: "default"},
	}
	reqs := mapper(context.Background(), other)

	g.Expect(reqs).To(BeEmpty(),
		"a Secret not referenced by any ControlPlane must enqueue nothing")
}

func TestSecretToControlPlaneMapper_ScopedToNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	// Two ControlPlanes in different namespaces referencing the same Secret name.
	// Only the one in the event's namespace must be enqueued.
	cpA := mapperControlPlane("cp-a", "ns-a", "shared-secret")
	cpB := mapperControlPlane("cp-b", "ns-b", "shared-secret")
	c := newControlPlaneMapperClient(t, cpA, cpB)
	mapper := secretToControlPlaneMapper(c)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "ns-a"},
	}
	reqs := mapper(context.Background(), secret)

	g.Expect(reqs).To(HaveLen(1),
		"only the ControlPlane in the Secret's namespace must be enqueued")
	g.Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "ns-a", Name: "cp-a"}))
}

// --- SetupWithManager Owns(ExternalSecret) ---

// TestSetupWithManager_OwnsExternalSecret verifies the two preconditions the
// SetupWithManager Owns(&esov1.ExternalSecret{}) wiring depends on so an event on
// the operator-owned, per-ControlPlane DB-credential ExternalSecret retriggers
// reconcile of its ControlPlane (CC-0116, REQ-012). controller-runtime cannot be
// introspected for its registered Owns watches without a live manager, so the
// contract is asserted through its two observable halves:
//
//  1. esov1.ExternalSecret is a kind known to the reconciler scheme — Owns
//     registers a watch by GVK, which panics at manager startup for an
//     unregistered type.
//  2. The ExternalSecret the reconciler creates carries a CONTROLLER owner
//     reference to the ControlPlane — the linkage controller-runtime's
//     EnqueueRequestForOwner handler (installed by Owns) walks to map an
//     ExternalSecret event back to the owning ControlPlane.
func TestSetupWithManager_OwnsExternalSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	// korcTestScheme registers esov1 alongside c5c3; controllerTestScheme does not.
	s := korcTestScheme(t)

	// (1) Owns can only register a watch for a scheme-known kind.
	_, _, err := s.ObjectKinds(&esov1.ExternalSecret{})
	g.Expect(err).NotTo(HaveOccurred(),
		"esov1.ExternalSecret must be registered in the reconciler scheme so Owns can watch it")

	// (2) The owned ExternalSecret must be controller-owned by the ControlPlane.
	cp := dbCredsManagedControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err = r.reconcileDBCredentials(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	es, ok := getDBCredentialES(t, c, cp)
	g.Expect(ok).To(BeTrue(), "the per-ControlPlane DB-credential ExternalSecret must be created")
	g.Expect(es.OwnerReferences).To(HaveLen(1))
	owner := es.OwnerReferences[0]
	g.Expect(owner.Kind).To(Equal("ControlPlane"))
	g.Expect(owner.Name).To(Equal(cp.Name))
	g.Expect(owner.Controller).NotTo(BeNil())
	g.Expect(*owner.Controller).To(BeTrue(),
		"the Owns(ExternalSecret) watch resolves an ExternalSecret event via its controller owner reference")
}
