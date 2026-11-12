# Shared/bootstrap OpenBao stage

Build with `kubectl kustomize deploy/bases/shared-openbao`. Owns only HelmRelease
`shared-services/openbao`; requires the namespace/source from the operator stage
and TLS. The source of truth is the [existing HelmRelease](../../flux-system/releases/openbao/helmrelease.yaml).
The base retains HA/three Raft replicas and mTLS, and removes the legacy
`local-path` storage-class pin so the site can select a class.

This is the shared credential/bootstrap backend, called management OpenBao in
existing configuration. That name describes its role; the split devstack runs it
on the infrastructure target cluster. It is distinct from the OpenBao operator,
the development proving instance and runtime dedicated service instances.

For a new install, patch `spec.values.server.dataStorage.storageClass` and `size`;
see the [renderable example](../../examples/base-customization/shared-openbao/).
Existing PVC/storage changes need a separate migration. Selecting one replica
alone does not produce the development standalone profile; that profile also
changes Raft configuration and omits Kubernetes service registration.

Rendering or installing the HelmRelease does not initialize/unseal/configure the
server. Supply the environment-owned [bootstrap lifecycle](../../openbao/README.md).
Its delivery gate must allow bootstrap to run before requiring unsealed readiness.
Choose retention/pruning deliberately: removing this release can remove the server;
protecting a namespace alone does not protect a Helm release or its data.
