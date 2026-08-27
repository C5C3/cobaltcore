// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration tests for the Neutron and NeutronMetadataAgent reconcilers running
// together against a live envtest API server.
//
// The timeouts below are generous on purpose. An envtest environment runs the
// API server and etcd and nothing else, so every state these pipelines wait on
// has to be produced by the test: the RabbitMQ Cluster Operator publishing its
// default user, the MariaDB operator turning the Database/User/Grant CRs ready,
// the Job controller completing the schema migration, and the Deployment and
// DaemonSet controllers counting ready pods. Between those writes the pipeline
// advances on its own wait intervals (RequeueDatabaseWait is 30s), so a budget
// sized to the happy path alone turns an ordinary requeue into a flake.
package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/neutron/internal/testutil"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// Test timeout constants for CI tuning.
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 30 * time.Second
	// eventuallyLongTimeout covers the states the pipeline reaches only after
	// waiting out one of its own requeue intervals, RequeueDatabaseWait above all.
	eventuallyLongTimeout = 2 * RequeueDatabaseWait
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// The object names the integration fixtures below share.
const (
	integrationNeutronName = "neutron"
	integrationAgentName   = "neutron-metadata"
	integrationCentralName = "ovn"
	integrationChassisName = "chassis"
	integrationIssuerName  = "openstack-ovn-ca"
	integrationMariaDBName = "mariadb"

	// #nosec G101 -- Secret object names, not credentials.
	integrationDBSecretName          = "neutron-db"
	integrationServiceUserSecretName = "neutron-service-user"
	integrationClientSecretName      = "ovn-client"
	integrationRabbitmqName          = "openstack-rabbitmq"
	integrationRabbitmqUserSecret    = "openstack-rabbitmq-default-user"

	// integrationBrokerPort is the port the default-user Secret publishes, and
	// therefore the port the NetworkPolicy has to open as messaging egress. It is
	// the TLS AMQP port rather than the 5672 default, so a rule built from the
	// fallback instead of from the transport URL is visible in the assertion.
	integrationBrokerPort = "5671"

	// integrationChassisLabel selects the nodes an OVNChassis configures, and is
	// what the metadata agent copies onto its own DaemonSet.
	integrationChassisLabel = "openstack.c5c3.io/it-chassis"
)

// The addresses the OVNCentral fixture publishes for its two databases, and the
// Northbound address the watch subtest re-publishes. The two databases get
// disjoint ranges so a config that mixed them up would be visible in the
// assertion rather than plausible.
const (
	integrationNorthboundAddress  = "ssl:10.96.0.11:6641"
	integrationSouthboundAddress  = "ssl:10.96.0.21:6642"
	integrationRepublishedAddress = "ssl:10.96.0.12:6641"
)

// --- Shared helpers ---

// registerNeutronControllers wires both reconcilers onto mgr through the
// production watch chain, with resolver as their target-cluster resolver.
//
// setupWithOptions is the chain SetupWithManager applies, so the legs, the field
// indexes and the Gateway API RESTMapper probe are the production ones rather
// than a hand-built copy that drifts the moment a leg is added. The only
// difference is SkipNameValidation: controller-runtime validates controller
// names against a process-global set, and this test binary starts one manager
// per test function. A skipped registration never claims the name, so nothing
// here can hide a duplicate registration in the operator binary.
//
// The Neutron reconciler is registered first, exactly as main.go does it: its
// setup is the single registration site for the Neutron field indexes.
//
// The health-check stub is what keeps the probe from firing slow HTTP GETs at a
// Service DNS name nothing answers; envtest runs no kubelet.
func registerNeutronControllers(mgr ctrl.Manager, mcMgr mcmanager.Manager, resolver commonmulticluster.ClusterResolver) error {
	opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
	opts.SkipNameValidation = ptr.To(true)

	neutron := &NeutronReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor("neutron-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		HTTPClient: &stubDoer{status: http.StatusOK},
		Resolver:   resolver,
	}
	if err := neutron.setupWithOptions(mcMgr, opts); err != nil {
		return err
	}

	agent := &NeutronMetadataAgentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("neutronmetadataagent-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		Resolver: resolver,
	}
	return agent.setupWithOptions(mcMgr, opts)
}

// registerNeutronWebhooks wires both webhook handlers onto mgr. The webhook
// manifests envtest installs carry both kinds (failurePolicy=Fail), so both
// handlers must be served or admission of the unserved kind fails.
//
// mgr.GetAPIReader() mirrors main.go: admission lookups read the API server
// directly, never a stale informer cache.
func registerNeutronWebhooks(mgr ctrl.Manager) error {
	if err := (&neutronv1alpha1.NeutronWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}
	return (&neutronv1alpha1.NeutronMetadataAgentWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
}

// setupEnvTestWithController wraps testutil.SetupNeutronEnvTestWithController
// with the v1alpha1 scheme and both registration callbacks. A nil provider
// engages no target cluster and a nil Resolver keeps every child on the
// management cluster, which is the single-cluster default path.
//
// gatewayAPIAvailable is not set by hand: the fake HTTPRoute CRD the helper
// installs is what the setup-time RESTMapper probe answers from, so the
// HTTPRoute watch and the gateway step run exactly as they do against a cluster
// that has Gateway API.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupNeutronEnvTestWithController(t,
		neutronv1alpha1.AddToScheme,
		registerNeutronWebhooks,
		func(mgr ctrl.Manager) error {
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			return registerNeutronControllers(mgr, mcMgr, nil)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test. It is called
// more than once per test where the fixtures span namespaces: the OVN control
// plane commonly lives in the privileged networking namespace while the Neutron
// API lives with the rest of the control plane.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "neutron-it-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	return ns.Name
}

// eventuallyExists polls c until the object at key exists, decoding it into obj
// so the caller can read what the operator applied.
func eventuallyExists(
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

// waitForNeutronCondition polls the Neutron CR until the named condition reaches
// the expected status. Returns the condition.
func waitForNeutronCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr neutronv1alpha1.Neutron
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"Neutron condition %s should reach %s", condType, expected)
	return cond
}

// waitForAgentCondition polls the NeutronMetadataAgent CR until the named
// condition reaches the expected status. Returns the condition.
func waitForAgentCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr neutronv1alpha1.NeutronMetadataAgent
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"NeutronMetadataAgent condition %s should reach %s", condType, expected)
	return cond
}

// integrationNeutronCR returns the Neutron this suite drives: managed database
// and managed messaging, a brownfield cache, and the OVNCentral named by
// centralName in centralNamespace, which may be another namespace than the CR's
// own.
func integrationNeutronCR(name, ns, centralName, centralNamespace string,
	targetRef *commonv1.TargetClusterRefSpec,
) *neutronv1alpha1.Neutron {
	return &neutronv1alpha1.Neutron{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: neutronv1alpha1.NeutronSpec{
			OpenStackRelease: "2026.1",
			Deployment:       neutronv1alpha1.DeploymentSpec{Replicas: 1},
			Workers:          neutronv1alpha1.WorkersSpec{Deployment: commonv1.DeploymentSpec{Replicas: 1}},
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/neutron", Tag: "2026.1"},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: integrationMariaDBName},
				Database:   "neutron",
				SecretRef:  commonv1.SecretRefSpec{Name: integrationDBSecretName},
			},
			Cache: commonv1.CacheSpec{
				Backend: commonv1.DefaultCacheBackend,
				Servers: []string{"mc:11211"},
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: neutronv1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: integrationServiceUserSecretName, Key: "password"},
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: integrationRabbitmqName},
			},
			OVN: neutronv1alpha1.OVNSpec{
				CentralRef: neutronv1alpha1.OVNCentralRef{Name: centralName, Namespace: centralNamespace},
			},
			// The NetworkPolicy is part of the fixture rather than an extra: the
			// messaging egress port is derived from the transport URL the messaging
			// step materialised, and there is nowhere else to observe it.
			NetworkPolicy: &neutronv1alpha1.NetworkPolicySpec{
				Ingress: []neutronv1alpha1.NetworkPolicyIngressSource{{
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
					},
				}},
			},
			TargetClusterRef: targetRef,
		},
	}
}

// integrationAgentCR returns the NeutronMetadataAgent this suite drives: the
// smallest CR the operator projects, naming neither a bus nor a Nova metadata
// API, beside the OVNChassis it shares its nodes with.
func integrationAgentCR(name, ns, chassisName string,
	targetRef *commonv1.TargetClusterRefSpec,
) *neutronv1alpha1.NeutronMetadataAgent {
	return &neutronv1alpha1.NeutronMetadataAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: neutronv1alpha1.NeutronMetadataAgentSpec{
			OpenStackRelease: "2026.1",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/neutron", Tag: "2026.1"},
			ChassisRef:       neutronv1alpha1.OVNChassisRef{Name: chassisName},
			TargetClusterRef: targetRef,
		},
	}
}

// integrationCentralCR returns the OVNCentral both kinds resolve their
// connection details from: only the required issuer name is set, so every other
// value comes from the CRD schema defaults.
func integrationCentralCR(name, ns string, targetRef *commonv1.TargetClusterRefSpec) *ovnv1alpha1.OVNCentral {
	return &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ovnv1alpha1.OVNCentralSpec{
			TLS:              ovnv1alpha1.OVNTLSSpec{IssuerRef: ovnv1alpha1.OVNIssuerRef{Name: integrationIssuerName}},
			TargetClusterRef: targetRef,
		},
	}
}

// integrationChassisCR returns the OVNChassis the metadata agent runs alongside,
// selecting the nodes labelled by this suite alone.
func integrationChassisCR(name, ns, centralName string, targetRef *commonv1.TargetClusterRefSpec) *ovnv1alpha1.OVNChassis {
	return &ovnv1alpha1.OVNChassis{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ovnv1alpha1.OVNChassisSpec{
			CentralRef:       ovnv1alpha1.OVNCentralRef{Name: centralName},
			NodeSelector:     map[string]string{integrationChassisLabel: "true"},
			TargetClusterRef: targetRef,
		},
	}
}

// publishCentral creates the OVNCentral and plays the part of the OVN operator
// and cert-manager: the two published database addresses and the name of the
// client Secret go into its status, and the Secret itself is written where the
// consumers read it, in the central's own namespace on the cluster its children
// live on. Neither the OVN operator nor cert-manager runs here, and without both
// halves every Neutron and every agent stops at its first OVN gate.
func publishCentral(t testing.TB, ctx context.Context, crClient, childClient client.Client,
	name, ns string, targetRef *commonv1.TargetClusterRefSpec,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	central := integrationCentralCR(name, ns, targetRef)
	g.Expect(crClient.Create(ctx, central)).To(Succeed(), "create the OVNCentral CR")

	republishCentral(t, ctx, crClient, name, ns, integrationNorthboundAddress)

	g.Expect(childClient.Create(ctx, ovnClientSecret(integrationClientSecretName, ns))).
		To(Succeed(), "write the client Secret cert-manager would write")
}

// republishCentral rewrites the OVNCentral's published status with nbAddress as
// its Northbound address. It is how the suite both seeds the status and, later,
// moves a database address under a settled Neutron.
func republishCentral(t testing.TB, ctx context.Context, crClient client.Client,
	name, ns, nbAddress string,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	central := &ovnv1alpha1.OVNCentral{}
	g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, central)).
		To(Succeed(), "read the OVNCentral before writing its status")

	central.Status.Northbound.InternalDbAddress = nbAddress
	central.Status.Southbound.InternalDbAddress = integrationSouthboundAddress
	central.Status.ClientSecretName = integrationClientSecretName
	g.Expect(crClient.Status().Update(ctx, central)).To(Succeed(), "publish the OVNCentral status")
}

// createNeutronPrerequisites materialises everything the Neutron pipeline reads
// but does not create: the secret store its credential gate checks, the two
// ESO-synced credential Secrets, and the MariaDB cluster its schema is
// provisioned in. They live on the cluster the children do, which is the target
// cluster for a placed CR.
func createNeutronPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	ensureReadyClusterSecretStore(t, ctx, c)

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationDBSecretName, Namespace: ns},
		Data:       map[string][]byte{"username": []byte("neutron"), "password": []byte("db-pw")},
	})).To(Succeed(), "create the database credentials Secret")

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationServiceUserSecretName, Namespace: ns},
		Data:       map[string][]byte{"password": []byte("svc-pw")},
	})).To(Succeed(), "create the service-user Secret")

	mariadbKey := client.ObjectKey{Namespace: ns, Name: integrationMariaDBName}
	g.Expect(c.Create(ctx, &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: mariadbKey.Name, Namespace: mariadbKey.Namespace},
	})).To(Succeed(), "create the MariaDB cluster CR")
	g.Expect(simulators.SimulateMariaDBReady(ctx, c, mariadbKey, 1)).
		To(Succeed(), "mark the MariaDB cluster ready")
}

// ensureReadyClusterSecretStore creates the cluster-scoped store the credential
// gate checks and marks it Ready. It tolerates one that already exists: the
// store is cluster-scoped, so the second namespace on the same cluster finds the
// object the first call created.
func ensureReadyClusterSecretStore(t testing.TB, ctx context.Context, c client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	err := c.Create(ctx, store)
	if apierrors.IsAlreadyExists(err) {
		return
	}
	g.Expect(err).NotTo(HaveOccurred(), "create the ClusterSecretStore")

	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "mark the ClusterSecretStore Ready")
}

// driveDatabase plays the MariaDB operator and the Job controller for one
// Neutron: the three provisioned CRs turn ready in the order the flow gates on
// them, then the schema migration completes. Each wait is the gate the pipeline
// actually has, so a helper that only wrote the simulated state would race the
// reconciler rather than drive it.
func driveDatabase(t testing.TB, ctx context.Context, childClient client.Client, name, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	key := client.ObjectKey{Namespace: ns, Name: name}

	eventuallyExists(t, ctx, childClient, key, &mariadbv1alpha1.Database{}, "MariaDB Database", eventuallyLongTimeout)
	g.Expect(simulators.SimulateDatabaseReady(ctx, childClient, key)).To(Succeed(), "mark the Database ready")

	eventuallyExists(t, ctx, childClient, key, &mariadbv1alpha1.User{}, "MariaDB User", eventuallyLongTimeout)
	g.Expect(simulators.SimulateUserReady(ctx, childClient, key)).To(Succeed(), "mark the User ready")

	eventuallyExists(t, ctx, childClient, key, &mariadbv1alpha1.Grant{}, "MariaDB Grant", eventuallyLongTimeout)
	g.Expect(simulators.SimulateGrantReady(ctx, childClient, key)).To(Succeed(), "mark the Grant ready")

	dbSyncKey := client.ObjectKey{Namespace: ns, Name: name + "-db-sync"}
	eventuallyExists(t, ctx, childClient, dbSyncKey, &batchv1.Job{}, "db-sync Job", eventuallyLongTimeout)
	g.Expect(simulators.SimulateJobComplete(ctx, childClient, dbSyncKey)).To(Succeed(), "complete the db-sync Job")
}

// mountedConfigMapName returns the name of the ConfigMap the workload's config
// volume mounts, which is the rendered neutron.conf / ml2_conf.ini carrier.
func mountedConfigMapName(spec *corev1.PodSpec) string {
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.Name == configVolumeName && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

// hasTCPEgressPort reports whether a NetworkPolicy opens the given TCP port for
// egress, whichever rule carries it. It is order-independent on purpose: what
// the messaging assertion pins is that the port of the transport URL is open
// over TCP, not which rule of the auto-derived set opened it.
func hasTCPEgressPort(policy *networkingv1.NetworkPolicy, want int) bool {
	for _, rule := range policy.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil {
				continue
			}
			if port.Port.IntValue() == want {
				return true
			}
		}
	}
	return false
}

// --- Tests ---

// TestIntegrationNeutron_ReachesReady walks the whole Neutron pipeline against a
// live API server: the messaging gate, the OVN endpoints resolved from a central
// in another namespace, the rendered config, the schema migration, the API and
// worker Deployments, and the post-deployment parallel group. It then moves one
// of the published OVN addresses under the settled CR, which nothing but the
// OVNCentral watch can carry into a re-rendered config.
func TestIntegrationNeutron_ReachesReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)
	// The OVN control plane lives in a namespace of its own, so this Neutron
	// resolves it across a namespace boundary, which is what the cross-namespace
	// ref exists for and what the watch has to map back.
	ovnNS := createTestNamespace(t, ctx, c)

	createNeutronPrerequisites(t, ctx, c, ns)
	publishCentral(t, ctx, c, c, integrationCentralName, ovnNS, nil)

	// The broker CR exists with no status at all, which is what the RabbitMQ
	// Cluster Operator leaves behind until the cluster's default user is created.
	g.Expect(c.Create(ctx, rabbitmqCluster(integrationRabbitmqName, ns))).
		To(Succeed(), "create the RabbitmqCluster")

	neutron := integrationNeutronCR(integrationNeutronName, ns, integrationCentralName, ovnNS, nil)
	g.Expect(c.Create(ctx, neutron)).To(Succeed(), "create the Neutron CR")
	neutronKey := types.NamespacedName{Name: integrationNeutronName, Namespace: ns}

	// The messaging gate. Every upstream credential is in place, so the only
	// thing missing is the default user the broker has not published, and the
	// pipeline reports exactly that.
	waiting := waitForNeutronCondition(t, ctx, c, neutronKey, "SecretsReady",
		metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(waiting.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))
	g.Expect(apierrors.IsNotFound(c.Get(ctx,
		client.ObjectKey{Namespace: ns, Name: messaging.TransportURLSecretName(integrationNeutronName)},
		&corev1.Secret{}))).To(BeTrue(), "no derived Secret may be written from partial credentials")

	// The broker publishes its default user, credentials and endpoint in one
	// Secret.
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationRabbitmqUserSecret, Namespace: ns},
		Data: map[string][]byte{
			"username": []byte("default_user_abc"),
			"password": []byte("s3cr3t"),
			"host":     []byte(integrationRabbitmqName + "." + ns + ".svc"),
			"port":     []byte(integrationBrokerPort),
		},
	})).To(Succeed(), "create the default-user Secret")
	g.Expect(simulators.SimulateRabbitmqClusterReady(ctx, c,
		client.ObjectKey{Namespace: ns, Name: integrationRabbitmqName}, integrationRabbitmqUserSecret)).
		To(Succeed(), "publish the RabbitmqCluster's default user")

	waitForNeutronCondition(t, ctx, c, neutronKey, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	waitForNeutronCondition(t, ctx, c, neutronKey, conditionTypeOVNEndpointsReady,
		metav1.ConditionTrue, eventuallyTimeout)

	// The derived transport-URL Secret and the mirrored OVN client identity: the
	// two Secrets this CR assembles rather than reads.
	transportSecret := &corev1.Secret{}
	eventuallyExists(t, ctx, c, client.ObjectKey{
		Namespace: ns, Name: messaging.TransportURLSecretName(integrationNeutronName),
	}, transportSecret, "derived transport-URL Secret", eventuallyTimeout)
	g.Expect(string(transportSecret.Data[commonv1.DefaultTransportURLSecretKey])).
		To(ContainSubstring(":"+integrationBrokerPort+"/"),
			"the derived URL should carry the port the default-user Secret published")

	mirror := &corev1.Secret{}
	eventuallyExists(t, ctx, c, client.ObjectKey{Namespace: ns, Name: integrationNeutronName + ovnClientSecretSuffix},
		mirror, "mirrored OVN client Secret", eventuallyTimeout)
	for _, key := range ovnClientSecretKeys {
		g.Expect(mirror.Data).To(HaveKey(key), "the mirror should carry the central's %s", key)
	}

	// The schema, then the three Deployments. envtest runs no Job controller and
	// no Deployment controller, so each rollout is completed here.
	driveDatabase(t, ctx, c, integrationNeutronName, ns)
	waitForNeutronCondition(t, ctx, c, neutronKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)

	apiKey := client.ObjectKey{Namespace: ns, Name: integrationNeutronName}
	deploy := &appsv1.Deployment{}
	eventuallyExists(t, ctx, c, apiKey, deploy, "API Deployment", eventuallyLongTimeout)
	g.Expect(c.Get(ctx, apiKey, &corev1.Service{})).To(Succeed(), "the API Service should exist")
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, apiKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "mark the API Deployment available")

	// The two worker Deployments are projected only once the API step reports
	// available, so they are driven after it rather than beside it.
	for _, component := range []string{componentPeriodicWorkers, componentOVNMaintenanceWorker} {
		workerKey := client.ObjectKey{Namespace: ns, Name: integrationNeutronName + "-" + component}
		worker := &appsv1.Deployment{}
		eventuallyExists(t, ctx, c, workerKey, worker, component+" Deployment", eventuallyLongTimeout)
		g.Expect(simulators.SimulateDeploymentReady(ctx, c, workerKey, ptr.Deref(worker.Spec.Replicas, 1))).
			To(Succeed(), "mark the %s Deployment available", component)
	}

	waitForNeutronCondition(t, ctx, c, neutronKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	var ready neutronv1alpha1.Neutron
	g.Expect(c.Get(ctx, neutronKey, &ready)).To(Succeed())
	g.Expect(ready.Status.InstalledRelease).To(Equal("2026.1"),
		"installedRelease should be promoted after the db-sync")
	g.Expect(ready.Status.Endpoint).NotTo(BeEmpty(),
		"the endpoint should be advertised once the workload is available")

	// The rendered ML2 configuration names both databases at the addresses the
	// central published, which is the whole point of the two OVN steps.
	g.Expect(c.Get(ctx, apiKey, deploy)).To(Succeed())
	configMapName := mountedConfigMapName(&deploy.Spec.Template.Spec)
	g.Expect(configMapName).To(HavePrefix(integrationNeutronName+"-config-"),
		"the config volume must mount the content-hashed ConfigMap")

	configMap := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: configMapName}, configMap)).
		To(Succeed(), "the rendered config ConfigMap should exist")
	g.Expect(configMap.Data[ml2ConfDataKey]).To(ContainSubstring(integrationNorthboundAddress))
	g.Expect(configMap.Data[ml2ConfDataKey]).To(ContainSubstring(integrationSouthboundAddress))

	// The NetworkPolicy opens the broker port the transport URL names. Nothing
	// else in the CR carries it: the port arrives with the default-user Secret and
	// reaches the policy through the messaging step.
	policy := &networkingv1.NetworkPolicy{}
	g.Expect(c.Get(ctx, apiKey, policy)).To(Succeed(), "the NetworkPolicy should exist")
	g.Expect(hasTCPEgressPort(policy, 5671)).To(BeTrue(),
		"the messaging egress rule should open the transport URL's port over TCP")

	// THE ACCEPTANCE RULE OF WHAT FOLLOWS: the Neutron CR is never written again.
	// The only write is a status field on an OVNCentral in another namespace, and
	// the operator has to get from there to a re-rendered config on its own. Take
	// the OVNCentral watch away and this sits out the budget below and fails.
	republishCentral(t, ctx, c, integrationCentralName, ovnNS, integrationRepublishedAddress)

	// The re-render reaches the schema migration first: the db-sync Job mounts the
	// same config as the pods, so its pod template changes with it and the shared
	// flow re-runs the Job before the API pods are pointed at the new file.
	dbSyncKey := client.ObjectKey{Namespace: ns, Name: integrationNeutronName + "-db-sync"}
	var rerenderedName string
	g.Eventually(func(ig Gomega) {
		job := &batchv1.Job{}
		ig.Expect(c.Get(ctx, dbSyncKey, job)).To(Succeed())
		rerenderedName = mountedConfigMapName(&job.Spec.Template.Spec)
		ig.Expect(rerenderedName).NotTo(Equal(configMapName),
			"a moved database address must render a new immutable ConfigMap")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(),
		"the new Northbound address should reach a re-rendered config through the OVNCentral watch")

	rendered := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: rerenderedName}, rendered)).
		To(Succeed(), "the re-rendered config ConfigMap should exist")
	g.Expect(rendered.Data[ml2ConfDataKey]).To(ContainSubstring(integrationRepublishedAddress),
		"the re-rendered ML2 configuration should name the new Northbound address")

	// envtest completes no Job on its own, and the API pods are rolled onto the
	// new file only once the migration against it has run.
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "complete the re-run db-sync Job")

	g.Eventually(func() string {
		rolled := &appsv1.Deployment{}
		if err := c.Get(ctx, apiKey, rolled); err != nil {
			return ""
		}
		return mountedConfigMapName(&rolled.Spec.Template.Spec)
	}, eventuallyLongTimeout, pollInterval).Should(Equal(rerenderedName),
		"the API pods should be rolled onto the re-rendered config")
}

// TestIntegrationNeutronMetadataAgent_ReachesReady walks the metadata agent's
// pipeline against a live API server: the chassis it shares its nodes with, the
// central that chassis attaches to, the rendered agent config, and the DaemonSet
// whose node counters the CR mirrors into its own status.
func TestIntegrationNeutronMetadataAgent_ReachesReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	// One namespace for all three CRs: spec.chassisRef is namespace-local,
	// because the agent pods mount the chassis's runtime directory, and the
	// chassis's central is resolved beside them.
	ns := createTestNamespace(t, ctx, c)

	publishCentral(t, ctx, c, c, integrationCentralName, ns, nil)

	chassis := integrationChassisCR(integrationChassisName, ns, integrationCentralName, nil)
	g.Expect(c.Create(ctx, chassis)).To(Succeed(), "create the OVNChassis CR")

	agent := integrationAgentCR(integrationAgentName, ns, integrationChassisName, nil)
	g.Expect(c.Create(ctx, agent)).To(Succeed(), "create the NeutronMetadataAgent CR")
	agentKey := types.NamespacedName{Name: integrationAgentName, Namespace: ns}

	resolved := waitForAgentCondition(t, ctx, c, agentKey, conditionTypeChassisReady,
		metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(resolved.Reason).To(Equal(conditionReasonChassisResolved))

	// The DaemonSet lands on the chassis's nodes, selected by the chassis's own
	// labels: the agent answers the metadata requests of the instances on exactly
	// the nodes the chassis programs.
	dsKey := client.ObjectKey{Namespace: ns, Name: integrationAgentName + "-" + metadataAgentComponent}
	ds := &appsv1.DaemonSet{}
	eventuallyExists(t, ctx, c, dsKey, ds, "metadata-agent DaemonSet", eventuallyTimeout)
	g.Expect(ds.Spec.Template.Spec.NodeSelector).To(Equal(chassis.Spec.NodeSelector),
		"the agent has to run where the chassis runs")
	g.Expect(mountedConfigMapName(&ds.Spec.Template.Spec)).To(HavePrefix(integrationAgentName+"-config-"),
		"the config volume must mount the content-hashed ConfigMap")

	// envtest runs no DaemonSet controller, so the node counters the gate reads
	// are written here.
	progressing := waitForAgentCondition(t, ctx, c, agentKey, conditionTypeDaemonSetReady,
		metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(progressing.Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(simulators.MarkDaemonSetReady(ctx, c, dsKey)).To(Succeed(), "mark the DaemonSet ready")

	waitForAgentCondition(t, ctx, c, agentKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	var ready neutronv1alpha1.NeutronMetadataAgent
	g.Expect(c.Get(ctx, agentKey, &ready)).To(Succeed())
	g.Expect(ready.Status.DesiredNumberScheduled).To(Equal(int32(1)))
	g.Expect(ready.Status.NumberReady).To(Equal(int32(1)),
		"status should mirror the DaemonSet's node counters")
	g.Expect(ready.Status.InstalledImage).To(Equal("ghcr.io/c5c3/neutron:2026.1"))
}
