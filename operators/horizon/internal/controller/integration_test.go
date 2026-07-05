// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package controller contains integration tests for the Horizon reconciler.
package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
	"github.com/c5c3/forge/operators/horizon/internal/testutil"
)

// Test timeout constants for CI tuning.
const (
	eventuallyTimeout = 30 * time.Second
	pollInterval      = 500 * time.Millisecond
)

// healthyHTTPClient responds 200 to every probe so HorizonAPIReady converges
// without a live dashboard (the internal Service DNS does not resolve in
// envtest).
type healthyHTTPClient struct{}

func (healthyHTTPClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

// setupEnvTestWithController wraps testutil.SetupHorizonEnvTestWithController
// with the v1alpha1 scheme, webhook, and controller registration callbacks.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupHorizonEnvTestWithController(
		t,
		horizonv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			return (&horizonv1alpha1.HorizonWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			r := &HorizonReconciler{
				Client:     mgr.GetClient(),
				Scheme:     mgr.GetScheme(),
				HTTPClient: healthyHTTPClient{},
				// envtest loads the fake HTTPRoute CRD from
				// internal/common/testutil/fake_crds/gateway-api, so the
				// Gateway API kind is available to the reconciler. Mirror
				// what SetupWithManager would set from the RESTMapper.
				gatewayAPIAvailable: true,
			}
			// Register the Horizon field indexer so secretToHorizonMapper's
			// MatchingFields lookup works in integration tests, mirroring
			// what SetupWithManager does in production.
			if err := registerSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&horizonv1alpha1.Horizon{}).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.ConfigMap{}).
				Owns(&policyv1.PodDisruptionBudget{}).
				Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
				Owns(&networkingv1.NetworkPolicy{}).
				Owns(&gatewayv1.HTTPRoute{}).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToHorizonMapper(mgr.GetClient()),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r)
		},
	)
}

// integrationHorizon returns a valid Horizon CR for integration tests
// (brownfield cache so no Memcached CR is needed).
func integrationHorizon(name, namespace string) *horizonv1alpha1.Horizon {
	return &horizonv1alpha1.Horizon{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: horizonv1alpha1.HorizonSpec{
			Deployment: horizonv1alpha1.DeploymentSpec{Replicas: 3},
			Image:      commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Tag: "2025.2"},
			Cache: commonv1.CacheSpec{
				Servers: []string{"memcached-0.memcached:11211"},
				Backend: horizonv1alpha1.DefaultCacheBackend,
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			SecretKeyRef:     commonv1.SecretRefSpec{Name: "horizon-secret-key", Key: "secret-key"},
		},
	}
}

// ensureReadyClusterSecretStore creates or refreshes the OpenBao-backed
// ClusterSecretStore with a Ready=True condition. reconcileSecrets gates on
// this status; without it every integration test would flip to
// SecretsReady=False with reason SecretStoreNotReady.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName},
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(store), store)
	if apierrors.IsNotFound(err) {
		g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")
	} else {
		g.Expect(err).NotTo(HaveOccurred(), "get ClusterSecretStore")
	}

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "update ClusterSecretStore status")
}

// createPrerequisites creates the SECRET_KEY ExternalSecret and Secret the
// Horizon reconciler expects to find, and simulates the ESO sync.
func createPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	ensureReadyClusterSecretStore(t, ctx, c)

	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "horizon-secret-key", Namespace: ns},
		Spec: esov1.ExternalSecretSpec{
			SecretStoreRef: esov1.SecretStoreRef{
				Kind: "ClusterSecretStore",
				Name: openBaoClusterStoreName,
			},
			Target: esov1.ExternalSecretTarget{
				Name: "horizon-secret-key",
			},
		},
	}
	g.Expect(c.Create(ctx, es)).To(Succeed(), "create SECRET_KEY ExternalSecret")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "horizon-secret-key", Namespace: ns},
		Data:       map[string][]byte{"secret-key": []byte("initial-django-secret")},
	}
	g.Expect(c.Create(ctx, secret)).To(Succeed(), "create SECRET_KEY Secret")

	g.Expect(simulators.SimulateExternalSecretSync(ctx, c, client.ObjectKey{Namespace: ns, Name: "horizon-secret-key"})).
		To(Succeed(), "simulate SECRET_KEY ExternalSecret sync")
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "horizon-it-"},
	}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	return ns.Name
}

// waitForCondition polls the Horizon CR until the named condition reaches
// the expected status, or the timeout is reached. Returns the condition.
func waitForCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expectedStatus metav1.ConditionStatus, timeout time.Duration) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		h := &horizonv1alpha1.Horizon{}
		if err := c.Get(ctx, key, h); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(h.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expectedStatus),
		fmt.Sprintf("condition %s should reach %s", condType, expectedStatus))

	return cond
}

// driveToReady simulates external dependencies until the CR reaches
// Ready=True: the SECRET_KEY sync happens in createPrerequisites; only the
// Deployment availability needs simulating (kubelet does not run in envtest).
func driveToReady(t testing.TB, ctx context.Context, c client.Client, name, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)
	key := types.NamespacedName{Name: name, Namespace: ns}

	waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, conditionTypeConfigReady, metav1.ConditionTrue, eventuallyTimeout)

	deployKey := client.ObjectKey{Namespace: ns, Name: name}
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, &appsv1.Deployment{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "dashboard Deployment should appear")

	var deploy appsv1.Deployment
	g.Expect(c.Get(ctx, deployKey, &deploy)).To(Succeed())
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, *deploy.Spec.Replicas)).
		To(Succeed(), "simulate Deployment availability")

	waitForCondition(t, ctx, c, key, "DeploymentReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionTrue, eventuallyTimeout)
}

// TestIntegrationHorizon_FullChainToReady drives a Horizon CR through the
// whole condition ladder to the aggregate Ready and asserts the endpoint plus
// the rendered artifacts.
func TestIntegrationHorizon_FullChainToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createPrerequisites(t, ctx, c, ns)

	h := integrationHorizon("horizon-full", ns)
	g.Expect(c.Create(ctx, h)).To(Succeed(), "create Horizon CR")

	driveToReady(t, ctx, c, "horizon-full", ns)

	key := types.NamespacedName{Name: "horizon-full", Namespace: ns}

	// Sub-conditions that need no external simulation converge on their own.
	httpRoute := waitForCondition(t, ctx, c, key, conditionTypeHTTPRouteReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(httpRoute.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))
	waitForCondition(t, ctx, c, key, conditionTypeHorizonAPIReady, metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, "HPAReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForCondition(t, ctx, c, key, conditionTypeNetworkPolicyReady, metav1.ConditionTrue, eventuallyTimeout)

	var got horizonv1alpha1.Horizon
	g.Expect(c.Get(ctx, key, &got)).To(Succeed())
	g.Expect(got.Status.Endpoint).To(Equal(fmt.Sprintf("http://horizon-full.%s.svc.cluster.local:8080/", ns)))
	g.Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))

	// The rendered ConfigMap holds local_settings.py with the env-sourced
	// SECRET_KEY line and never the raw key material.
	var deploy appsv1.Deployment
	g.Expect(c.Get(ctx, key, &deploy)).To(Succeed())
	cmName := deploy.Spec.Template.Spec.Volumes[0].ConfigMap.Name
	var cm corev1.ConfigMap
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: cmName}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]
	g.Expect(rendered).To(ContainSubstring(`SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`))
	g.Expect(rendered).NotTo(ContainSubstring("initial-django-secret"))
}

// TestIntegrationHorizon_SecretRotationRollsDeployment proves that rotating
// the SECRET_KEY at the source flips the pod-template hash annotation so the
// Deployment rolls (the key is env-var-consumed and needs a pod restart).
func TestIntegrationHorizon_SecretRotationRollsDeployment(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	createPrerequisites(t, ctx, c, ns)

	h := integrationHorizon("horizon-rotate", ns)
	g.Expect(c.Create(ctx, h)).To(Succeed(), "create Horizon CR")
	driveToReady(t, ctx, c, "horizon-rotate", ns)

	deployKey := types.NamespacedName{Namespace: ns, Name: "horizon-rotate"}
	var deploy appsv1.Deployment
	g.Expect(c.Get(ctx, deployKey, &deploy)).To(Succeed())
	hashBefore := deploy.Spec.Template.Annotations[secretKeyHashAnnotation]
	g.Expect(hashBefore).NotTo(BeEmpty(), "the hash annotation must be stamped once Secrets are ready")

	// Rotate the key material; the Secret watch re-enqueues the CR.
	var secret corev1.Secret
	g.Expect(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "horizon-secret-key"}, &secret)).To(Succeed())
	secret.Data["secret-key"] = []byte("rotated-django-secret")
	g.Expect(c.Update(ctx, &secret)).To(Succeed())

	g.Eventually(func() string {
		var d appsv1.Deployment
		if err := c.Get(ctx, deployKey, &d); err != nil {
			return hashBefore
		}
		return d.Spec.Template.Annotations[secretKeyHashAnnotation]
	}, eventuallyTimeout, pollInterval).ShouldNot(Equal(hashBefore),
		"a rotated SECRET_KEY must change the pod-template hash annotation")
}

// TestIntegrationHorizon_WebhookRejectsInvalidEndpoint proves the validating
// webhook is wired through envtest admission: a non-URL keystoneEndpoint is
// rejected by the API server, not just by a direct validate() call.
func TestIntegrationHorizon_WebhookRejectsInvalidEndpoint(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)

	h := integrationHorizon("horizon-invalid", ns)
	h.Spec.KeystoneEndpoint = "ldap://keystone:389"

	err := c.Create(ctx, h)
	g.Expect(err).To(HaveOccurred(), "webhook must reject a non-http(s) keystoneEndpoint")
	g.Expect(err.Error()).To(ContainSubstring("keystoneEndpoint"))
}

// TestIntegrationHorizon_MissingSecretKeyBlocksReady covers the error path:
// without the SECRET_KEY Secret, the chain must stop at SecretsReady=False
// with an aggregate Ready=False and no Deployment created.
func TestIntegrationHorizon_MissingSecretKeyBlocksReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	// Store is ready but no ExternalSecret/Secret exists.
	ensureReadyClusterSecretStore(t, ctx, c)

	h := integrationHorizon("horizon-blocked", ns)
	g.Expect(c.Create(ctx, h)).To(Succeed(), "create Horizon CR")

	key := types.NamespacedName{Name: "horizon-blocked", Namespace: ns}
	cond := waitForCondition(t, ctx, c, key, "SecretsReady", metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(cond.Reason).To(Equal("WaitingForSecretKey"))
	waitForCondition(t, ctx, c, key, "Ready", metav1.ConditionFalse, eventuallyTimeout)

	err := c.Get(ctx, key, &appsv1.Deployment{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Deployment may exist while the secret gate blocks")
}
