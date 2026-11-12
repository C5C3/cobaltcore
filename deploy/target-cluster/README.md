# Target-cluster access

[`target-cluster-access/`](target-cluster-access/) is a Helm chart installed on
the cluster that will host projected workloads. It creates the access
ServiceAccount/token, RBAC, optional namespaces and the order-resource CRDs that
the target must serve. It does not install the full CobaltCore stack or register
the target with management.

Use the [target-cluster guide](../../docs/guides/deploy-to-a-target-cluster.md) to:

1. Prepare the target's infrastructure and install the access chart with the
   intended namespace grants.
2. Assemble a kubeconfig for the chart's access identity.
3. Create the registration Secret on the management cluster.
4. Place a workload using that registered cluster name and verify it.

The chart's `namespaces` and `assignedNamespaces` grants must match the
registration Secret. Set `createNamespaces=false` for namespaces already owned
by another installation. Review the
[chart values](target-cluster-access/values.yaml) for optional cluster-level
grants and namespace behavior.

The CRDs in this chart are intentional packaging copies of c5c3 CRDs. Keep them
synchronized with `make verify-crd-sync`; do not remove them as duplicate
manifests. Existing targets need the documented explicit CRD upgrade procedure.
`make helm-package-target-cluster` packages this chart from its existing path.

See the [target-cluster reference](../../docs/reference/target-clusters.md) for
registration, placement, lifecycle and authorization details.
