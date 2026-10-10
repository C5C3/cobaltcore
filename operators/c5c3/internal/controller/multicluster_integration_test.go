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
	"errors"
	"path/filepath"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/c5c3/internal/testutil"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// TestIntegration_Multicluster_ControlPlanePlacement runs the ControlPlane
// reconciler on a management cluster with a second envtest environment
// registered as target cluster, and walks the per-service placement split:
// registration, a ControlPlane that places its Keystone service on the target,
// the admin password it has to read back off that cluster, a placed built-in
// service whose registration stays home while its credentials are mirrored onto
// the target, a placed network service that takes the shared bus and its OVN gate
// with it, a ControlPlane naming an unregistered cluster, and the deletion that
// sweeps the placed namespaces off the target again.
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
		mcGlanceNamespace   = "mc-cp-image"
		mcNetworkNamespace  = "mc-cp-network"
		mcBlockNamespace    = "mc-cp-block"
		mcComputeNamespace  = "mc-cp-compute"
		mcControlPlane      = "cp"

		// The OVN control plane the network service programs. It is deployed
		// outside the ControlPlane and only referenced, so this test creates it
		// and drives its status the way the ovn-operator would.
		mcOVNCentral = "mc-ovn"

		// The shared message bus, declared brownfield: this plane has no broker to
		// simulate, and a URL in a Secret is what a brownfield block resolves to.
		// mcBusSecret lives in the ControlPlane's own namespace, which is where the
		// bus is declared and read; mcBusTransportURL is what a placed service must
		// receive on the cluster it runs on.
		mcBusSecret       = "mc-bus-url"
		mcBusTransportURL = "rabbit://u:p@bus.mc-cp.svc:5672/"

		mcUnknownNamespace  = "mc-unknown"
		mcUnknownKeystoneNS = "mc-unknown-identity"
		mcUnknownCluster    = "does-not-exist"

		// The two namespaces assigned on the target cluster: the first exists
		// there only, the second exists nowhere.
		mcAssignedNamespace        = "mc-tenant"
		mcMissingAssignedNamespace = "mc-tenant-missing"

		// The ControlPlane a KeystoneUser on the target cluster orders from. It
		// is a second, unplaced plane: the first one places Keystone on the
		// target and publishes it from the start, so an order there would never
		// meet KeystoneNotPublished.
		mcOrderNamespace    = "mc-order"
		mcOrderControlPlane = "order-cp"
		mcOrderPublicURL    = "https://keystone.example.test/v3"

		// The managed bus the order plane gains for the RabbitMQVhost order, and
		// the address it is published at for a consumer on the target cluster.
		mcOrderBus       = "order-bus"
		mcOrderBusHost   = "order-bus.mc-order.svc"
		mcOrderPublished = "broker.example.test:5672"

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
	// none of the sibling service-operator ones: a target cluster holds the
	// workload, never the service CRs. The K-ORC kinds ARE served here, because
	// they ship in those same shared dirs, and that is what makes the "the K-ORC
	// ensemble is not on B" assertions below a real absence rather than a kind the
	// cluster could never have answered for. The order kinds are the c5c3 CRDs a
	// target serves: an order for a namespace assigned there lives there, as the
	// target-cluster-access chart installs it.
	orderCRDs := filepath.Join(testutil.C5c3WebhookDir(), "..", "crd", "bases")
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, mcScheme, append(commonenvtest.CommonFakeCRDDirs(),
		filepath.Join(orderCRDs, "c5c3.io_keystoneusers.yaml"),
		filepath.Join(orderCRDs, "c5c3.io_keystoneprojects.yaml"),
		filepath.Join(orderCRDs, "c5c3.io_keystoneroleassignments.yaml"),
		filepath.Join(orderCRDs, "c5c3.io_keystonecatalogentries.yaml"),
		filepath.Join(orderCRDs, "c5c3.io_keystoneapplicationcredentials.yaml"),
		filepath.Join(orderCRDs, "c5c3.io_rabbitmqvhosts.yaml")))

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
			if err := (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			// The webhook manifests installed by envtest carry the KeystoneService
			// and SizingProfile entries (failurePolicy=Fail), so their handlers must
			// be served here too.
			if err := (&c5c3v1alpha1.KeystoneServiceWebhook{}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&c5c3v1alpha1.SizingProfileWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
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
			// In production the KeystoneService controller registers the
			// spec.controlPlaneRef index the K-ORC catalog refresh lists
			// registrations through. That controller does not run here.
			if err := registerKeystoneServiceControlPlaneRefIndex(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
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
			if err := r.setupWithOptions(mcMgr, opts); err != nil {
				return err
			}
			// The order reconcilers resolve an order's cluster through the same
			// multicluster manager, so an order on the target is reconciled from
			// here.
			if err := (&KeystoneUserReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Resolver: mcMgr,
			}).setupWithOptions(mcMgr, opts); err != nil {
				return err
			}
			if err := (&KeystoneProjectReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Resolver: mcMgr,
			}).setupWithOptions(mcMgr, opts); err != nil {
				return err
			}
			if err := (&KeystoneRoleAssignmentReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Resolver: mcMgr,
			}).setupWithOptions(mcMgr, opts); err != nil {
				return err
			}
			if err := (&KeystoneCatalogEntryReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Resolver: mcMgr,
			}).setupWithOptions(mcMgr, opts); err != nil {
				return err
			}
			if err := (&KeystoneApplicationCredentialReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Resolver: mcMgr,
			}).setupWithOptions(mcMgr, opts); err != nil {
				return err
			}
			return (&RabbitMQVhostReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Resolver: mcMgr,
			}).setupWithOptions(mcMgr, opts)
		},
	})

	// The provider watches this namespace for registration Secrets.
	mcEnsureNamespace(t, ctx, mgmtClient, mcClustersNamespace)

	cp := integrationManagedControlPlane(mcControlPlane, mcNamespace)
	// The bus is declared from the start rather than added with the network
	// service: the validating webhook requires it beside services.neutron, and a
	// messaging block cannot be added to a live ControlPlane and then removed
	// again, so declaring it once keeps the later update to the service block
	// alone.
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: mcBusSecret},
	}
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
	// The object the three placed BUS CONSUMERS take with them that no other
	// service has: the shared bus, delivered as a Secret on the cluster each of them
	// runs on, under a name of its own. Each is asserted in its own subtest and
	// swept in the deletion one.
	neutronBusKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: neutronMessagingSecretName(cp)}
	cinderBusKey := client.ObjectKey{Namespace: mcBlockNamespace, Name: cinderMessagingSecretName(cp)}
	novaBusKey := client.ObjectKey{Namespace: mcComputeNamespace, Name: novaMessagingSecretName(cp)}

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
		// The brownfield bus the ControlPlane declares. It is read in the
		// ControlPlane's own namespace on the management cluster, whatever cluster
		// the service consuming it runs on.
		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: mcBusSecret, Namespace: mcNamespace},
			Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(mcBusTransportURL)},
		})).To(Succeed(), "seed the brownfield transport-URL Secret")

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
		// issued by the cert-manager there. The KEYSTONE namespace gets no copy at
		// home, because every path delivering into it — the admin password, the DB
		// credentials, the service accounts — resolves its store on the cluster the
		// service runs on. Only a namespace hosting a projected registration also
		// needs one at home, which the image service below exercises.
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

	t.Run("a placed built-in service registers at home and mirrors its credentials", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The image service joins the plane on the same target cluster, in a
		// namespace of its own. A placed catalog service has to advertise an
		// externally routable address for the reason Keystone does: its catalog row
		// is read from every cluster, and an in-cluster Service DNS name resolves on
		// none of the others.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return err
			}
			live.Spec.Services.Glance = integrationGlanceService()
			live.Spec.Services.Glance.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
				Name:      mcGlanceNamespace,
				Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			}
			live.Spec.Services.Glance.PublicEndpoint = "https://glance.example.com"
			live.Spec.Services.Glance.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: mcTargetCluster}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "place the image service on the target cluster")

		// --- The backing services follow the service, so the shared database and
		// cache materialise a second time in the image service's namespace, on the
		// cluster that namespace lives on. Infrastructure short-circuits the pipeline
		// while either is converging, so nothing below runs until they report.
		glanceMariaDBKey := client.ObjectKey{
			Namespace: mcGlanceNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		glanceMemcachedKey := client.ObjectKey{
			Namespace: mcGlanceNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		mcEventuallyExists(t, ctx, targetClient, glanceMariaDBKey, &mariadbv1alpha1.MariaDB{}, "image-side MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, glanceMariaDBKey, &mariadbv1alpha1.MariaDB{}, "image-side MariaDB")
		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, glanceMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, glanceMemcachedKey)

		// --- The tenant-store trio of the placed namespace exists on BOTH clusters.
		// The copy on the target is what the ESO there materialises the service's
		// Secrets through; the copy at home is what the registration resolves, since
		// a KeystoneService is reconciled on the cluster its CR lives on.
		glanceSAKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: esoTenantServiceAccountName}
		glanceCertKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: esoTenantClientCertName}
		glanceStoreKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: esoTenantStoreName}
		for _, cluster := range []struct {
			name string
			c    client.Client
		}{
			{"management", mgmtClient},
			{"target", targetClient},
		} {
			mcEventuallyExists(t, ctx, cluster.c, glanceSAKey, &corev1.ServiceAccount{},
				cluster.name+"-side tenant ServiceAccount")
			mcEventuallyExists(t, ctx, cluster.c, glanceCertKey, mcCertificate(),
				cluster.name+"-side tenant Certificate")
			mcEventuallyExists(t, ctx, cluster.c, glanceStoreKey, &esov1.SecretStore{},
				cluster.name+"-side tenant SecretStore")
		}

		// --- The home copy alone does not open the gate: the plane holds on the
		// target's, and its condition names the cluster it is waiting on.
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcGlanceNamespace)
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeESOTenantStoreReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal("SecretStoreNotReady"))
			ig.Expect(cond.Message).To(ContainSubstring(mcGlanceNamespace))
			ig.Expect(cond.Message).To(ContainSubstring("on the target cluster"))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"a placed namespace is gated on both of its tenant stores, and the message says which one is missing")

		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcGlanceNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The registration itself is reconciled at home whatever cluster the
		// service runs on: it authenticates through the admin credential, which is
		// materialised on the management cluster alone.
		registrationKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: mcControlPlane + "-glance"}
		mcEventuallyExists(t, ctx, mgmtClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Glance registration")
		mcExpectAbsent(t, ctx, targetClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Glance registration")

		// --- Its credentials, though, follow the service. The registration delivers
		// them at home only, so the ControlPlane materialises the same OpenBao path a
		// second time on the cluster the image service runs on, under the same name
		// its pods read.
		mirrorKey := client.ObjectKey{Namespace: mcGlanceNamespace, Name: mcControlPlane + "-glance-credentials"}
		mirror := &esov1.ExternalSecret{}
		mcEventuallyExists(t, ctx, targetClient, mirrorKey, mirror, "registration credentials mirror")
		g.Expect(mirror.Spec.Data).NotTo(BeEmpty())
		g.Expect(mirror.Spec.Data[0].RemoteRef.Key).To(Equal(
			"openstack/keystone/"+mcGlanceNamespace+"/"+mcControlPlane+"-glance/service-accounts/credentials"),
			"the mirror reads the registration's own per-CR OpenBao path")
		g.Expect(mirror.Spec.SecretStoreRef.Kind).To(Equal(string(commonv1.SecretStoreKindNamespaced)))
		g.Expect(mirror.Spec.SecretStoreRef.Name).To(Equal(esoTenantStoreName),
			"the mirror routes through the ControlPlane's effective store, which is the tenant store beside it")
		mcExpectRemoteClaim(t, ctx, targetClient, mirrorKey, &esov1.ExternalSecret{},
			"registration credentials mirror", cp)

		// --- And that is as far as this plane goes: no KeystoneService controller
		// runs here, so the registration never provisions the Keystone account and
		// GlanceReady parks on it rather than projecting a Glance that would
		// authenticate as a user nothing created.
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeGlanceReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	})

	t.Run("a placed network service takes its bus credentials and its OVN gate with it", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The OVNCentral the network service programs, created beside the
		// ControlPlane rather than in the network namespace: the ref below spells no
		// namespace, and an empty one resolves to the ControlPlane's own namespace.
		// The defaulting webhook writes that value into the ref, and
		// NeutronOVNCentralNamespace() reads an empty one the same way for a CR that
		// bypassed admission. Its targetClusterRef names the cluster the service is
		// placed on, so reconcileOVN selects the in-cluster database addresses and
		// never demands externallyReachable.
		ovnKey := client.ObjectKey{Namespace: mcNamespace, Name: mcOVNCentral}
		g.Expect(mgmtClient.Create(ctx, &ovnv1alpha1.OVNCentral{
			ObjectMeta: metav1.ObjectMeta{Name: mcOVNCentral, Namespace: mcNamespace},
			Spec: ovnv1alpha1.OVNCentralSpec{
				TLS:              ovnv1alpha1.OVNTLSSpec{IssuerRef: ovnv1alpha1.OVNIssuerRef{Name: "test-issuer"}},
				TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcTargetCluster},
			},
		})).To(Succeed(), "create the referenced OVNCentral on the management cluster")
		simulateOVNCentralReadyWhenPresent(t, ctx, mgmtClient, ovnKey)

		// The network service joins the plane on the same target cluster, in a
		// namespace of its own, advertising an externally routable address for the
		// reason its image sibling does.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return err
			}
			live.Spec.Services.Neutron = &c5c3v1alpha1.ServiceNeutronSpec{
				OVN: c5c3v1alpha1.NeutronOVNSpec{
					CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: mcOVNCentral},
				},
				Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
					Name:      mcNetworkNamespace,
					Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
				},
				PublicEndpoint:   "https://neutron.example.com",
				TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcTargetCluster},
			}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "place the network service on the target cluster")

		// --- The namespace is created on both clusters, and the backing services
		// follow the service onto the target. Infrastructure short-circuits the
		// pipeline while either is converging, so nothing below runs until they
		// report.
		networkNSKey := client.ObjectKey{Name: mcNetworkNamespace}
		mcEventuallyExists(t, ctx, targetClient, networkNSKey, &corev1.Namespace{}, "network service namespace")
		mcEventuallyExists(t, ctx, mgmtClient, networkNSKey, &corev1.Namespace{}, "network service namespace")

		neutronMariaDBKey := client.ObjectKey{
			Namespace: mcNetworkNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		neutronMemcachedKey := client.ObjectKey{
			Namespace: mcNetworkNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		mcEventuallyExists(t, ctx, targetClient, neutronMariaDBKey, &mariadbv1alpha1.MariaDB{}, "network-side MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, neutronMariaDBKey, &mariadbv1alpha1.MariaDB{}, "network-side MariaDB")
		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, neutronMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, neutronMemcachedKey)

		// --- The tenant store of the placed namespace exists on both clusters, for
		// the reason the image service's does, and the plane is gated on both.
		networkStoreKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: esoTenantStoreName}
		mcEventuallyExists(t, ctx, mgmtClient, networkStoreKey, &esov1.SecretStore{}, "management-side tenant SecretStore")
		mcEventuallyExists(t, ctx, targetClient, networkStoreKey, &esov1.SecretStore{}, "target-side tenant SecretStore")
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcNetworkNamespace)
		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcNetworkNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The OVN gate. The ControlPlane owns nothing of it: it reads the central
		// on the management cluster and mirrors the verdict, which is what the
		// projection behind it consumes.
		// The wait is on the reason as well as on the status: before the network
		// service was declared OVNReady was already True, with reason OVNNotManaged.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeOVNReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			ig.Expect(cond.Reason).To(Equal("OVNCentralReady"))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the referenced central serves both databases on the cluster the service was placed on")

		// --- The bus follows the service. It is declared and read in the
		// ControlPlane's own namespace on the management cluster, and delivered as a
		// Secret in the network namespace on the cluster the service runs on, claimed
		// by the ownership labels because no owner reference crosses a cluster.
		busSecret := &corev1.Secret{}
		mcEventuallyExists(t, ctx, targetClient, neutronBusKey, busSecret, "neutron messaging Secret")
		g.Expect(string(busSecret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(mcBusTransportURL),
			"the placed service receives the URL the ControlPlane's own bus block declares")
		mcExpectRemoteClaim(t, ctx, targetClient, neutronBusKey, &corev1.Secret{}, "neutron messaging Secret", cp)
		mcExpectAbsent(t, ctx, mgmtClient, neutronBusKey, &corev1.Secret{}, "neutron messaging Secret")

		// --- The registration is reconciled at home whatever cluster the service
		// runs on, exactly as the image service's is.
		registrationKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: mcControlPlane + "-neutron"}
		mcEventuallyExists(t, ctx, mgmtClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Neutron registration")
		mcExpectAbsent(t, ctx, targetClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Neutron registration")

		// --- Its credentials, though, follow the service: the ControlPlane
		// materialises the registration's own OpenBao path a second time on the
		// cluster the network service runs on, under the name its pods read.
		mirrorKey := client.ObjectKey{Namespace: mcNetworkNamespace, Name: mcControlPlane + "-neutron-credentials"}
		mirror := &esov1.ExternalSecret{}
		mcEventuallyExists(t, ctx, targetClient, mirrorKey, mirror, "registration credentials mirror")
		g.Expect(mirror.Spec.Data).NotTo(BeEmpty())
		g.Expect(mirror.Spec.Data[0].RemoteRef.Key).To(Equal(
			"openstack/keystone/"+mcNetworkNamespace+"/"+mcControlPlane+"-neutron/service-accounts/credentials"),
			"the mirror reads the registration's own per-CR OpenBao path")
		mcExpectRemoteClaim(t, ctx, targetClient, mirrorKey, &esov1.ExternalSecret{},
			"registration credentials mirror", cp)

		// --- And that is as far as this plane goes: no KeystoneService controller
		// runs here, so the registration never provisions the Keystone account and
		// NeutronReady parks on it rather than projecting a Neutron that would
		// authenticate as a user nothing created.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeNeutronReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the network service parks on the account its registration has not provisioned")
	})

	t.Run("a placed block-storage service takes its bus credentials with it", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The block-storage service joins the plane on the same target cluster, in a
		// namespace of its own, advertising an externally routable address for the
		// reason its image and network siblings do. It declares the volume backend of
		// the shared fixture and no backup backend: what this subtest walks is the
		// placement split, not the satellite projection, which the single-cluster
		// full-chain test covers.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return err
			}
			live.Spec.Services.Cinder = &c5c3v1alpha1.ServiceCinderSpec{
				Backends: integrationCinderService().Backends,
				Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
					Name:      mcBlockNamespace,
					Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
				},
				PublicEndpoint:   "https://cinder.example.com",
				TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcTargetCluster},
			}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"place the block-storage service on the target cluster")

		// --- The namespace is created on both clusters, and the backing services
		// follow the service onto the target. Infrastructure short-circuits the
		// pipeline while either is converging, so nothing below runs until they
		// report.
		blockNSKey := client.ObjectKey{Name: mcBlockNamespace}
		mcEventuallyExists(t, ctx, targetClient, blockNSKey, &corev1.Namespace{}, "block-storage service namespace")
		mcEventuallyExists(t, ctx, mgmtClient, blockNSKey, &corev1.Namespace{}, "block-storage service namespace")

		cinderMariaDBKey := client.ObjectKey{
			Namespace: mcBlockNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		cinderMemcachedKey := client.ObjectKey{
			Namespace: mcBlockNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		mcEventuallyExists(t, ctx, targetClient, cinderMariaDBKey, &mariadbv1alpha1.MariaDB{}, "block-side MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, cinderMariaDBKey, &mariadbv1alpha1.MariaDB{}, "block-side MariaDB")
		mcEventuallyExists(t, ctx, targetClient, cinderMemcachedKey, mcMemcached(), "block-side Memcached")
		mcExpectAbsent(t, ctx, mgmtClient, cinderMemcachedKey, mcMemcached(), "block-side Memcached")
		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, cinderMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, cinderMemcachedKey)

		// --- The tenant store of the placed namespace exists on both clusters, for
		// the reason the image service's does, and the plane is gated on both.
		blockStoreKey := client.ObjectKey{Namespace: mcBlockNamespace, Name: esoTenantStoreName}
		mcEventuallyExists(t, ctx, mgmtClient, blockStoreKey, &esov1.SecretStore{}, "management-side tenant SecretStore")
		mcEventuallyExists(t, ctx, targetClient, blockStoreKey, &esov1.SecretStore{}, "target-side tenant SecretStore")
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcBlockNamespace)
		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcBlockNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The bus follows the service, the way it follows the network one: read
		// in the ControlPlane's own namespace on the management cluster, delivered as
		// a Secret in the block-storage namespace on the cluster the service runs on,
		// claimed by the ownership labels because no owner reference crosses a
		// cluster.
		busSecret := &corev1.Secret{}
		mcEventuallyExists(t, ctx, targetClient, cinderBusKey, busSecret, "cinder messaging Secret")
		g.Expect(string(busSecret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(mcBusTransportURL),
			"the placed service receives the URL the ControlPlane's own bus block declares")
		mcExpectRemoteClaim(t, ctx, targetClient, cinderBusKey, &corev1.Secret{}, "cinder messaging Secret", cp)
		mcExpectAbsent(t, ctx, mgmtClient, cinderBusKey, &corev1.Secret{}, "cinder messaging Secret")

		// --- The registration is reconciled at home whatever cluster the service
		// runs on, exactly as the image and network services' are.
		registrationKey := client.ObjectKey{Namespace: mcBlockNamespace, Name: mcControlPlane + "-cinder"}
		mcEventuallyExists(t, ctx, mgmtClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Cinder registration")
		mcExpectAbsent(t, ctx, targetClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Cinder registration")

		// --- Its credentials, though, follow the service: the ControlPlane
		// materialises the registration's own OpenBao path a second time on the
		// cluster the block-storage service runs on, under the name its pods read.
		mirrorKey := client.ObjectKey{Namespace: mcBlockNamespace, Name: mcControlPlane + "-cinder-credentials"}
		mirror := &esov1.ExternalSecret{}
		mcEventuallyExists(t, ctx, targetClient, mirrorKey, mirror, "registration credentials mirror")
		g.Expect(mirror.Spec.Data).NotTo(BeEmpty())
		g.Expect(mirror.Spec.Data[0].RemoteRef.Key).To(Equal(
			"openstack/keystone/"+mcBlockNamespace+"/"+mcControlPlane+"-cinder/service-accounts/credentials"),
			"the mirror reads the registration's own per-CR OpenBao path")
		mcExpectRemoteClaim(t, ctx, targetClient, mirrorKey, &esov1.ExternalSecret{},
			"registration credentials mirror", cp)

		// --- And that is as far as this plane goes: no KeystoneService controller
		// runs here, so the registration never provisions the Keystone account and
		// CinderReady parks on it rather than projecting a Cinder that would
		// authenticate as a user nothing created.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeCinderReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the block-storage service parks on the account its registration has not provisioned")
	})

	t.Run("a placed compute service takes its bus credentials with it", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The compute service joins the plane on the same target cluster, in a
		// namespace of its own, advertising an externally routable address for the
		// reason its siblings do.
		//
		// The placement service comes with it, co-located. The webhook requires
		// services.placement, services.neutron and services.glance beside
		// services.nova (the three services the compute service calls on the path of
		// every instance it boots), and placement is the one this plane does not
		// declare yet.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
				return err
			}
			live.Spec.Services.Placement = integrationPlacementService()
			live.Spec.Services.Nova = &c5c3v1alpha1.ServiceNovaSpec{
				Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{
					Name:      mcComputeNamespace,
					Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
				},
				PublicEndpoint:   "https://nova.example.com",
				TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcTargetCluster},
			}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"place the compute service on the target cluster")

		// --- The placement service is the first this plane keeps at home, so the
		// shared backing services are provisioned in the ControlPlane's OWN namespace
		// for the first time. Infrastructure sits in the blocking prefix, so the whole
		// pipeline stops behind them until they report.
		homeMariaDBKey := client.ObjectKey{
			Namespace: mcNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		homeMemcachedKey := client.ObjectKey{
			Namespace: mcNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		simulateMariaDBReadyWhenPresent(t, ctx, mgmtClient, homeMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, mgmtClient, homeMemcachedKey)

		// --- The namespace is created on both clusters, and the backing services
		// follow the service onto the target. Infrastructure short-circuits the
		// pipeline while either is converging, so nothing below runs until they
		// report.
		computeNSKey := client.ObjectKey{Name: mcComputeNamespace}
		mcEventuallyExists(t, ctx, targetClient, computeNSKey, &corev1.Namespace{}, "compute service namespace")
		mcEventuallyExists(t, ctx, mgmtClient, computeNSKey, &corev1.Namespace{}, "compute service namespace")

		novaMariaDBKey := client.ObjectKey{
			Namespace: mcComputeNamespace,
			Name:      cp.Spec.Infrastructure.Database.ClusterRef.Name,
		}
		novaMemcachedKey := client.ObjectKey{
			Namespace: mcComputeNamespace,
			Name:      cp.Spec.Infrastructure.Cache.ClusterRef.Name,
		}
		mcEventuallyExists(t, ctx, targetClient, novaMariaDBKey, &mariadbv1alpha1.MariaDB{}, "compute-side MariaDB")
		mcExpectAbsent(t, ctx, mgmtClient, novaMariaDBKey, &mariadbv1alpha1.MariaDB{}, "compute-side MariaDB")
		mcEventuallyExists(t, ctx, targetClient, novaMemcachedKey, mcMemcached(), "compute-side Memcached")
		mcExpectAbsent(t, ctx, mgmtClient, novaMemcachedKey, mcMemcached(), "compute-side Memcached")
		simulateMariaDBReadyWhenPresent(t, ctx, targetClient, novaMariaDBKey)
		simulateMemcachedReadyWhenPresent(t, ctx, targetClient, novaMemcachedKey)

		// --- The tenant store of the placed namespace exists on both clusters, for
		// the reason the image service's does, and the plane is gated on both.
		computeStoreKey := client.ObjectKey{Namespace: mcComputeNamespace, Name: esoTenantStoreName}
		mcEventuallyExists(t, ctx, mgmtClient, computeStoreKey, &esov1.SecretStore{}, "management-side tenant SecretStore")
		mcEventuallyExists(t, ctx, targetClient, computeStoreKey, &esov1.SecretStore{}, "target-side tenant SecretStore")
		ensureReadySecretStore(t, ctx, mgmtClient, esoTenantStoreName, mcComputeNamespace)
		ensureReadySecretStore(t, ctx, targetClient, esoTenantStoreName, mcComputeNamespace)
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeESOTenantStoreReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The compute leg writes nothing at all while the placement service it
		// was declared with has not converged: every instance claims its resources in
		// Placement before it boots, so the bus delivery, the registration and the
		// credentials mirror all sit behind that gate.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeNovaReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal("WaitingForPlacement"))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the compute service defers everything while its placement sibling is not ready")
		mcExpectAbsent(t, ctx, targetClient, novaBusKey, &corev1.Secret{}, "nova messaging Secret")

		// --- Open that gate. The placement service is co-located, so every step of
		// it runs against the management cluster: its registration reported
		// converged by hand (no KeystoneService controller runs here), the
		// engine-issued DB credential behind it, and the child itself.
		mcMarkRegistrationConverged(t, ctx, mgmtClient,
			client.ObjectKey{Namespace: mcNamespace, Name: placementName(cp)})
		simulatePlacementDBCredentialSyncWhenPresent(t, ctx, mgmtClient, cp)
		simulatePlacementReadyWhenPresent(t, ctx, mgmtClient,
			client.ObjectKey{Namespace: mcNamespace, Name: placementName(cp)})
		waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypePlacementReady, metav1.ConditionTrue, itEventuallyTimeout)

		// --- The bus follows the service, the way it follows the network and
		// block-storage ones: read in the ControlPlane's own namespace on the
		// management cluster, delivered as a Secret in the compute namespace on the
		// cluster the service runs on, claimed by the ownership labels because no
		// owner reference crosses a cluster.
		busSecret := &corev1.Secret{}
		mcEventuallyExists(t, ctx, targetClient, novaBusKey, busSecret, "nova messaging Secret")
		g.Expect(string(busSecret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(mcBusTransportURL),
			"the placed service receives the URL the ControlPlane's own bus block declares")
		mcExpectRemoteClaim(t, ctx, targetClient, novaBusKey, &corev1.Secret{}, "nova messaging Secret", cp)
		mcExpectAbsent(t, ctx, mgmtClient, novaBusKey, &corev1.Secret{}, "nova messaging Secret")

		// --- The registration is reconciled at home whatever cluster the service
		// runs on, exactly as its siblings' are.
		registrationKey := client.ObjectKey{Namespace: mcComputeNamespace, Name: mcControlPlane + "-nova"}
		mcEventuallyExists(t, ctx, mgmtClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Nova registration")
		mcExpectAbsent(t, ctx, targetClient, registrationKey, &c5c3v1alpha1.KeystoneService{},
			"Nova registration")

		// --- Its credentials, though, follow the service: the ControlPlane
		// materialises the registration's own OpenBao path a second time on the
		// cluster the compute service runs on, under the name its pods read.
		mirrorKey := client.ObjectKey{Namespace: mcComputeNamespace, Name: mcControlPlane + "-nova-credentials"}
		mirror := &esov1.ExternalSecret{}
		mcEventuallyExists(t, ctx, targetClient, mirrorKey, mirror, "registration credentials mirror")
		g.Expect(mirror.Spec.Data).NotTo(BeEmpty())
		g.Expect(mirror.Spec.Data[0].RemoteRef.Key).To(Equal(
			"openstack/keystone/"+mcComputeNamespace+"/"+mcControlPlane+"-nova/service-accounts/credentials"),
			"the mirror reads the registration's own per-CR OpenBao path")
		mcExpectRemoteClaim(t, ctx, targetClient, mirrorKey, &esov1.ExternalSecret{},
			"registration credentials mirror", cp)

		// --- And that is as far as this plane goes: the compute registration is the
		// one this subtest leaves standing, so NovaReady parks on it rather than
		// projecting a Nova that would authenticate as a user nothing created.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeNovaReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			ig.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"the compute service parks on the account its registration has not provisioned")
	})

	t.Run("a namespace assignment on the target cluster reports its namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The assigned namespace exists on the target cluster alone, so a read
		// against the management cluster would report it missing.
		mcEnsureNamespace(t, ctx, targetClient, mcAssignedNamespace)

		target := &commonv1.TargetClusterRefSpec{Name: mcTargetCluster}
		setAssignments := func(entries []c5c3v1alpha1.NamespaceAssignmentSpec, what string) {
			t.Helper()
			g.Eventually(func() error {
				live := &c5c3v1alpha1.ControlPlane{}
				if err := mgmtClient.Get(ctx, cpKey, live); err != nil {
					return err
				}
				live.Spec.NamespaceAssignments = entries
				return mgmtClient.Update(ctx, live)
			}, itEventuallyTimeout, itPollInterval).Should(Succeed(), what)
		}
		setAssignments([]c5c3v1alpha1.NamespaceAssignmentSpec{
			{Namespace: mcAssignedNamespace, TargetClusterRef: target, AllowedRoles: []string{"member"}},
			{Namespace: mcMissingAssignedNamespace, TargetClusterRef: target},
		}, "assign two namespaces on the target cluster")

		// Read back from the API server, not from the in-memory CR.
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			ig.Expect(live.Status.NamespaceAssignments).To(HaveLen(2))

			present := live.Status.NamespaceAssignments[0]
			ig.Expect(present.Namespace).To(Equal(mcAssignedNamespace))
			ig.Expect(present.TargetClusterRef).To(Equal(target))
			ig.Expect(present.AllowedRoles).To(Equal([]string{"member"}))
			ig.Expect(present.ClusterReachable).To(BeTrue())
			ig.Expect(present.NamespaceExists).To(BeTrue())
			ig.Expect(present.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentAssigned))

			missing := live.Status.NamespaceAssignments[1]
			ig.Expect(missing.Namespace).To(Equal(mcMissingAssignedNamespace))
			ig.Expect(missing.ClusterReachable).To(BeTrue())
			ig.Expect(missing.NamespaceExists).To(BeFalse())
			ig.Expect(missing.Reason).To(Equal(c5c3v1alpha1.NamespaceAssignmentNamespaceNotFound))

			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeNamespaceAssignmentsReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"both entries report what the target cluster holds")

		// The step only reads: neither namespace appears where it was missing.
		mcExpectAbsent(t, ctx, targetClient, client.ObjectKey{Name: mcMissingAssignedNamespace},
			&corev1.Namespace{}, "missing assigned namespace")
		mcExpectAbsent(t, ctx, mgmtClient, client.ObjectKey{Name: mcAssignedNamespace},
			&corev1.Namespace{}, "assigned namespace")

		// An entry naming an unregistered cluster is refused in status.
		setAssignments([]c5c3v1alpha1.NamespaceAssignmentSpec{
			{Namespace: mcAssignedNamespace, TargetClusterRef: target},
			{Namespace: mcAssignedNamespace, TargetClusterRef: &commonv1.TargetClusterRefSpec{Name: mcUnknownCluster}},
		}, "assign a namespace on an unregistered cluster")
		cond := waitForControlPlaneCondition(t, ctx, mgmtClient, cpKey,
			conditionTypeNamespaceAssignmentsReady, metav1.ConditionFalse, itEventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(ContainSubstring(mcAssignedNamespace + ` on target cluster "` + mcUnknownCluster + `"`))

		// Removing the field returns the condition to its default.
		setAssignments(nil, "remove every namespace assignment")
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.ControlPlane{}
			ig.Expect(mgmtClient.Get(ctx, cpKey, live)).To(Succeed())
			ig.Expect(live.Status.NamespaceAssignments).To(BeEmpty())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeNamespaceAssignmentsReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Reason).To(Equal("NoNamespaceAssignments"))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"removing every entry clears the status")
	})

	t.Run("a KeystoneUser on the target cluster is delivered beside it", func(t *testing.T) {
		g := NewGomegaWithT(t)

		target := &commonv1.TargetClusterRefSpec{Name: mcTargetCluster}
		mcEnsureNamespace(t, ctx, mgmtClient, mcOrderNamespace)
		mcEnsureNamespace(t, ctx, targetClient, mcAssignedNamespace)
		orderCP := integrationManagedControlPlane(mcOrderControlPlane, mcOrderNamespace)
		// The roles are the next subtest's: the pieces order a role beside a user
		// on this same plane.
		orderCP.Spec.NamespaceAssignments = []c5c3v1alpha1.NamespaceAssignmentSpec{
			{Namespace: mcAssignedNamespace, TargetClusterRef: target, AllowedRoles: []string{"member"}},
		}
		g.Expect(mgmtClient.Create(ctx, orderCP)).To(Succeed(), "create the ControlPlane the order names")
		driveControlPlaneToAdminCredentialReady(t, ctx, mgmtClient, orderCP)
		orderCPKey := client.ObjectKeyFromObject(orderCP)

		order := &c5c3v1alpha1.KeystoneUser{
			ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: mcAssignedNamespace},
			Spec: c5c3v1alpha1.KeystoneUserSpec{
				ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: mcOrderControlPlane, Namespace: mcOrderNamespace},
			},
		}
		g.Expect(targetClient.Create(ctx, order)).To(Succeed(), "create the order on the target cluster")
		orderKey := client.ObjectKeyFromObject(order)
		prefix := keystoneUserChildPrefix(order, mcTargetCluster)
		wantLabels := map[string]string{
			keystoneUserNameLabel:      order.Name,
			keystoneUserNamespaceLabel: mcAssignedNamespace,
			keystoneUserClusterLabel:   mcTargetCluster,
		}

		// envtest runs no K-ORC, so the probe is answered by hand: no such user.
		g.Eventually(func() error {
			probe := &orcv1alpha1.User{}
			if err := mgmtClient.Get(ctx, client.ObjectKey{Namespace: mcOrderNamespace, Name: prefix + "user-probe"}, probe); err != nil {
				return err
			}
			probe.Status.Conditions = pendingImportConditions(0)
			return mgmtClient.Status().Update(ctx, probe)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the order probes for its user on the management cluster")

		user := &orcv1alpha1.User{}
		userKey := client.ObjectKey{Namespace: mcOrderNamespace, Name: prefix + "user"}
		mcEventuallyExists(t, ctx, mgmtClient, userKey, user, "managed User")
		g.Expect(user.Labels).To(Equal(wantLabels))
		g.Expect(user.OwnerReferences).To(BeEmpty(), "a child on another cluster than its order carries labels only")
		targetUsers := &orcv1alpha1.UserList{}
		g.Expect(targetClient.List(ctx, targetUsers)).To(Succeed())
		g.Expect(targetUsers.Items).To(BeEmpty(), "nothing K-ORC reads is written on the target")

		g.Eventually(func() error {
			live := &orcv1alpha1.User{}
			if err := mgmtClient.Get(ctx, userKey, live); err != nil {
				return err
			}
			live.Status.ID = ptr.To("workflow-user-id")
			live.Status.Conditions = availableImportConditions()
			live.Status.Conditions[0].ObservedGeneration = live.Generation
			live.Status.Resource = &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: prefix + "password-v1"}
			return mgmtClient.Status().Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "report the user Available with v1 applied")

		// The plane publishes no endpoint yet, and the order is on another
		// cluster than Keystone. The delivery stops before it writes anything.
		pushKey := client.ObjectKey{Namespace: mcOrderNamespace, Name: prefix + "backup"}
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.KeystoneUser{}
			ig.Expect(targetClient.Get(ctx, orderKey, live)).To(Succeed())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeKeystoneUserDeliveryReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Reason).To(Equal(reasonKeystoneUserKeystoneNotPublished))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "an unpublished Keystone refuses the delivery")
		mcExpectAbsent(t, ctx, targetClient, client.ObjectKey{Namespace: mcAssignedNamespace, Name: "workflow-credentials"},
			&corev1.Secret{}, "delivered Secret")
		mcExpectAbsent(t, ctx, mgmtClient, pushKey, &esov1alpha1.PushSecret{}, "order PushSecret")

		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, orderCPKey, live); err != nil {
				return err
			}
			live.Spec.Services.Keystone.PublicEndpoint = mcOrderPublicURL
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "publish the Keystone endpoint")
		// No ControlPlane watch reaches an order on a target cluster; it comes back
		// on orderRefreshAfter, which a test cannot wait for. An annotation
		// edit wakes it the same way.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.KeystoneUser{}
			if err := targetClient.Get(ctx, orderKey, live); err != nil {
				return err
			}
			live.Annotations = map[string]string{"test.c5c3.io/nudge": "published"}
			return targetClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "nudge the order")

		// envtest runs no ESO either: the backup is reported pushed by hand, once
		// the operator has stamped the document's hash on it, by moving
		// syncedResourceVersion the way a completed push does.
		g.Eventually(func() error {
			live := &esov1alpha1.PushSecret{}
			if err := mgmtClient.Get(ctx, pushKey, live); err != nil {
				return err
			}
			hash := live.Annotations[keystoneUserPushContentHashAnnotation]
			if hash == "" {
				return errors.New("the PushSecret carries no content hash yet")
			}
			live.Status.SyncedResourceVersion = "pushed-" + hash
			live.Status.Conditions = []esov1alpha1.PushSecretStatusCondition{{
				Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue, Reason: "PushSecretSynced",
				LastTransitionTime: metav1.Now(),
			}}
			return mgmtClient.Status().Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "report the stamped document pushed")
		push := &esov1alpha1.PushSecret{}
		g.Expect(mgmtClient.Get(ctx, pushKey, push)).To(Succeed())
		g.Expect(push.Labels).To(Equal(wantLabels))
		g.Expect(push.OwnerReferences).To(BeEmpty())

		secretKey := client.ObjectKey{Namespace: mcAssignedNamespace, Name: "workflow-credentials"}
		expectDelivered := func(what string) {
			t.Helper()
			g.Eventually(func(ig Gomega) {
				live := &c5c3v1alpha1.KeystoneUser{}
				ig.Expect(targetClient.Get(ctx, orderKey, live)).To(Succeed())
				ig.Expect(meta.IsStatusConditionTrue(live.Status.Conditions, conditionTypeReady)).To(BeTrue())
				secret := &corev1.Secret{}
				ig.Expect(targetClient.Get(ctx, secretKey, secret)).To(Succeed())
				ig.Expect(metav1.IsControlledBy(secret, live)).To(BeTrue(), "the order owns its Secret on the target")
				ig.Expect(string(secret.Data["clouds.yaml"])).To(ContainSubstring(mcOrderPublicURL))
				ig.Expect(string(secret.Data["clouds.yaml"])).NotTo(ContainSubstring("project_name"))
				ig.Expect(secret.Data["password"]).NotTo(BeEmpty())
			}, itEventuallyTimeout, itPollInterval).Should(Succeed(), what)
		}
		expectDelivered("the credentials are delivered beside the order")
		mcExpectAbsent(t, ctx, mgmtClient, secretKey, &corev1.Secret{}, "delivered Secret on the management cluster")

		g.Expect(targetClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
		})).To(Succeed())
		expectDelivered("a deleted Secret is delivered again")

		g.Expect(targetClient.Delete(ctx, order)).To(Succeed())
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, orderKey, &c5c3v1alpha1.KeystoneUser{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, userKey, &orcv1alpha1.User{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, pushKey, &esov1alpha1.PushSecret{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"deleting the order removes the User, the PushSecret and the Secret")
	})

	t.Run("the Keystone pieces are ordered beside a user on the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		orderCPKey := client.ObjectKey{Namespace: mcOrderNamespace, Name: mcOrderControlPlane}
		cpRef := c5c3v1alpha1.ControlPlaneRefSpec{Name: mcOrderControlPlane, Namespace: mcOrderNamespace}
		objMeta := func(name string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: name, Namespace: mcAssignedNamespace}
		}
		user := &c5c3v1alpha1.KeystoneUser{ObjectMeta: objMeta("pieces"), Spec: c5c3v1alpha1.KeystoneUserSpec{ControlPlaneRef: cpRef}}
		project := &c5c3v1alpha1.KeystoneProject{
			ObjectMeta: objMeta("pieces-project"), Spec: c5c3v1alpha1.KeystoneProjectSpec{ControlPlaneRef: cpRef},
		}
		assignment := &c5c3v1alpha1.KeystoneRoleAssignment{
			ObjectMeta: objMeta("pieces-member"),
			Spec: c5c3v1alpha1.KeystoneRoleAssignmentSpec{
				ControlPlaneRef: cpRef,
				UserRef:         c5c3v1alpha1.KeystoneOrderRef{Name: user.Name},
				ProjectRef:      c5c3v1alpha1.KeystoneOrderRef{Name: project.Name},
				Role:            "member",
			},
		}
		entry := &c5c3v1alpha1.KeystoneCatalogEntry{
			ObjectMeta: objMeta("pieces-dns"),
			Spec: c5c3v1alpha1.KeystoneCatalogEntrySpec{
				ControlPlaneRef: cpRef,
				ServiceType:     "dns",
				Endpoints: []c5c3v1alpha1.KeystoneServiceEndpointSpec{
					{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://dns.example.test/v2"},
				},
			},
		}
		for _, obj := range []client.Object{user, project, assignment, entry} {
			g.Expect(targetClient.Create(ctx, obj)).To(Succeed(), "create %T %s on the target cluster", obj, obj.GetName())
		}

		userPrefix := keystoneUserChildPrefix(user, mcTargetCluster)
		projectRef := keystoneProjectRef(project, mcTargetCluster)
		assignmentRef := keystoneRoleAssignmentRef(assignment, mcTargetCluster)
		entryRef := keystoneCatalogEntryRef(entry, mcTargetCluster)
		childKey := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: mcOrderNamespace, Name: name} }

		// envtest runs no K-ORC: a probe is answered absent, a child is reported
		// Available for its live generation, by hand.
		markStatus := func(obj client.Object, name string, set func()) {
			t.Helper()
			g.Eventually(func() error {
				if err := mgmtClient.Get(ctx, childKey(name), obj); err != nil {
					return err
				}
				set()
				return mgmtClient.Status().Update(ctx, obj)
			}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "report %s converged", name)
		}
		available := func(obj client.Object) []metav1.Condition {
			conds := availableImportConditions()
			conds[0].ObservedGeneration = obj.GetGeneration()
			return conds
		}
		expectClaimed := func(obj client.Object, name string, ref orderRef) {
			t.Helper()
			mcEventuallyExists(t, ctx, mgmtClient, childKey(name), obj, name)
			g.Expect(obj.GetLabels()).To(Equal(ref.childLabels()), name)
			g.Expect(obj.GetOwnerReferences()).To(BeEmpty(), "%s carries labels only", name)
		}
		expectCondition := func(obj client.Object, key client.ObjectKey, conds func() []metav1.Condition,
			condType string, status metav1.ConditionStatus, reason, what string,
		) {
			t.Helper()
			g.Eventually(func(ig Gomega) {
				ig.Expect(targetClient.Get(ctx, key, obj)).To(Succeed())
				cond := meta.FindStatusCondition(conds(), condType)
				ig.Expect(cond).NotTo(BeNil())
				ig.Expect(cond.Status).To(Equal(status))
				ig.Expect(cond.Reason).To(Equal(reason))
			}, itEventuallyTimeout, itPollInterval).Should(Succeed(), what)
		}

		// The user and the project converge first; the assignment waits on them.
		userProbe := &orcv1alpha1.User{}
		markStatus(userProbe, userPrefix+"user-probe", func() { userProbe.Status.Conditions = pendingImportConditions(0) })
		managedUser := &orcv1alpha1.User{}
		markStatus(managedUser, userPrefix+"user", func() {
			managedUser.Status.ID = ptr.To("pieces-user-id")
			managedUser.Status.Conditions = available(managedUser)
			managedUser.Status.Resource = &orcv1alpha1.UserResourceStatus{AppliedPasswordRef: userPrefix + "password-v1"}
		})
		projectProbe := &orcv1alpha1.Project{}
		markStatus(projectProbe, projectRef.childPrefix()+"project-probe", func() {
			projectProbe.Status.Conditions = pendingImportConditions(0)
		})
		managedProject := &orcv1alpha1.Project{}
		expectClaimed(managedProject, projectRef.childPrefix()+"project", projectRef)
		markStatus(managedProject, projectRef.childPrefix()+"project", func() {
			managedProject.Status.ID = ptr.To("pieces-project-id")
			managedProject.Status.Conditions = available(managedProject)
		})
		liveProject := &c5c3v1alpha1.KeystoneProject{}
		expectCondition(liveProject, client.ObjectKeyFromObject(project), func() []metav1.Condition { return liveProject.Status.Conditions },
			conditionTypeKeystoneProjectProjectReady, metav1.ConditionTrue, reasonKeystoneProjectProvisioned,
			"the project is provisioned")
		g.Expect(liveProject.Status.ProjectID).To(Equal("pieces-project-id"))

		role := &orcv1alpha1.Role{}
		expectClaimed(role, assignmentRef.childPrefix()+"role", assignmentRef)
		roleAssignment := &orcv1alpha1.RoleAssignment{}
		expectClaimed(roleAssignment, assignmentRef.childPrefix()+"assignment", assignmentRef)
		g.Expect(string(*roleAssignment.Spec.Resource.UserRef)).To(Equal(userPrefix + "user"))
		g.Expect(string(*roleAssignment.Spec.Resource.ProjectRef)).To(Equal(projectRef.childPrefix() + "project"))
		markStatus(role, assignmentRef.childPrefix()+"role", func() {
			role.Status.ID = ptr.To("member-role-id")
			role.Status.Conditions = available(role)
		})
		markStatus(roleAssignment, assignmentRef.childPrefix()+"assignment", func() {
			roleAssignment.Status.Conditions = available(roleAssignment)
			roleAssignment.Status.Resource = &orcv1alpha1.RoleAssignmentResourceStatus{
				RoleID: "member-role-id", UserID: "pieces-user-id", ProjectID: "pieces-project-id",
			}
		})
		liveAssignment := &c5c3v1alpha1.KeystoneRoleAssignment{}
		expectCondition(liveAssignment, client.ObjectKeyFromObject(assignment),
			func() []metav1.Condition { return liveAssignment.Status.Conditions },
			conditionTypeKeystoneRoleAssignmentAssignmentReady, metav1.ConditionTrue, reasonKeystoneRoleAssignmentAssigned,
			"the role is assigned")
		g.Expect(liveAssignment.Status.RoleID).To(Equal("member-role-id"))
		g.Expect(liveAssignment.Status.UserID).To(Equal("pieces-user-id"))
		g.Expect(liveAssignment.Status.ProjectID).To(Equal("pieces-project-id"))

		// With the role assigned, an application credential is minted as the user:
		// the mint document and generation 1 appear on the management cluster.
		// envtest runs no K-ORC, so the credential never turns Available here.
		credential := &c5c3v1alpha1.KeystoneApplicationCredential{
			ObjectMeta: objMeta("pieces-appcred"),
			Spec: c5c3v1alpha1.KeystoneApplicationCredentialSpec{
				ControlPlaneRef: cpRef,
				UserRef:         c5c3v1alpha1.KeystoneOrderRef{Name: user.Name},
				ProjectRef:      c5c3v1alpha1.KeystoneOrderRef{Name: project.Name},
			},
		}
		g.Expect(targetClient.Create(ctx, credential)).To(Succeed(), "create the credential order on the target cluster")
		credentialRef := keystoneApplicationCredentialRef(credential, mcTargetCluster)
		mintCloud := &corev1.Secret{}
		expectClaimed(mintCloud, credentialRef.childPrefix()+"mint-cloud", credentialRef)
		g.Expect(string(mintCloud.Data[appCredCloudsYAMLKey])).To(ContainSubstring(`username: "pieces"`))
		g.Expect(string(mintCloud.Data[appCredCloudsYAMLKey])).To(ContainSubstring(`project_name: "pieces-project"`))
		expectClaimed(&corev1.Secret{}, credentialRef.childPrefix()+"secret-v1", credentialRef)
		applicationCredential := &orcv1alpha1.ApplicationCredential{}
		expectClaimed(applicationCredential, credentialRef.childPrefix()+"credential-v1", credentialRef)
		g.Expect(string(applicationCredential.Spec.Resource.UserRef)).To(Equal(userPrefix + "user"))
		g.Expect(applicationCredential.Spec.CloudCredentialsRef.SecretName).To(Equal(credentialRef.childPrefix() + "mint-cloud"))
		liveCredential := &c5c3v1alpha1.KeystoneApplicationCredential{}
		credentialKey := client.ObjectKeyFromObject(credential)
		expectCondition(liveCredential, credentialKey, func() []metav1.Condition { return liveCredential.Status.Conditions },
			conditionTypeKeystoneApplicationCredentialCredentialReady, metav1.ConditionFalse,
			reasonKeystoneApplicationCredentialWaiting, "the credential waits for K-ORC")

		// The credential order goes before the pieces it uses.
		g.Expect(targetClient.Delete(ctx, credential)).To(Succeed())
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, credentialKey,
				&c5c3v1alpha1.KeystoneApplicationCredential{}))).To(BeTrue())
			for _, name := range []string{"mint-cloud", "secret-v1"} {
				ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(credentialRef.childPrefix()+name),
					&corev1.Secret{}))).To(BeTrue(), name)
			}
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(credentialRef.childPrefix()+"credential-v1"),
				&orcv1alpha1.ApplicationCredential{}))).To(BeTrue())
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "deleting the credential order removes its children")

		// The entry has no catalog consent yet, and projects nothing.
		liveEntry := &c5c3v1alpha1.KeystoneCatalogEntry{}
		entryKey := client.ObjectKeyFromObject(entry)
		expectCondition(liveEntry, entryKey, func() []metav1.Condition { return liveEntry.Status.Conditions },
			conditionTypeKeystoneCatalogEntryCatalogReady, metav1.ConditionFalse, reasonKeystoneCatalogEntryNotAllowed,
			"the entry is refused without allowCatalogEntries")
		mcExpectAbsent(t, ctx, mgmtClient, childKey(entryRef.childPrefix()+"service-probe"), &orcv1alpha1.Service{},
			"catalog probe")

		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, orderCPKey, live); err != nil {
				return err
			}
			live.Spec.NamespaceAssignments[0].AllowCatalogEntries = true
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "admit catalog entries")
		// No ControlPlane watch reaches an order on a target cluster; an
		// annotation edit wakes it the way the refresh would.
		g.Eventually(func() error {
			if err := targetClient.Get(ctx, entryKey, liveEntry); err != nil {
				return err
			}
			liveEntry.Annotations = map[string]string{"test.c5c3.io/nudge": "admitted"}
			return targetClient.Update(ctx, liveEntry)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "nudge the entry")

		serviceProbe := &orcv1alpha1.Service{}
		markStatus(serviceProbe, entryRef.childPrefix()+"service-probe", func() {
			serviceProbe.Status.Conditions = pendingImportConditions(0)
		})
		service := &orcv1alpha1.Service{}
		expectClaimed(service, entryRef.childPrefix()+"service", entryRef)
		region := &orcv1alpha1.Region{}
		expectClaimed(region, entryRef.childPrefix()+"region", entryRef)
		endpoint := &orcv1alpha1.Endpoint{}
		expectClaimed(endpoint, entryRef.childPrefix()+"endpoint-public", entryRef)
		markStatus(service, entryRef.childPrefix()+"service", func() {
			service.Status.ID = ptr.To("dns-service-id")
			service.Status.Conditions = available(service)
		})
		markStatus(region, entryRef.childPrefix()+"region", func() { region.Status.Conditions = available(region) })
		markStatus(endpoint, entryRef.childPrefix()+"endpoint-public", func() {
			endpoint.Status.ID = ptr.To("dns-public-id")
			endpoint.Status.Conditions = available(endpoint)
		})
		expectCondition(liveEntry, entryKey, func() []metav1.Condition { return liveEntry.Status.Conditions },
			conditionTypeKeystoneCatalogEntryCatalogReady, metav1.ConditionTrue, reasonKeystoneServiceCatalogRegistered,
			"the entry is registered")
		g.Expect(liveEntry.Status.ServiceID).To(Equal("dns-service-id"))

		// A referenced user holds its deletion until the assignment is gone.
		g.Expect(targetClient.Delete(ctx, user)).To(Succeed())
		liveUser := &c5c3v1alpha1.KeystoneUser{}
		expectCondition(liveUser, client.ObjectKeyFromObject(user), func() []metav1.Condition { return liveUser.Status.Conditions },
			conditionTypeKeystoneUserUserReady, metav1.ConditionFalse, reasonOrderReferencedByRoleAssignments,
			"a referenced user holds")
		g.Expect(meta.FindStatusCondition(liveUser.Status.Conditions, conditionTypeKeystoneUserUserReady).Message).
			To(ContainSubstring(`["pieces-member"]`))
		g.Expect(mgmtClient.Get(ctx, childKey(userPrefix+"user"), &orcv1alpha1.User{})).To(Succeed(),
			"the held user's K-ORC User stays")

		g.Expect(targetClient.Delete(ctx, assignment)).To(Succeed())
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, client.ObjectKeyFromObject(assignment),
				&c5c3v1alpha1.KeystoneRoleAssignment{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(assignmentRef.childPrefix()+"assignment"),
				&orcv1alpha1.RoleAssignment{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(assignmentRef.childPrefix()+"role"),
				&orcv1alpha1.Role{}))).To(BeTrue())
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "deleting the assignment removes its children")
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, client.ObjectKeyFromObject(user),
				&c5c3v1alpha1.KeystoneUser{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(userPrefix+"user"), &orcv1alpha1.User{}))).To(BeTrue())
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the released user is torn down")

		g.Expect(targetClient.Delete(ctx, project)).To(Succeed())
		g.Expect(targetClient.Delete(ctx, entry)).To(Succeed())
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, client.ObjectKeyFromObject(project),
				&c5c3v1alpha1.KeystoneProject{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, entryKey, &c5c3v1alpha1.KeystoneCatalogEntry{}))).To(BeTrue())
			for _, child := range []struct {
				name string
				obj  client.Object
			}{
				{projectRef.childPrefix() + "project", &orcv1alpha1.Project{}},
				{entryRef.childPrefix() + "service", &orcv1alpha1.Service{}},
				{entryRef.childPrefix() + "region", &orcv1alpha1.Region{}},
				{entryRef.childPrefix() + "endpoint-public", &orcv1alpha1.Endpoint{}},
			} {
				ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(child.name), child.obj))).To(BeTrue(), child.name)
			}
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "deleting the project and the entry removes their children")
	})

	t.Run("a RabbitMQVhost on the target cluster is delivered beside it", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The order plane of the KeystoneUser subtest, which assigns
		// mcAssignedNamespace on the target, gains a managed bus. envtest runs
		// no RabbitMQ Cluster Operator, so the broker is reported up by hand.
		orderCPKey := client.ObjectKey{Namespace: mcOrderNamespace, Name: mcOrderControlPlane}
		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, orderCPKey, live); err != nil {
				return err
			}
			live.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: mcOrderBus},
			}
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "declare the managed bus")
		busKey := client.ObjectKey{Namespace: mcOrderNamespace, Name: mcOrderBus}
		simulateRabbitmqClusterReadyWhenPresent(t, ctx, mgmtClient, busKey)
		g.Expect(mgmtClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: mcOrderBus + "-default-user", Namespace: mcOrderNamespace},
			Data: map[string][]byte{
				"username": []byte("default_user"), "password": []byte("admin-password"),
				"host": []byte(mcOrderBusHost), "port": []byte("5672"),
			},
		})).To(Succeed(), "create the default-user Secret")

		order := &c5c3v1alpha1.RabbitMQVhost{
			ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: mcAssignedNamespace},
			Spec: c5c3v1alpha1.RabbitMQVhostSpec{
				ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: mcOrderControlPlane, Namespace: mcOrderNamespace},
			},
		}
		g.Expect(targetClient.Create(ctx, order)).To(Succeed(), "create the order on the target cluster")
		orderKey := client.ObjectKeyFromObject(order)
		prefix := rabbitMQVhostChildPrefix(order, mcTargetCluster)
		vhost := rabbitMQVhostName(order, mcTargetCluster)
		wantLabels := rabbitMQVhostRef(order, mcTargetCluster).childLabels()
		childKey := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: mcOrderNamespace, Name: name} }

		// envtest runs no topology operator either: each topology child is
		// reported Ready by hand once the order has applied it.
		markReady := func(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
			t.Helper()
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(gvk)
			g.Eventually(func() error {
				if err := mgmtClient.Get(ctx, childKey(name), live); err != nil {
					return err
				}
				live.Object["status"] = map[string]any{
					"observedGeneration": live.GetGeneration(),
					"conditions": []any{map[string]any{
						"type": "Ready", "status": "True", "reason": "SuccessfulCreateOrUpdate",
						"lastTransitionTime": metav1.Now().Format(time.RFC3339),
					}},
				}
				return mgmtClient.Status().Update(ctx, live)
			}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "report %s %s Ready", gvk.Kind, name)
			return live
		}
		for _, child := range []struct {
			gvk  schema.GroupVersionKind
			name string
		}{
			{messaging.VhostGVK, prefix + "vhost"},
			{messaging.UserGVK, prefix + "user-v1"},
			{messaging.PermissionGVK, prefix + "permission-v1"},
		} {
			live := markReady(child.gvk, child.name)
			g.Expect(live.GetLabels()).To(Equal(wantLabels), child.name)
			g.Expect(live.GetOwnerReferences()).To(BeEmpty(), "a child on another cluster than its order carries labels only")
			targetObjs := &unstructured.UnstructuredList{}
			targetObjs.SetGroupVersionKind(child.gvk.GroupVersion().WithKind(child.gvk.Kind + "List"))
			g.Expect(targetClient.List(ctx, targetObjs)).To(Succeed())
			g.Expect(targetObjs.Items).To(BeEmpty(), "nothing the topology operator reads is written on the target")
		}

		// The plane publishes no broker yet, and the order is on another cluster
		// than the broker. The delivery stops before it writes anything.
		pushKey := childKey(prefix + "backup")
		secretKey := client.ObjectKey{Namespace: mcAssignedNamespace, Name: "workflow-credentials"}
		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.RabbitMQVhost{}
			ig.Expect(targetClient.Get(ctx, orderKey, live)).To(Succeed())
			ig.Expect(meta.IsStatusConditionTrue(live.Status.Conditions, conditionTypeRabbitMQVhostVhostReady)).To(BeTrue())
			cond := meta.FindStatusCondition(live.Status.Conditions, conditionTypeRabbitMQVhostDeliveryReady)
			ig.Expect(cond).NotTo(BeNil())
			ig.Expect(cond.Reason).To(Equal(reasonRabbitMQVhostMessagingNotPublished))
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "an unpublished bus refuses the delivery")
		mcExpectAbsent(t, ctx, targetClient, secretKey, &corev1.Secret{}, "delivered Secret")
		mcExpectAbsent(t, ctx, mgmtClient, pushKey, &esov1alpha1.PushSecret{}, "order PushSecret")

		g.Eventually(func() error {
			live := &c5c3v1alpha1.ControlPlane{}
			if err := mgmtClient.Get(ctx, orderCPKey, live); err != nil {
				return err
			}
			live.Spec.Infrastructure.PublishedMessagingEndpoint = mcOrderPublished
			return mgmtClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "publish the broker")
		// No ControlPlane watch reaches an order on a target cluster; an
		// annotation edit wakes it, as in the KeystoneUser subtest.
		g.Eventually(func() error {
			live := &c5c3v1alpha1.RabbitMQVhost{}
			if err := targetClient.Get(ctx, orderKey, live); err != nil {
				return err
			}
			live.Annotations = map[string]string{"test.c5c3.io/nudge": "published"}
			return targetClient.Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "nudge the order")

		// envtest runs no ESO: the backup is reported pushed by hand.
		g.Eventually(func() error {
			live := &esov1alpha1.PushSecret{}
			if err := mgmtClient.Get(ctx, pushKey, live); err != nil {
				return err
			}
			hash := live.Annotations[rabbitMQVhostPushContentHashAnnotation]
			if hash == "" {
				return errors.New("the PushSecret carries no content hash yet")
			}
			live.Status.SyncedResourceVersion = "pushed-" + hash
			live.Status.Conditions = []esov1alpha1.PushSecretStatusCondition{{
				Type: esov1alpha1.PushSecretReady, Status: corev1.ConditionTrue, Reason: "PushSecretSynced",
				LastTransitionTime: metav1.Now(),
			}}
			return mgmtClient.Status().Update(ctx, live)
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "report the stamped document pushed")
		push := &esov1alpha1.PushSecret{}
		g.Expect(mgmtClient.Get(ctx, pushKey, push)).To(Succeed())
		g.Expect(push.Labels).To(Equal(wantLabels))
		g.Expect(push.OwnerReferences).To(BeEmpty())
		g.Expect(push.Spec.Data[0].Match.RemoteRef.RemoteKey).To(Equal(
			"openstack/rabbitmq/" + mcOrderNamespace + "/" + vhost + "-vhost/credentials"))

		g.Eventually(func(ig Gomega) {
			live := &c5c3v1alpha1.RabbitMQVhost{}
			ig.Expect(targetClient.Get(ctx, orderKey, live)).To(Succeed())
			ig.Expect(meta.IsStatusConditionTrue(live.Status.Conditions, conditionTypeReady)).To(BeTrue())
			secret := &corev1.Secret{}
			ig.Expect(targetClient.Get(ctx, secretKey, secret)).To(Succeed())
			ig.Expect(metav1.IsControlledBy(secret, live)).To(BeTrue(), "the order owns its Secret on the target")
			ig.Expect(string(secret.Data["host"])).To(Equal("broker.example.test"))
			ig.Expect(string(secret.Data["port"])).To(Equal("5672"))
			ig.Expect(string(secret.Data["vhost"])).To(Equal(vhost))
			ig.Expect(string(secret.Data["username"])).To(Equal(vhost + "-v1"))
			ig.Expect(string(secret.Data["transport_url"])).To(HavePrefix("rabbit://" + vhost + "-v1:"))
			ig.Expect(string(secret.Data["transport_url"])).To(HaveSuffix("@" + mcOrderPublished + "/" + vhost))
			ig.Expect(secret.Data["password"]).NotTo(BeEmpty())
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(), "the credentials are delivered beside the order")
		mcExpectAbsent(t, ctx, mgmtClient, secretKey, &corev1.Secret{}, "delivered Secret on the management cluster")

		g.Expect(targetClient.Delete(ctx, order)).To(Succeed())
		g.Eventually(func(ig Gomega) {
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, orderKey, &c5c3v1alpha1.RabbitMQVhost{}))).To(BeTrue())
			for _, gvk := range []schema.GroupVersionKind{messaging.VhostGVK, messaging.UserGVK, messaging.PermissionGVK} {
				list := &unstructured.UnstructuredList{}
				list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
				ig.Expect(mgmtClient.List(ctx, list, client.InNamespace(mcOrderNamespace),
					client.MatchingLabels(wantLabels))).To(Succeed())
				ig.Expect(list.Items).To(BeEmpty(), gvk.Kind)
			}
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, pushKey, &esov1alpha1.PushSecret{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(mgmtClient.Get(ctx, childKey(prefix+"password-v1"), &corev1.Secret{}))).To(BeTrue())
			ig.Expect(apierrors.IsNotFound(targetClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
		}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
			"deleting the order removes the topology children, the backup and the Secret")
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
			{neutronBusKey, &corev1.Secret{}, "neutron messaging Secret"},
			{cinderBusKey, &corev1.Secret{}, "cinder messaging Secret"},
			{novaBusKey, &corev1.Secret{}, "nova messaging Secret"},
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
			for _, namespace := range []string{
				mcKeystoneNamespace, mcNetworkNamespace, mcBlockNamespace, mcComputeNamespace,
			} {
				g.Eventually(func() bool {
					ns := &corev1.Namespace{}
					if err := c.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
						return apierrors.IsNotFound(err)
					}
					return !ns.DeletionTimestamp.IsZero()
				}, itEventuallyTimeout, itPollInterval).Should(BeTrue(),
					"the Managed %s namespace must be deleted on the %s cluster", namespace, name)
			}
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

// mcMarkRegistrationConverged reports a projected KeystoneService child as fully
// registered, by hand: the account is provisioned and the aggregate is Ready.
//
// No KeystoneService controller runs in this suite (see the header), so a service
// whose own gates sit BEHIND its registration would otherwise never get far enough
// to be asserted: the compute service is gated on PlacementReady, which the
// placement service only reaches past its own registration. Every other subtest
// here deliberately parks on the un-provisioned account instead.
func mcMarkRegistrationConverged(t testing.TB, ctx context.Context, c client.Client, key client.ObjectKey) {
	t.Helper()
	g := NewGomegaWithT(t)

	ks := &c5c3v1alpha1.KeystoneService{}
	g.Eventually(func() error {
		return c.Get(ctx, key, ks)
	}, itEventuallyTimeout, itPollInterval).Should(Succeed(),
		"the KeystoneService child %s should be projected", key)

	for _, condType := range []string{conditionTypeKeystoneServiceAccountReady, conditionTypeReady} {
		meta.SetStatusCondition(&ks.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: ks.Generation,
			Reason:             "AllReady",
			Message:            "simulated converged",
		})
	}
	g.Expect(c.Status().Update(ctx, ks)).To(Succeed(), "report the registration as converged")
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
