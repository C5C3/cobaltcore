// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/neutron/internal/testutil"
)

// --- Helpers ---

// setupEnvTest wraps testutil.SetupNeutronEnvTest with the v1alpha1 scheme
// registration and both webhook setups (Neutron + NeutronMetadataAgent),
// avoiding the import cycle between testutil and this package. The webhook
// manifests envtest installs carry both kinds (failurePolicy=Fail), so both
// handlers must be served or admission of the unserved kind fails.
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupNeutronEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		// mgr.GetAPIReader() mirrors production wiring in main.go: webhook
		// admission lookups (PriorityClass existence) read the API server
		// directly, never a stale informer cache.
		if err := (&NeutronWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
			return err
		}
		return (&NeutronMetadataAgentWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
	})
}

// setupEnvTestNoWebhook wraps testutil.SetupNeutronEnvTestNoWebhook with the
// v1alpha1 scheme registration. Used by the CRD-only tests so no webhook can
// mask a missing CEL rule or supply a default the CRD schema must supply itself.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupNeutronEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationNeutron returns validNeutron() stamped with name/namespace for API
// server submission. validNeutron() is a webhook-unit helper whose validators do
// not enforce the CRD required-field markers, so it omits spec.database.database
// and spec.database.secretRef.name; those are filled here so the object clears
// the real CRD schema (both are required even in managed/clusterRef mode).
func integrationNeutron(name, namespace string) *Neutron {
	neutron := validNeutron()
	neutron.Name = name
	neutron.Namespace = namespace
	neutron.Spec.Database.Database = "neutron"
	neutron.Spec.Database.SecretRef = commonv1.SecretRefSpec{Name: "neutron-db"}
	return neutron
}

// integrationAgent returns validNeutronMetadataAgent() stamped with
// name/namespace and pointed at chassisRef, for API server submission.
func integrationAgent(name, namespace, chassisRef string) *NeutronMetadataAgent {
	agent := validNeutronMetadataAgent()
	agent.Name = name
	agent.Namespace = namespace
	agent.Spec.ChassisRef = OVNChassisRef{Name: chassisRef}
	return agent
}

// expectRejected asserts err is the API server's rejection of a schema rule and
// that its message names the rule. Both status reasons are accepted because a
// CEL transition rule surfaces as Invalid while a few schema paths report
// Forbidden, and which one a rule takes is not what these tests pin.
func expectRejected(t testing.TB, err error, wantSubstring string) {
	t.Helper()
	g := NewGomegaWithT(t)

	g.Expect(err).To(HaveOccurred(), "the schema must reject this object")
	g.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
		"expected Invalid or Forbidden status error, got: %v", err)
	g.Expect(err.Error()).To(ContainSubstring(wantSubstring))
}

// --- CRD-level CEL / schema enforcement (no validating webhook installed) ---

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange pins the
// targetClusterRef rename rule on both kinds: re-pointing a live Neutron or a
// live metadata agent at another target cluster is rejected by the CRD CEL rule
// alone, without the validating webhook.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("Neutron", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "neutron-targetcluster-rename-")

		neutron := integrationNeutron("neutron", ns)
		neutron.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, neutron)).To(Succeed(), "valid Neutron should be accepted")

		got := &Neutron{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "neutron", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef.Name = "cluster-b"

		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})

	t.Run("NeutronMetadataAgent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "agent-targetcluster-rename-")

		agent := integrationAgent("agent", ns, "chassis")
		agent.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, agent)).To(Succeed(), "valid NeutronMetadataAgent should be accepted")

		got := &NeutronMetadataAgent{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "agent", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef.Name = "cluster-b"

		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip pins the
// presence rule of both kinds in both directions: a CR created without the ref
// cannot gain one, and a CR created with it cannot drop it. Either edit would
// move the children away from the cluster that already holds them.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("Neutron", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "neutron-targetcluster-flip-")

		local := integrationNeutron("neutron-local", ns)
		g.Expect(c.Create(ctx, local)).To(Succeed(), "Neutron without targetClusterRef should be accepted")

		got := &Neutron{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "neutron-local", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")

		remote := integrationNeutron("neutron-remote", ns)
		remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, remote)).To(Succeed(), "Neutron with targetClusterRef should be accepted")

		got = &Neutron{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "neutron-remote", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = nil
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})

	t.Run("NeutronMetadataAgent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "agent-targetcluster-flip-")

		local := integrationAgent("agent-local", ns, "chassis")
		g.Expect(c.Create(ctx, local)).To(Succeed(), "agent without targetClusterRef should be accepted")

		got := &NeutronMetadataAgent{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "agent-local", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")

		remote := integrationAgent("agent-remote", ns, "chassis")
		remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, remote)).To(Succeed(), "agent with targetClusterRef should be accepted")

		got = &NeutronMetadataAgent{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "agent-remote", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = nil
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})
}

// TestIntegration_CRD_CELOnly_RejectsChassisRefChange pins the chassisRef rule:
// an agent re-pointed at another chassis lands on another set of nodes, whose
// local OVS databases carry none of the ports it was answering for.
func TestIntegration_CRD_CELOnly_RejectsChassisRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "chassisref-immutable-")

	agent := integrationAgent("agent", ns, "chassis-a")
	g.Expect(c.Create(ctx, agent)).To(Succeed(), "valid NeutronMetadataAgent should be accepted")

	got := &NeutronMetadataAgent{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "agent", Namespace: ns}, got)).To(Succeed())
	got.Spec.ChassisRef.Name = "chassis-b"

	expectRejected(t, c.Update(ctx, got), "chassisRef is immutable")
}

// TestIntegration_CRD_CELOnly_RejectsMessagingXOR pins the shared
// commonv1.MessagingSpec union rule on both kinds. Neutron embeds the block
// required, the agent as an optional pointer, and the rule rides along with the
// type in both cases: naming a managed cluster and a brownfield Secret at once,
// or neither of them, leaves the transport URL underdetermined.
func TestIntegration_CRD_CELOnly_RejectsMessagingXOR(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	const wantSub = "exactly one of clusterRef or secretRef must be set"

	t.Run("Neutron", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "neutron-messaging-xor-")

		both := integrationNeutron("neutron-both", ns)
		both.Spec.Messaging.SecretRef = &commonv1.SecretRefSpec{Name: "neutron-transport-url"}
		expectRejected(t, c.Create(ctx, both), wantSub)

		neither := integrationNeutron("neutron-neither", ns)
		neither.Spec.Messaging = commonv1.MessagingSpec{}
		expectRejected(t, c.Create(ctx, neither), wantSub)
	})

	t.Run("NeutronMetadataAgent", func(t *testing.T) {
		ns := newNamespace(t, ctx, c, "agent-messaging-xor-")

		both := integrationAgent("agent-both", ns, "chassis")
		both.Spec.Messaging = &commonv1.MessagingSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
			SecretRef:  &commonv1.SecretRefSpec{Name: "neutron-transport-url"},
		}
		expectRejected(t, c.Create(ctx, both), wantSub)

		neither := integrationAgent("agent-neither", ns, "chassis")
		neither.Spec.Messaging = &commonv1.MessagingSpec{}
		expectRejected(t, c.Create(ctx, neither), wantSub)
	})
}

// TestIntegration_CRD_NestedDefaultsMaterialized proves the API server fills the
// nested schema defaults of the Neutron kind without a mutating webhook. Each
// value below sits under an object the CR carries but leaves empty, so it only
// materializes when the leaf marker is in place: drop one and the operator would
// read a zero replica count off a CR that looks complete, or run the OVN sync in
// a mode it never resolves.
func TestIntegration_CRD_NestedDefaultsMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "neutron-defaults-")

	neutron := integrationNeutron("neutron", ns)
	neutron.Spec.Deployment = DeploymentSpec{}
	neutron.Spec.Workers = WorkersSpec{}
	neutron.Spec.OVNDBSync = &OVNDBSyncSpec{}
	g.Expect(c.Create(ctx, neutron)).To(Succeed(), "a Neutron with empty nested blocks should be accepted")

	got := &Neutron{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "neutron", Namespace: ns}, got)).To(Succeed())

	g.Expect(got.Spec.Deployment.Replicas).To(BeEquivalentTo(3), "deployment.replicas")
	g.Expect(got.Spec.Workers.Deployment.Replicas).To(BeEquivalentTo(3), "workers.deployment.replicas")
	g.Expect(got.Spec.Messaging.Replicas).To(BeEquivalentTo(3), "messaging.replicas")
	g.Expect(got.Spec.OVNDBSync.SyncMode).To(Equal(DefaultOVNDBSyncMode), "ovnDBSync.syncMode")
}

// --- Webhook round-trip (both webhooks installed) ---

// TestIntegration_WebhookDefaults proves the mutating webhooks fill, through the
// real admission chain, the values no CRD default can supply: the Neutron
// service-user identity, the OVN namespace that has to be read off the CR's own
// metadata, and the agent's Nova metadata port.
func TestIntegration_WebhookDefaults(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTest(t)

	t.Run("Neutron", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "neutron-webhook-defaults-")

		neutron := integrationNeutron("neutron", ns)
		// Minimal service-user block: only the password Secret name. The OVN
		// reference names no namespace, so the webhook has to read it off the CR.
		neutron.Spec.ServiceUser = ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "neutron-service-password"}}
		neutron.Spec.OVN.CentralRef.Namespace = ""

		g.Expect(c.Create(ctx, neutron)).To(Succeed(), "minimal Neutron should be accepted after defaults")

		got := &Neutron{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "neutron", Namespace: ns}, got)).To(Succeed())
		g.Expect(got.Spec.ServiceUser.Username).To(Equal("neutron"))
		g.Expect(got.Spec.ServiceUser.ProjectName).To(Equal("service"))
		g.Expect(got.Spec.ServiceUser.UserDomainName).To(Equal("Default"))
		g.Expect(got.Spec.ServiceUser.ProjectDomainName).To(Equal("Default"))
		g.Expect(got.Spec.ServiceUser.SecretRef.Key).To(Equal("password"))
		g.Expect(got.Spec.OVN.CentralRef.Namespace).To(Equal(ns))
	})

	t.Run("NeutronMetadataAgent", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "agent-webhook-defaults-")

		agent := integrationAgent("agent", ns, "chassis")
		agent.Spec.NovaMetadata = &NovaMetadataSpec{
			Host:            "nova-metadata.openstack.svc.cluster.local",
			SharedSecretRef: &commonv1.SecretRefSpec{Name: "metadata-proxy-secret"},
		}

		g.Expect(c.Create(ctx, agent)).To(Succeed(), "agent without a port should be accepted after defaults")

		got := &NeutronMetadataAgent{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "agent", Namespace: ns}, got)).To(Succeed())
		g.Expect(got.Spec.NovaMetadata.Port).To(Equal(DefaultNovaMetadataPort))
		g.Expect(got.Spec.NovaMetadata.SharedSecretRef.Key).To(Equal("shared_secret"))
		g.Expect(got.Spec.Logging).NotTo(BeNil())
		g.Expect(got.Spec.Logging.Level).To(Equal("INFO"))
	})
}
