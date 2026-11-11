# Documentation Review Tasks

Review each listed page from first line through its final section. For every page, run the consistency, expression, and structure checks; run drift checks wherever the page describes code, CRDs, deployment, tests, CI, or release behavior. Record concrete source evidence for factual claims. Apply mechanical findings with `fix-docs`; pause for confirmation on judgment calls and research unresolved claims before editing. Re-run the originating checks after fixes. Commit the page changes and this task's completion record before checking it off.

## Architecture
- [x] `docs/architecture/core-components.md` — read all sections; service/CRD inventory checked against `ServicesSpec` and `operators/c5c3/api/v1alpha1/`; infrastructure names checked against `deploy/flux-system/`; metrics checked against operator instrumentation; database and vhost cadence checked against their API types; Cyberus approval and roadmap checked against the linked announcement. Added the missing `SecretAggregate` CRD link. Follow-up outside docs scope: `ControlPlane` type comment still says `today: keystone` while `ServicesSpec` has eight services.
- [ ] `docs/architecture/index.md`

## Contributing
- [ ] `docs/contributing/adding-a-new-operator.md`
- [ ] `docs/contributing/adding-a-new-release.md`
- [ ] `docs/contributing/architecture-diagrams.md`
- [ ] `docs/contributing/claude-skills.md`
- [ ] `docs/contributing/dependency-management.md`
- [ ] `docs/contributing/guide-conventions.md`
- [ ] `docs/contributing/nix-dev-environment.md`
- [ ] `docs/contributing/publishing-to-cobaltcore-dev.md`

## Guides
- [ ] `docs/guides/advanced-configuration.md`
- [ ] `docs/guides/day-2-operations.md`
- [ ] `docs/guides/deploy-to-a-target-cluster.md`
- [ ] `docs/guides/dedicated-service-namespaces.md`
- [ ] `docs/guides/end-to-end-sso.md`
- [ ] `docs/guides/multi-tenant-deployment.md`
- [ ] `docs/guides/observability.md`
- [ ] `docs/guides/order-resources-from-a-controlplane.md`
- [ ] `docs/guides/register-a-foreign-service.md`
- [ ] `docs/guides/barbican/attach-external-openbao.md`
- [ ] `docs/guides/barbican/barbican-dedicated-openbao.md`
- [ ] `docs/guides/barbican/enable-barbican-operator-metrics.md`
- [ ] `docs/guides/barbican/enable-barbican-operator-networkpolicy.md`
- [ ] `docs/guides/barbican/migrate-barbican-db-to-dynamic-credentials.md`
- [ ] `docs/guides/cinder/attach-an-nfs-backend.md`
- [ ] `docs/guides/cinder/attach-an-rbd-backend.md`
- [ ] `docs/guides/cinder/configure-nfs-backups.md`
- [ ] `docs/guides/cinder/configure-rbd-backups.md`
- [ ] `docs/guides/cinder/enable-cinder-operator-metrics.md`
- [ ] `docs/guides/cinder/enable-cinder-operator-networkpolicy.md`
- [ ] `docs/guides/cinder/enable-image-volume-cache.md`
- [ ] `docs/guides/cinder/enable-volume-encryption-with-barbican.md`
- [ ] `docs/guides/cinder/migrate-cinder-db-to-dynamic-credentials.md`
- [ ] `docs/guides/glance/enable-glance-image-caching.md`
- [ ] `docs/guides/glance/enable-glance-image-conversion.md`
- [ ] `docs/guides/glance/enable-glance-operator-metrics.md`
- [ ] `docs/guides/glance/enable-glance-operator-networkpolicy.md`
- [ ] `docs/guides/glance/filter-web-download-imports.md`
- [ ] `docs/guides/glance/glance-s3-multistore.md`
- [ ] `docs/guides/glance/large-image-uploads.md`
- [ ] `docs/guides/glance/migrate-glance-db-to-dynamic-credentials.md`
- [ ] `docs/guides/horizon/enable-horizon-operator-metrics.md`
- [ ] `docs/guides/horizon/enable-horizon-operator-networkpolicy.md`
- [ ] `docs/guides/keystone/adopt-external-keystone.md`
- [ ] `docs/guides/keystone/enable-keystone-database-tls.md`
- [ ] `docs/guides/keystone/enable-keystone-operator-metrics.md`
- [ ] `docs/guides/keystone/enable-keystone-operator-networkpolicy.md`
- [ ] `docs/guides/keystone/keystone-admin-password-rotation.md`
- [ ] `docs/guides/keystone/keystone-admin-password-scheduled-rotation.md`
- [ ] `docs/guides/keystone/keystone-key-rotation.md`
- [ ] `docs/guides/keystone/ldap-domain-backend.md`
- [ ] `docs/guides/keystone/migrate-keystone-db-to-dynamic-credentials.md`
- [ ] `docs/guides/keystone/oidc-federation.md`
- [ ] `docs/guides/keystone/saml-federation.md`
- [ ] `docs/guides/neutron/create-a-provider-network.md`
- [ ] `docs/guides/neutron/enable-neutron-operator-metrics.md`
- [ ] `docs/guides/neutron/enable-neutron-operator-networkpolicy.md`
- [ ] `docs/guides/neutron/repair-ovn-drift-with-db-sync.md`
- [ ] `docs/guides/nova/connect-a-compute-cluster.md`
- [ ] `docs/guides/nova/drain-a-compute-node.md`
- [ ] `docs/guides/nova/enable-nova-operator-metrics.md`
- [ ] `docs/guides/nova/enable-nova-operator-networkpolicy.md`
- [ ] `docs/guides/nova/expose-the-console-proxy.md`
- [ ] `docs/guides/nova/migrate-nova-db-to-dynamic-credentials.md`
- [ ] `docs/guides/nova/run-a-fake-compute-for-testing.md`
- [ ] `docs/guides/ovn/drain-a-chassis-node.md`
- [ ] `docs/guides/ovn/enable-ovn-operator-metrics.md`
- [ ] `docs/guides/ovn/enable-ovn-operator-networkpolicy.md`
- [ ] `docs/guides/ovn/label-a-chassis-node.md`
- [ ] `docs/guides/ovn/restore-an-ovn-database-snapshot.md`
- [ ] `docs/guides/placement/enable-placement-operator-metrics.md`
- [ ] `docs/guides/placement/enable-placement-operator-networkpolicy.md`
- [ ] `docs/guides/placement/migrate-placement-db-to-dynamic-credentials.md`
- [ ] `docs/guides/placement/standalone-placement.md`

## Quick Starts
- [ ] `docs/quick-start-controlplane.md`
- [ ] `docs/quick-start-extended.md`
- [ ] `docs/quick-start-metal-stack.md`
- [ ] `docs/quick-start-orders.md`
- [ ] `docs/quick-start.md`

## References: Backend
- [ ] `docs/reference/backend/helm-values-schema.md`
- [ ] `docs/reference/backend/keystone-operator-packaging.md`
- [ ] `docs/reference/backend/kubernetes-packages.md`
- [ ] `docs/reference/backend/rotation-scripts.md`

## References: Barbican
- [ ] `docs/reference/barbican/barbican-crd.md`
- [ ] `docs/reference/barbican/barbican-events.md`
- [ ] `docs/reference/barbican/barbican-reconciler.md`
- [ ] `docs/reference/barbican/barbican-secret-store-crd.md`
- [ ] `docs/reference/barbican/index.md`

## References: C5c3
- [ ] `docs/reference/c5c3/controlplane-crd.md`
- [ ] `docs/reference/c5c3/controlplane-reconciler.md`
- [ ] `docs/reference/c5c3/keystone-orders-reconciler.md`
- [ ] `docs/reference/c5c3/keystoneapplicationcredential-crd.md`
- [ ] `docs/reference/c5c3/keystonecatalogentry-crd.md`
- [ ] `docs/reference/c5c3/keystoneproject-crd.md`
- [ ] `docs/reference/c5c3/keystoneroleassignment-crd.md`
- [ ] `docs/reference/c5c3/keystoneservice-crd.md`
- [ ] `docs/reference/c5c3/keystoneservice-reconciler.md`
- [ ] `docs/reference/c5c3/keystoneuser-crd.md`
- [ ] `docs/reference/c5c3/keystoneuser-reconciler.md`
- [ ] `docs/reference/c5c3/mariadbdatabase-crd.md`
- [ ] `docs/reference/c5c3/mariadbdatabase-reconciler.md`
- [ ] `docs/reference/c5c3/rabbitmqvhost-crd.md`
- [ ] `docs/reference/c5c3/rabbitmqvhost-reconciler.md`

## References: Cinder
- [ ] `docs/reference/cinder/cinder-backend-crd.md`
- [ ] `docs/reference/cinder/cinder-backup-backend-crd.md`
- [ ] `docs/reference/cinder/cinder-crd.md`
- [ ] `docs/reference/cinder/cinder-events.md`
- [ ] `docs/reference/cinder/cinder-reconciler.md`
- [ ] `docs/reference/cinder/cinder-upgrade-flow.md`
- [ ] `docs/reference/cinder/index.md`

## References: CI/CD
- [ ] `docs/reference/ci-cd/build-images-workflow.md`
- [ ] `docs/reference/ci-cd/ci-workflow.md`
- [ ] `docs/reference/ci-cd/container-images.md`
- [ ] `docs/reference/ci-cd/nix-image-build.md`

## References: Glance
- [ ] `docs/reference/glance/glance-backend-crd.md`
- [ ] `docs/reference/glance/glance-crd.md`
- [ ] `docs/reference/glance/glance-events.md`
- [ ] `docs/reference/glance/glance-reconciler.md`
- [ ] `docs/reference/glance/glance-upgrade-flow.md`
- [ ] `docs/reference/glance/index.md`

## References: Horizon
- [ ] `docs/reference/horizon/horizon-crd.md`
- [ ] `docs/reference/horizon/horizon-reconciler.md`
- [ ] `docs/reference/horizon/index.md`

## References: Infrastructure
- [ ] `docs/reference/infrastructure/e2e-deployment.md`
- [ ] `docs/reference/infrastructure/infrastructure-manifests.md`
- [ ] `docs/reference/infrastructure/openbao-bootstrap.md`

## References: Keystone
- [ ] `docs/reference/keystone/identity-backend-crd.md`
- [ ] `docs/reference/keystone/index.md`
- [ ] `docs/reference/keystone/keystone-crd.md`
- [ ] `docs/reference/keystone/keystone-events.md`
- [ ] `docs/reference/keystone/keystone-operator-networkpolicy.md`
- [ ] `docs/reference/keystone/keystone-reconciler.md`
- [ ] `docs/reference/keystone/keystone-schema-drift-detection.md`
- [ ] `docs/reference/keystone/keystone-upgrade-flow.md`
- [ ] `docs/reference/keystone-operator-metrics.md`

## References: Neutron
- [ ] `docs/reference/neutron/index.md`
- [ ] `docs/reference/neutron/neutron-crd.md`
- [ ] `docs/reference/neutron/neutron-events.md`
- [ ] `docs/reference/neutron/neutron-metadata-agent-crd.md`
- [ ] `docs/reference/neutron/neutron-reconciler.md`

## References: Nova
- [ ] `docs/reference/nova/index.md`
- [ ] `docs/reference/nova/nova-cells.md`
- [ ] `docs/reference/nova/nova-crd.md`
- [ ] `docs/reference/nova/nova-events.md`
- [ ] `docs/reference/nova/nova-reconciler.md`
- [ ] `docs/reference/nova/nova-upgrade-flow.md`
- [ ] `docs/reference/nova/novacompute-crd.md`

## References: OVN
- [ ] `docs/reference/ovn/index.md`
- [ ] `docs/reference/ovn/ovn-central-crd.md`
- [ ] `docs/reference/ovn/ovn-chassis-crd.md`
- [ ] `docs/reference/ovn/ovn-events.md`
- [ ] `docs/reference/ovn/ovn-reconciler.md`

## References: Placement
- [ ] `docs/reference/placement/index.md`
- [ ] `docs/reference/placement/placement-crd.md`
- [ ] `docs/reference/placement/placement-events.md`
- [ ] `docs/reference/placement/placement-reconciler.md`

## References: Testing
- [ ] `docs/reference/testing/chaos-e2e-tests.md`
- [ ] `docs/reference/testing/cinder-e2e-tests.md`
- [ ] `docs/reference/testing/controlplane-e2e-tests.md`
- [ ] `docs/reference/testing/dizzy-chaos-testing.md`
- [ ] `docs/reference/testing/keystone-e2e-tests.md`
- [ ] `docs/reference/testing/keystone-pressure-testing.md`
- [ ] `docs/reference/testing/nova-e2e-tests.md`
- [ ] `docs/reference/testing/operator-upgrade-e2e-tests.md`
- [ ] `docs/reference/testing/rbac-usage-audit.md`
- [ ] `docs/reference/testing/reconcile-performance-benchmark.md`
- [ ] `docs/reference/testing/sizing-calibration.md`
- [ ] `docs/reference/testing/tempest-test-infrastructure.md`

## References: Target Clusters
- [ ] `docs/reference/target-clusters.md`

## Other
- [ ] `docs/future/brownfield-keystone-adoption.md`
- [ ] `docs/future/hypervisor-cluster.md`
- [ ] `docs/future/index.md`
- [ ] `docs/future/management-cluster.md`
- [ ] `docs/future/service-account-application-credentials.md`
- [ ] `docs/future/storage-cluster.md`
- [ ] `docs/index.md`
