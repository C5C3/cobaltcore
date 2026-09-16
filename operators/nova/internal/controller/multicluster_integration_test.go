// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All multicluster envtest coverage of the Nova reconciler lives in the one test
// function below, on purpose. The kubeconfig provider registers its
// registration-Secret watch under the fixed controller name "kubeconfig-provider"
// and exposes no SkipNameValidation escape, while controller-runtime validates
// controller names against a process-global set. A second provider anywhere in
// this test binary would therefore fail to register. One manager, one provider,
// one function: the scenarios are ordered subtests over the shared setup, and
// each one builds on the state the previous left behind.
//
// The reconciler is registered through the production watch wiring
// (setupWithOptions, the chain SetupWithManager applies) with SkipNameValidation
// set, exactly as the single-cluster suite in integration_test.go registers it. A
// skipped registration never claims the controller name, so the constraint that
// only one registration per test binary may run under the real name stays intact.

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/testutil"
)

// TestIntegration_Multicluster_NovaTargetCluster runs the reconciler on a
// management cluster with a second envtest environment registered as target
// cluster, and walks the target-cluster lifecycle of one Nova: registration, a CR
// that projects its whole fleet onto the target, a CR naming an unregistered
// cluster, and the teardown that sweeps off the target everything the CR put
// there.
func TestIntegration_Multicluster_NovaTargetCluster(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	const (
		// clustersNamespace mirrors the --clusters-namespace default the
		// operator binary passes to the provider.
		clustersNamespace = "c5c3-clusters"
		targetClusterName = "target-nova"
		unknownCluster    = "does-not-exist"

		// The namespaces are fixed rather than generated so the same name can be
		// created on both clusters: a child lands in the CR's namespace,
		// whichever cluster it is written to.
		targetNamespace  = "mc-nova"
		unknownNamespace = "mc-unknown"

		// survivorConfigMap is written into the target namespace by nobody in
		// particular, carrying none of the ownership labels. The teardown subtest
		// reads it back afterwards: a sweep that took it would be taking somebody
		// else's object.
		survivorConfigMap = "mc-unrelated"

		// engageTimeout bounds cluster engagement: the provider has to parse the
		// kubeconfig, build a cluster, and sync its cache before GetCluster
		// answers.
		engageTimeout = 60 * time.Second
	)

	targetRef := &commonv1.TargetClusterRefSpec{Name: targetClusterName}

	// --- Environment B: the target cluster.
	//
	// It carries the fake CRDs of the external operators whose objects the
	// children include (MariaDB, RabbitMQ and ESO above all) and deliberately NOT
	// the Nova CRD: a target cluster holds the workload, never the CR. That is
	// also what keeps the children alive here. Their owner references would point
	// at a CR this API server cannot resolve, and envtest runs no garbage
	// collector to act on that.
	targetScheme := commonenvtest.BuildScheme(commonenvtest.CommonExternalSchemes()...)
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, targetScheme, commonenvtest.CommonFakeCRDDirs())

	// --- Environment A: the management cluster, hosting the manager and the Nova
	// kind it reconciles.
	mgmtScheme := commonenvtest.BuildScheme(append(commonenvtest.CommonExternalSchemes(),
		novav1alpha1.AddToScheme)...)

	provider := commonmulticluster.NewKubeconfigProvider(commonmulticluster.KubeconfigProviderOptions{
		Namespace: clustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme, which knows no CRD kind, and the first
		// MariaDB Database the schema step applies fails with "no kind is
		// registered".
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = mgmtScheme }},
	})

	crdDir, webhookDir := multiclusterNovaPaths(t)

	var mcMgr mcmanager.Manager
	mgmtClient, ctx, _ := commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Nova-multicluster",
		Scheme:            mgmtScheme,
		CRDDirectoryPaths: append([]string{crdDir}, commonenvtest.CommonFakeCRDDirs()...),
		WebhookDir:        webhookDir,
		BuildManager: func(cfg *rest.Config, opts ctrl.Options) (ctrl.Manager, error) {
			m, err := mcmanager.New(cfg, provider, opts)
			if err != nil {
				return nil, err
			}
			mcMgr = m
			// The multicluster manager is not a ctrl.Manager (its Add takes the
			// multicluster Runnable), so the helper hosts and starts the local one.
			// That is the same thing: the multicluster manager's Start adds a
			// runnable provider to the local manager and then starts it, and the
			// kubeconfig provider is not a runnable one. Its Secret watch is an
			// ordinary controller on the local manager, registered by
			// SetupWithManager below.
			return m.GetLocalManager(), nil
		},
		RegisterWebhooks: registerNovaWebhooks,
		RegisterController: func(mgr ctrl.Manager) error {
			// The provider's engagement machinery has to be registered before the
			// controller, exactly as internal/common/bootstrap does it, so
			// engagement precedes the first reconcile.
			if err := provider.SetupWithManager(context.Background(), mcMgr); err != nil {
				return err
			}
			// The multicluster manager is the Resolver: it turns
			// spec.targetClusterRef into the client the children are written with.
			return registerNovaController(mgr, mcMgr, mcMgr)
		},
	})

	// The provider watches this namespace for registration Secrets.
	multiclusterEnsureNamespace(t, ctx, mgmtClient, clustersNamespace)

	novaKey := types.NamespacedName{Name: integrationNovaName, Namespace: targetNamespace}

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

	t.Run("targeted Nova projects its children onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The CR lives on the management cluster, everything it reads and
		// everything it creates lives on the target cluster. The broker and the
		// MariaDB cluster are among the inputs: the transport-URL and schema flows
		// read and write in the CR's namespace through the children client, which
		// for a placed CR is the target's.
		multiclusterEnsureNamespace(t, ctx, mgmtClient, targetNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, targetNamespace)
		createNovaPrerequisites(t, ctx, targetClient, targetNamespace)

		g.Expect(mgmtClient.Create(ctx, integrationNovaCR(integrationNovaName, targetNamespace, targetRef))).
			To(Succeed(), "create the placed Nova CR")

		waitForNovaCondition(t, ctx, mgmtClient, novaKey, "SecretsReady",
			metav1.ConditionTrue, eventuallyTimeout)

		// Both schemas are provisioned on the target too: a placed CR projects its
		// MariaDB CRs beside the workloads that query them.
		driveDatabaseCRs(t, ctx, targetClient, integrationNovaName, targetNamespace)
		completeDBSync(t, ctx, targetClient, integrationNovaName, targetNamespace)
		waitForNovaCondition(t, ctx, mgmtClient, novaKey, "DatabaseReady",
			metav1.ConditionTrue, eventuallyLongTimeout)

		// envtest runs no Deployment controller on either cluster, so the five
		// rollouts are completed here. The archive CronJob is projected behind the
		// API step, so without this the inventory below would be missing its tail.
		for _, key := range novaWorkloadKeys(integrationNovaName, targetNamespace) {
			markDeploymentReady(t, ctx, targetClient, key)
		}
		waitForNovaCondition(t, ctx, mgmtClient, novaKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		api := &appsv1.Deployment{}
		g.Expect(targetClient.Get(ctx, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationNovaName,
		}, api)).To(Succeed())
		configMapName := mountedConfigMapName(&api.Spec.Template.Spec)
		g.Expect(configMapName).To(HavePrefix(integrationNovaName+"-config-"),
			"the config volume must mount the content-hashed ConfigMap")

		// Every kind the fleet projects, claimed by the ownership labels and by
		// nothing else. A reference would name a UID this cluster cannot resolve,
		// and the labels are the only handle the teardown sweep has.
		for _, child := range novaRemoteChildren(integrationNovaName, targetNamespace, configMapName) {
			multiclusterExpectRemoteOwnership(t, ctx, targetClient, child.key, child.obj, child.what,
				"Nova", integrationNovaName, targetNamespace)
			// None of it on the management cluster, where only the CR lives.
			multiclusterExpectAbsent(t, ctx, mgmtClient, child.key, child.obj, child.what)
		}

		// Status and the finalizers stay with the CR.
		after := &novav1alpha1.Nova{}
		g.Expect(mgmtClient.Get(ctx, novaKey, after)).To(Succeed())
		g.Expect(after.Status.Conditions).NotTo(BeEmpty(), "status should be populated on the management cluster")
		g.Expect(after.Status.ComputeConfigSecretRef).NotTo(BeNil(),
			"the compute contract is named on the CR wherever its Secret was written")
		g.Expect(controllerutil.ContainsFinalizer(after, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"the remote-children finalizer should be on a CR whose children live on another cluster")
	})

	t.Run("a Nova naming an unregistered cluster creates nothing and carries no finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)

		multiclusterEnsureNamespace(t, ctx, mgmtClient, unknownNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, unknownNamespace)

		unknownRef := &commonv1.TargetClusterRefSpec{Name: unknownCluster}
		g.Expect(mgmtClient.Create(ctx, integrationNovaCR(integrationNovaName, unknownNamespace, unknownRef))).
			To(Succeed())

		// The unresolvable cluster is reported on the pipeline's first gate, which
		// is the condition the rest of the graph waits behind.
		unresolved := types.NamespacedName{Name: integrationNovaName, Namespace: unknownNamespace}
		cond := waitForNovaCondition(t, ctx, mgmtClient, unresolved, "SecretsReady",
			metav1.ConditionFalse, eventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))

		// Nothing was created, so nothing has to be cleaned up: the CR may not
		// carry a finalizer that would only block its deletion.
		got := &novav1alpha1.Nova{}
		g.Expect(mgmtClient.Get(ctx, unresolved, got)).To(Succeed())
		g.Expect(got.Finalizers).To(BeEmpty(), "an unresolvable Nova should carry no finalizer")

		// The config ConfigMap of a CR that rendered none has no name, so the
		// inventory is asked for the fixed children alone.
		for _, c := range []client.Client{mgmtClient, targetClient} {
			for _, child := range novaRemoteChildren(integrationNovaName, unknownNamespace, "") {
				multiclusterExpectAbsent(t, ctx, c, child.key, child.obj, child.what)
			}
		}
	})

	t.Run("deleting the Nova sweeps its children off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// What the sweep must leave standing. It carries none of the ownership
		// labels, so a sweep that took it would be taking somebody else's object.
		g.Expect(targetClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: survivorConfigMap, Namespace: targetNamespace},
			Data:       map[string]string{"owner": "nobody"},
		})).To(Succeed(), "seed an unlabelled ConfigMap beside the children")

		api := &appsv1.Deployment{}
		g.Expect(targetClient.Get(ctx, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationNovaName,
		}, api)).To(Succeed())

		projected := novaRemoteChildren(integrationNovaName, targetNamespace,
			mountedConfigMapName(&api.Spec.Template.Spec))
		for _, child := range projected {
			g.Expect(targetClient.Get(ctx, child.key, child.obj)).To(Succeed(),
				"%s %s must be on the cluster before the deletion, or its absence afterwards proves nothing",
				child.what, child.key)
		}

		nova := &novav1alpha1.Nova{}
		g.Expect(mgmtClient.Get(ctx, novaKey, nova)).To(Succeed())
		g.Expect(mgmtClient.Delete(ctx, nova)).To(Succeed(), "delete the placed Nova")

		// Two finalizers come off in order: the named MariaDB cleanup holds the CR
		// for one more pass so the teardown of both schemas is triggered, and the
		// label-selected sweep runs behind it.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, novaKey, &novav1alpha1.Nova{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CR should leave etcd once both finalizers are released")

		// The sweep does not wait for the objects it deleted, and a delete is
		// asynchronous, so each child is polled until it is gone.
		g.Eventually(func(ig Gomega) {
			for _, child := range projected {
				expectSwept(ig, ctx, targetClient, child)
			}
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())

		g.Expect(targetClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: survivorConfigMap},
			&corev1.ConfigMap{})).To(Succeed(), "an unlabelled ConfigMap should survive the sweep")
		// The credentials the pipeline reads are inputs rather than children:
		// nobody labelled them, so the sweep has no claim on them either.
		g.Expect(targetClient.Get(ctx, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationServiceUserSecretName,
		}, &corev1.Secret{})).To(Succeed(), "the service-user Secret is an input, not a child")
	})
}

// remoteChild is one projected object: where it lives, an empty instance of its
// kind to read it into, and what to call it in a failure message.
type remoteChild struct {
	key  client.ObjectKey
	obj  client.Object
	what string
}

// expectSwept asserts the sweep reached child on c: the object is gone. None of
// the kinds a Nova projects carries a protection finalizer, so an object still
// readable here is one the sweep missed.
func expectSwept(ig Gomega, ctx context.Context, c client.Client, child remoteChild) {
	err := c.Get(ctx, child.key, child.obj)
	ig.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"%s %s should be swept off the target cluster, got %v", child.what, child.key, err)
}

// novaRemoteChildren enumerates the objects a Nova projects that this walk drives
// it far enough to create, which is both the inventory the projection has to
// produce and the inventory the teardown sweep has to remove. An empty
// configMapName leaves the rendered config out, for a CR that rendered none.
func novaRemoteChildren(name, ns, configMapName string) []remoteChild {
	children := []remoteChild{
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &appsv1.Deployment{}, what: "API Deployment"},
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &corev1.Service{}, what: "API Service"},
		{
			key: client.ObjectKey{Namespace: ns, Name: name},
			obj: &policyv1.PodDisruptionBudget{}, what: "API PodDisruptionBudget",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentMetadata},
			obj: &appsv1.Deployment{}, what: "metadata Deployment",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentMetadata},
			obj: &corev1.Service{}, what: "metadata Service",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentScheduler},
			obj: &appsv1.Deployment{}, what: "scheduler Deployment",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentConductor},
			obj: &appsv1.Deployment{}, what: "conductor Deployment",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentConsoleProxy},
			obj: &appsv1.Deployment{}, what: "console proxy Deployment",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentConsoleProxy},
			obj: &corev1.Service{}, what: "console proxy Service",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + dbSyncJobSuffix},
			obj: &batchv1.Job{}, what: "db-sync Job",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: dbArchiveCronJobName(name)},
			obj: &batchv1.CronJob{}, what: "db-archive CronJob",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: database.ConnectionSecretName(name + "-api")},
			obj: &corev1.Secret{}, what: "derived nova_api DB-connection Secret",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: database.ConnectionSecretName(name)},
			obj: &corev1.Secret{}, what: "derived cell DB-connection Secret",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: messaging.TransportURLSecretName(name)},
			obj: &corev1.Secret{}, what: "derived transport-URL Secret",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentComputeConfig},
			obj: &corev1.Secret{}, what: "compute-contract Secret",
		},
	}
	for _, block := range novaDatabaseBlocks(name, ns) {
		children = append(children,
			remoteChild{key: block.key, obj: &mariadbv1alpha1.Database{}, what: "MariaDB Database"},
			remoteChild{key: block.key, obj: &mariadbv1alpha1.Grant{}, what: "MariaDB Grant"})
		if block.user {
			children = append(children,
				remoteChild{key: block.key, obj: &mariadbv1alpha1.User{}, what: "MariaDB User"})
		}
	}
	if configMapName != "" {
		children = append(children, remoteChild{
			key: client.ObjectKey{Namespace: ns, Name: configMapName},
			obj: &corev1.ConfigMap{}, what: "rendered config ConfigMap",
		})
	}
	return children
}

// multiclusterNovaPaths returns the Nova CRD and webhook manifest directories,
// resolved relative to this source file. The per-operator testutil package
// resolves them the same way for its own helpers, which do not expose the manager
// hook this test needs.
func multiclusterNovaPaths(t testing.TB) (crdDir, webhookDir string) {
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

// multiclusterExpectRemoteOwnership polls c until the object at key is claimed
// the way a remote child has to be: by the three ownership labels naming its
// owner, and by no owner reference at all. It polls rather than reads once
// because the projection is asynchronous.
func multiclusterExpectRemoteOwnership(
	t testing.TB,
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
	obj client.Object,
	what, ownerKind, ownerName, ownerNamespace string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	want := map[string]string{
		commonmulticluster.OwnerKindLabel:      ownerKind,
		commonmulticluster.OwnerNameLabel:      ownerName,
		commonmulticluster.OwnerNamespaceLabel: ownerNamespace,
	}

	g.Eventually(func() error {
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
		if refs := obj.GetOwnerReferences(); len(refs) != 0 {
			return fmt.Errorf("%s %s still carries owner references: %v", what, key, refs)
		}
		labels := obj.GetLabels()
		for label, value := range want {
			if labels[label] != value {
				return fmt.Errorf("%s %s label %s is %q, want %q", what, key, label, labels[label], value)
			}
		}
		return nil
	}, eventuallyTimeout, pollInterval).Should(Succeed(),
		"%s %s should be labelled as owned by %s %s/%s and carry no owner reference",
		what, key, ownerKind, ownerNamespace, ownerName)
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
