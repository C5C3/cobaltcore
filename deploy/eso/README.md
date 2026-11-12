# Shared ESO store

This compatibility entry point renders the shared
`ClusterSecretStore/openbao-cluster-store`. New consumers can use the
[`eso-store` base](../bases/eso-store/). It does not install External Secrets
Operator or declare every application's ExternalSecret.

The store requires ESO, the OpenBao CA/client certificate Secrets, and a shared
OpenBao server with its Kubernetes auth mount, role and policies configured.
Client mTLS admits the transport connection; Kubernetes authentication and
OpenBao policy still determine authorization.

The store permits references from `openstack` and `shared-services`, the
namespaces containing its static bootstrap consumers. That allow-list is a
privilege boundary. ControlPlane-managed secret traffic uses separate
tenant-specific stores; adding every service namespace here would change the
authorization model.

See the [store manifest](clustersecretstore.yaml),
[OpenBao roles and bootstrap](../openbao/README.md), and
[base catalog](../bases/README.md) for composition and deployment order. Preserve
the store's identity and one reconciliation owner when changing its import path.
