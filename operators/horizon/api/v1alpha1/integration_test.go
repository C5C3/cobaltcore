// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/horizon/internal/testutil"
)

// --- Helpers ---

// setupEnvTestNoWebhook wraps testutil.SetupHorizonEnvTestNoWebhook with the
// v1alpha1 scheme registration, avoiding the import cycle between testutil and
// this package. The CRD-only setup keeps the validating webhook out of the way
// so no webhook check can mask a missing CEL rule.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupHorizonEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationHorizon returns validHorizon() stamped with name/namespace for API
// server submission. validHorizon() is a webhook-unit helper, so it carries the
// same field values but the ObjectMeta of a fixture rather than of a submitted
// object.
func integrationHorizon(name, namespace string) *Horizon {
	horizon := validHorizon()
	horizon.Name = name
	horizon.Namespace = namespace
	return horizon
}

// --- CRD-level CEL / schema enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange pins the
// targetClusterRef rename rule: re-pointing a Horizon at another target cluster
// is rejected by the CRD CEL rule alone, without the validating webhook.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "targetcluster-rename-")

	horizon := integrationHorizon("horizon", ns)
	horizon.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, horizon)).To(Succeed(), "valid Horizon should be accepted")

	got := &Horizon{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "horizon", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef.Name = "cluster-b"

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "renaming targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip pins the
// presence rule in both directions: a CR created without the ref cannot gain
// one, and a CR created with it cannot drop it. Either edit would move the
// children away from the cluster that already holds them.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "targetcluster-flip-")

	local := integrationHorizon("horizon-local", ns)
	g.Expect(c.Create(ctx, local)).To(Succeed(), "Horizon without targetClusterRef should be accepted")

	got := &Horizon{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "horizon-local", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}

	err := c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "adding targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))

	remote := integrationHorizon("horizon-remote", ns)
	remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
	g.Expect(c.Create(ctx, remote)).To(Succeed(), "Horizon with targetClusterRef should be accepted")

	got = &Horizon{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "horizon-remote", Namespace: ns}, got)).To(Succeed())
	got.Spec.TargetClusterRef = nil

	err = c.Update(ctx, got)
	g.Expect(err).To(HaveOccurred(), "removing targetClusterRef must be rejected on update")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		fmt.Sprintf("expected Invalid or Forbidden status error, got: %v", err))
	g.Expect(err.Error()).To(ContainSubstring("targetClusterRef is immutable"))
}
