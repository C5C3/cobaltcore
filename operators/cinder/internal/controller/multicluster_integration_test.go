// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All multicluster envtest coverage of the three Cinder reconcilers lives in the
// one test function below, on purpose. The kubeconfig provider registers its
// registration-Secret watch under the fixed controller name
// "kubeconfig-provider" and exposes no SkipNameValidation escape, while
// controller-runtime validates controller names against a process-global set.
// A second provider anywhere in this test binary would therefore fail to
// register. One manager, one provider, one function: the scenarios are ordered
// subtests over the shared setup, and each one builds on the state the previous
// left behind.
//
// All three reconcilers are registered through the production watch wiring
// (setupWithOptions, the chain SetupWithManager applies) with
// SkipNameValidation set, exactly as the single-cluster suite in
// integration_test.go registers them. A skipped registration never claims the
// controller name, so the constraint that only one registration per test binary
// may run under the real name stays intact.

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
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/cinder/internal/testutil"
)

// TestIntegration_Multicluster_CinderTargetCluster runs all three reconcilers on
// a management cluster with a second envtest environment registered as target
// cluster, and walks the target-cluster lifecycle of the trio: registration, a
// Cinder that projects its whole fleet onto the target, a CinderBackend that
// observes that projection across the cluster boundary, a Cinder naming an
// unregistered cluster, and the teardown that sweeps off the target everything
// the CR put there.
func TestIntegration_Multicluster_CinderTargetCluster(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	const (
		// clustersNamespace mirrors the --clusters-namespace default the
		// operator binary passes to the provider.
		clustersNamespace = "c5c3-clusters"
		targetClusterName = "target-cinder"
		unknownCluster    = "does-not-exist"

		// The namespaces are fixed rather than generated so the same name can be
		// created on both clusters: a child lands in the CR's namespace,
		// whichever cluster it is written to.
		targetNamespace  = "mc-cinder"
		unknownNamespace = "mc-unknown"

		// survivorConfigMap is written into the target namespace by nobody in
		// particular, carrying none of the ownership labels. The teardown
		// subtest reads it back afterwards: a sweep that took it would be taking
		// somebody else's object.
		survivorConfigMap = "mc-unrelated"

		// engageTimeout bounds cluster engagement: the provider has to parse
		// the kubeconfig, build a cluster, and sync its cache before
		// GetCluster answers.
		engageTimeout = 60 * time.Second
	)

	targetRef := &commonv1.TargetClusterRefSpec{Name: targetClusterName}

	// --- Environment B: the target cluster.
	//
	// It carries the fake CRDs of the external operators whose objects the
	// children include (MariaDB and ESO above all) and deliberately NOT the
	// Cinder CRDs: a target cluster holds the workload, never the CR. That is
	// also what keeps the children alive here. Their owner references would
	// point at CRs this API server cannot resolve, and envtest runs no garbage
	// collector to act on that.
	targetScheme := commonenvtest.BuildScheme(commonenvtest.CommonExternalSchemes()...)
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, targetScheme, commonenvtest.CommonFakeCRDDirs())

	// --- Environment A: the management cluster, hosting the manager and the
	// three Cinder kinds it reconciles.
	mgmtScheme := commonenvtest.BuildScheme(append(commonenvtest.CommonExternalSchemes(),
		cinderv1alpha1.AddToScheme)...)

	provider := commonmulticluster.NewKubeconfigProvider(commonmulticluster.KubeconfigProviderOptions{
		Namespace: clustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme, which knows no CRD kind, and the first
		// MariaDB Database the schema step applies fails with "no kind is
		// registered".
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = mgmtScheme }},
	})

	crdDir, webhookDir := multiclusterCinderPaths(t)

	var mcMgr mcmanager.Manager
	mgmtClient, ctx, _ := commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "Cinder-multicluster",
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
			// multicluster Runnable), so the helper hosts and starts the local
			// one. That is the same thing: the multicluster manager's Start adds
			// a runnable provider to the local manager and then starts it, and
			// the kubeconfig provider is not a runnable one. Its Secret watch is
			// an ordinary controller on the local manager, registered by
			// SetupWithManager below.
			return m.GetLocalManager(), nil
		},
		RegisterWebhooks: registerCinderWebhooks,
		RegisterController: func(mgr ctrl.Manager) error {
			// The provider's engagement machinery has to be registered before
			// the controllers, exactly as internal/common/bootstrap does it,
			// so engagement precedes the first reconcile.
			if err := provider.SetupWithManager(context.Background(), mcMgr); err != nil {
				return err
			}
			// The multicluster manager is the Resolver: it turns
			// spec.targetClusterRef into the client the children are written
			// with.
			return registerCinderControllers(mgr, mcMgr, mcMgr)
		},
	})

	// The provider watches this namespace for registration Secrets.
	multiclusterEnsureNamespace(t, ctx, mgmtClient, clustersNamespace)

	cinderKey := types.NamespacedName{Name: integrationCinderName, Namespace: targetNamespace}
	backendKey := types.NamespacedName{Name: integrationBackendName, Namespace: targetNamespace}
	backupBackendKey := types.NamespacedName{Name: integrationBackupBackendName, Namespace: targetNamespace}

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

	t.Run("targeted Cinder projects its children onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The CRs live on the management cluster, everything they read and
		// everything they create lives on the target cluster.
		multiclusterEnsureNamespace(t, ctx, mgmtClient, targetNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, targetNamespace)
		createCinderPrerequisites(t, ctx, targetClient, targetNamespace)

		g.Expect(mgmtClient.Create(ctx, integrationCinderCR(integrationCinderName, targetNamespace, targetRef))).
			To(Succeed(), "create the placed Cinder CR")

		waitForCinderCondition(t, ctx, mgmtClient, cinderKey, "SecretsReady",
			metav1.ConditionTrue, eventuallyTimeout)

		// The schema is provisioned on the target too: a placed CR projects its
		// MariaDB CRs beside the workload that queries them.
		driveDatabase(t, ctx, targetClient, integrationCinderName, targetNamespace)
		waitForCinderCondition(t, ctx, mgmtClient, cinderKey, "DatabaseReady",
			metav1.ConditionTrue, eventuallyLongTimeout)

		// The API and the scheduler are there before any storage is attached.
		apiKey := client.ObjectKey{Namespace: targetNamespace, Name: integrationCinderName}
		api := &appsv1.Deployment{}
		eventuallyExists(t, ctx, targetClient, apiKey, api, "API Deployment", eventuallyLongTimeout)
		eventuallyExists(t, ctx, targetClient, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationCinderName + "-" + componentScheduler,
		}, &appsv1.Deployment{}, "scheduler Deployment", eventuallyLongTimeout)

		// Attaching storage adds the two remaining Deployments. The satellites
		// are CRs, so they live on the management cluster beside their parent
		// while the workloads they describe run on the target.
		g.Expect(mgmtClient.Create(ctx, integrationBackendCR(
			integrationBackendName, targetNamespace, integrationCinderName))).
			To(Succeed(), "create the CinderBackend CR")
		g.Expect(mgmtClient.Create(ctx, integrationBackupBackendCR(
			integrationBackupBackendName, targetNamespace, integrationCinderName))).
			To(Succeed(), "create the CinderBackupBackend CR")

		volume := &appsv1.Deployment{}
		eventuallyExists(t, ctx, targetClient, client.ObjectKey{
			Namespace: targetNamespace,
			Name:      integrationCinderName + "-" + componentVolumePrefix + integrationBackendName,
		}, volume, "volume Deployment", eventuallyLongTimeout)
		backup := &appsv1.Deployment{}
		eventuallyExists(t, ctx, targetClient, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationCinderName + "-" + componentBackup,
		}, backup, "backup Deployment", eventuallyLongTimeout)

		// envtest runs no Deployment controller on either cluster, so the four
		// rollouts are completed here. The API step polls until its Deployment
		// reports available, and the purge CronJob is projected behind it, so
		// without this the inventory below would be missing its tail.
		for _, key := range cinderWorkloadKeys(integrationCinderName, targetNamespace, integrationBackendName) {
			markDeploymentReady(t, ctx, targetClient, key)
		}
		waitForCinderCondition(t, ctx, mgmtClient, cinderKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		g.Expect(targetClient.Get(ctx, apiKey, api)).To(Succeed())
		configMapName := mountedConfigMapName(&api.Spec.Template.Spec)
		g.Expect(configMapName).To(HavePrefix(integrationCinderName+"-config-"),
			"the config volume must mount the content-hashed ConfigMap")

		// Every kind the fleet projects, claimed by the ownership labels and by
		// nothing else. A reference would name a UID this cluster cannot
		// resolve, and the labels are the only handle the teardown sweep has.
		children := cinderRemoteChildren(integrationCinderName, targetNamespace, configMapName,
			mountedSecretName(&volume.Spec.Template.Spec, backendsVolumeName),
			mountedSecretName(&backup.Spec.Template.Spec, backupVolumeName))
		for _, child := range children {
			multiclusterExpectRemoteOwnership(t, ctx, targetClient, child.key, child.obj, child.what,
				"Cinder", integrationCinderName, targetNamespace)
			// None of it on the management cluster, where only the CRs live.
			multiclusterExpectAbsent(t, ctx, mgmtClient, child.key, child.obj, child.what)
		}

		// Status and the finalizers stay with the CR.
		after := &cinderv1alpha1.Cinder{}
		g.Expect(mgmtClient.Get(ctx, cinderKey, after)).To(Succeed())
		g.Expect(after.Status.Conditions).NotTo(BeEmpty(), "status should be populated on the management cluster")
		g.Expect(controllerutil.ContainsFinalizer(after, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"the remote-children finalizer should be on a CR whose children live on another cluster")
	})

	t.Run("targeted CinderBackend observes the projection through the parent's cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The satellites carry no target of their own: they exist to serve one
		// Cinder, so the Deployment they read belongs wherever that Cinder's
		// workload runs. Reaching ConfigProjected proves they resolved the
		// parent's cluster rather than looking on their own.
		waitForBackendCondition(t, ctx, mgmtClient, backendKey, conditionTypeConfigProjected,
			metav1.ConditionTrue, eventuallyLongTimeout)
		waitForBackendCondition(t, ctx, mgmtClient, backendKey, "Ready",
			metav1.ConditionTrue, eventuallyLongTimeout)
		waitForBackupBackendCondition(t, ctx, mgmtClient, backupBackendKey, conditionTypeConfigProjected,
			metav1.ConditionTrue, eventuallyLongTimeout)

		// The parent aggregates the same fact from the other side.
		waitForCinderCondition(t, ctx, mgmtClient, cinderKey, conditionTypeBackendsReady,
			metav1.ConditionTrue, eventuallyLongTimeout)
		placed := &cinderv1alpha1.Cinder{}
		g.Expect(mgmtClient.Get(ctx, cinderKey, placed)).To(Succeed())
		g.Expect(placed.Status.VolumeServices).To(Equal([]cinderv1alpha1.VolumeServiceStatus{{
			Backend: integrationBackendName,
			Host:    integrationCinderName + "@" + integrationBackendName,
		}}), "the host identity is reported on the CR, wherever its volume service runs")
	})

	t.Run("a Cinder naming an unregistered cluster creates nothing and carries no finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)

		multiclusterEnsureNamespace(t, ctx, mgmtClient, unknownNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, unknownNamespace)

		unknownRef := &commonv1.TargetClusterRefSpec{Name: unknownCluster}
		g.Expect(mgmtClient.Create(ctx, integrationCinderCR(integrationCinderName, unknownNamespace, unknownRef))).
			To(Succeed())

		// The unresolvable cluster is reported on the pipeline's first gate,
		// which is the condition the rest of the graph waits behind.
		unresolved := types.NamespacedName{Name: integrationCinderName, Namespace: unknownNamespace}
		cond := waitForCinderCondition(t, ctx, mgmtClient, unresolved, "SecretsReady",
			metav1.ConditionFalse, eventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))

		// Nothing was created, so nothing has to be cleaned up: the CR may not
		// carry a finalizer that would only block its deletion.
		got := &cinderv1alpha1.Cinder{}
		g.Expect(mgmtClient.Get(ctx, unresolved, got)).To(Succeed())
		g.Expect(got.Finalizers).To(BeEmpty(), "an unresolvable Cinder should carry no finalizer")

		// The config ConfigMap and the two projection Secrets of a CR that
		// rendered none have no name, so the inventory is asked for the fixed
		// children alone.
		for _, c := range []client.Client{mgmtClient, targetClient} {
			for _, child := range cinderRemoteChildren(integrationCinderName, unknownNamespace, "", "", "") {
				multiclusterExpectAbsent(t, ctx, c, child.key, child.obj, child.what)
			}
		}
	})

	t.Run("deleting the Cinder sweeps its children off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// What the sweep must leave standing. It carries none of the ownership
		// labels, so a sweep that took it would be taking somebody else's object.
		g.Expect(targetClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: survivorConfigMap, Namespace: targetNamespace},
			Data:       map[string]string{"owner": "nobody"},
		})).To(Succeed(), "seed an unlabelled ConfigMap beside the children")

		api := &appsv1.Deployment{}
		g.Expect(targetClient.Get(ctx, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationCinderName,
		}, api)).To(Succeed())
		volume := &appsv1.Deployment{}
		g.Expect(targetClient.Get(ctx, client.ObjectKey{
			Namespace: targetNamespace,
			Name:      integrationCinderName + "-" + componentVolumePrefix + integrationBackendName,
		}, volume)).To(Succeed())
		backup := &appsv1.Deployment{}
		g.Expect(targetClient.Get(ctx, client.ObjectKey{
			Namespace: targetNamespace, Name: integrationCinderName + "-" + componentBackup,
		}, backup)).To(Succeed())

		projected := cinderRemoteChildren(integrationCinderName, targetNamespace,
			mountedConfigMapName(&api.Spec.Template.Spec),
			mountedSecretName(&volume.Spec.Template.Spec, backendsVolumeName),
			mountedSecretName(&backup.Spec.Template.Spec, backupVolumeName))
		for _, child := range projected {
			g.Expect(targetClient.Get(ctx, child.key, child.obj)).To(Succeed(),
				"%s %s must be on the cluster before the deletion, or its absence afterwards proves nothing",
				child.what, child.key)
		}

		cinder := &cinderv1alpha1.Cinder{}
		g.Expect(mgmtClient.Get(ctx, cinderKey, cinder)).To(Succeed())
		g.Expect(mgmtClient.Delete(ctx, cinder)).To(Succeed(), "delete the placed Cinder")

		// Two finalizers come off in order: the named MariaDB cleanup holds the
		// CR for one more pass so the schema teardown is triggered, and the
		// label-selected sweep runs behind it.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, cinderKey, &cinderv1alpha1.Cinder{}))
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
		g.Expect(targetClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: integrationDBSecretName},
			&corev1.Secret{})).To(Succeed(), "the database credentials Secret is an input, not a child")
		g.Expect(targetClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: integrationBusSecretName},
			&corev1.Secret{})).To(Succeed(), "the brownfield transport-URL Secret is an input, not a child")
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
// the kinds a Cinder projects carries a protection finalizer, so an object still
// readable here is one the sweep missed.
func expectSwept(ig Gomega, ctx context.Context, c client.Client, child remoteChild) {
	err := c.Get(ctx, child.key, child.obj)
	ig.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"%s %s should be swept off the target cluster, got %v", child.what, child.key, err)
}

// cinderRemoteChildren enumerates the objects a Cinder projects that this walk
// drives it far enough to create, which is both the inventory the projection has
// to produce and the inventory the teardown sweep has to remove. The three
// content-hashed names are read off the live workloads by the caller; empty ones
// leave their entry out, for a CR that rendered none.
func cinderRemoteChildren(name, ns, configMapName, backendSecretName, backupSecretName string) []remoteChild {
	children := []remoteChild{
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &appsv1.Deployment{}, what: "API Deployment"},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentScheduler},
			obj: &appsv1.Deployment{}, what: "scheduler Deployment",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentVolumePrefix + integrationBackendName},
			obj: &appsv1.Deployment{}, what: "volume Deployment",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: name + "-" + componentBackup},
			obj: &appsv1.Deployment{}, what: "backup Deployment",
		},
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &corev1.Service{}, what: "API Service"},
		{
			key: client.ObjectKey{Namespace: ns, Name: name},
			obj: &policyv1.PodDisruptionBudget{}, what: "API PodDisruptionBudget",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: dbPurgeCronJobName(name)},
			obj: &batchv1.CronJob{}, what: "db-purge CronJob",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: database.ConnectionSecretName(name)},
			obj: &corev1.Secret{}, what: "derived DB-connection Secret",
		},
		{
			key: client.ObjectKey{Namespace: ns, Name: messaging.TransportURLSecretName(name)},
			obj: &corev1.Secret{}, what: "derived transport-URL Secret",
		},
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &mariadbv1alpha1.Database{}, what: "MariaDB Database"},
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &mariadbv1alpha1.User{}, what: "MariaDB User"},
		{key: client.ObjectKey{Namespace: ns, Name: name}, obj: &mariadbv1alpha1.Grant{}, what: "MariaDB Grant"},
	}
	for _, hashed := range []struct {
		name string
		obj  client.Object
		what string
	}{
		{configMapName, &corev1.ConfigMap{}, "rendered config ConfigMap"},
		{backendSecretName, &corev1.Secret{}, "backend projection Secret"},
		{backupSecretName, &corev1.Secret{}, "backup projection Secret"},
	} {
		if hashed.name == "" {
			continue
		}
		children = append(children, remoteChild{
			key: client.ObjectKey{Namespace: ns, Name: hashed.name}, obj: hashed.obj, what: hashed.what,
		})
	}
	return children
}

// multiclusterCinderPaths returns the Cinder CRD and webhook manifest
// directories, resolved relative to this source file. The per-operator testutil
// package resolves them the same way for its own helpers, which do not expose
// the manager hook this test needs.
func multiclusterCinderPaths(t testing.TB) (crdDir, webhookDir string) {
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
