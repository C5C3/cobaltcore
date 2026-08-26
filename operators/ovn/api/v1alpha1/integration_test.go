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
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/ovn/internal/testutil"
)

// --- Helpers ---

// setupEnvTestNoWebhook wraps testutil.SetupOVNEnvTestNoWebhook with the
// v1alpha1 scheme registration, avoiding the import cycle between testutil and
// this package. No webhook is installed, so every rejection below is attributable
// to the CRD schema alone: the OVN rules are enforced twice, and a SetupOVNEnvTest
// based test could keep passing on the validating webhook after a CEL rule was
// dropped.
func setupEnvTestNoWebhook(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupOVNEnvTestNoWebhook(t, AddToScheme)
}

// newNamespace creates a uniquely named namespace for a test.
func newNamespace(t testing.TB, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create namespace")
	return ns.Name
}

// integrationCentral returns validOVNCentral() stamped with name/namespace for
// API server submission. The webhook-unit fixture already carries the one
// required field (spec.tls.issuerRef.name), so nothing has to be added for the
// object to clear the real CRD schema.
func integrationCentral(name, namespace string) *OVNCentral {
	central := validOVNCentral()
	central.Name = name
	central.Namespace = namespace
	return central
}

// integrationChassis returns validOVNChassis() stamped with name/namespace and
// pointed at centralRef, for API server submission.
func integrationChassis(name, namespace, centralRef string) *OVNChassis {
	chassis := validOVNChassis()
	chassis.Name = name
	chassis.Namespace = namespace
	chassis.Spec.CentralRef = OVNCentralRef{Name: centralRef}
	return chassis
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
// targetClusterRef rename rule on both kinds: re-pointing a live control plane
// or a live chassis at another target cluster is rejected by the CRD CEL rule
// alone, without the validating webhook.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("OVNCentral", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "central-targetcluster-rename-")

		central := integrationCentral("ovn", ns)
		central.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, central)).To(Succeed(), "valid OVNCentral should be accepted")

		got := &OVNCentral{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef.Name = "cluster-b"

		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})

	t.Run("OVNChassis", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "chassis-targetcluster-rename-")

		chassis := integrationChassis("chassis", ns, "ovn")
		chassis.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, chassis)).To(Succeed(), "valid OVNChassis should be accepted")

		got := &OVNChassis{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "chassis", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef.Name = "cluster-b"

		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})
}

// TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip pins the
// presence rule of both kinds in both directions: a CR created without the ref
// cannot gain one, and a CR created with it cannot drop it. Either edit would
// move the children away from the cluster that already holds them, and for an
// OVNChassis it would leave the chassis registrations behind in a Southbound
// database nothing points at any more.
func TestIntegration_CRD_CELOnly_RejectsTargetClusterRefPresenceFlip(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("OVNCentral", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "central-targetcluster-flip-")

		local := integrationCentral("ovn-local", ns)
		g.Expect(c.Create(ctx, local)).To(Succeed(), "OVNCentral without targetClusterRef should be accepted")

		got := &OVNCentral{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn-local", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")

		remote := integrationCentral("ovn-remote", ns)
		remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, remote)).To(Succeed(), "OVNCentral with targetClusterRef should be accepted")

		got = &OVNCentral{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn-remote", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = nil
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})

	t.Run("OVNChassis", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "chassis-targetcluster-flip-")

		local := integrationChassis("chassis-local", ns, "ovn")
		g.Expect(c.Create(ctx, local)).To(Succeed(), "OVNChassis without targetClusterRef should be accepted")

		got := &OVNChassis{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "chassis-local", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")

		remote := integrationChassis("chassis-remote", ns, "ovn")
		remote.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "cluster-a"}
		g.Expect(c.Create(ctx, remote)).To(Succeed(), "OVNChassis with targetClusterRef should be accepted")

		got = &OVNChassis{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "chassis-remote", Namespace: ns}, got)).To(Succeed())
		got.Spec.TargetClusterRef = nil
		expectRejected(t, c.Update(ctx, got), "targetClusterRef is immutable")
	})
}

// TestIntegration_CRD_CELOnly_RejectsReplicasChange pins the Raft membership
// rule: growing a running database from three members to five needs an
// ovsdb-tool procedure against the live cluster that the operator does not
// perform, so the field is frozen at the schema layer rather than applied to a
// StatefulSet the members would not join.
func TestIntegration_CRD_CELOnly_RejectsReplicasChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "replicas-immutable-")

	central := integrationCentral("ovn", ns)
	central.Spec.Northbound.Replicas = 3
	g.Expect(c.Create(ctx, central)).To(Succeed(), "a three-member Northbound cluster should be accepted")

	got := &OVNCentral{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn", Namespace: ns}, got)).To(Succeed())
	got.Spec.Northbound.Replicas = 5

	expectRejected(t, c.Update(ctx, got), "replicas is immutable")
}

// TestIntegration_CRD_CELOnly_RejectsElectionTimerChange pins the election-timer
// rule: the value is written into the database when its first member creates it,
// so a later edit would reach the ovn-ctl option and not the database.
func TestIntegration_CRD_CELOnly_RejectsElectionTimerChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "electiontimer-immutable-")

	central := integrationCentral("ovn", ns)
	g.Expect(c.Create(ctx, central)).To(Succeed(), "valid OVNCentral should be accepted")

	got := &OVNCentral{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.Southbound.ElectionTimerMs).To(BeEquivalentTo(1000),
		"the CRD default must materialize before the transition rule can be exercised")
	got.Spec.Southbound.ElectionTimerMs = 5000

	expectRejected(t, c.Update(ctx, got), "electionTimerMs is immutable")
}

// TestIntegration_CRD_CELOnly_RejectsStorageChange pins the storage rule: the
// size and the class come from the StatefulSet's volumeClaimTemplates, which the
// API server itself refuses updates to, so the CRD reports the constraint at
// admission instead of letting the apply fail on every later pass.
func TestIntegration_CRD_CELOnly_RejectsStorageChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "storage-immutable-")

	central := integrationCentral("ovn", ns)
	g.Expect(c.Create(ctx, central)).To(Succeed(), "valid OVNCentral should be accepted")

	got := &OVNCentral{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn", Namespace: ns}, got)).To(Succeed())
	g.Expect(got.Spec.Northbound.Storage.Size).To(Equal("1Gi"),
		"the CRD default must materialize before the transition rule can be exercised")
	got.Spec.Northbound.Storage.Size = "10Gi"

	expectRejected(t, c.Update(ctx, got), "storage is immutable")
}

// TestIntegration_CRD_CELOnly_RejectsEvenReplicas pins the odd-count rule on
// create: an even Raft cluster tolerates no more failures than the odd one below
// it and has two ways to split the vote.
func TestIntegration_CRD_CELOnly_RejectsEvenReplicas(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "even-replicas-")

	central := integrationCentral("ovn", ns)
	central.Spec.Northbound.Replicas = 2

	expectRejected(t, c.Create(ctx, central), "replicas must be odd")
}

// TestIntegration_CRD_CELOnly_RejectsCentralRefChange pins the centralRef rule:
// re-pointing a live chassis at another OVNCentral leaves its registration
// behind in the old Southbound database, where it keeps claiming the ports of
// workloads that have moved on.
func TestIntegration_CRD_CELOnly_RejectsCentralRefChange(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "centralref-immutable-")

	chassis := integrationChassis("chassis", ns, "ovn-a")
	g.Expect(c.Create(ctx, chassis)).To(Succeed(), "valid OVNChassis should be accepted")

	got := &OVNChassis{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "chassis", Namespace: ns}, got)).To(Succeed())
	got.Spec.CentralRef.Name = "ovn-b"

	expectRejected(t, c.Update(ctx, got), "centralRef is immutable")
}

// TestIntegration_CRD_CELOnly_RejectsEmptyNodeSelector pins the minProperties
// bound on spec.nodeSelector: an empty selector matches every node in the
// cluster, which would start ovn-controller on the control-plane nodes and on
// whatever else joins later.
func TestIntegration_CRD_CELOnly_RejectsEmptyNodeSelector(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)
	ns := newNamespace(t, ctx, c, "empty-nodeselector-")

	chassis := integrationChassis("chassis", ns, "ovn")
	chassis.Spec.NodeSelector = map[string]string{}

	expectRejected(t, c.Create(ctx, chassis), "spec.nodeSelector")
}

// TestIntegration_CRD_NestedDefaultsMaterialized proves the API server fills the
// nested schema defaults of both kinds without a mutating webhook. Every value
// below sits under an object the CR omits entirely, so it only materializes when
// the parent block carries its own empty-object default: drop one of those and
// the operator would read a zero replica count, a zero election timer and an
// unparseable empty storage size off a CR that looks complete.
func TestIntegration_CRD_NestedDefaultsMaterialized(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)

	c, ctx, _ := setupEnvTestNoWebhook(t)

	t.Run("OVNCentral", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "central-defaults-")

		central := &OVNCentral{
			ObjectMeta: metav1.ObjectMeta{Name: "ovn", Namespace: ns},
			Spec: OVNCentralSpec{
				TLS: OVNTLSSpec{IssuerRef: OVNIssuerRef{Name: "openstack-ovn-ca"}},
			},
		}
		g.Expect(c.Create(ctx, central)).To(Succeed(), "an OVNCentral with only the issuer name should be accepted")

		got := &OVNCentral{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "ovn", Namespace: ns}, got)).To(Succeed())

		for _, db := range []struct {
			what string
			spec OVNDatabaseSpec
		}{
			{"northbound", got.Spec.Northbound},
			{"southbound", got.Spec.Southbound},
		} {
			g.Expect(db.spec.Replicas).To(BeEquivalentTo(3), "%s.replicas", db.what)
			g.Expect(db.spec.Storage.Size).To(Equal("1Gi"), "%s.storage.size", db.what)
			g.Expect(db.spec.ElectionTimerMs).To(BeEquivalentTo(1000), "%s.electionTimerMs", db.what)
			g.Expect(db.spec.InactivityProbeMs).To(BeEquivalentTo(60000), "%s.inactivityProbeMs", db.what)
		}
		g.Expect(got.Spec.Northd.Threads).To(BeEquivalentTo(1), "northd.threads")
		g.Expect(got.Spec.TLS.IssuerRef.Kind).To(Equal("ClusterIssuer"), "tls.issuerRef.kind")
	})

	t.Run("OVNChassis", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ns := newNamespace(t, ctx, c, "chassis-defaults-")

		chassis := &OVNChassis{
			ObjectMeta: metav1.ObjectMeta{Name: "chassis", Namespace: ns},
			Spec: OVNChassisSpec{
				CentralRef:   OVNCentralRef{Name: "ovn"},
				NodeSelector: map[string]string{"openstack.c5c3.io/network-node": "true"},
			},
		}
		g.Expect(c.Create(ctx, chassis)).To(Succeed(),
			"an OVNChassis with only centralRef and nodeSelector should be accepted")

		got := &OVNChassis{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: "chassis", Namespace: ns}, got)).To(Succeed())

		g.Expect(got.Spec.EncapType).To(Equal("geneve"), "encapType")
		g.Expect(got.Spec.UpdateStrategy.Type).To(Equal("RollingUpdate"), "updateStrategy.type")
		g.Expect(got.Spec.RemoteProbeIntervalMs).To(BeEquivalentTo(60000), "remoteProbeIntervalMs")
	})
}
