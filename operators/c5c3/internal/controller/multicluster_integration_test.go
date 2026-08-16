// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All multicluster envtest coverage of the ControlPlane reconciler lives in the
// one test function below, on purpose. The kubeconfig provider registers its
// registration-Secret watch under the fixed controller name
// "kubeconfig-provider" and exposes no SkipNameValidation escape, while
// controller-runtime validates controller names against a process-global set. A
// second provider anywhere in this test binary would therefore fail to register.
// One manager, one provider, one function: the scenarios are ordered subtests
// over the shared setup, and each one builds on the state the previous left
// behind.
//
// The reconciler itself is registered through the production watch wiring
// (setupWithOptions, the chain SetupWithManager applies) with SkipNameValidation
// set, so it does not claim the real controller name either. That name belongs
// to TestSetupWithManager_BothControllersStart, which is the one test in this
// binary allowed to call the real SetupWithManager methods (see the header of
// setupwithmanager_integration_test.go).

package controller

import (
	"context"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/bootstrap"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/forge/operators/c5c3/internal/testutil"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// TestIntegration_Multicluster_ControlPlanePlacement runs the ControlPlane
// reconciler on a management cluster with a second envtest environment
// registered as target cluster, and walks the per-service placement split:
// registration, a ControlPlane that places its Keystone service on the target,
// the admin password it has to read back off that cluster, a ControlPlane naming
// an unregistered cluster, and the deletion that sweeps the placed namespace off
// the target again.
func TestIntegration_Multicluster_ControlPlanePlacement(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	const (
		// mcClustersNamespace mirrors the --clusters-namespace default the
		// operator binary passes to the provider.
		mcClustersNamespace = "c5c3-clusters"
		mcTargetCluster     = "target-b"

		// The two ControlPlanes and their namespaces. Namespaces are fixed rather
		// than generated so the same name can be looked up on both clusters: a
		// placed namespace exists on the management cluster too, and the split
		// assertions read one key through both clients.
		mcNamespace         = "mc-cp"
		mcKeystoneNamespace = "mc-cp-identity"
		mcControlPlane      = "cp"

		mcUnknownNamespace  = "mc-unknown"
		mcUnknownKeystoneNS = "mc-unknown-identity"
		mcUnknownCluster    = "does-not-exist"

		// The cleartext admin password reconcileKORC reads across the cluster
		// boundary, and the decoy of the same shape planted on the management
		// cluster to prove it is not the one being read.
		mcAdminPassword = "super-secret-admin-password"
		mcDecoyPassword = "decoy-on-the-management-cluster"

		// mcEngageTimeout bounds cluster engagement: the provider has to parse the
		// kubeconfig, build a cluster, and sync its cache before GetCluster
		// answers.
		mcEngageTimeout = 60 * time.Second
	)

	// One scheme for both environments, the manager, and the provider's
	// per-cluster clients. They all write the same kinds, and a client built on a
	// scheme that does not know a CRD kind fails its first write with "no kind is
	// registered" — which is what the ClusterOptions override below prevents on
	// the clusters the provider builds.
	mcScheme := testutil.BuildControllerScheme(c5c3v1alpha1.AddToScheme)

	// --- Environment B: the target cluster.
	//
	// It carries the shared fake CRDs of the external operators whose objects the
	// ControlPlane places (MariaDB, Memcached, ESO, cert-manager, openbao) and
	// deliberately NEITHER the c5c3 CRDs NOR the sibling service-operator ones: a
	// target cluster holds the workload, never the CRs. The K-ORC kinds ARE
	// served here, because they ship in those same shared dirs, and that is what
	// makes the "the K-ORC ensemble is not on B" assertions below a real absence
	// rather than a kind the cluster could never have answered for.
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, mcScheme, commonenvtest.CommonFakeCRDDirs())

	// --- Environment A: the management cluster, hosting the manager.
	provider := commonmulticluster.NewKubeconfigProvider(commonmulticluster.KubeconfigProviderOptions{
		Namespace: mcClustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme, which knows no CRD kind, and the first
		// MariaDB or ESO write fails with "no kind is registered".
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = mcScheme }},
	})

	var mcMgr mcmanager.Manager
	mgmtClient, ctx, _ := commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "c5c3-multicluster",
		Scheme:            mcScheme,
		CRDDirectoryPaths: testutil.CRDDirectoryPaths(),
		WebhookDir:        testutil.C5c3WebhookDir(),
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
		RegisterWebhooks: func(mgr ctrl.Manager) error {
			// mgr.GetAPIReader() mirrors main.go: admission lookups read the API
			// server directly, never a stale cache.
			return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		RegisterController: func(mgr ctrl.Manager) error {
			// The provider's engagement machinery has to be registered before the
			// controllers, exactly as internal/common/bootstrap does it, so
			// engagement precedes the first reconcile.
			if err := provider.SetupWithManager(context.Background(), mcMgr); err != nil {
				return err
			}

			r := &ControlPlaneReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
				// The Resolver is the whole point of this test: it turns a
				// service's targetClusterRef into the client that service's
				// children are written with.
				Resolver: mcMgr,
			}
			// The production watch wiring, shared with SetupWithManager: the legs
			// pinned to the management cluster, and the same kinds again on the
			// clusters a service can be placed on, keyed on the ownership labels.
			// A child written on the target therefore produces a watch event here,
			// and the field index and the discovery guard come with the same call.
			// SkipNameValidation keeps the one-real-controller-name-per-test-binary
			// constraint at the top of this file intact.
			opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
			opts.SkipNameValidation = ptr.To(true)
			return r.setupWithOptions(mcMgr, opts)
		},
	})

	// The provider watches this namespace for registration Secrets.
	mcEnsureNamespace(t, ctx, mgmtClient, mcClustersNamespace)

	cp := integrationManagedControlPlane(mcControlPlane, mcNamespace)
	cpKey := types.NamespacedName{Name: mcControlPlane, Namespace: mcNamespace}

	// Every object the placed Keystone service takes with it, keyed in the
	// service namespace. Each one is looked up through BOTH clients below: on the
	// target it must exist and carry the claim, on the management cluster it must
	// not exist at all. The names are derived from the CR rather than written out,
	// so a renamed child fails the lookup instead of silently changing what the
	// split is asserted over.
	placedKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: keystoneName(cp)}
	mariadbKey := client.ObjectKey{
		Namespace: mcKeystoneNamespace,
		Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
	}
	memcachedKey := client.ObjectKey{
		Namespace: mcKeystoneNamespace,
		Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
	}
	tenantStoreKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: esoTenantStoreName}
	tenantSAKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: esoTenantServiceAccountName}
	tenantCertKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: esoTenantClientCertName}
	dbCredSAKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: dbCredentialServiceAccountName}
	dbCredKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: dbCredentialSecretName(cp)}
	dbCredCertKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: dbCredentialClientCertName(cp)}
	adminPasswordKey := client.ObjectKey{Namespace: mcKeystoneNamespace, Name: adminPasswordSecretName(cp)}

	t.Run("register the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		kubeconfig, err := commonenvtest.KubeconfigBytes(targetCfg, mcTargetCluster)
		g.Expect(err).NotTo(HaveOccurred(), "build kubeconfig for the target environment")

		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mcTargetCluster,
				Namespace: mcClustersNamespace,
				Labels:    map[string]string{"sigs.k8s.io/multicluster-runtime-kubeconfig": "true"},
			},
			Data: map[string][]byte{"kubeconfig": kubeconfig},
		})).To(Succeed(), "create the registration Secret")

		g.Eventually(func() error {
			_, err := mcMgr.GetCluster(ctx, mcruntime.ClusterName(mcTargetCluster))
			return err
		}, mcEngageTimeout, itPollInterval).Should(Succeed(),
			"the provider should engage the target cluster from its registration Secret")
	})

	t.Run("placing keystone moves its ensemble onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		mcEnsureNamespace(t, ctx, mgmtClient, mcNamespace)
		ensureReadyClusterSecretStore(t, ctx, mgmtClient)
		// The ControlPlane's own namespace stays on the management cluster, so its
		// tenant store is seeded here. The one in the placed namespace cannot be
		// seeded yet: that namespace does not exist on either cluster until the
		// reconciler creates it.
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcNamespace)

		cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
			Name:      mcKeystoneNamespace,
			Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		}
		// A placed catalog service must advertise an externally routable address:
		// the webhook rejects one whose catalog row would carry only an in-cluster
		// Service DNS name nothing outside the target cluster can resolve.
		cp.Spec.Services.Keystone.PublicEndpoint = "https://keystone.example.com/v3"
		cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: mcTargetCluster}
		g.Expect(mgmtClient.Create(ctx, cp)).To(Succeed(), "create the placing ControlPlane")

		// --- The namespace itself is ensured on BOTH clusters, and that is not a
		// leak: the Keystone CR is created and reconciled on the management
		// cluster, in the namespace its service is assigned to, while the
		// keystone-operator then projects the workload into the namespace of the
		// same name on the target. Both sides have to exist, so both are checked —
		// and the target-side one carries the claim.
		targetNS := &corev1.Namespace{}
		mcExpectRemoteClaim(t, ctx, targetClient, client.ObjectKey{Name: mcKeystoneNamespace},
			targetNS, "service namespace", cp)
		g.Expect(targetNS.Labels).To(HaveKeyWithValue(managedByLabel, managedByValue),
			"the operator records that it owns the namespace it created")
		mcEventuallyExists(t, ctx, mgmtClient, client.ObjectKey{Name: mcKeystoneNamespace},
			&corev1.Namespace{}, "service namespace")

		// --- The backing services land on the target and nowhere else. They are
		// simulated Ready there too: the pipeline short-circuits at Infrastructure
		// while either is converging, so nothing below runs until they report.
		mcEventuallyExists(t, ctx, targetClient, mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB")
		mcEventuallyExists(t, ctx, targetClient, memcachedKey, mcMemcached(), "Memcached")
		mcExpectAbsent(t, ctx, mgmtClient, memcachedKey, mcMemcached(), "Memcached")

		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, mariadbKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, memcachedKey)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeInfrastructureReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The ESO tenant trio: the store has to authenticate from the cluster
		// whose ESO materialises the Secrets, and its client certificate has to be
		// issued by the cert-manager there.
		mcEventuallyExists(t, ctx, targetClient, tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount")
		mcEventuallyExists(t, ctx, targetClient, tenantCertKey, mcCertificate(), "tenant Certificate")
		mcEventuallyExists(t, ctx, targetClient, tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore")
		for _, absent := range []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount"},
			{tenantCertKey, mcCertificate(), "tenant Certificate"},
			{tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore"},
		} {
			mcExpectAbsent(t, ctx, mgmtClient, absent.key, absent.obj, absent.what)
		}
		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcKeystoneNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The DB-credential quartet follows the database it issues against.
		mcEventuallyExists(t, ctx, targetClient, dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount")
		mcEventuallyExists(t, ctx, targetClient, dbCredCertKey, mcCertificate(), "DB-credential Certificate")
		mcEventuallyExists(t, ctx, targetClient, dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator")
		mcEventuallyExists(t, ctx, targetClient, dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret")
		for _, absent := range []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount"},
			{dbCredCertKey, mcCertificate(), "DB-credential Certificate"},
			{dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator"},
			{dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret"},
		} {
			mcExpectAbsent(t, ctx, mgmtClient, absent.key, absent.obj, absent.what)
		}
		g.Expect(simulators.SimulateExternalSecretSync(ctx, targetClient, dbCredKey)).
			To(Succeed(), "simulate the DB-credential ExternalSecret sync on the target cluster")
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeDBCredentialsReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The admin-password ExternalSecret is materialised beside the Keystone
		// child, so it rides the target cluster's ESO. The helper resolves the
		// namespace from the CR and asserts the label claim on a placed one.
		simulateAdminPasswordExternalSecretSyncWhenPresent(t, ctx, targetClient, cp)
		mcExpectAbsent(t, ctx, mgmtClient, adminPasswordKey, &esov1.ExternalSecret{}, "admin-password ExternalSecret")
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeAdminPasswordReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- Every remote object is claimed by the five labels and by nothing
		// else. An owner reference would name a UID the target cluster cannot
		// resolve, and the labels are the only handle the teardown sweep has.
		for _, claimed := range []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB"},
			{memcachedKey, mcMemcached(), "Memcached"},
			{tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount"},
			{tenantCertKey, mcCertificate(), "tenant Certificate"},
			{tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore"},
			{dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount"},
			{dbCredCertKey, mcCertificate(), "DB-credential Certificate"},
			{dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator"},
			{dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret"},
			{adminPasswordKey, &esov1.ExternalSecret{}, "admin-password ExternalSecret"},
		} {
			mcExpectRemoteClaim(t, ctx, targetClient, claimed.key, claimed.obj, claimed.what, cp)
		}

		// --- The Keystone CR stays on the management cluster, carrying the ref its
		// own operator projects by. The target cluster does not even serve the
		// Keystone kind, so it could not be there.
		keystone := &keystonev1alpha1.Keystone{}
		mcEventuallyExists(t, ctx, mgmtClient, placedKey, keystone, "Keystone child")
		g.Expect(keystone.Spec.TargetClusterRef).NotTo(BeNil(),
			"the Keystone child must carry the ref the ControlPlane placed its service with")
		g.Expect(keystone.Spec.TargetClusterRef.Name).To(Equal(mcTargetCluster))
		g.Expect(keystone.OwnerReferences).To(BeEmpty(),
			"the API server rejects a cross-namespace controller owner reference")
		mcExpectAbsent(t, ctx, targetClient, placedKey, &keystonev1alpha1.Keystone{}, "Keystone child")

		// --- Placing children on a target cluster is what installs the
		// remote-children finalizer: nothing else can sweep them.
		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(live, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"a ControlPlane whose children live on another cluster must carry the remote-children finalizer")
		g.Expect(controllerutil.ContainsFinalizer(live, controlPlaneORCFinalizer)).To(BeTrue(),
			"the ORC-teardown finalizer must be installed as well")
	})

	t.Run("the K-ORC ensemble stays home and reads the admin password off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The blocking prefix ends at Keystone, and the tail group (KORC among it)
		// only runs once the child reports Ready.
		simulateKeystoneReadyWhenPresent(t, ctx, mgmtClient, placedKey)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeKeystoneReady, metav1.ConditionTrue, itEventuallyTimeout)

		// The admin password is the ONE thing a reconcile pass reads off another
		// cluster. No ESO runs on either environment, so the ExternalSecret synced
		// above materialised nothing and reconcileKORC has nothing to read yet.
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeKORCReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal("WaitingForAdminPassword"),
			"the mint must defer while the admin password Secret does not exist on the target cluster")

		// A decoy of exactly the right name and namespace, on the WRONG cluster. It
		// is what makes the seed below discriminating: a reconcileKORC that read
		// through the local client instead of the resolved one would find this and
		// advance, and every assertion after it would hold for the wrong reason.
		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminPasswordKey.Name, Namespace: adminPasswordKey.Namespace},
			Data:       map[string][]byte{"password": []byte(mcDecoyPassword)},
		})).To(Succeed(), "plant the decoy admin password on the management cluster")

		g.Consistently(func() string {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return ""
			}
			c := meta.FindStatusCondition(live.Status.Conditions, conditionTypeKORCReady)
			if c == nil {
				return ""
			}
			return c.Reason
		}, korcRequeueAfter+5*time.Second, itPollInterval).Should(Equal("WaitingForAdminPassword"),
			"the decoy on the management cluster must not satisfy a read that belongs to the target cluster")

		// The real one, where the placed ExternalSecret would have materialised it.
		g.Expect(targetClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: adminPasswordKey.Name, Namespace: adminPasswordKey.Namespace},
			Data:       map[string][]byte{"password": []byte(mcAdminPassword)},
		})).To(Succeed(), "seed the materialised admin password on the target cluster")

		// --- Everything the mint writes stays on the management cluster: K-ORC
		// runs there, whatever cluster the workload was placed on.
		acKey := client.ObjectKey{Namespace: mcNamespace, Name: adminAppCredentialName(cp)}
		mcEventuallyExists(t, ctx, mgmtClient, acKey, &orcv1alpha1.ApplicationCredential{}, "admin ApplicationCredential")
		mcExpectAbsent(t, ctx, targetClient, acKey, &orcv1alpha1.ApplicationCredential{}, "admin ApplicationCredential")

		simulateApplicationCredentialAvailableWhenPresent(t, ctx, mgmtClient, acKey)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeKORCReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- On to the catalog, which is where the K-ORC Service/Endpoint pair the
		// ControlPlane advertises the placed Keystone with materialises. Every step
		// of it runs against the management cluster.
		cloudsYamlKey := client.ObjectKey{Namespace: mcNamespace, Name: korcCloudsYamlSecretName}
		mcEventuallyExists(t, ctx, mgmtClient, cloudsYamlKey, &esov1.ExternalSecret{}, "clouds.yaml ExternalSecret")
		g.Expect(simulators.SimulateExternalSecretSync(ctx, mgmtClient, cloudsYamlKey)).
			To(Succeed(), "simulate the k-orc clouds.yaml ExternalSecret sync")
		simulatePushSecretSyncedWhenPresent(t, ctx, mgmtClient,
			client.ObjectKey{Namespace: mcNamespace, Name: adminAppCredentialPushSecretName(cp)})
		simulateCloudsYamlMaterializedWhenPresent(t, ctx, mgmtClient, cp)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeAdminCredentialReady, metav1.ConditionTrue, itEventuallyTimeout)

		simulateCatalogServiceEndpointAvailableWhenPresent(t, ctx, mgmtClient, cp)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeCatalogReady, metav1.ConditionTrue, itEventuallyTimeout)

		catalogServiceKey := client.ObjectKey{Namespace: mcNamespace, Name: keystoneServiceName(cp)}
		catalogEndpointKey := client.ObjectKey{Namespace: mcNamespace, Name: keystoneEndpointName(cp)}
		g.Expect(mgmtClient.Get(ctx, catalogServiceKey, &orcv1alpha1.Service{})).To(Succeed(),
			"the catalog Service must be registered on the management cluster")
		g.Expect(mgmtClient.Get(ctx, catalogEndpointKey, &orcv1alpha1.Endpoint{})).To(Succeed(),
			"the catalog Endpoint must be registered on the management cluster")
		mcExpectAbsent(t, ctx, targetClient, catalogServiceKey, &orcv1alpha1.Service{}, "catalog Service")
		mcExpectAbsent(t, ctx, targetClient, catalogEndpointKey, &orcv1alpha1.Endpoint{}, "catalog Endpoint")
	})

	t.Run("a ControlPlane naming an unregistered cluster creates nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)

		mcEnsureNamespace(t, ctx, mgmtClient, mcUnknownNamespace)

		unknown := integrationManagedControlPlane("unknown-cp", mcUnknownNamespace)
		unknown.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
			Name:      mcUnknownKeystoneNS,
			Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
		}
		unknown.Spec.Services.Keystone.PublicEndpoint = "https://keystone.elsewhere.example.com/v3"
		unknown.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: mcUnknownCluster}
		g.Expect(mgmtClient.Create(ctx, unknown)).To(Succeed(),
			"a ref naming an unregistered cluster is admitted: registration is a runtime fact, not a schema one")

		unknownKey := types.NamespacedName{Name: unknown.Name, Namespace: mcUnknownNamespace}
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, unknownKey,
			conditionTypeNamespacesReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(ContainSubstring("cluster not found"),
			"the resolver's message should reach the condition verbatim")

		// The client is resolved BEFORE anything is written, so the namespace is
		// created on neither cluster — not even on the management one, where it
		// would have been ensured for a resolvable ref.
		for _, c := range []client.Client{mgmtClient, targetClient} {
			mcExpectAbsent(t, ctx, c, client.ObjectKey{Name: mcUnknownKeystoneNS},
				&corev1.Namespace{}, "service namespace")
		}

		// Nothing was placed, so there is nothing for the remote-children finalizer
		// to hold the CR open for.
		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(mgmtClient.Get(ctx, unknownKey, live)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(live, commonmulticluster.RemoteChildrenFinalizer)).To(BeFalse(),
			"the remote-children finalizer must not go on a CR whose target cluster never resolved")
	})

	t.Run("deleting the ControlPlane sweeps the placed namespace off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		live := &c5c3v1alpha1.ControlPlane{}
		g.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
		g.Expect(mgmtClient.Delete(ctx, live)).To(Succeed(), "delete the placing ControlPlane")

		// The Keystone child is deleted on the management cluster, where it lives,
		// and the sweep waits for it: its own operator's ESO cleanup authenticates
		// through the tenant store the sweep is about to remove.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, placedKey, &keystonev1alpha1.Keystone{}))
		}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
			"the cross-namespace Keystone child must be deleted explicitly — no GC cascade reaches it")

		// Everything the ControlPlane placed is swept off the target cluster. The
		// sweep does not wait for what it deleted, and a delete is asynchronous, so
		// each object is polled until it is gone rather than read once.
		swept := []struct {
			key  client.ObjectKey
			obj  client.Object
			what string
		}{
			{mariadbKey, &mariadbv1alpha1.MariaDB{}, "MariaDB"},
			{memcachedKey, mcMemcached(), "Memcached"},
			{tenantSAKey, &corev1.ServiceAccount{}, "tenant ServiceAccount"},
			{tenantCertKey, mcCertificate(), "tenant Certificate"},
			{tenantStoreKey, &esov1.SecretStore{}, "tenant SecretStore"},
			{dbCredSAKey, &corev1.ServiceAccount{}, "DB-credential ServiceAccount"},
			{dbCredCertKey, mcCertificate(), "DB-credential Certificate"},
			{dbCredKey, &esgenv1alpha1.VaultDynamicSecret{}, "DB-credential generator"},
			{dbCredKey, &esov1.ExternalSecret{}, "DB-credential ExternalSecret"},
			{adminPasswordKey, &esov1.ExternalSecret{}, "admin-password ExternalSecret"},
		}
		g.Eventually(func(ig Gomega) {
			for _, child := range swept {
				ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, child.key, child.obj))).
					To(BeTrue(), "%s %s should be swept off the target cluster", child.what, child.key)
			}
			pushSecrets := &esov1alpha1.PushSecretList{}
			ig.Expect(targetClient.List(ctx, pushSecrets, client.InNamespace(mcKeystoneNamespace))).To(Succeed())
			ig.Expect(pushSecrets.Items).To(BeEmpty(), "no PushSecret should survive the sweep")
		}, itEventuallyTimeout, itPollInterval).Should(Succeed())

		// envtest runs no namespace controller, so a deleted namespace stays
		// Terminating forever. The DeletionTimestamp is what the operator is
		// responsible for, on both clusters — it created the namespace on both.
		for name, c := range map[string]client.Client{"target": targetClient, "management": mgmtClient} {
			g.Eventually(func() bool {
				ns := &corev1.Namespace{}
				if err := c.Get(ctx, client.ObjectKey{Name: mcKeystoneNamespace}, ns); err != nil {
					return apierrors.IsNotFound(err)
				}
				return !ns.DeletionTimestamp.IsZero()
			}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
				"the Managed service namespace must be deleted on the %s cluster", name)
		}

		// Both finalizers released, so the CR leaves etcd.
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, cpKey, &c5c3v1alpha1.ControlPlane{}))
		}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
			"the ControlPlane should leave etcd once the ORC and remote-children finalizers are released")

		// What the ControlPlane only ever READ stays. The sweep selects on the
		// ownership labels, and nothing stamped them on the admin-password Secret
		// this test seeded, so a sweep that took it would be taking somebody else's
		// object.
		g.Expect(targetClient.Get(ctx, adminPasswordKey, &corev1.Secret{})).To(Succeed(),
			"the seeded admin-password Secret is an input, not a child, and must survive the sweep")
	})
}

// mcMemcached returns an empty Memcached carrier: the memcached.c5c3.io CRD
// ships no Go module, so the reconciler creates it — and this test reads it —
// as an *unstructured.Unstructured carrying memcachedGVK. A fresh object per
// call, because every Get populates the one it is handed.
func mcMemcached() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(memcachedGVK)
	return u
}

// mcCertificate returns an empty cert-manager Certificate carrier, for the same
// reason as mcMemcached: no Go type ships for it, so it is handled unstructured.
func mcCertificate() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(certificateGVK)
	return u
}

// mcEnsureNamespace creates the namespace on c, tolerating one that already
// exists so a subtest can seed the same name on both clusters.
func mcEnsureNamespace(t testing.TB, ctx context.Context, c client.Client, name string) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if apierrors.IsAlreadyExists(err) {
		return
	}
	g.Expect(err).NotTo(HaveOccurred(), "create namespace %s", name)
}

// mcEventuallyExists polls c until the object at key exists.
func mcEventuallyExists(
	t testing.TB, ctx context.Context, c client.Client,
	key client.ObjectKey, obj client.Object, what string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Eventually(func() error {
		return c.Get(ctx, key, obj)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "%s %s should exist", what, key)
}

// mcExpectAbsent asserts the object at key does not exist on c. A missing
// namespace answers NotFound too, and a kind the cluster does not serve at all
// answers no-match — both mean the object is not there, which is what the
// management-side half of every split assertion is claiming.
func mcExpectAbsent(
	t testing.TB, ctx context.Context, c client.Client,
	key client.ObjectKey, obj client.Object, what string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	err := c.Get(ctx, key, obj)
	g.Expect(apierrors.IsNotFound(err) || meta.IsNoMatchError(err)).To(BeTrue(),
		"%s %s should not exist on this cluster, got %v", what, key, err)
}

// mcExpectRemoteClaim polls c until the object at key is claimed the way a
// remote child has to be: by the five ownership labels — the owner triple the
// shared teardown selects on, plus the cross-namespace pair this operator's
// watch legs map a child back to its ControlPlane by — and by no owner
// reference at all. It polls rather than reads once because the projection is
// asynchronous.
func mcExpectRemoteClaim(
	t testing.TB, ctx context.Context, c client.Client,
	key client.ObjectKey, obj client.Object, what string, cp *c5c3v1alpha1.ControlPlane,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	want := map[string]string{
		commonmulticluster.OwnerKindLabel:      "ControlPlane",
		commonmulticluster.OwnerNameLabel:      cp.Name,
		commonmulticluster.OwnerNamespaceLabel: cp.Namespace,
		controlPlaneNameLabel:                  cp.Name,
		controlPlaneNamespaceLabel:             cp.Namespace,
	}

	g.Eventually(func(ig Gomega) {
		ig.Expect(c.Get(ctx, key, obj)).To(Succeed())
		ig.Expect(obj.GetOwnerReferences()).To(BeEmpty(),
			"%s %s must carry no owner reference: it would name a UID this cluster cannot resolve", what, key)
		for label, value := range want {
			ig.Expect(obj.GetLabels()).To(HaveKeyWithValue(label, value),
				"%s %s should be labelled as owned by ControlPlane %s/%s", what, key, cp.Namespace, cp.Name)
		}
	}, itEventuallyTimeout, itPollInterval).Should(Succeed())
}
