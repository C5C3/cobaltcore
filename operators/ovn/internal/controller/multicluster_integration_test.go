// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// All multicluster envtest coverage of the two OVN reconcilers lives in the one
// test function below, on purpose. The kubeconfig provider registers its
// registration-Secret watch under the fixed controller name
// "kubeconfig-provider" and exposes no SkipNameValidation escape, while
// controller-runtime validates controller names against a process-global set.
// A second provider anywhere in this test binary would therefore fail to
// register. One manager, one provider, one function: the scenarios are ordered
// subtests over the shared setup, and each one builds on the state the previous
// left behind.
//
// Both reconcilers are registered through the production watch wiring
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

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/ovn/internal/testutil"
)

// TestIntegration_Multicluster_OVNTargetCluster runs both reconcilers on a
// management cluster with a second envtest environment registered as target
// cluster, and walks the target-cluster lifecycle of the pair: registration, an
// OVNCentral that projects its whole control plane onto the target, an
// OVNChassis that puts its DaemonSets on the target's nodes, a target-side node
// relabelling that has to reach the CR through the remote input watch, both
// kinds naming an unregistered cluster, and the two teardowns that sweep what
// each CR projected off the target.
func TestIntegration_Multicluster_OVNTargetCluster(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	const (
		// clustersNamespace mirrors the --clusters-namespace default the
		// operator binary passes to the provider.
		clustersNamespace = "c5c3-clusters"
		targetClusterName = "target-ovn"
		unknownCluster    = "does-not-exist"

		// The namespaces are fixed rather than generated so the same name can be
		// created on both clusters: a child lands in the CR's namespace,
		// whichever cluster it is written to. Both CRs share one namespace
		// because spec.centralRef is namespace-local.
		targetNamespace  = "mc-ovn"
		unknownNamespace = "mc-unknown"

		// The node the OVNChassis configures. It exists on the target cluster
		// alone: the nodes a chassis renders entries for are the ones its
		// children run on.
		targetNodeName = "mc-ovn-chassis-node"

		// survivorConfigMap is written into the target namespace by nobody in
		// particular, carrying none of the ownership labels. Both teardown
		// subtests read it back afterwards: a sweep that took it would be taking
		// somebody else's object.
		survivorConfigMap = "mc-unrelated"

		// engageTimeout bounds cluster engagement: the provider has to parse
		// the kubeconfig, build a cluster, and sync its cache before
		// GetCluster answers.
		engageTimeout = 60 * time.Second

		// watchLatency is the budget the node-relabelling subtest gets. Every
		// sub-reconciler of a settled OVNChassis returns a zero result, so the CR
		// carries no requeue at all and nothing inside this window can wake it
		// except an event from the remote input watch.
		watchLatency = 10 * time.Second
	)

	targetRef := &commonv1.TargetClusterRefSpec{Name: targetClusterName}

	// --- Environment B: the target cluster.
	//
	// It carries the fake CRDs of the external operators whose objects the
	// children include (cert-manager above all) and deliberately NOT the OVN
	// CRDs: a target cluster holds the workload, never the CR. That is also what
	// keeps the children alive here. Their owner references point at CRs this API
	// server cannot resolve, and envtest runs no garbage collector to act on that.
	targetScheme := commonenvtest.BuildScheme(commonenvtest.CommonExternalSchemes()...)
	targetClient, targetCfg := commonenvtest.StartEnvTestWithConfig(t, targetScheme, commonenvtest.CommonFakeCRDDirs())

	// --- Environment A: the management cluster, hosting the manager.
	mgmtScheme := commonenvtest.BuildScheme(append(commonenvtest.CommonExternalSchemes(), ovnv1alpha1.AddToScheme)...)

	provider := commonmulticluster.NewKubeconfigProvider(commonmulticluster.KubeconfigProviderOptions{
		Namespace: clustersNamespace,
		// Without this the provider builds every target cluster's client on
		// client-go's global scheme, which knows no CRD kind, and the first
		// Certificate the TLS step applies fails with "no kind is registered".
		ClusterOptions: []cluster.Option{func(o *cluster.Options) { o.Scheme = mgmtScheme }},
	})

	crdDir, webhookDir := multiclusterOVNPaths(t)

	var mcMgr mcmanager.Manager
	mgmtClient, ctx, _ := commonenvtest.StartManagedEnvTest(t, commonenvtest.ManagedEnvTestConfig{
		Name:              "OVN-multicluster",
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
		RegisterWebhooks: registerOVNWebhooks,
		RegisterController: func(mgr ctrl.Manager) error {
			// The provider's engagement machinery has to be registered before
			// the controllers, exactly as internal/common/bootstrap does it,
			// so engagement precedes the first reconcile.
			if err := provider.SetupWithManager(context.Background(), mcMgr); err != nil {
				return err
			}
			// The multicluster manager is the Resolver: it turns
			// spec.targetClusterRef into the client the children are written
			// with. The management environment carries the fake cert-manager
			// CRD, so the setup-time probe latches certManagerAvailable true
			// there, and the target cluster's own RESTMapper answers for the
			// children.
			return registerOVNControllers(mgr, mcMgr, mcMgr)
		},
	})

	// The provider watches this namespace for registration Secrets.
	multiclusterEnsureNamespace(t, ctx, mgmtClient, clustersNamespace)

	centralKey := types.NamespacedName{Name: integrationCentralName, Namespace: targetNamespace}
	chassisKey := types.NamespacedName{Name: integrationChassisName, Namespace: targetNamespace}

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

	t.Run("targeted OVNCentral projects its children onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The CR lives on the management cluster, everything it creates lives on
		// the target cluster.
		multiclusterEnsureNamespace(t, ctx, mgmtClient, targetNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, targetNamespace)

		driveCentralToReady(t, ctx, mgmtClient, targetClient, targetNamespace, integrationCentralName, targetRef)

		cr := &ovnv1alpha1.OVNCentral{}
		g.Expect(mgmtClient.Get(ctx, centralKey, cr)).To(Succeed())
		nb, sb := northboundDB(cr), southboundDB(cr)

		// Every kind the control plane projects, claimed by the ownership labels
		// and by nothing else. A reference would name a UID this cluster cannot
		// resolve, and the labels are the only handle the teardown sweep has.
		children := centralChildren(cr, nb, sb)
		for _, child := range children {
			multiclusterExpectRemoteOwnership(t, ctx, targetClient, child.key, child.obj, child.what,
				"OVNCentral", integrationCentralName, targetNamespace)
		}

		// None of it on the management cluster, where only the CR lives.
		for _, child := range centralChildren(cr, nb, sb) {
			multiclusterExpectAbsent(t, ctx, mgmtClient, child.key, child.obj, child.what)
		}

		// Status and the finalizer stay with the CR.
		g.Expect(cr.Status.Conditions).NotTo(BeEmpty(), "status should be populated on the management cluster")
		g.Expect(controllerutil.ContainsFinalizer(cr, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"the remote-children finalizer should be on a CR whose children live on another cluster")
	})

	t.Run("targeted OVNChassis projects its DaemonSets onto the target cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The node a chassis configures is the target cluster's, so this is where
		// it is created. The management cluster never sees it.
		g.Expect(targetClient.Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   targetNodeName,
				Labels: map[string]string{integrationChassisLabel: "true"},
			},
		})).To(Succeed(), "create the selected node on the target cluster")

		chassis := integrationChassisCR(integrationChassisName, targetNamespace, integrationCentralName, targetRef)
		g.Expect(mgmtClient.Create(ctx, chassis)).To(Succeed(), "create the OVNChassis CR")

		// The per-node values land on the target, keyed by the node they
		// configure.
		nodesKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisNodesName(chassis)}
		g.Eventually(func(ig Gomega) {
			cm := &corev1.ConfigMap{}
			ig.Expect(targetClient.Get(ctx, nodesKey, cm)).To(Succeed())
			ig.Expect(cm.Data).To(HaveKey(targetNodeName))
			ig.Expect(parseNodeEntry(cm.Data[targetNodeName]).systemID).NotTo(BeEmpty())
		}, eventuallyLongTimeout, pollInterval).Should(Succeed(),
			"the nodes ConfigMap should be rendered on the target cluster")

		// The DaemonSets, in the pipeline's own order: ovn-controller is gated on
		// Open vSwitch being ready, because the local database it registers into
		// is the one Open vSwitch owns. envtest runs no DaemonSet controller.
		ovsKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisOVSName(chassis)}
		eventuallyExists(t, ctx, targetClient, ovsKey, &appsv1.DaemonSet{}, "ovs DaemonSet", eventuallyTimeout)
		g.Expect(simulators.MarkDaemonSetReady(ctx, targetClient, ovsKey)).To(Succeed())

		controllerKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisControllerName(chassis)}
		eventuallyExists(t, ctx, targetClient, controllerKey, &appsv1.DaemonSet{}, "ovn-controller DaemonSet", eventuallyLongTimeout)
		g.Expect(simulators.MarkDaemonSetReady(ctx, targetClient, controllerKey)).To(Succeed())

		waitForChassisCondition(t, ctx, mgmtClient, chassisKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		for _, child := range chassisChildren(chassis) {
			multiclusterExpectRemoteOwnership(t, ctx, targetClient, child.key, child.obj, child.what,
				"OVNChassis", integrationChassisName, targetNamespace)
		}
		for _, child := range chassisChildren(chassis) {
			multiclusterExpectAbsent(t, ctx, mgmtClient, child.key, child.obj, child.what)
		}

		after := &ovnv1alpha1.OVNChassis{}
		g.Expect(mgmtClient.Get(ctx, chassisKey, after)).To(Succeed())
		g.Expect(controllerutil.ContainsFinalizer(after, commonmulticluster.RemoteChildrenFinalizer)).To(BeTrue(),
			"the remote-children finalizer should be on a CR whose children live on another cluster")
	})

	t.Run("relabelling a target node re-renders its entry at watch latency", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// THE ACCEPTANCE RULE OF THIS SUBTEST: it never writes the OVNChassis CR.
		// The previous subtest left it settled, and every sub-reconciler of a
		// settled chassis returns a zero result, so the pipeline asks for no
		// requeue at all. The only write here is a label on a Node of the target
		// cluster, and the operator has to get from there to a re-rendered entry
		// on its own. Take the remote input leg away and this subtest sits out
		// the budget below and fails.
		chassis := &ovnv1alpha1.OVNChassis{}
		g.Expect(mgmtClient.Get(ctx, chassisKey, chassis)).To(Succeed())
		nodesKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisNodesName(chassis)}

		before := &corev1.ConfigMap{}
		g.Expect(targetClient.Get(ctx, nodesKey, before)).To(Succeed())
		g.Expect(parseNodeEntry(before.Data[targetNodeName]).gateway).To(BeFalse(),
			"the node carries no gateway label yet")

		node := &corev1.Node{}
		g.Expect(targetClient.Get(ctx, client.ObjectKey{Name: targetNodeName}, node)).To(Succeed())
		node.Labels[integrationGatewayLabel] = "true"
		g.Expect(targetClient.Update(ctx, node)).To(Succeed(), "promote the target node to a gateway")

		g.Eventually(func(ig Gomega) {
			cm := &corev1.ConfigMap{}
			ig.Expect(targetClient.Get(ctx, nodesKey, cm)).To(Succeed())
			ig.Expect(parseNodeEntry(cm.Data[targetNodeName]).gateway).To(BeTrue())
		}, watchLatency, pollInterval).Should(Succeed(),
			"the gateway role should reach the node's entry through the remote Node watch, with no write to the CR")
	})

	t.Run("CRs naming an unregistered cluster create nothing and carry no finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)

		multiclusterEnsureNamespace(t, ctx, mgmtClient, unknownNamespace)
		multiclusterEnsureNamespace(t, ctx, targetClient, unknownNamespace)

		unknownRef := &commonv1.TargetClusterRefSpec{Name: unknownCluster}
		central := integrationCentralCR(integrationCentralName, unknownNamespace, unknownRef)
		g.Expect(mgmtClient.Create(ctx, central)).To(Succeed())
		chassis := integrationChassisCR(integrationChassisName, unknownNamespace, integrationCentralName, unknownRef)
		g.Expect(mgmtClient.Create(ctx, chassis)).To(Succeed())

		// Each kind reports the unresolvable cluster on its pipeline's first
		// gate, which is the condition the rest of the graph waits behind.
		unresolvedCentral := types.NamespacedName{Name: integrationCentralName, Namespace: unknownNamespace}
		cond := waitForCentralCondition(t, ctx, mgmtClient, unresolvedCentral, conditionTypeTLSReady,
			metav1.ConditionFalse, eventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))

		unresolvedChassis := types.NamespacedName{Name: integrationChassisName, Namespace: unknownNamespace}
		cond = waitForChassisCondition(t, ctx, mgmtClient, unresolvedChassis, conditionTypeCentralReady,
			metav1.ConditionFalse, eventuallyTimeout)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))

		// Nothing was created, so nothing has to be cleaned up: neither CR may
		// carry a finalizer that would only block its deletion.
		gotCentral := &ovnv1alpha1.OVNCentral{}
		g.Expect(mgmtClient.Get(ctx, unresolvedCentral, gotCentral)).To(Succeed())
		g.Expect(gotCentral.Finalizers).To(BeEmpty(), "an unresolvable OVNCentral should carry no finalizer")
		gotChassis := &ovnv1alpha1.OVNChassis{}
		g.Expect(mgmtClient.Get(ctx, unresolvedChassis, gotChassis)).To(Succeed())
		g.Expect(gotChassis.Finalizers).To(BeEmpty(), "an unresolvable OVNChassis should carry no finalizer")

		nb, sb := northboundDB(gotCentral), southboundDB(gotCentral)
		for _, c := range []client.Client{mgmtClient, targetClient} {
			for _, child := range centralChildren(gotCentral, nb, sb) {
				multiclusterExpectAbsent(t, ctx, c, child.key, child.obj, child.what)
			}
			for _, child := range chassisChildren(gotChassis) {
				multiclusterExpectAbsent(t, ctx, c, child.key, child.obj, child.what)
			}
		}
	})

	t.Run("deleting the OVNChassis sweeps its children off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		chassis := &ovnv1alpha1.OVNChassis{}
		g.Expect(mgmtClient.Get(ctx, chassisKey, chassis)).To(Succeed())

		// What the sweep must leave standing. It carries none of the ownership
		// labels, so a sweep that took it would be taking somebody else's object.
		g.Expect(targetClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: survivorConfigMap, Namespace: targetNamespace},
			Data:       map[string]string{"owner": "nobody"},
		})).To(Succeed(), "seed an unlabelled ConfigMap beside the children")

		projected := chassisChildren(chassis)
		for _, child := range projected {
			g.Expect(targetClient.Get(ctx, child.key, child.obj)).To(Succeed(),
				"%s %s must be on the cluster before the deletion, or its absence afterwards proves nothing",
				child.what, child.key)
		}

		g.Expect(mgmtClient.Delete(ctx, chassis)).To(Succeed(), "delete the targeted OVNChassis")

		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, chassisKey, &ovnv1alpha1.OVNChassis{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CR should leave etcd once the remote-children finalizer is released")

		// The sweep does not wait for the objects it deleted, and a delete is
		// asynchronous, so each child is polled until it is gone.
		g.Eventually(func(ig Gomega) {
			for _, child := range chassisChildren(chassis) {
				expectSwept(ig, ctx, targetClient, child)
			}
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())

		g.Expect(targetClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: survivorConfigMap},
			&corev1.ConfigMap{})).To(Succeed(), "an unlabelled ConfigMap should survive the sweep")
		g.Expect(targetClient.Get(ctx, client.ObjectKey{Name: targetNodeName}, &corev1.Node{})).
			To(Succeed(), "the node itself is not a child and should survive the sweep")
	})

	t.Run("deleting the OVNCentral sweeps its children off the target", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cr := &ovnv1alpha1.OVNCentral{}
		g.Expect(mgmtClient.Get(ctx, centralKey, cr)).To(Succeed())
		nb, sb := northboundDB(cr), southboundDB(cr)

		projected := centralChildren(cr, nb, sb)
		for _, child := range projected {
			g.Expect(targetClient.Get(ctx, child.key, child.obj)).To(Succeed(),
				"%s %s must be on the cluster before the deletion, or its absence afterwards proves nothing",
				child.what, child.key)
		}

		g.Expect(mgmtClient.Delete(ctx, cr)).To(Succeed(), "delete the targeted OVNCentral")

		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, centralKey, &ovnv1alpha1.OVNCentral{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CR should leave etcd once the remote-children finalizer is released")

		g.Eventually(func(ig Gomega) {
			for _, child := range centralChildren(cr, nb, sb) {
				expectSwept(ig, ctx, targetClient, child)
			}
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())

		// The client Secret is cert-manager's output rather than a child of this
		// CR, which is why Secret is off the remote-child list: cert-manager
		// deletes it with the Certificate that wrote it, and no such controller
		// runs here.
		g.Expect(targetClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: clientSecretName(cr)},
			&corev1.Secret{})).To(Succeed(), "the Secret cert-manager owns is not swept by this operator")
		g.Expect(targetClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: survivorConfigMap},
			&corev1.ConfigMap{})).To(Succeed(), "an unlabelled ConfigMap should survive the sweep")
	})

	t.Run("a placed OVNChassis attaches to a home OVNCentral over its node ports", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// A central on the management cluster, published outside it on both
		// databases and on its relay, and a chassis on the target. The chassis
		// can reach neither the central's cluster IPs nor its client Secret, so
		// it dials the relay's node address and mounts a copy of the Secret on
		// its own cluster.
		homeName := integrationCentralName + "-home"
		home := integrationCentralCR(homeName, targetNamespace, nil)
		home.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 1, ExternallyReachable: true}
		driveCentralCRToReady(t, ctx, mgmtClient, mgmtClient, home)

		homeKey := types.NamespacedName{Name: homeName, Namespace: targetNamespace}
		g.Expect(mgmtClient.Get(ctx, homeKey, home)).To(Succeed())
		g.Expect(home.Status.RelayDbAddress).To(Equal(fmt.Sprintf("ssl:%s:%d", relayHostIP,
			ovnv1alpha1.DefaultRelayNodePort)), "the relay's node address is what the chassis dials")
		sourceKey := client.ObjectKey{Namespace: targetNamespace, Name: clientSecretName(home)}
		source := &corev1.Secret{}
		g.Expect(mgmtClient.Get(ctx, sourceKey, source)).To(Succeed())

		chassis := integrationChassisCR("chassis-home", targetNamespace, homeName, targetRef)
		g.Expect(mgmtClient.Create(ctx, chassis)).To(Succeed(), "create the placed OVNChassis CR")
		placedKey := client.ObjectKeyFromObject(chassis)

		// The copy lands on the target, beside the chassis's other children,
		// carrying exactly the three source values and claimed by the ownership
		// labels the teardown sweeps by.
		copyKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisClientSecretName(chassis)}
		multiclusterExpectRemoteOwnership(t, ctx, targetClient, copyKey, &corev1.Secret{}, "client Secret copy",
			"OVNChassis", chassis.Name, targetNamespace)
		copied := &corev1.Secret{}
		g.Expect(targetClient.Get(ctx, copyKey, copied)).To(Succeed())
		g.Expect(copied.Data).To(Equal(map[string][]byte{
			"tls.crt": source.Data["tls.crt"],
			"tls.key": source.Data["tls.key"],
			"ca.crt":  source.Data["ca.crt"],
		}))
		g.Expect(copied.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "ovn-client"))

		// The two DaemonSets, in the pipeline's order, and a settled chassis:
		// from here on no sub-reconciler asks for a requeue, which is what makes
		// the renewal below a test of the Secret watch.
		ovsKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisOVSName(chassis)}
		eventuallyExists(t, ctx, targetClient, ovsKey, &appsv1.DaemonSet{}, "ovs DaemonSet", eventuallyTimeout)
		g.Expect(simulators.MarkDaemonSetReady(ctx, targetClient, ovsKey)).To(Succeed())
		controllerKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisControllerName(chassis)}
		controller := &appsv1.DaemonSet{}
		eventuallyExists(t, ctx, targetClient, controllerKey, controller, "ovn-controller DaemonSet", eventuallyLongTimeout)
		g.Expect(simulators.MarkDaemonSetReady(ctx, targetClient, controllerKey)).To(Succeed())
		waitForChassisCondition(t, ctx, mgmtClient, placedKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		g.Expect(targetClient.Get(ctx, controllerKey, controller)).To(Succeed())
		var remotes []string
		for _, container := range append(controller.Spec.Template.Spec.InitContainers,
			controller.Spec.Template.Spec.Containers...) {
			for _, env := range container.Env {
				if env.Name == "OVN_REMOTE" {
					remotes = append(remotes, env.Value)
				}
			}
		}
		g.Expect(remotes).NotTo(BeEmpty(), "ovn-controller has to be handed a remote")
		g.Expect(remotes).To(HaveEach(home.Status.RelayDbAddress))
		var mounted []string
		for _, volume := range controller.Spec.Template.Spec.Volumes {
			if volume.Secret != nil {
				mounted = append(mounted, volume.Secret.SecretName)
			}
		}
		g.Expect(mounted).To(ConsistOf(copyKey.Name), "the pods mount the copy, not the central's Secret")

		placed := &ovnv1alpha1.OVNChassis{}
		g.Expect(mgmtClient.Get(ctx, placedKey, placed)).To(Succeed())
		g.Expect(placed.Status.ClientSecretName).To(Equal(copyKey.Name))
		cond := meta.FindStatusCondition(placed.Status.Conditions, conditionTypeCentralReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonCentralResolved))

		// THE ACCEPTANCE RULE OF THIS HALF: it never writes the OVNChassis CR. A
		// renewal rewrites the source on the management cluster, and the copy on
		// the target has to follow through the Secret watch alone.
		g.Expect(mgmtClient.Get(ctx, sourceKey, source)).To(Succeed())
		source.Data["tls.crt"] = []byte("renewed-client-certificate")
		g.Expect(mgmtClient.Update(ctx, source)).To(Succeed(), "renew the source client certificate")

		g.Eventually(func(ig Gomega) {
			renewed := &corev1.Secret{}
			ig.Expect(targetClient.Get(ctx, copyKey, renewed)).To(Succeed())
			ig.Expect(renewed.Data).To(HaveKeyWithValue("tls.crt", []byte("renewed-client-certificate")))
		}, watchLatency, pollInterval).Should(Succeed(),
			"the copy should follow the renewal through the Secret watch, with no write to the CR")

		// The copy goes with the chassis, swept off the target by its ownership
		// labels together with the DaemonSets and ConfigMaps, while the
		// central's own Secret stays where cert-manager wrote it.
		projected := append(chassisChildren(chassis),
			remoteChild{key: copyKey, obj: &corev1.Secret{}, what: "client Secret copy"})
		g.Expect(mgmtClient.Delete(ctx, placed)).To(Succeed(), "delete the placed OVNChassis")
		g.Eventually(func() bool {
			return apierrors.IsNotFound(mgmtClient.Get(ctx, placedKey, &ovnv1alpha1.OVNChassis{}))
		}, eventuallyLongTimeout, pollInterval).Should(BeTrue(),
			"the CR should leave etcd once the remote-children finalizer is released")
		g.Eventually(func(ig Gomega) {
			for _, child := range projected {
				expectSwept(ig, ctx, targetClient, child)
			}
		}, eventuallyLongTimeout, pollInterval).Should(Succeed())
		g.Expect(mgmtClient.Get(ctx, sourceKey, &corev1.Secret{})).To(Succeed(),
			"the central's client Secret is not the chassis's to sweep")
	})

	t.Run("a home OVNChassis follows a renewal on its placed OVNCentral's cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// The mirror of the previous subtest: the central on the target, where
		// cert-manager renews its client Secret, and the chassis with its copy on
		// the management cluster. A renewal there can reach the chassis through
		// the remote leg of the Secret watch alone; the local leg never sees it.
		// The central that projected onto the target before has been swept, so
		// its node ports are free again.
		placedName := integrationCentralName + "-placed"
		placed := integrationCentralCR(placedName, targetNamespace, targetRef)
		driveCentralCRToReady(t, ctx, mgmtClient, targetClient, placed)
		sourceKey := client.ObjectKey{Namespace: targetNamespace, Name: clientSecretName(placed)}

		g.Expect(mgmtClient.Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mc-ovn-home-chassis-node",
				Labels: map[string]string{integrationChassisLabel: "true"},
			},
		})).To(Succeed(), "create the selected node on the management cluster")

		chassis := integrationChassisCR("chassis-mgmt", targetNamespace, placedName, nil)
		g.Expect(mgmtClient.Create(ctx, chassis)).To(Succeed(), "create the home OVNChassis CR")
		homeKey := client.ObjectKeyFromObject(chassis)

		// A settled chassis: from here on no sub-reconciler asks for a requeue.
		ovsKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisOVSName(chassis)}
		eventuallyExists(t, ctx, mgmtClient, ovsKey, &appsv1.DaemonSet{}, "ovs DaemonSet", eventuallyTimeout)
		g.Expect(simulators.MarkDaemonSetReady(ctx, mgmtClient, ovsKey)).To(Succeed())
		controllerKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisControllerName(chassis)}
		eventuallyExists(t, ctx, mgmtClient, controllerKey, &appsv1.DaemonSet{}, "ovn-controller DaemonSet",
			eventuallyLongTimeout)
		g.Expect(simulators.MarkDaemonSetReady(ctx, mgmtClient, controllerKey)).To(Succeed())
		waitForChassisCondition(t, ctx, mgmtClient, homeKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

		copyKey := client.ObjectKey{Namespace: targetNamespace, Name: chassisClientSecretName(chassis)}
		g.Expect(mgmtClient.Get(ctx, copyKey, &corev1.Secret{})).To(Succeed(),
			"the copy lands beside the chassis on the management cluster")

		// THE ACCEPTANCE RULE OF THIS HALF: it writes neither CR. The renewal
		// rewrites the source on the target in place, leaving the central's
		// status as it was, so nothing but the remote Secret leg can wake the
		// chassis inside the budget.
		source := &corev1.Secret{}
		g.Expect(targetClient.Get(ctx, sourceKey, source)).To(Succeed())
		source.Data["tls.crt"] = []byte("renewed-on-the-target")
		g.Expect(targetClient.Update(ctx, source)).To(Succeed(), "renew the source client certificate")

		g.Eventually(func(ig Gomega) {
			renewed := &corev1.Secret{}
			ig.Expect(mgmtClient.Get(ctx, copyKey, renewed)).To(Succeed())
			ig.Expect(renewed.Data).To(HaveKeyWithValue("tls.crt", []byte("renewed-on-the-target")))
		}, watchLatency, pollInterval).Should(Succeed(),
			"the copy should follow a renewal on the target through the remote Secret watch")
	})
}

// remoteChild is one projected object: where it lives, an empty instance of its
// kind to read it into, and what to call it in a failure message.
type remoteChild struct {
	key  client.ObjectKey
	obj  client.Object
	what string
	// heldByFinalizer marks a child the API server stamps a protection
	// finalizer on. The sweep's Delete reaches it like every other child, but
	// the finalizer is released by a controller envtest does not run, so the
	// object stays in etcd Terminating instead of leaving it.
	heldByFinalizer bool
}

// expectSwept asserts the sweep reached child on c: the object is gone, or, when
// a protection finalizer holds it in etcd, it is at least Terminating. Every
// other child has to be gone outright, so a Deployment or a Service left
// standing still fails here.
func expectSwept(ig Gomega, ctx context.Context, c client.Client, child remoteChild) {
	err := c.Get(ctx, child.key, child.obj)
	if apierrors.IsNotFound(err) {
		return
	}
	ig.Expect(err).NotTo(HaveOccurred(), "reading %s %s", child.what, child.key)
	ig.Expect(child.heldByFinalizer).To(BeTrue(),
		"%s %s should be swept off the target cluster", child.what, child.key)
	ig.Expect(child.obj.GetDeletionTimestamp().IsZero()).To(BeFalse(),
		"%s %s is held by a protection finalizer, so the sweep should at least have left it Terminating",
		child.what, child.key)
}

// centralChildren enumerates every object an OVNCentral projects, which is both
// the inventory the projection has to produce and the inventory the teardown
// sweep has to remove. A kind missing here is a kind that keeps running after
// its CR is gone.
func centralChildren(cr *ovnv1alpha1.OVNCentral, nb, sb raftDB) []remoteChild {
	ns := cr.Namespace
	children := []remoteChild{
		{key: client.ObjectKey{Namespace: ns, Name: centralScriptsName(cr)}, obj: &corev1.ConfigMap{}, what: "scripts ConfigMap"},
		{key: client.ObjectKey{Namespace: ns, Name: northdName(cr)}, obj: &appsv1.Deployment{}, what: "northd Deployment"},
		// The API server's storage-object-in-use protection stamps
		// kubernetes.io/pvc-protection on every claim, and the controller that
		// releases it does not run here.
		{
			key: client.ObjectKey{Namespace: ns, Name: backupVolumeName(cr)},
			obj: &corev1.PersistentVolumeClaim{}, what: "backup PersistentVolumeClaim",
			heldByFinalizer: true,
		},
		{key: client.ObjectKey{Namespace: ns, Name: backupCronJobName(cr.Name)}, obj: &batchv1.CronJob{}, what: "backup CronJob"},
	}
	for _, certName := range centralCertificateNames(cr.Name) {
		children = append(children, remoteChild{
			key: client.ObjectKey{Namespace: ns, Name: certName}, obj: &certmanagerv1.Certificate{}, what: "Certificate",
		})
	}
	for _, db := range []raftDB{nb, sb} {
		children = append(children,
			remoteChild{key: client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}, obj: &appsv1.StatefulSet{}, what: db.suffix + " StatefulSet"},
			remoteChild{key: client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}, obj: &corev1.Service{}, what: db.suffix + " headless Service"},
			remoteChild{key: client.ObjectKey{Namespace: ns, Name: raftName(cr, db)}, obj: &policyv1.PodDisruptionBudget{}, what: db.suffix + " PodDisruptionBudget"},
		)
		for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
			children = append(children, remoteChild{
				key: client.ObjectKey{Namespace: ns, Name: raftMemberName(cr, db, ordinal)},
				obj: &corev1.Service{}, what: fmt.Sprintf("%s member %d Service", db.suffix, ordinal),
			})
		}
	}
	return children
}

// chassisChildren enumerates every object an OVNChassis projects. The list is
// short because a chassis owns no state of its own: the two ConfigMaps its pods
// mount and the two DaemonSets that mount them.
func chassisChildren(cr *ovnv1alpha1.OVNChassis) []remoteChild {
	ns := cr.Namespace
	return []remoteChild{
		{key: client.ObjectKey{Namespace: ns, Name: chassisNodesName(cr)}, obj: &corev1.ConfigMap{}, what: "nodes ConfigMap"},
		{key: client.ObjectKey{Namespace: ns, Name: chassisScriptsName(cr)}, obj: &corev1.ConfigMap{}, what: "chassis scripts ConfigMap"},
		{key: client.ObjectKey{Namespace: ns, Name: chassisOVSName(cr)}, obj: &appsv1.DaemonSet{}, what: "ovs DaemonSet"},
		{key: client.ObjectKey{Namespace: ns, Name: chassisControllerName(cr)}, obj: &appsv1.DaemonSet{}, what: "ovn-controller DaemonSet"},
	}
}

// multiclusterOVNPaths returns the OVN CRD and webhook manifest directories,
// resolved relative to this source file. The per-operator testutil package
// resolves them the same way for its own helpers, which do not expose the
// manager hook this test needs.
func multiclusterOVNPaths(t testing.TB) (crdDir, webhookDir string) {
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
