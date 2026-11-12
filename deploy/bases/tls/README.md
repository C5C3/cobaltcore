# TLS stage

Build with `kubectl kustomize deploy/bases/tls`. Requires declared namespaces and
ready cert-manager APIs, controller and admission webhook.

Owns the self-signed issuer, OpenBao CA/server/client certificates and database/OVN
trust issuers from the [canonical TLS list](../../flux-system/infrastructure/tls/kustomization.yaml).
It installs neither cert-manager nor an OpenBao server. The legacy infrastructure
aggregate uses the same list, so changes have one selection point.

Use targeted patches for site certificate/issuer requirements; preserve issuerRef,
Secret names, server identities and client-auth usages unless migrating all
consumers. OpenBao mTLS transport does not replace Kubernetes/AppRole authorization.

`make deploy-infra` still applies its early TLS subset in the established order;
this base does not change those waits. A GitOps consumer must wait for usable TLS
before shared OpenBao delivery. Treat issuer/certificate removal as a trust and
secret-lifecycle change, not a harmless directory cleanup.
