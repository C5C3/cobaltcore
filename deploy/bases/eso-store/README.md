# Shared ESO store stage

Build with `kubectl kustomize deploy/bases/eso-store`. Owns only the
`openbao-cluster-store` ClusterSecretStore from [deploy/eso](../../eso/).
Requires ESO installed, TLS material and initialized/auth-configured shared OpenBao.
It installs no controller and creates no application ExternalSecret.

The store uses mTLS plus Kubernetes authentication as `eso-management`.
Its `openstack`/`shared-services` namespace allow-list is an authorization boundary.
Do not widen it as a namespace-routing shortcut. Most ControlPlane credential
traffic uses separately provisioned namespaced tenant stores.

Patch endpoint/trust/auth settings only together with the backend and corresponding
bootstrap contract. Preserve the store name referenced by infrastructure secrets.
Wait for validation and required secret synchronization before consumers proceed.
Deleting the store breaks refresh for its consumers; it is not a secret cleanup
operation. See the [OpenBao lifecycle](../../openbao/README.md).
