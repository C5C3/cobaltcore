// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All multicluster envtest coverage of the Keystone reconciler lives in the one
// test function below, on purpose. The kubeconfig provider registers its
// registration-Secret watch under the fixed controller name
// "kubeconfig-provider" and exposes no SkipNameValidation escape, while
// controller-runtime validates controller names against a process-global set.
// A second provider anywhere in this test binary would therefore fail to
// register. One manager, one provider, one function: the scenarios are ordered
// subtests over the shared setup, and each one builds on the state the previous
// left behind.

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	kubeconfigprovider "sigs.k8s.io/multicluster-runtime/providers/kubeconfig"

	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
	"github.com/c5c3/forge/operators/keystone/internal/testutil"
)

// TestIntegration_Multicluster_KeystoneTargetCluster runs the reconciler on a
// management cluster with a second envtest environment registered as target
// cluster, and walks the target-cluster lifecycle: registration, a CR that
// projects its children onto the target, a CR that keeps them local, a CR
// naming an unregistered cluster, deregistration under a running CR, and a
// registration Secret the provider cannot parse.
func TestIntegration_Multicluster_KeystoneTargetCluster(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	const (
		// clustersNamespace mirrors the --clusters-namespace default the
		// operator binary passes to the provider.
		clustersNamespace = "c5c3-clusters"
		targetClusterName = "target-b"
		brokenClusterName = "target-broken"

		// The three Keystone CRs and their namespaces. Namespaces are fixed
		// rather than generated so the same name can be created on both
		// clusters: a child lands in the CR's namespace, whichever cluster it
		// is written to.
		targetNamespace  = "mc-target"
		targetKeystone   = "mc-target-keystone"
		localNamespace   = "mc-local"
		localKeystone    = "mc-local-keystone"
		unknownNamespace = "mc-unknown"
		unknownKeystone  = "mc-unknown-keystone"
		brokenNamespace  = "mc-broken"
		brokenKeystone   = "mc-broken-keystone"

		// engageTimeout bounds cluster engagement: the provider has to parse
		// the kubeconfig, build a cluster, and sync its cache before
		// GetCluster answers.
		engageTimeout = 60 * time.Second
	)

	// --- Environment B: the target cluster.
	//
	// It carries the fake CRDs of the external operators whose objects the
	// children include (MariaDB, ESO, cert-manager, Gateway API) and
	// deliberately NOT the Keystone CRD: a target cluster holds the workload,
	// never the CR. That is also what keeps the children alive here. Their
	// owner references point at a Keystone this API server cannot resolve, and
	// envtest runs no garbage collector to act on that.
	targetScheme := commonenvtest.BuildScheme(commonenvtest.CommonExternalSchemes()...)
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, targetScheme, commonenvtest.CommonFakeCRDDirs())

	// --- Environment A: the management cluster, hosting the manager.
	mgmtScheme := commonenvtest.BuildScheme(append(commonenvtest.CommonExternalSchemes(), keystonev1alpha1.AddToScheme)...)

	provider := kubeconfigprovider.New(kubeconfigprovider.Options{
		Namespace: clustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme, which knows no CRD kind, and the first
		// MariaDB or ESO child write fails with "no kind is registered".
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = mgmtScheme }},
	})

	crdDir, webhookDir := multiclusterKeystonePaths(t)

	var mcMgr mcmanager.Manager
	mgmtClient, ctx, _ := commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Keystone-multicluster",
		Scheme:            mgmtScheme,
		CRDDirectoryPaths: append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...),
		WebhookDir:        webhookDir,
		BuildManager: func(cfg *rest.Config, opts ctrl.Options) (ctrl.Manager, error) {
			m, err := mcmanager.New(cfg, provider, opts)
			if err != nil {
				return nil, err
			}
			mcMgr = m
			// The multicluster manager is not a ctrl.Manager (its Add takes
			// the multicluster Runnable), so the helper hosts and starts the
			// local one. That is the same thing: the multicluster manager's
			// Start adds a runnable provider to the local manager and then
			// starts it, and the kubeconfig provider is not a runnable one.
			// Its Secret watch is an ordinary controller on the local manager,
			// registered by SetupWithManager below.
			return m.GetLocalManager(), nil
		},
		RegisterWebhooks: func(mgr ctrl.Manager) error {
			if err := (&keystonev1alpha1.KeystoneWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&keystonev1alpha1.KeystoneIdentityBackendWebhook{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr)
		},
		RegisterController: func(mgr ctrl.Manager) error {
			// The provider's engagement machinery has to be registered before
			// the controllers, exactly as internal/common/bootstrap does it,
			// so engagement precedes the first reconcile.
			if err := provider.SetupWithManager(context.Background(), mcMgr); err != nil {
				return err
			}

			r := &KeystoneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("keystone-controller"),
				// The Resolver is the whole point of this test: it turns
				// spec.targetClusterRef into the client the children are
				// written with.
				Resolver:            mcMgr,
				HTTPClient:          testHealthyHTTPClient(),
				gatewayAPIAvailable: true,
			}
			// The sibling-backend list selects on a local cache field index,
			// so both indexes go on the local manager, mirroring what
			// SetupWithManager does in production.
			if err := registerSecretNameIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			if err := registerIdentityBackendIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			// Only the children the local-cluster subtest waits on are owned
			// here. A child on the target cluster produces no watch event on
			// this manager at all, so that CR advances on the sub-reconcilers'
			// own requeues.
			return ctrl.NewControllerManagedBy(mgr).
				For(&keystonev1alpha1.Keystone{}).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.ConfigMap{}).
				Owns(&batchv1.Job{}).
				Owns(&batchv1.CronJob{}).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToKeystoneWithBackendsMapper(mgr.GetClient()),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r)
		},
	})

	// The provider watches this namespace for registration Secrets.
	multiclusterEnsureNamespace(t, ctx, mgmtClient, clustersNamespace)

	targetKey := types.NamespacedName{Name: targetKeystone, Namespace: targetNamespace}

	t.Run("register", func(t *testing.T) {
		g := NewGomegaWithT(t)

		kubeconfig, err := commonenvtest.KubeconfigBytes(targetCfg, targetClusterName)
		g.Expect(err).NotTo(HaveOccurred(), "build kubeconfig for the target environment")

		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      targetClusterName,
				Namespace: clustersNamespace,
				Labels:    map[string]string{"sigs.k8s.io/multicluster-runtime-kubeconfig": "true"},
			},
			Data: map[string][]byte{"kubeconfig": kubeconfig},
		})).To(Succeed(), "create the registration Secret")

		g.Eventually(func() error {
			_, err := mcMgr.GetCluster(ctx, mcruntime.ClusterName(targetClusterName))
			return err
		}, engageTimeout, pollInterval).Should(Succeed(),
			"the provider should engage the target cluster from its registration Secret")
	})

	t.Run("targeted CR projects its children onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The CR lives on the management cluster, everything it needs and
		// everything it creates lives on the target cluster.
		multiclusterEnsureNamespace(t, ctx, mgmtClient, targetNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, targetNamespace)
		createPrerequisites(t, ctx, targetClient, targetNamespace)

		// Managed mode, so the MariaDB CRs are part of the projection. The
		// cluster CR is target-side infrastructure like the input Secrets.
		mariadbKey := client.ObjectKey{Namespace: targetNamespace, Name: "mariadb"}
		g.Expect(targetClient.Create(ctx, &mariadbv1alpha1.MariaDB{
			ObjectMeta: metav1.ObjectMeta{Name: mariadbKey.Name, Namespace: mariadbKey.Namespace},
		})).To(Succeed(), "create the MariaDB cluster CR on the target cluster")
		g.Expect(simulators.SimulateMariaDBReady(ctx, targetClient, mariadbKey, 1)).
			To(Succeed(), "simulate the MariaDB cluster ready")

		ks := integrationManagedKeystone(targetKeystone, targetNamespace)
		ks.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetClusterName}
		g.Expect(mgmtClient.Create(ctx, ks)).To(Succeed())

		// Conditions are read on the management cluster throughout, children
		// on the target cluster.
		waitForCondition(t, ctx, mgmtClient, targetKey, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
		waitForCondition(t, ctx, mgmtClient, targetKey, "FernetKeysReady", metav1.ConditionTrue, eventuallyTimeout)

		// The MariaDB CRs are created in sequence, each gated on the previous
		// one being ready. Nothing on the target cluster is watched, so every
		// step waits out the reconciler's own database requeue.
		childKey := client.ObjectKey{Namespace: targetNamespace, Name: targetKeystone}
		multiclusterEventuallyExists(t, ctx, targetClient, childKey, &mariadbv1alpha1.Database{}, "MariaDB Database", eventuallyLongTimeout)
		g.Expect(simulators.SimulateDatabaseReady(ctx, targetClient, childKey)).To(Succeed())

		multiclusterEventuallyExists(t, ctx, targetClient, childKey, &mariadbv1alpha1.User{}, "MariaDB User", eventuallyLongTimeout)
		g.Expect(simulators.SimulateUserReady(ctx, targetClient, childKey)).To(Succeed())

		multiclusterEventuallyExists(t, ctx, targetClient, childKey, &mariadbv1alpha1.Grant{}, "MariaDB Grant", eventuallyLongTimeout)
		g.Expect(simulators.SimulateGrantReady(ctx, targetClient, childKey)).To(Succeed())

		dbSyncKey := client.ObjectKey{Namespace: targetNamespace, Name: fmt.Sprintf("%s-db-sync", targetKeystone)}
		multiclusterEventuallyExists(t, ctx, targetClient, dbSyncKey, &batchv1.Job{}, "db-sync Job", eventuallyLongTimeout)
		g.Expect(simulators.SimulateJobComplete(ctx, targetClient, dbSyncKey)).To(Succeed())

		schemaCheckKey := client.ObjectKey{Namespace: targetNamespace, Name: fmt.Sprintf("%s-schema-check", targetKeystone)}
		multiclusterEventuallyExists(t, ctx, targetClient, schemaCheckKey, &batchv1.Job{}, "schema-check Job", eventuallyLongTimeout)
		g.Expect(simulators.SimulateJobComplete(ctx, targetClient, schemaCheckKey)).To(Succeed())

		waitForCondition(t, ctx, mgmtClient, targetKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)

		// The workload itself.
		multiclusterEventuallyExists(t, ctx, targetClient, childKey, &appsv1.Deployment{}, "Deployment", eventuallyLongTimeout)
		multiclusterEventuallyExists(t, ctx, targetClient, childKey, &corev1.Service{}, "Service", eventuallyTimeout)
		multiclusterEventuallyExists(t, ctx, targetClient,
			client.ObjectKey{Namespace: targetNamespace, Name: fmt.Sprintf("%s-fernet-keys", targetKeystone)},
			&corev1.Secret{}, "fernet-keys Secret", eventuallyTimeout)
		g.Expect(multiclusterConfigMapNames(t, ctx, targetClient, targetNamespace, targetKeystone+"-config-")).
			To(HaveLen(1), "expected exactly one rendered config ConfigMap on the target cluster")

		// None of it on the management cluster, where only the CR lives.
		multiclusterExpectAbsent(t, ctx, mgmtClient, childKey, &appsv1.Deployment{}, "Deployment")
		multiclusterExpectAbsent(t, ctx, mgmtClient, childKey, &corev1.Service{}, "Service")
		multiclusterExpectAbsent(t, ctx, mgmtClient, childKey, &mariadbv1alpha1.Database{}, "MariaDB Database")
		multiclusterExpectAbsent(t, ctx, mgmtClient, childKey, &mariadbv1alpha1.User{}, "MariaDB User")
		multiclusterExpectAbsent(t, ctx, mgmtClient, childKey, &mariadbv1alpha1.Grant{}, "MariaDB Grant")
		multiclusterExpectAbsent(t, ctx, mgmtClient, dbSyncKey, &batchv1.Job{}, "db-sync Job")
		multiclusterExpectAbsent(t, ctx, mgmtClient,
			client.ObjectKey{Namespace: targetNamespace, Name: fmt.Sprintf("%s-fernet-keys", targetKeystone)},
			&corev1.Secret{}, "fernet-keys Secret")
		g.Expect(multiclusterConfigMapNames(t, ctx, mgmtClient, targetNamespace, targetKeystone+"-config-")).
			To(BeEmpty(), "no config ConfigMap should be rendered on the management cluster")

		// Status and finalizers stay with the CR.
		ksAfter := &keystonev1alpha1.Keystone{}
		g.Expect(mgmtClient.Get(ctx, targetKey, ksAfter)).To(Succeed())
		g.Expect(ksAfter.Status.Conditions).NotTo(BeEmpty(), "status should be populated on the management cluster")
		g.Expect(controllerutil.ContainsFinalizer(ksAfter, keystoneFinalizer)).To(BeTrue(),
			"the main finalizer should be on the CR")
		g.Expect(controllerutil.ContainsFinalizer(ksAfter, keystoneOpenBaoFinalizer)).To(BeTrue(),
			"the OpenBao finalizer should be on the CR")
	})

	t.Run("CR without a ref keeps its children local", func(t *testing.T) {
		g := NewGomegaWithT(t)

		multiclusterEnsureNamespace(t, ctx, mgmtClient, localNamespace)
		createPrerequisites(t, ctx, mgmtClient, localNamespace)

		ks := integrationBrownfieldKeystone(localKeystone, localNamespace)
		g.Expect(ks.Spec.TargetClusterRef).To(BeNil(), "this CR must name no target cluster")
		g.Expect(mgmtClient.Create(ctx, ks)).To(Succeed())

		driveFullReconciliation(t, ctx, mgmtClient, localKeystone, localNamespace)

		childKey := client.ObjectKey{Namespace: localNamespace, Name: localKeystone}
		fernetKey := client.ObjectKey{Namespace: localNamespace, Name: fmt.Sprintf("%s-fernet-keys", localKeystone)}
		g.Expect(mgmtClient.Get(ctx, childKey, &appsv1.Deployment{})).To(Succeed(),
			"the Deployment should exist on the management cluster")
		g.Expect(mgmtClient.Get(ctx, fernetKey, &corev1.Secret{})).To(Succeed(),
			"the fernet-keys Secret should exist on the management cluster")

		multiclusterExpectAbsent(t, ctx, targetClient, childKey, &appsv1.Deployment{}, "Deployment")
		multiclusterExpectAbsent(t, ctx, targetClient, fernetKey, &corev1.Secret{}, "fernet-keys Secret")
	})

	t.Run("CR naming an unregistered cluster creates nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)

		multiclusterEnsureNamespace(t, ctx, mgmtClient, unknownNamespace)

		ks := integrationBrownfieldKeystone(unknownKeystone, unknownNamespace)
		ks.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "does-not-exist"}
		g.Expect(mgmtClient.Create(ctx, ks)).To(Succeed())

		key := types.NamespacedName{Name: unknownKeystone, Namespace: unknownNamespace}
		cond := multiclusterWaitForUnavailable(t, ctx, mgmtClient, key)
		g.Expect(cond.Message).To(ContainSubstring("cluster not found"),
			"the resolver's message should reach the condition verbatim")

		// Nothing was created, so nothing has to be cleaned up: the CR must
		// stay free of finalizers that would only block its deletion.
		ksAfter := &keystonev1alpha1.Keystone{}
		g.Expect(mgmtClient.Get(ctx, key, ksAfter)).To(Succeed())
		g.Expect(ksAfter.Finalizers).To(BeEmpty(), "an unresolvable CR should carry no finalizer")

		childKey := client.ObjectKey{Namespace: unknownNamespace, Name: unknownKeystone}
		fernetKey := client.ObjectKey{Namespace: unknownNamespace, Name: fmt.Sprintf("%s-fernet-keys", unknownKeystone)}
		for _, c := range []client.Client{mgmtClient, targetClient} {
			multiclusterExpectAbsent(t, ctx, c, childKey, &appsv1.Deployment{}, "Deployment")
			multiclusterExpectAbsent(t, ctx, c, fernetKey, &corev1.Secret{}, "fernet-keys Secret")
		}
	})

	t.Run("deregistration flips the running CR to TargetClusterUnavailable", func(t *testing.T) {
		g := NewGomegaWithT(t)

		g.Expect(mgmtClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: targetClusterName, Namespace: clustersNamespace},
		})).To(Succeed(), "delete the registration Secret")

		g.Eventually(func() error {
			_, err := mcMgr.GetCluster(ctx, mcruntime.ClusterName(targetClusterName))
			return err
		}, engageTimeout, pollInterval).ShouldNot(Succeed(), "the provider should disengage the target cluster")

		// The standing requeue would get there too; an annotation bump makes
		// the next pass immediate. The CR is fetched from the management
		// cluster, which is where it lives.
		g.Eventually(func() error {
			ks := &keystonev1alpha1.Keystone{}
			if err := mgmtClient.Get(ctx, targetKey, ks); err != nil {
				return err
			}
			if ks.Annotations == nil {
				ks.Annotations = map[string]string{}
			}
			ks.Annotations["test.c5c3.io/poke"] = time.Now().Format(time.RFC3339Nano)
			return mgmtClient.Update(ctx, ks)
		}, eventuallyTimeout, pollInterval).Should(Succeed(), "bump an annotation to trigger a reconcile")

		multiclusterWaitForUnavailable(t, ctx, mgmtClient, targetKey)

		// The children stay where they were written. Nothing deletes them: the
		// reconciler never reaches its sub-reconcilers without a client.
		childKey := client.ObjectKey{Namespace: targetNamespace, Name: targetKeystone}
		g.Expect(targetClient.Get(ctx, childKey, &appsv1.Deployment{})).To(Succeed(),
			"the Deployment on the target cluster should survive deregistration")
		g.Expect(targetClient.Get(ctx, childKey, &mariadbv1alpha1.Database{})).To(Succeed(),
			"the MariaDB Database on the target cluster should survive deregistration")
	})

	t.Run("deleting a CR whose cluster is gone releases its finalizers", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// Both halves of the abandon window start at wall-clock now: the API
		// server stamps the deletion timestamp, and this operator has not yet
		// failed to resolve the deregistered cluster on a deletion pass.
		// Compress the window, or this subtest would have to sit out the
		// production five minutes before the finalizers are released.
		abandonAfter := commonmulticluster.AbandonAfter
		t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
		commonmulticluster.AbandonAfter = time.Second

		ks := &keystonev1alpha1.Keystone{}
		g.Expect(mgmtClient.Get(ctx, targetKey, ks)).To(Succeed())
		g.Expect(ks.Finalizers).NotTo(BeEmpty(),
			"the CR still carries the finalizers its provisioning put on it")

		g.Expect(mgmtClient.Delete(ctx, ks)).To(Succeed())

		// Nothing can reach the deregistered cluster any more, so the cleanup
		// runs without it. Holding the finalizers instead would leave the CR —
		// and its namespace — Terminating until someone stripped them by hand.
		// The first pass falls inside the window and only requeues, so the
		// budget covers that requeue on top of the window itself.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, targetKey, &keystonev1alpha1.Keystone{}))
		}, commonmulticluster.AbandonAfter+commonreconcile.RequeueSecretPolling+eventuallyTimeout,
			pollInterval).Should(BeTrue(),
			"a CR whose target cluster was deregistered must still leave etcd")

		// Its children stay behind on the target cluster, unreachable and
		// unowned: the leak the remote-ownership work has to close.
		childKey := client.ObjectKey{Namespace: targetNamespace, Name: targetKeystone}
		g.Expect(targetClient.Get(ctx, childKey, &appsv1.Deployment{})).To(Succeed(),
			"the Deployment on the deregistered cluster is abandoned, not deleted")
		g.Expect(targetClient.Get(ctx, childKey, &mariadbv1alpha1.Database{})).To(Succeed(),
			"the MariaDB Database on the deregistered cluster is abandoned, not deleted")
	})

	t.Run("unparseable registration Secret never engages", func(t *testing.T) {
		g := NewGomegaWithT(t)

		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      brokenClusterName,
				Namespace: clustersNamespace,
				Labels:    map[string]string{"sigs.k8s.io/multicluster-runtime-kubeconfig": "true"},
			},
			Data: map[string][]byte{"kubeconfig": []byte("not a kubeconfig")},
		})).To(Succeed())

		g.Consistently(func() error {
			_, err := mcMgr.GetCluster(ctx, mcruntime.ClusterName(brokenClusterName))
			return err
		}, 5*time.Second, pollInterval).ShouldNot(Succeed(),
			"a kubeconfig the provider cannot parse must never yield an engaged cluster")

		multiclusterEnsureNamespace(t, ctx, mgmtClient, brokenNamespace)
		ks := integrationBrownfieldKeystone(brokenKeystone, brokenNamespace)
		ks.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: brokenClusterName}
		g.Expect(mgmtClient.Create(ctx, ks)).To(Succeed())

		key := types.NamespacedName{Name: brokenKeystone, Namespace: brokenNamespace}
		multiclusterWaitForUnavailable(t, ctx, mgmtClient, key)

		childKey := client.ObjectKey{Namespace: brokenNamespace, Name: brokenKeystone}
		fernetKey := client.ObjectKey{Namespace: brokenNamespace, Name: fmt.Sprintf("%s-fernet-keys", brokenKeystone)}
		for _, c := range []client.Client{mgmtClient, targetClient} {
			multiclusterExpectAbsent(t, ctx, c, childKey, &appsv1.Deployment{}, "Deployment")
			multiclusterExpectAbsent(t, ctx, c, fernetKey, &corev1.Secret{}, "fernet-keys Secret")
		}
	})
}

// multiclusterKeystonePaths returns the Keystone CRD and webhook manifest
// directories, resolved relative to this source file. The per-operator testutil
// package resolves them the same way for its own helpers, which do not expose
// the manager hook this test needs.
func multiclusterKeystonePaths(t testing.TB) (crdDir, webhookDir string) {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to determine the source file path")
	}
	base := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(base, "config", "crd", "bases"), filepath.Join(base, "config", "webhook")
}

// multiclusterEnsureNamespace creates the namespace on c, tolerating one that
// already exists so a subtest can seed the same name on both clusters.
func multiclusterEnsureNamespace(t testing.TB, ctx context.Context, c client.Client, name string) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsAlreadyExists(err) {
		return
	}
	g.Expect(err).NotTo(HaveOccurred(), "create namespace %s", name)
}

// multiclusterEventuallyExists polls c until the object at key exists.
func multiclusterEventuallyExists(
	t testing.TB,
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	obj client.Object,
	what string,
	timeout time.Duration,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return c.Get(ctx, key, obj)
	}, timeout, pollInterval).Should(Succeed(), "%s %s should exist", what, key)
}

// multiclusterExpectAbsent asserts the object at key does not exist on c. A
// missing namespace answers NotFound too, so this also covers a cluster the CR
// never touched at all.
func multiclusterExpectAbsent(
	t testing.TB,
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	obj client.Object,
	what string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Get(ctx, key, obj)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%s %s should not exist, got %v", what, key, err)
}

// multiclusterConfigMapNames returns the names of the ConfigMaps in ns on c
// whose name starts with prefix. The rendered config ConfigMap is immutable and
// content-addressed, so it can only be found by prefix.
func multiclusterConfigMapNames(t testing.TB, ctx context.Context, c client.Client, ns, prefix string) []string {
	t.Helper()
	g := NewGomegaWithT(t)

	list := &corev1.ConfigMapList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns))).To(Succeed())

	var names []string
	for _, cm := range list.Items {
		if strings.HasPrefix(cm.Name, prefix) {
			names = append(names, cm.Name)
		}
	}
	return names
}

// multiclusterWaitForUnavailable polls the Keystone CR on the management
// cluster until SecretsReady reports the failed target-cluster resolution, and
// returns that condition.
func multiclusterWaitForUnavailable(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() string {
		ks := &keystonev1alpha1.Keystone{}
		if err := c.Get(ctx, key, ks); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(ks.Status.Conditions, "SecretsReady")
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return ""
		}
		return cond.Reason
	}, eventuallyTimeout, pollInterval).Should(Equal(commonmulticluster.TargetClusterUnavailable),
		"SecretsReady should report the unresolvable target cluster")

	return cond
}
