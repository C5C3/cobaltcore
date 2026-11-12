# Operator stage

Build with `kubectl kustomize deploy/bases/operators` from the repository root.
Requires installed Flux controllers/APIs. Owns the shared namespace declarations,
Helm/OCI/Git sources, third-party controllers and OpenStack service operator
reconciliation objects. It does **not** own FluxInstance, shared OpenBao, c5c3,
service instances, gateways or bootstrap Jobs.

The source catalog is [sources](../../flux-system/sources/kustomization.yaml);
the controller catalog is [releases](../../flux-system/releases/kustomization.yaml).
They are direct, positive resource lists. A new controller belongs in those lists,
with its namespace in the existing [raw namespace file](../../flux-system/namespaces.yaml).

This stage retains a three-object compatibility filter over the legacy aggregate.
Kustomize's default file restrictions prevent a new directory from directly
loading the raw namespace file. Keeping the filter preserves raw callers and
avoids a generated namespace copy or symlink. Regression checks enforce the stage
boundaries, source references and namespace pruning protection.

Patch a specific HelmRelease's `spec.values` using its chart schema; patch an
upstream Flux Kustomization's `spec.patches`/`spec.images` for that installer.
Preserve pinned source/image relationships and sourceRefs. The
[customization example](../../examples/base-customization/operators/) changes
Keystone controller resources. Review rendered output before reconciliation.

Namespaces are protected from pruning. Other objects follow the consuming Flux
Kustomization's policy: deleting a HelmRelease can uninstall its chart. Retain the
same reconciler and object identities when changing only a source path.
