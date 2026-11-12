# Customize reusable bases

These independent overlays demonstrate the [public base catalog](../../bases/README.md).
They render locally without network access or generated inputs:

```sh
kubectl kustomize deploy/examples/base-customization/operators
kubectl kustomize deploy/examples/base-customization/shared-openbao
```

The first changes Keystone **controller** replicas/resource requests. The second
selects example storage for a **new shared OpenBao** installation while preserving
HA and TLS. Replace `example-fast-storage` with an actual storage class; the
numbers are illustrative, not capacity recommendations or a PVC migration plan.

Copy the overlay you need into your deployment repository and replace its local
resource reference with a relative vendored checkout or a pinned fork Git URL as
shown in the catalog. Retain prerequisites and separate stages. These examples do
not supply Flux bootstrap, TLS, initialization/unseal, a complete GitOps deployment
graph or a ControlPlane. Do not apply the shared OpenBao stage before satisfying
those prerequisites. For service sizing, use the existing [two-phase sizing
example](../sizing-overlay/) and ControlPlane sizing fields.
