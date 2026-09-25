# Placement on target clusters (verified 2026-09-23 at `12202e40`, re-verify at HEAD)

Read this when answering the profile's "operator-side dial-outs when
placed?" question and when drafting the Phase 2 (operator) and Phase 4
(ControlPlane) checkboxes: the five artefacts are scaffold, not follow-up.

Since the multicluster conversion, every service CR is born placeable:
`spec.targetClusterRef` keeps the CR, its status, and its webhook on the
management cluster while every projected child lands on the named target.
The five artefacts below are one contract and land with the scaffold —
[[check-service-parity]] P12 audits them, and a follower service picking
up only the spec field is the drift it exists to catch:

- **API** — `TargetClusterRef *commonv1.TargetClusterRefSpec` on the
  Spec (shared type in `internal/common/types`; carries its own CEL
  immutability rule).
- **Webhook** — mirror the immutability via
  `validation.TargetClusterRefImmutable` plus the create-time checks
  via `validation.TargetClusterRef`.
- **invalid-cr fixture** — the `targetclusterref-empty-name` rejection
  (every operator's corpus carries one; copy the keystone shape).
- **Controller** — the multicluster builder
  (`mcbuilder.ControllerManagedBy` with `EngageLocalCluster` /
  `EngageNoProviderClusters`), remote requests mapped via
  `commonmulticluster.TargetClusterOf`, remote child watches via
  `AddRemoteChildWatches`, and **every** child write through the
  resolved children client (`ResolveChildrenClient`; a resolver miss
  fails the operator's first gate condition with reason
  `TargetClusterUnavailable`). Remote children are written label-owned
  via the ownership claim (`Claim` stamps labels instead of a
  cross-cluster ownerReference, which cannot exist).
- **Deletion** — a conditional `RemoteChildrenFinalizer` plus
  `SweepRemoteChildren`: nothing cascades for label-owned remote
  children, so an operator that skips the sweep leaks every placed
  child.

The profile question is
what *breaks* when the children live elsewhere: an operator that dials
a cluster-local endpoint itself (OpenBao provisioning, DB admin
connections) needs the port-forward tunnel seam
(`commonmulticluster.NewPortForwardDialer` — barbican's OpenBao dials
are the worked example), HTTP health probes go through
`ResolveHTTPDoer` (the API-server service proxy when placed), and any
optional child kind (HTTPRoute) needs the per-cluster capability probe
(`ChildrenServeKind`).

What stays service-specific: cluster-local dial-outs from the operator
need the port-forward tunnel (`NewPortForwardDialer` — barbican's
OpenBao provisioning is the worked example), HTTP health probes route
through `ResolveHTTPDoer` (API-server service proxy when placed), and
optional child kinds gate on the per-cluster capability probe
(`ChildrenServeKind` — the HTTPRoute precedent). Prove the split on the
dual envtest (`internal/common/testutil/multicluster`; keystone's
two-cluster and remote-teardown tests are the templates:
`TestIntegration_Multicluster_KeystoneTargetCluster` in
`operators/keystone/internal/controller/multicluster_integration_test.go`).
Keep **one kubeconfig provider per test binary**: the multicluster envtest
is the only test that registers a provider, and the controller needs a
`setupWithOptions` seam (with `SkipNameValidation`, the keystone shape)
that the single-cluster envtests in the same binary register through too.
The c5c3 side
adds `targetClusterRef` to the service's `Service<Svc>Spec`, the
placement rules to the ControlPlane webhook (+ invalid-cr fixtures),
and the placed ensemble to the deletion sweep. Whether the service
joins the two-cluster placed-services suite
(`tests/e2e-multicluster/placed-services/`, keystone + barbican +
OVNCentral + neutron today) is a Phase-3
decision — membership also means extending the hard-coded image list
in the ci.yaml `e2e-multicluster` job. Nova stayed out (#1038, author):
every chart grant it would exercise is already exercised by the four
members, and its scheduler/conductor readiness needs a live broker the
placed Neutron's `.invalid` transport URL does not provide — record the
same reasoning when a new service stays out.
`docs/reference/target-clusters.md`
is the authoritative contract (ownership labels, teardown order,
per-service placement notes) and gains the new service's row in Phase 5.

Every child kind a placed service projects must be grantable by the
target-cluster access chart
(`deploy/target-cluster/target-cluster-access/templates/clusterrole.yaml`,
pinned by `tests/unit/ci/target_cluster_chart_output_test.sh`). A
cluster-scoped child is the warning sign: Cinder mounts its NFS shares as
inline CSI volumes (`volumes[].csi`, driver `nfs.csi.k8s.io`) rather than a
static PV/PVC pair, because a placed Cinder would otherwise need an
unbounded `persistentvolumes` write in that ClusterRole (#987, author
decision).
