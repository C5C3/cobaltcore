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
	"k8s.io/apimachinery/pkg/api/resource"
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

// expectAdmission asserts that err admits the object when wantSub is empty,
// and otherwise that it is an Invalid status error carrying wantSub.
func expectAdmission(t testing.TB, err error, wantSub string) {
	t.Helper()
	g := NewGomegaWithT(t)
	if wantSub == "" {
		g.Expect(err).NotTo(HaveOccurred(), "admission must admit the object")
		return
	}
	g.Expect(err).To(HaveOccurred(), "admission must reject: %s", wantSub)
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid status error, got: %v", err)
	g.Expect(err.Error()).To(ContainSubstring(wantSub))
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

// TestIntegration_CRD_CELOnly_VerticalAutoscaling pins the CEL rules of
// verticalAutoscaling on the Horizon CRD against an envtest API server
// with NO validating webhook installed, so the webhook twins cannot mask a
// missing rule.
func TestIntegration_CRD_CELOnly_VerticalAutoscaling(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	for _, tc := range []struct {
		name    string
		mutate  func(o *Horizon)
		wantSub string
	}{
		{
			name: "foreign-resource",
			mutate: func(o *Horizon) {
				o.Spec.Deployment.VerticalAutoscaling = &VerticalAutoscalingSpec{
					UpdateMode: "Off",
					MinAllowed: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
				}
			},
			wantSub: "minAllowed may only name cpu and memory",
		},
		{
			name: "beside-autoscaling",
			mutate: func(o *Horizon) {
				cpu := int32(80)
				o.Spec.Autoscaling = &AutoscalingSpec{MaxReplicas: 5, TargetCPUUtilization: &cpu}
				o.Spec.Deployment.VerticalAutoscaling = &VerticalAutoscalingSpec{UpdateMode: "Off"}
			},
			wantSub: "spec.deployment.verticalAutoscaling cannot be set while spec.autoscaling scales the same Deployment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ns := newNamespace(t, ctx, c, "celonly-vpa-")
			o := integrationHorizon("vpa-"+tc.name, ns)
			tc.mutate(o)

			err := c.Create(ctx, o)
			g.Expect(err).To(HaveOccurred())
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), fmt.Sprintf("expected Invalid status error, got: %v", err))
			g.Expect(err.Error()).To(ContainSubstring(tc.wantSub))
		})
	}
}

// TestIntegration_CRD_CELOnly_WebSSO pins the CEL rules of the websso block
// against an envtest API server with NO webhook installed. The validating
// webhook twins these rules, and the defaulting webhook prepends the
// "credentials" choice; neither runs here, so a reject case fails only when
// the CRD rule itself fires.
//
// Rules: horizon_types.go:290, 291, 292
func TestIntegration_CRD_CELOnly_WebSSO(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	local := WebSSOChoice{ID: "credentials", Label: "Local"}
	keycloak := map[string]WebSSOIDPTarget{"oidc": {IdentityProvider: "keycloak", Protocol: "openid"}}

	for _, tc := range []struct {
		name    string
		websso  *WebSSOSpec
		wantSub string // empty: the object must be admitted
	}{
		{
			name:    "enabled-without-choices",
			websso:  &WebSSOSpec{Enabled: true},
			wantSub: "websso.choices must contain at least one choice when websso.enabled is true",
		},
		{
			name:    "initial-choice-names-no-choice",
			websso:  &WebSSOSpec{Enabled: true, Choices: []WebSSOChoice{local}, InitialChoice: "oidc"},
			wantSub: "websso.initialChoice must match the id of one of websso.choices",
		},
		{
			name:    "idp-mapping-key-names-no-choice",
			websso:  &WebSSOSpec{Enabled: true, Choices: []WebSSOChoice{local}, IDPMapping: keycloak},
			wantSub: "every websso.idpMapping key must match the id of one of websso.choices",
		},
		{
			name: "enabled-with-matching-choices",
			websso: &WebSSOSpec{
				Enabled:       true,
				Choices:       []WebSSOChoice{local, {ID: "oidc", Label: "Keycloak"}},
				InitialChoice: "credentials",
				IDPMapping:    keycloak,
			},
		},
		{
			// The choices rule is inert while the block is disabled.
			name:   "disabled-without-choices",
			websso: &WebSSOSpec{Enabled: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, ctx, c, "celonly-websso-")
			o := integrationHorizon("websso-"+tc.name, ns)
			o.Spec.WebSSO = tc.websso

			expectAdmission(t, c.Create(ctx, o), tc.wantSub)
		})
	}
}

// TestIntegration_CRD_CELOnly_MultiDomain pins the CEL rules of the
// multiDomain block against an envtest API server with NO webhook installed,
// so the webhook twins cannot mask a missing rule.
//
// Rules: horizon_types.go:381, 382
func TestIntegration_CRD_CELOnly_MultiDomain(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := setupEnvTestNoWebhook(t)

	defaultDomain := []DomainChoice{{Name: "Default", Label: "Default"}}

	for _, tc := range []struct {
		name        string
		multiDomain *MultiDomainSpec
		wantSub     string // empty: the object must be admitted
	}{
		{
			name:        "dropdown-without-enabled",
			multiDomain: &MultiDomainSpec{Enabled: false, DomainDropdown: true, DomainChoices: defaultDomain},
			wantSub:     "multiDomain.domainDropdown requires multiDomain.enabled",
		},
		{
			name:        "dropdown-without-choices",
			multiDomain: &MultiDomainSpec{Enabled: true, DomainDropdown: true},
			wantSub:     "multiDomain.domainChoices must contain at least one choice when multiDomain.domainDropdown is true",
		},
		{
			name:        "dropdown-with-a-choice",
			multiDomain: &MultiDomainSpec{Enabled: true, DomainDropdown: true, DomainChoices: defaultDomain},
		},
		{
			name:        "enabled-without-dropdown",
			multiDomain: &MultiDomainSpec{Enabled: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, ctx, c, "celonly-multidomain-")
			o := integrationHorizon("multidomain-"+tc.name, ns)
			o.Spec.MultiDomain = tc.multiDomain

			expectAdmission(t, c.Create(ctx, o), tc.wantSub)
		})
	}
}
