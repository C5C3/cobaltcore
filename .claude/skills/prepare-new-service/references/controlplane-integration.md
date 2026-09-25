# ControlPlane integration mechanics (verified 2026-09-23 at `12202e40`, re-verify at HEAD)

Read this when answering the profile's database, message-bus, service-user
and catalog questions (step 1) and when drafting the Phase 4 checkboxes
(step 5). It names the code a service leg in `operators/c5c3/` touches;
[../SKILL.md](../SKILL.md) § The five layers lists the enumeration points.

## Database credentials

A database-backed service adopts the shared
`database.ReconcileUpgrade` flow (`internal/common/database/upgrade.go`),
keying it off the image tag (keystone) or `spec.openStackRelease`
(glance), or documents in its reference set why single-pass is
acceptable. A DB service also inherits the **dynamic
credential chain**: the shared `credentialsMode` on `DatabaseSpec`, a
per-service `databaseCredentialsMode` override on its c5c3 service spec,
a `SERVICE_TENANTS` row (`"<role> <spec-service> <schemas>"`, which
`provision_service_tenant` consumes) in
`deploy/openbao/bootstrap/setup-database-tenant.sh`, a `<role>-db` auth
role in `setup-auth.sh` + a `deploy/openbao/policies/<role>-db-dynamic.hcl`
policy, the generator-backed ExternalSecret
glue (`reconcile_<svc>_dbcredentials.go` on the shared builders in
`reconcile_dbcredentials.go`), and a
`migrate-<svc>-db-to-dynamic-credentials` guide (keystone, glance,
placement, barbican and cinder ship one). The row, the policy path and the
auth role must agree on one name, so they land in one change with one
test (#1037). Also extend the per-service loops the kind devstack keeps:
the seed loop in `deploy/openbao/bootstrap/write-bootstrap-secrets.sh` and
the MariaDB tenant wait set in `hack/deploy-infra.sh`.

**Several databases** (#1012, consumed by Nova): a second `DatabaseSpec`
block is named by convention, not by a new field — the consumer passes a
derived instance name (`<cr>-api`) to `ReconcileProvision`,
`ReconcileConnectionSecret` and `FinalizeResources`, and extra schemas on
the same SQL user go in `ProvisionFlowParams.AdditionalDatabaseNames`
(objects named by `AdditionalResourceName`, e.g. `nova-nova-cell0`). The
recipe paragraph "A second database block" lives in
`docs/contributing/adding-a-new-operator.md`. One `SERVICE_TENANTS` row
per engine role, with a comma-separated schema list. **Role names must be
prefix-free**: the engine role is `<role>-<namespace>`, so a role `nova`
in namespace `api-x` and a role `nova-api` in namespace `x` would both
flatten to `nova-api-x`; Nova's roles are therefore `nova-api` and
`nova-cell`, and `test_shipped_role_names_are_prefix_free`
(`tests/unit/deploy/setup_database_tenant_multi_schema_test.sh`) pins
the rule. The c5c3 side gets one credential target per block
(`reconcile_nova_dbcredentials.go`).

## Message bus

The shared bus exists: `commonv1.MessagingSpec` at
`spec.infrastructure.messaging` (opt-in, managed `clusterRef` or brownfield
`secretRef`), the `RabbitmqCluster` the ControlPlane provisions in its own
namespace, and the rabbitmq-cluster-operator delivered through Flux. So does
the consumer side, in `internal/common/messaging`:
`ReconcileTransportURLSecret` turns either mode into the derived
`<instance>-transport-url` Secret, `TransportURLEnvVar` is the
`OS_DEFAULT__TRANSPORT_URL` override that sources it, `RabbitSection`
renders the `[oslo_messaging_rabbit]` posture, and `EgressPort` gives the
NetworkPolicy port. Neutron was its first consumer (#904 built it);
Cinder and Nova followed. The helper reads and
writes in the consumer's own namespace through the consumer's own client, so
a consumer on another cluster or in another namespace than the bus is handed
a brownfield `secretRef` by whoever projects it. On the ControlPlane side that
projector is `reconcileServiceMessaging` (`reconcile_service_messaging.go`),
called on the consumer's `<svc>MessagingTarget(cp)` in `reconcile_<svc>.go`
(`neutronMessagingTarget`, `cinderMessagingTarget`, `novaMessagingTarget`;
#980 replaced the Neutron-only file): it resolves
`spec.infrastructure.messaging` read-only through
`messaging.ResolveTransportURL`, writes
`{cp}-<svc>-messaging` (and `{cp}-<svc>-messaging-ca` when the bus declares
`tls`) into the service's own namespace on the service's own cluster, and
hands the child a brownfield `secretRef` naming it (never
`-transport-url`, which the service operator claims). The validating
webhook (`validateMessagingConsumers`) requires
`spec.infrastructure.messaging` beside `services.neutron`,
`services.cinder` and `services.nova`, because each of those CRDs
requires `spec.messaging`; a new consumer adds its arm there.

Image and probe side: a service whose every process needs a live broker
ships an `<svc>-amqp-ready` readiness probe in its image (cinder's, copied
by nova). A process that only parses the URL can run on a placeholder:
Neutron's standalone e2e fixtures use a bogus `rabbit://` URL at the
`.invalid` TLD, and only its broker-outage chaos suite creates a real
`RabbitmqCluster`.

## Service user

A service user (`[keystone_authtoken]`-style) is the `account` block of
the `KeystoneService` child the ControlPlane projects for the service:
user `<svc>`, its own project `service-<svc>` with `create: true`, and
the roles the builder passes, all assembled by `builtinRegistration` in
`builtin_registrations.go`. `service` alone is the default; a service
that acts on user-owned resources of another service in admin contexts
holds `service` + `admin` (cinder deletes an encrypted volume's Barbican
secret, #979 D9; nova calls cinder through its `[cinder]` user, #1019
D10). Add the two name constants beside
`GlanceServiceAccountName` / `GlanceServiceProjectName` in
`controlplane_webhook.go`. Each service creates its own project;
sharing one would make two registrations adopt each other's Keystone
row. A second account for one service (a notifier) is an account-only
registration that references the service's project instead of creating
it: `desiredNeutronNovaNotifierRegistration` (`neutron-nova`, `service` +
`admin`, because nova answers a notifier holding `service` alone with
404). The registration delivers a consumer Secret `{child}-credentials`
carrying `clouds.yaml` and `password` keys, and the service leg reads
only the `password` key into the child's service-user secret ref
(`reconcile_glance.go` is the template). A service placed on a target
cluster gets those credentials mirrored there by
`ensureBuiltinRegistrationMirror`; a dedicated service namespace gets
its tenant store from `reconcileRegistrationTenantStores`. Glance was
the first consumer and #654/#655 are the pre-work templates; #846 is
the mechanism.

## Service catalog

The catalog entry is the `catalog` block of the same projected
`KeystoneService` child. `builtinRegistration`
registers **public and internal from birth** (the glance D6 posture is
now the builder's shape), so adding an interface later is still a
catalog migration. The `internal` URL is the service's
`<svc>EndpointURL` in `reconcile_<svc>.go` wrapped by
`internalCatalogURL`; the `public` URL is `<svc>CatalogURL` in
`reconcile_catalog.go`, which prefers an explicit `publicEndpoint`,
then the gateway hostname, then the in-cluster URL. A service whose API
lives under a path carries it in both URLs (cinder `/v3`, nova `/v2.1`),
and the service type is the current one (`block-storage`, which Horizon
and keystoneauth resolve for the legacy `volumev3` alias, #979 D8). Do
**not** add a
row to `managedCatalogRows`: that table holds the identity row alone
and never gains another. Teardown needs no `reconcile_delete.go` edit
either, since the registration's own finalizer removes its rows and
`deleteRegistrationsBeforeTeardown` sweeps every projected child; what
the service leg owns is deleting its registration in
`deleteOrphaned<Svc>`. Add the service's row to
`declaredServiceTargetClusters` (`controlplane_webhook.go`) with
`catalog: true`, which is what requires a placed service to publish a
reachable URL. A service the ControlPlane will not manage skips all of
this and registers itself: see
`docs/guides/register-a-foreign-service.md`.

## Public surface

Ingress is `commonv1.GatewaySpec` / HTTPRoute via
`internal/common/gateway` — plus the full public surface that hangs off
it: an `https-<svc>` listener + hostname on the shared kind Gateway
(`deploy/kind/base/openstack-gateway.yaml`) with a
`<svc>-nip-io-tls-certificate.yaml` Certificate — one pair per public
hostname (glance: `glance` + `glance-upload`; nova: `nova`,
`nova-metadata`, `nova-console`, one `GatewaySpec` each) —, a `publicEndpoint`
catalog override on the c5c3 service spec (webhook-validated; it may be
projected into no child, making the webhook the only gate), a
`gateway-quick-start-smoke` e2e suite driving real host→listener→pod
traffic, and the quick-start walkthrough doing its user-facing calls
through the gateway. Two routes on one hostname are a trap: the Gateway
API hands an equal match to the older route, so nova's smoke suite runs
`concurrent: false` beside the console-proxy suite that shares its
console hostname.

## Dependencies, satellites and contracts

- **Cross-service dependencies** gate the service leg on the dependency's
  condition (Horizon gates on `KeystoneReady`). A hard runtime dependency
  becomes an admission rule too: `validateNovaDependencies` requires
  `services.placement`, `services.neutron` and `services.glance` beside
  `services.nova`, the first cross-service rules in the ControlPlane
  webhook. The dependency also sets the e2e substrate: the nova e2e leg
  deploys five sibling operators (keystone, placement, glance, ovn,
  neutron) and loads their images.
- **Satellite backends** are projected as a curated `backends[]` list and
  pruned with `pruneProjectedChildren` / swept with
  `sweepProjectedChildren` (`reconcile_projected_children.go`, #980); the
  satellite entry names are bounded by `projectedChildNameBound` in
  `controlplane_webhook.go`.
- **A published contract for another cluster** (Nova's
  `{nova}-compute-config` Secret for nova-compute, written by
  `reconcile_computeconfig.go`) is mirrored by the ControlPlane through a
  seam of its own (`novaComputeConfigMirrorTargets` in
  `reconcile_nova.go`, #1013 Q4).
