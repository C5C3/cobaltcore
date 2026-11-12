# metal-stack lab

This environment deploys to the existing cluster selected by the current
kubeconfig. It reuses kind development definitions and adds Gardener networking
labels, default-class storage, a public gateway, persistent metrics and optional
Ceph/physical-hypervisor support. These differences are intentional.

Start with the prerequisites and cluster selection in the
[metal-stack quick start](../../../docs/quick-start-metal-stack.md). From the
repository root, its ControlPlane/Ceph infrastructure workflow is:

```bash
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_CEPH=true make deploy-infra
```

No kind cluster is created. The script defaults `EXTERNAL_OVERLAY` to
`deploy/lab/metal-stack`. It prepares infrastructure and prints the next steps;
the lab ControlPlane is applied separately as described by the quick start.

## Entry points and order

| Directory | Purpose and caller |
| --- | --- |
| [`base/`](base/) | `deploy-infra` controller/release phase; inherits kind sizing and replaces gateway settings |
| [`infrastructure/`](infrastructure/) | `deploy-infra` CR phase; inherits shared development instances and replaces kind proxy/certificates |
| `chaos-mesh/`, `dizzy/`, `nfs/`, `prometheus/` | Matching opt-in flags, with lab-specific namespace, persistence and node preparation settings |
| [`ceph/`](ceph/), [`ceph/cluster/`](ceph/cluster/) | `WITH_CEPH=true`: operator before CRDs are consumed, then the Ceph cluster and credential handoff |
| [`controlplane/`](controlplane/) | Manual lab ControlPlane and OVNCentral after infrastructure |
| [`controlplane-nfs/`](controlplane-nfs/) | Manual variant adding an NFS backend to the Ceph ControlPlane |
| [`hypervisor-fixtures/`](hypervisor-fixtures/), [`hypervisor/`](hypervisor/) | Manual fixtures and physical compute setup after ControlPlane readiness |
| [`migration-ports/`](migration-ports/) | Port reservation for QEMU migration; also included by `hypervisor/` |
| [`probe/`](probe/) | Node diagnostic Job; its adjacent module-loading manifest is a separate operation |
| [`dizzy-soak/`](dizzy-soak/) | Persistent test resources used by `make dizzy-soak-start` |

`gateway-certificate.yaml`, `nfs/client-policy.yaml`, and the Dizzy soak Job and
scenario are script inputs, not resources included automatically by their
directory's Kustomization. Their runtime values are resolved by the relevant
scripts. Do not recursively apply all YAML files in this tree.

The platform supplies metrics-server and VPA; kind-specific opt-ins are not a
substitute. Prometheus and Dizzy inherit generated dashboard inputs from kind;
their deployment flags stage those inputs before rendering.
The lab Prometheus overlay uses the shared `prometheus-managed` variant, which
omits the Namespace already owned and labeled by the platform base.

## Custom external overlays

An `EXTERNAL_OVERLAY` must keep `base/`, `infrastructure/`, the gateway certificate
template, and the optional child directories required by its enabled flags.
The script validates this contract and relies on named resources for its waits.
Relative overlay paths resolve from the repository root; absolute overlay roots
are also accepted by the script. This is separate from Kustomize's resource-path
rules inside an overlay.

Keep the lab's storage and network choices explicit when deriving another
environment. For full details, including teardown, see the
[lab infrastructure reference](../../../docs/reference/infrastructure/infrastructure-manifests.md#metal-stack-lab)
and [`hack/deploy-infra.sh`](../../../hack/deploy-infra.sh).
