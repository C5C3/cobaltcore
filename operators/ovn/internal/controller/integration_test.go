// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration tests for the OVNCentral and OVNChassis reconcilers running
// together against a live envtest API server.
//
// The timeouts below are generous on purpose. An envtest environment runs the
// API server and etcd and nothing else, so every state this operator waits on
// has to be produced by the test: cert-manager issuing the Certificates, the
// StatefulSet controller counting ready members, the kubelet putting a node
// address on a pod, the Deployment controller reporting availability, and the
// Job controller completing a run. Between those writes the pipeline advances on
// its own wait intervals (RequeueRaftWait is 15s), so a budget sized to the
// happy path alone turns an ordinary requeue into a flake.
package controller

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strings"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/ovn/internal/testutil"
)

// Test timeout constants for CI tuning.
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 60 * time.Second
	// eventuallyLongTimeout covers the states the pipeline reaches only after
	// waiting out one of its own requeue intervals.
	eventuallyLongTimeout = 3 * time.Minute
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// The object names and labels the integration fixtures below share.
const (
	integrationCentralName = "ovn"
	integrationChassisName = "chassis"
	integrationIssuerName  = "openstack-ovn-ca"

	// integrationChassisLabel selects the nodes an OVNChassis configures. It is
	// specific to this suite so a node it labels cannot be picked up by another
	// CR.
	integrationChassisLabel = "openstack.c5c3.io/it-chassis"
	integrationGatewayLabel = "openstack.c5c3.io/it-gateway"
)

// --- Shared helpers ---

// registerOVNControllers wires both reconcilers onto mgr through the production
// watch chain, with resolver as their target-cluster resolver.
//
// setupWithOptions is the chain SetupWithManager applies, so the legs, the field
// index and the cert-manager RESTMapper probe are the production ones rather
// than a hand-built copy that drifts the moment a leg is added. The only
// difference is SkipNameValidation: controller-runtime validates controller
// names against a process-global set, and this test binary starts one manager
// per test function. A skipped registration never claims the name, so nothing
// here can hide a duplicate registration in the operator binary.
//
// The OVNCentral reconciler is registered first, exactly as main.go does it: its
// setup is the single registration site for the OVNChassis field index the
// chassis controller's central-to-chassis mapper lists through.
func registerOVNControllers(mgr ctrl.Manager, mcMgr mcmanager.Manager, resolver commonmulticluster.ClusterResolver) error {
	opts := bootstrap.TypedControllerOptions[mcreconcile.Request](1)
	opts.SkipNameValidation = ptr.To(true)

	central := &OVNCentralReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("ovncentral-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		Resolver: resolver,
	}
	if err := central.setupWithOptions(mcMgr, opts); err != nil {
		return err
	}

	chassis := &OVNChassisReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("ovnchassis-controller"), //nolint:staticcheck // SA1019: reconciler consumes record.EventRecorder (old events API); GetEventRecorder returns the incompatible events/v1 type.
		Resolver: resolver,
	}
	return chassis.setupWithOptions(mcMgr, opts)
}

// registerOVNWebhooks wires both webhook handlers onto mgr. The webhook
// manifests envtest installs carry both kinds (failurePolicy=Fail), so both
// handlers must be served or admission of the unserved kind fails.
//
// mgr.GetAPIReader() mirrors main.go: admission lookups read the API server
// directly, never a stale informer cache.
func registerOVNWebhooks(mgr ctrl.Manager) error {
	if err := (&ovnv1alpha1.OVNCentralWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
		return err
	}
	return (&ovnv1alpha1.OVNChassisWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
}

// setupEnvTestWithController wraps testutil.SetupOVNEnvTestWithController with
// the v1alpha1 scheme and both registration callbacks. A nil provider engages no
// target cluster and a nil Resolver keeps every child on the management
// cluster, which is the single-cluster default path.
//
// certManagerAvailable is not set by hand: the fake cert-manager CRD the helper
// installs is what the setup-time RESTMapper probe answers from, so the
// Certificate watch and the TLS step run exactly as they do against a cluster
// that has cert-manager.
func setupEnvTestWithController(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupOVNEnvTestWithController(t,
		ovnv1alpha1.AddToScheme,
		registerOVNWebhooks,
		func(mgr ctrl.Manager) error {
			mcMgr, err := mcmanager.WithMultiCluster(mgr, nil)
			if err != nil {
				return err
			}
			return registerOVNControllers(mgr, mcMgr, nil)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-it-"}}
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

// waitForCentralCondition polls the OVNCentral CR until the named condition
// reaches the expected status. Returns the condition.
func waitForCentralCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr ovnv1alpha1.OVNCentral
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"OVNCentral condition %s should reach %s", condType, expected)
	return cond
}

// waitForChassisCondition polls the OVNChassis CR until the named condition
// reaches the expected status. Returns the condition.
func waitForChassisCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName,
	condType string, expected metav1.ConditionStatus, timeout time.Duration,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var cr ovnv1alpha1.OVNChassis
		if err := c.Get(ctx, key, &cr); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(cr.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		"OVNChassis condition %s should reach %s", condType, expected)
	return cond
}

// integrationCentralCR returns the OVNCentral this suite drives: only the
// required issuer name is set, so every other value comes from the CRD schema
// defaults or from the operator's own reconcile-time resolution.
func integrationCentralCR(name, ns string, targetRef *commonv1.TargetClusterRefSpec) *ovnv1alpha1.OVNCentral {
	return &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ovnv1alpha1.OVNCentralSpec{
			TLS: ovnv1alpha1.OVNTLSSpec{IssuerRef: ovnv1alpha1.OVNIssuerRef{Name: integrationIssuerName}},
			// Both databases are published on node ports, which is not the
			// default: this suite asserts the node-facing half of the endpoint
			// step, and there is nothing to assert on a cluster-internal one.
			Northbound:       ovnv1alpha1.OVNDatabaseSpec{ExternallyReachable: true},
			Southbound:       ovnv1alpha1.OVNDatabaseSpec{ExternallyReachable: true},
			TargetClusterRef: targetRef,
		},
	}
}

// integrationChassisCR returns the OVNChassis this suite drives, selecting the
// nodes labelled by this suite alone.
func integrationChassisCR(name, ns, centralRef string, targetRef *commonv1.TargetClusterRefSpec) *ovnv1alpha1.OVNChassis {
	return &ovnv1alpha1.OVNChassis{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ovnv1alpha1.OVNChassisSpec{
			CentralRef:       ovnv1alpha1.OVNCentralRef{Name: centralRef},
			NodeSelector:     map[string]string{integrationChassisLabel: "true"},
			Gateway:          &ovnv1alpha1.OVNGatewaySpec{NodeSelector: map[string]string{integrationGatewayLabel: "true"}},
			TargetClusterRef: targetRef,
		},
	}
}

// centralCertificateNames lists the three Certificates an OVNCentral requests:
// one server keypair per database and the client keypair everything that dials
// them shares.
func centralCertificateNames(name string) []string {
	return []string{name + "-nb-server", name + "-sb-server", name + "-client"}
}

// raftHostIP is the node address the test puts on one member's pod. The two
// databases get disjoint ranges so a connection string that mixed them up would
// be visible in the assertion rather than plausible.
func raftHostIP(db raftDB, ordinal int32) string {
	octet := 10 + ordinal
	if db.suffix == suffixSouthbound {
		octet = 20 + ordinal
	}
	return fmt.Sprintf("192.168.1.%d", octet)
}

// expectedExternalAddress is the connection string the endpoint step has to
// publish for clients outside the cluster: each member's node address and its
// own node port, in ordinal order.
func expectedExternalAddress(db raftDB) string {
	members := make([]string, 0, db.spec.Replicas)
	for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
		members = append(members, fmt.Sprintf("ssl:%s:%d", raftHostIP(db, ordinal), db.base+ordinal))
	}
	return strings.Join(members, ",")
}

// expectIPLiteralAddresses asserts every member of a connection string is an
// "ssl:<host>:<port>" triple whose host parses as an IP address. ovsdb-server
// resolves a remote once at startup and never again, so a DNS name here would
// leave every client wedged against the address it resolved to first.
func expectIPLiteralAddresses(t testing.TB, address, what string) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Expect(address).NotTo(BeEmpty(), "%s should be published", what)
	for _, member := range strings.Split(address, ",") {
		g.Expect(member).To(HavePrefix("ssl:"), "%s member %q should be an ssl remote", what, member)
		host, port, ok := strings.Cut(strings.TrimPrefix(member, "ssl:"), ":")
		g.Expect(ok).To(BeTrue(), "%s member %q should be ssl:<host>:<port>", what, member)
		g.Expect(net.ParseIP(host)).NotTo(BeNil(),
			"%s member %q must carry an IP literal, not a DNS name", what, member)
		g.Expect(port).NotTo(BeEmpty(), "%s member %q should name a port", what, member)
	}
}

// createRaftMemberPod plays the part of the StatefulSet controller and the
// kubelet for one Raft member: the pod the per-member Service selects, carrying
// the node address the endpoint step reads off it. envtest schedules nothing and
// runs no node agent, so both have to be written here.
func createRaftMemberPod(t testing.TB, ctx context.Context, c client.Client,
	cr *ovnv1alpha1.OVNCentral, db raftDB, sts *appsv1.StatefulSet, ordinal int32,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	name := raftMemberName(cr, db, ordinal)
	labels := map[string]string{}
	maps.Copy(labels, sts.Spec.Template.Labels)
	// The per-member Service selects on this label alone, and the StatefulSet
	// controller is what stamps it on every pod it creates.
	labels[appsv1.StatefulSetPodNameLabel] = name

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "ovsdb", Image: "ovn"}},
		},
	}
	g.Expect(c.Create(ctx, pod)).To(Succeed(), "create Raft member pod %s", name)

	pod.Status.HostIP = raftHostIP(db, ordinal)
	g.Expect(c.Status().Update(ctx, pod)).To(Succeed(), "put a node address on Raft member pod %s", name)
}

// driveCentralToReady creates the OVNCentral named name in ns on crClient and
// plays every part of the environment envtest does not run until the CR reports
// Ready: cert-manager issuing the three Certificates and writing the client
// Secret, the StatefulSet controller counting ready Raft members, the kubelet
// giving each member's pod a node address, and the Deployment controller
// reporting northd available. Its children are read and written on childClient,
// which is the same client on a single cluster and the target cluster's on a
// placed CR.
//
// The waits between those writes are the gates the pipeline actually has: each
// one is what lets the next step run at all, so a helper that only wrote the
// simulated state would race the reconciler rather than drive it.
func driveCentralToReady(t testing.TB, ctx context.Context, crClient, childClient client.Client,
	ns, name string, targetRef *commonv1.TargetClusterRefSpec,
) {
	t.Helper()
	g := NewGomegaWithT(t)

	cr := integrationCentralCR(name, ns, targetRef)
	g.Expect(crClient.Create(ctx, cr)).To(Succeed(), "create the OVNCentral CR")
	crKey := types.NamespacedName{Name: name, Namespace: ns}

	// The TLS gate: all three Certificates are requested on the first pass, and
	// the step reports the wait for the first of them cert-manager has not
	// issued.
	pending := waitForCentralCondition(t, ctx, crClient, crKey, conditionTypeTLSReady,
		metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(pending.Reason).To(Equal(conditionReasonCertificatePending))

	for _, certName := range centralCertificateNames(name) {
		key := client.ObjectKey{Namespace: ns, Name: certName}
		eventuallyExists(t, ctx, childClient, key, &certmanagerv1.Certificate{}, "Certificate", eventuallyTimeout)
		g.Expect(simulators.SimulateCertificateReady(ctx, childClient, key)).
			To(Succeed(), "issue Certificate %s", certName)
	}
	// The step ends at the issued client Secret rather than at the Certificates,
	// because the Secret is what the workloads mount and what an OVNChassis is
	// pointed at. cert-manager writes it in production; here the test does.
	g.Expect(childClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clientSecretName(cr), Namespace: ns},
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("client-certificate"),
			corev1.TLSPrivateKeyKey: []byte("client-key"),
			cmmeta.TLSCAKey:         []byte("issuing-ca"),
		},
	})).To(Succeed(), "write the client Secret cert-manager would write")
	waitForCentralCondition(t, ctx, crClient, crKey, conditionTypeTLSReady, metav1.ConditionTrue, eventuallyTimeout)

	// The two Raft clusters. envtest runs no StatefulSet controller, so the
	// member counters both database gates read are set here.
	nb, sb := northboundDB(cr), southboundDB(cr)
	for _, db := range []raftDB{nb, sb} {
		key := client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}
		sts := &appsv1.StatefulSet{}
		eventuallyExists(t, ctx, childClient, key, sts, db.suffix+" StatefulSet", eventuallyTimeout)
		g.Expect(simulators.MarkStatefulSetReady(ctx, childClient, key)).
			To(Succeed(), "mark the %s Raft members ready", db.suffix)
	}
	waitForCentralCondition(t, ctx, crClient, crKey, conditionTypeNorthboundReady, metav1.ConditionTrue, eventuallyLongTimeout)
	waitForCentralCondition(t, ctx, crClient, crKey, conditionTypeSouthboundReady, metav1.ConditionTrue, eventuallyLongTimeout)

	// The member pods. The endpoint step publishes the node address it finds on
	// each of them, and envtest has no kubelet to put one there.
	for _, db := range []raftDB{nb, sb} {
		sts := &appsv1.StatefulSet{}
		g.Expect(childClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}, sts)).
			To(Succeed(), "read the %s StatefulSet for its pod labels", db.suffix)
		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			createRaftMemberPod(t, ctx, childClient, cr, db, sts, ordinal)
		}
	}
	waitForCentralCondition(t, ctx, crClient, crKey, conditionTypeEndpointsReady, metav1.ConditionTrue, eventuallyLongTimeout)

	// northd, the one member of the post-endpoint parallel group with a workload
	// whose rollout envtest cannot complete on its own.
	northdKey := client.ObjectKey{Namespace: ns, Name: northdName(cr)}
	deploy := &appsv1.Deployment{}
	eventuallyExists(t, ctx, childClient, northdKey, deploy, "northd Deployment", eventuallyLongTimeout)
	g.Expect(simulators.SimulateDeploymentReady(ctx, childClient, northdKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "mark the northd Deployment available")

	waitForCentralCondition(t, ctx, crClient, crKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)
}

// --- Tests ---

// TestIntegrationOVNCentral_ProjectsChildrenAndReachesReady walks the whole
// OVNCentral pipeline against a live API server and pins what it projects: the
// three Certificates, the two Raft clusters with their headless and per-member
// Services, the shared scripts, the addresses the endpoint step publishes,
// northd, and the backup volume and schedule.
//
// The addresses are the part of this that no unit test can cover. They are
// assembled from cluster IPs the API server assigns and from node addresses the
// test writes onto the member pods, so a step that published a DNS name, or the
// members out of ordinal order, only shows up here.
func TestIntegrationOVNCentral_ProjectsChildrenAndReachesReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)

	driveCentralToReady(t, ctx, c, c, ns, integrationCentralName, nil)

	crKey := types.NamespacedName{Name: integrationCentralName, Namespace: ns}
	cr := &ovnv1alpha1.OVNCentral{}
	g.Expect(c.Get(ctx, crKey, cr)).To(Succeed())
	nb, sb := northboundDB(cr), southboundDB(cr)

	// Every certificate is requested from the issuer the spec names, into a
	// Secret named after the Certificate itself.
	for _, certName := range centralCertificateNames(integrationCentralName) {
		cert := &certmanagerv1.Certificate{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: certName}, cert)).
			To(Succeed(), "Certificate %s should exist", certName)
		g.Expect(cert.Spec.SecretName).To(Equal(certName))
		g.Expect(cert.Spec.IssuerRef.Name).To(Equal(integrationIssuerName))
		g.Expect(cert.Spec.IssuerRef.Kind).To(Equal("ClusterIssuer"))
	}
	g.Expect(cr.Status.ClientSecretName).To(Equal(clientSecretName(cr)),
		"the client Secret an OVNChassis mounts should be published")

	// Two headless Services, one per database, plus one NodePort Service per
	// Raft member. Nothing else: this CR asks for no relay.
	services := &corev1.ServiceList{}
	g.Expect(c.List(ctx, services, client.InNamespace(ns))).To(Succeed())
	g.Expect(services.Items).To(HaveLen(8),
		"expected 2 headless Services and 6 per-member NodePort Services")

	for _, db := range []raftDB{nb, sb} {
		headless := &corev1.Service{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}, headless)).
			To(Succeed(), "%s headless Service should exist", db.suffix)
		g.Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		g.Expect(headless.Spec.PublishNotReadyAddresses).To(BeTrue(),
			"a member joins the cluster through peers it can resolve before it is ready")

		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			member := &corev1.Service{}
			name := raftMemberName(cr, db, ordinal)
			g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, member)).
				To(Succeed(), "per-member Service %s should exist", name)
			g.Expect(member.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
			g.Expect(member.Spec.ClusterIP).NotTo(BeEmpty(), "the API server should assign %s a cluster IP", name)
			g.Expect(member.Spec.ClusterIP).NotTo(Equal(corev1.ClusterIPNone))
			g.Expect(member.Spec.Ports).To(HaveLen(1))
			g.Expect(member.Spec.Ports[0].NodePort).To(Equal(db.base+ordinal),
				"member %d of %s should be published on its own node port", ordinal, db.suffix)
		}
	}

	// One StatefulSet per database, and one scripts ConfigMap shared by both:
	// two run scripts, two set-connection scripts, and the backup script the
	// CronJob mounts from the same object.
	for _, db := range []raftDB{nb, sb} {
		sts := &appsv1.StatefulSet{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}, sts)).
			To(Succeed(), "%s StatefulSet should exist", db.suffix)
		g.Expect(ptr.Deref(sts.Spec.Replicas, 0)).To(Equal(db.spec.Replicas))
	}
	scripts := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: centralScriptsName(cr)}, scripts)).
		To(Succeed(), "the central scripts ConfigMap should exist")
	g.Expect(scripts.Data).To(HaveLen(5))

	// The published addresses, member by member and in ordinal order. The
	// internal ones are built from the cluster IPs the API server assigned, so
	// the expectation cannot be satisfied by a string the operator invented.
	for _, db := range []raftDB{nb, sb} {
		status := databaseStatus(cr, db)
		internal := make([]string, 0, db.spec.Replicas)
		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			member := &corev1.Service{}
			g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: raftMemberName(cr, db, ordinal)}, member)).To(Succeed())
			internal = append(internal, fmt.Sprintf("ssl:%s:%d", member.Spec.ClusterIP, db.clientPort))
		}
		g.Expect(status.InternalDbAddress).To(Equal(strings.Join(internal, ",")),
			"%s internalDbAddress should list every member's cluster IP in ordinal order", db.suffix)
		g.Expect(status.DbAddress).To(Equal(expectedExternalAddress(db)),
			"%s dbAddress should list every member's node address and node port in ordinal order", db.suffix)
		g.Expect(status.ReadyReplicas).To(Equal(db.spec.Replicas))

		expectIPLiteralAddresses(t, status.InternalDbAddress, db.suffix+" internalDbAddress")
		expectIPLiteralAddresses(t, status.DbAddress, db.suffix+" dbAddress")
	}
	g.Expect(cr.Status.Northbound.DbAddress).
		To(Equal("ssl:192.168.1.10:30641,ssl:192.168.1.11:30642,ssl:192.168.1.12:30643"))

	// The recurring backup: the volume the snapshots go on and the schedule that
	// writes them.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: backupVolumeName(cr)}, &corev1.PersistentVolumeClaim{})).
		To(Succeed(), "the backup PersistentVolumeClaim should exist")
	cronJob := &batchv1.CronJob{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: backupCronJobName(cr.Name)}, cronJob)).
		To(Succeed(), "the backup CronJob should exist")
	g.Expect(cronJob.Spec.Schedule).To(Equal(ovnv1alpha1.DefaultBackupSchedule))
	g.Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
	g.Expect(ptr.Deref(cronJob.Spec.Suspend, true)).To(BeFalse(), "the backup should not be suspended")

	// The two members of the parallel group that create nothing here still have
	// to report: the aggregate Ready is True only when every sub-condition is.
	relay := meta.FindStatusCondition(cr.Status.Conditions, conditionTypeRelayReady)
	g.Expect(relay).NotTo(BeNil())
	g.Expect(relay.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(relay.Reason).To(Equal(conditionReasonRelayNotRequired))
	g.Expect(cr.Status.RelayAddress).To(BeEmpty())

	backup := meta.FindStatusCondition(cr.Status.Conditions, conditionTypeBackupReady)
	g.Expect(backup).NotTo(BeNil())
	g.Expect(backup.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(backup.Reason).To(Equal(conditionReasonBackupScheduled))

	g.Expect(meta.IsStatusConditionTrue(cr.Status.Conditions, "Ready")).To(BeTrue())
}

// TestIntegrationOVNChassis_ProjectsDaemonSetsAndReachesReady walks the
// OVNChassis lifecycle on a live API server: a CR applied before any node
// carries its label, the node joining and being rendered an identity, and the
// node leaving again, which is the one path that has to outlive the selection.
//
// The chassis identity is the part no unit test covers end to end. It is
// generated once, kept in the ConfigMap the pods read and in status, and it is
// the only handle the Southbound database has on a chassis once its node is
// gone, so the deregistration Job is named after it.
func TestIntegrationOVNChassis_ProjectsDaemonSetsAndReachesReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestWithController(t)
	ns := createTestNamespace(t, ctx, c)

	// The chassis attach to a control plane that has published its Southbound
	// address and its client Secret; without both the pipeline stops at its
	// first gate.
	driveCentralToReady(t, ctx, c, c, ns, integrationCentralName, nil)

	chassis := integrationChassisCR(integrationChassisName, ns, integrationCentralName, nil)
	g.Expect(c.Create(ctx, chassis)).To(Succeed(), "create the OVNChassis CR")
	chassisKey := types.NamespacedName{Name: integrationChassisName, Namespace: ns}

	// No node carries the label yet. The DaemonSets and both ConfigMaps are
	// projected all the same: the empty nodes ConfigMap is what the pods mount,
	// and a volume whose ConfigMap does not exist keeps a pod from starting.
	empty := waitForChassisCondition(t, ctx, c, chassisKey, conditionTypeNodesReady,
		metav1.ConditionFalse, eventuallyTimeout)
	g.Expect(empty.Reason).To(Equal(conditionReasonNoMatchingNodes))

	nodesKey := client.ObjectKey{Namespace: ns, Name: chassisNodesName(chassis)}
	nodesCM := &corev1.ConfigMap{}
	eventuallyExists(t, ctx, c, nodesKey, nodesCM, "nodes ConfigMap", eventuallyTimeout)
	g.Expect(nodesCM.Data).To(BeEmpty(), "no node is selected, so no entry should be rendered")

	scripts := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: chassisScriptsName(chassis)}, scripts)).
		To(Succeed(), "the chassis scripts ConfigMap should exist")
	g.Expect(scripts.Data).To(HaveLen(5))

	// Both DaemonSets are projected while nothing is selected, in the pipeline's
	// own order: ovn-controller writes its chassis record into the local Open
	// vSwitch database, so a node that ran it without Open vSwitch would have
	// nothing to register against, and the step is gated on the first DaemonSet
	// being ready. envtest runs no DaemonSet controller, so the node counters
	// both gates read are set here.
	ovsKey := client.ObjectKey{Namespace: ns, Name: chassisOVSName(chassis)}
	eventuallyExists(t, ctx, c, ovsKey, &appsv1.DaemonSet{}, "ovs DaemonSet", eventuallyTimeout)
	g.Expect(simulators.MarkDaemonSetReady(ctx, c, ovsKey)).To(Succeed(), "mark the ovs DaemonSet ready")

	controllerKey := client.ObjectKey{Namespace: ns, Name: chassisControllerName(chassis)}
	eventuallyExists(t, ctx, c, controllerKey, &appsv1.DaemonSet{}, "ovn-controller DaemonSet", eventuallyLongTimeout)
	g.Expect(simulators.MarkDaemonSetReady(ctx, c, controllerKey)).To(Succeed(), "mark the ovn-controller DaemonSet ready")

	// A node joins the selection. envtest has no nodes of its own, so this one is
	// created rather than labelled.
	nodeName := "ovn-it-chassis-node"
	g.Expect(c.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{integrationChassisLabel: "true"},
		},
	})).To(Succeed(), "create the selected node")

	// The rendered entry is the whole channel between the operator and that node:
	// its chassis identity, whether it announces the gateway role, and the
	// encapsulation the tunnels use.
	var entry nodeEntry
	g.Eventually(func(ig Gomega) {
		cm := &corev1.ConfigMap{}
		ig.Expect(c.Get(ctx, nodesKey, cm)).To(Succeed())
		ig.Expect(cm.Data).To(HaveLen(1), "exactly the selected node should be rendered")
		ig.Expect(cm.Data).To(HaveKey(nodeName))
		entry = parseNodeEntry(cm.Data[nodeName])
		ig.Expect(entry.systemID).NotTo(BeEmpty(), "the node should be rendered a chassis identity")
	}, eventuallyTimeout, pollInterval).Should(Succeed())

	g.Expect(entry.gateway).To(BeFalse(), "the node carries no gateway label")
	g.Expect(entry.encapType).To(Equal("geneve"))

	g.Eventually(func(ig Gomega) {
		cr := &ovnv1alpha1.OVNChassis{}
		ig.Expect(c.Get(ctx, chassisKey, cr)).To(Succeed())
		ig.Expect(cr.Status.Nodes).To(HaveLen(1))
		ig.Expect(cr.Status.Nodes[0].Name).To(Equal(nodeName))
		ig.Expect(cr.Status.Nodes[0].SystemID).To(Equal(entry.systemID),
			"status must record the identity the ConfigMap carries, or the deregistration would name another chassis")
	}, eventuallyTimeout, pollInterval).Should(Succeed())

	waitForChassisCondition(t, ctx, c, chassisKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	// The node leaves the selection. Its entry survives the deselection with the
	// leaving marker, because the chassis registration outlives the node and the
	// identity to deregister has to outlive it too.
	node := &corev1.Node{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: nodeName}, node)).To(Succeed())
	delete(node.Labels, integrationChassisLabel)
	g.Expect(c.Update(ctx, node)).To(Succeed(), "drop the chassis label from the node")

	g.Eventually(func(ig Gomega) {
		cm := &corev1.ConfigMap{}
		ig.Expect(c.Get(ctx, nodesKey, cm)).To(Succeed())
		ig.Expect(cm.Data).To(HaveKey(nodeName))
		ig.Expect(parseNodeEntry(cm.Data[nodeName]).leaving).To(BeTrue(),
			"a deselected node keeps its entry, marked as leaving")
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())

	running := waitForChassisCondition(t, ctx, c, chassisKey, conditionTypeMaintenanceReady,
		metav1.ConditionTrue, eventuallyLongTimeout)
	g.Expect(running.Reason).To(Equal(conditionReasonMaintenanceRunning))

	delJobKey := client.ObjectKey{
		Namespace: ns,
		Name:      maintenanceJobName(chassis, maintenanceKindChassisDel, nodeName),
	}
	delJob := &batchv1.Job{}
	eventuallyExists(t, ctx, c, delJobKey, delJob, "chassis-del Job", eventuallyLongTimeout)
	g.Expect(delJob.Spec.Template.Spec.Containers[0].Env).To(ContainElement(
		corev1.EnvVar{Name: "CHASSIS", Value: entry.systemID}),
		"the deregistration must name the identity the node was registered under")
	g.Expect(simulators.SimulateJobComplete(ctx, c, delJobKey)).To(Succeed(), "complete the chassis-del Job")

	// The successful deregistration is what lets the operator forget the node:
	// its ConfigMap key and its status entry go together, or the next pass would
	// schedule a second deletion for a chassis that no longer exists.
	g.Eventually(func(ig Gomega) {
		cm := &corev1.ConfigMap{}
		ig.Expect(c.Get(ctx, nodesKey, cm)).To(Succeed())
		ig.Expect(cm.Data).NotTo(HaveKey(nodeName))

		cr := &ovnv1alpha1.OVNChassis{}
		ig.Expect(c.Get(ctx, chassisKey, cr)).To(Succeed())
		ig.Expect(cr.Status.Nodes).To(BeEmpty())
	}, eventuallyLongTimeout, pollInterval).Should(Succeed())
}
