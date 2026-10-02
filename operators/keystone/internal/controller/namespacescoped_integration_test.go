// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/keystone/internal/testutil"
)

// TestIntegration_NamespaceScoped_StartsUnderRoleOnlyRBAC runs the production
// watch chain the way a release installed with rbac.namespaceScoped=true runs
// it: as a user bound only to a Role carrying the rules of config/rbac/role.yaml,
// with the cache restricted to the one namespace. envtest's API server
// authorizes with RBAC, so a watch or a cached read on a cluster-scoped kind is
// forbidden here as it is in a cluster. Such an informer never syncs:
// every other source then times out in WaitForCacheSync and Start returns, or a
// reconcile worker blocks in the cached Get and the CR gets no condition.
//
// The manager has to stay up past twice its cache-sync timeout, a Keystone that
// selects a namespaced SecretStore has to be reconciled, and one that omits
// spec.secretStoreRef has to be refused with a condition until its spec selects
// a SecretStore. There is no subtest with NamespaceScoped false: that variant
// ends in the cache-sync timeout or, in some runs, in a Go runtime fatal while
// controller-runtime formats the error, which would take the whole test binary
// down.
func TestIntegration_NamespaceScoped_StartsUnderRoleOnlyRBAC(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	const (
		namespace    = "team-alpha"
		operatorName = "keystone-operator"
		operatorUser = "system:serviceaccount:team-alpha:keystone-operator"
		// cacheSyncTimeout replaces controller-runtime's two-minute default, so
		// a source that cannot sync ends Start well inside the test.
		cacheSyncTimeout = 15 * time.Second
		// upWindow is how long Start must not return after launch.
		upWindow = 2 * cacheSyncTimeout
	)

	scheme := commonenvtest.BuildScheme(append(commonenvtest.CommonExternalSchemes(), keystonev1alpha1.AddToScheme)...)
	crdDir, _ := multiclusterKeystonePaths(t)
	admin, cfg := commonenvtest.StartEnvTestWithConfig(t, scheme,
		append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...))
	ctx := context.Background()

	// The Role carries exactly the rules the chart renders into its Role for
	// rbac.namespaceScoped=true. Its cluster-scoped entries (clustersecretstores,
	// priorityclasses) grant nothing in a Role, which is the point.
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	g.Expect(err).NotTo(HaveOccurred(), "read config/rbac/role.yaml")
	var clusterRole rbacv1.ClusterRole
	g.Expect(yaml.Unmarshal(raw, &clusterRole)).To(Succeed(), "unmarshal config/rbac/role.yaml")
	g.Expect(clusterRole.Rules).NotTo(BeEmpty(), "config/rbac/role.yaml carries no rules")

	g.Expect(admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	g.Expect(admin.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: operatorName, Namespace: namespace},
		Rules:      clusterRole.Rules,
	})).To(Succeed())
	g.Expect(admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: operatorName, Namespace: namespace},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: operatorUser}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: operatorName},
	})).To(Succeed())

	// Every request the manager makes goes out as the operator's service
	// account; system:authenticated keeps the discovery the RESTMapper needs.
	operatorCfg := rest.CopyConfig(cfg)
	operatorCfg.Impersonate = rest.ImpersonationConfig{
		UserName: operatorUser,
		Groups:   []string{"system:authenticated"},
	}

	// The test proves nothing unless the API server enforces the boundary: the
	// operator's user must not be able to list the cluster-scoped store kind.
	operatorClient, err := client.New(operatorCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred(), "create impersonating client")
	err = operatorClient.List(ctx, &esov1.ClusterSecretStoreList{})
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(),
		"listing ClusterSecretStores as %s must be forbidden, got %v", operatorUser, err)

	mgr, err := ctrl.NewManager(operatorCfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	g.Expect(err).NotTo(HaveOccurred(), "create manager")
	mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
	g.Expect(err).NotTo(HaveOccurred(), "wrap manager")

	r := &KeystoneReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Recorder:        mgr.GetEventRecorderFor("keystone-controller"),
		Resolver:        mcMgr,
		HTTPClient:      testHealthyHTTPClient(),
		NamespaceScoped: true,
	}
	opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
	opts.SkipNameValidation = ptr.To(true)
	opts.CacheSyncTimeout = cacheSyncTimeout
	g.Expect(r.setupWithOptions(mcMgr, opts)).To(Succeed())

	tenantStore := integrationBrownfieldKeystone("keystone-tenant-store", namespace)
	tenantStore.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "openbao-tenant-store",
	}
	defaultStore := integrationBrownfieldKeystone("keystone-default-store", namespace)
	g.Expect(admin.Create(ctx, tenantStore)).To(Succeed())
	g.Expect(admin.Create(ctx, defaultStore)).To(Succeed())

	mgrCtx, cancel := context.WithCancel(ctx)
	// done is closed once Start returns, after startErr is written, so every
	// check below can observe the stop without consuming it.
	done := make(chan struct{})
	var startErr error
	launched := time.Now()
	go func() {
		startErr = mcMgr.Start(mgrCtx)
		close(done)
	}()
	// Registered after envtest's own cleanup, so it runs first and the manager
	// stops before the API server does.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("manager did not stop within 30s of cancellation")
		}
	})
	managerRunning := func() error {
		select {
		case <-done:
			return fmt.Errorf("manager stopped %s after launch: %w", time.Since(launched).Round(time.Second), startErr)
		default:
			return nil
		}
	}

	secretsReason := func(cr *keystonev1alpha1.Keystone) func(Gomega) string {
		return func(g Gomega) string {
			if err := managerRunning(); err != nil {
				StopTrying(err.Error()).Now()
			}
			var got keystonev1alpha1.Keystone
			g.Expect(admin.Get(ctx, client.ObjectKeyFromObject(cr), &got)).To(Succeed())
			cond := meta.FindStatusCondition(got.Status.Conditions, "SecretsReady")
			g.Expect(cond).NotTo(BeNil(), "SecretsReady not set yet")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			return cond.Reason
		}
	}

	// The namespaced store does not exist, so the gate's cached read returns
	// NotFound and the store is reported not ready: the worker is not blocked.
	g.Eventually(secretsReason(tenantStore), eventuallyTimeout, pollInterval).
		Should(Equal("SecretStoreNotReady"))
	// The omitted field resolves to the cluster-scoped default, which the
	// operator refuses without reading it.
	g.Eventually(secretsReason(defaultStore), eventuallyTimeout, pollInterval).
		Should(Equal("ClusterSecretStoreUnsupported"))

	// Selecting a SecretStore clears the refusal: the spec change wakes the
	// Keystone, and the gate reads the namespaced store instead.
	var refused keystonev1alpha1.Keystone
	g.Expect(admin.Get(ctx, client.ObjectKeyFromObject(defaultStore), &refused)).To(Succeed())
	patch := client.MergeFrom(refused.DeepCopy())
	refused.Spec.SecretStoreRef = &commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: "openbao-tenant-store",
	}
	g.Expect(admin.Patch(ctx, &refused, patch)).To(Succeed())
	g.Eventually(secretsReason(defaultStore), eventuallyTimeout, pollInterval).
		Should(Equal("SecretStoreNotReady"))

	if remaining := upWindow - time.Since(launched); remaining > 0 {
		g.Consistently(managerRunning, remaining, pollInterval).Should(Succeed(),
			"manager must stay up for %s after launch", upWindow)
	}
	g.Expect(managerRunning()).To(Succeed(), "manager stopped before the test ended")
}
