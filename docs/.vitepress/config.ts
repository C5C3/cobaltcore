// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'CobaltCore',
  description: 'A Kubernetes-native OpenStack distribution — reference documentation',
  base: '/cobaltcore/',
  // Port-forward URLs (e.g. http://localhost:9080 for Flux Web UI) are
  // documented for reader use and never resolve during the build.
  ignoreDeadLinks: [/^https?:\/\/localhost(:\d+)?/],
  themeConfig: {
    nav: [
      { text: 'Home', link: '/' },
      { text: 'Architecture', link: '/architecture/' },
      { text: 'Quick Start', link: '/quick-start' },
      { text: 'Guides', link: '/guides/observability' },
      { text: 'Reference', link: '/reference/keystone/' },
      { text: 'Future', link: '/future/' },
      { text: 'Contributing', link: '/contributing/adding-a-new-operator' },
    ],
    sidebar: [
      {
        text: 'Getting Started',
        items: [
          { text: 'Overview', link: '/' },
          { text: 'Quick Start', link: '/quick-start' },
          { text: 'Quick Start (Extended)', link: '/quick-start-extended' },
          { text: 'Quick Start (ControlPlane)', link: '/quick-start-controlplane' },
        ],
      },
      {
        text: 'Architecture',
        items: [
          { text: 'Overview', link: '/architecture/' },
          { text: 'Core Components', link: '/architecture/core-components' },
        ],
      },
      {
        text: 'Guides',
        items: [
          { text: 'Observability & Diagnostics', link: '/guides/observability' },
          { text: 'Day 2 Operations', link: '/guides/day-2-operations' },
          { text: 'Advanced Configuration', link: '/guides/advanced-configuration' },
          { text: 'Multi-Tenant Deployment', link: '/guides/multi-tenant-deployment' },
          { text: 'Deploy Services into Dedicated Namespaces', link: '/guides/dedicated-service-namespaces' },
          { text: 'End-to-End SSO', link: '/guides/end-to-end-sso' },
          { text: 'Deploy to a Target Cluster', link: '/guides/deploy-to-a-target-cluster' },
          {
            text: 'Keystone',
            collapsed: true,
            items: [
              { text: 'Rotate Keystone Keys', link: '/guides/keystone/keystone-key-rotation' },
              { text: 'Rotate Keystone Admin Password', link: '/guides/keystone/keystone-admin-password-rotation' },
              { text: 'Schedule Keystone Admin Password Rotation', link: '/guides/keystone/keystone-admin-password-scheduled-rotation' },
              { text: 'Attach an LDAP Domain Backend', link: '/guides/keystone/ldap-domain-backend' },
              { text: 'Attach an OIDC Federation Backend', link: '/guides/keystone/oidc-federation' },
              { text: 'Attach a SAML Federation Backend', link: '/guides/keystone/saml-federation' },
              { text: 'Enable Keystone Database TLS/mTLS', link: '/guides/keystone/enable-keystone-database-tls' },
              { text: 'Enable Keystone Operator Metrics', link: '/guides/keystone/enable-keystone-operator-metrics' },
              { text: 'Enable Keystone Operator NetworkPolicy', link: '/guides/keystone/enable-keystone-operator-networkpolicy' },
              { text: 'Migrate Keystone DB to Dynamic Credentials', link: '/guides/keystone/migrate-keystone-db-to-dynamic-credentials' },
              { text: 'Adopt an External Keystone', link: '/guides/keystone/adopt-external-keystone' },
            ],
          },
          {
            text: 'Horizon',
            collapsed: true,
            items: [
              { text: 'Enable Horizon Operator Metrics', link: '/guides/horizon/enable-horizon-operator-metrics' },
              { text: 'Enable Horizon Operator NetworkPolicy', link: '/guides/horizon/enable-horizon-operator-networkpolicy' },
            ],
          },
          {
            text: 'Glance',
            collapsed: true,
            items: [
              { text: 'Attach S3 Multi-Store Backends to Glance', link: '/guides/glance/glance-s3-multistore' },
              { text: 'Enable Glance Image Caching', link: '/guides/glance/enable-glance-image-caching' },
              { text: 'Enable Glance Image Conversion', link: '/guides/glance/enable-glance-image-conversion' },
              { text: 'Enable Glance Operator Metrics', link: '/guides/glance/enable-glance-operator-metrics' },
              { text: 'Enable Glance Operator NetworkPolicy', link: '/guides/glance/enable-glance-operator-networkpolicy' },
              { text: 'Filter web-download Image Imports', link: '/guides/glance/filter-web-download-imports' },
              { text: 'Large Image Uploads through the Gateway', link: '/guides/glance/large-image-uploads' },
              { text: 'Migrate Glance DB to Dynamic Credentials', link: '/guides/glance/migrate-glance-db-to-dynamic-credentials' },
            ],
          },
          {
            text: 'Placement',
            collapsed: true,
            items: [
              { text: 'Enable Placement Operator Metrics', link: '/guides/placement/enable-placement-operator-metrics' },
              { text: 'Enable Placement Operator NetworkPolicy', link: '/guides/placement/enable-placement-operator-networkpolicy' },
              { text: 'Migrate Placement DB to Dynamic Credentials', link: '/guides/placement/migrate-placement-db-to-dynamic-credentials' },
              { text: 'Standalone Placement', link: '/guides/placement/standalone-placement' },
            ],
          },
          {
            text: 'Barbican',
            collapsed: true,
            items: [
              { text: 'Run Barbican on a Dedicated OpenBao', link: '/guides/barbican/barbican-dedicated-openbao' },
              { text: 'Attach an External OpenBao to Barbican', link: '/guides/barbican/attach-external-openbao' },
              { text: 'Enable Barbican Operator Metrics', link: '/guides/barbican/enable-barbican-operator-metrics' },
              { text: 'Enable Barbican Operator NetworkPolicy', link: '/guides/barbican/enable-barbican-operator-networkpolicy' },
              { text: 'Migrate Barbican DB to Dynamic Credentials', link: '/guides/barbican/migrate-barbican-db-to-dynamic-credentials' },
            ],
          },
        ],
      },
      {
        text: 'Reference',
        items: [
          {
            text: 'Keystone',
            link: '/reference/keystone/',
            collapsed: true,
            items: [
              { text: 'Overview', link: '/reference/keystone/' },
              { text: 'CRD', link: '/reference/keystone/keystone-crd' },
              { text: 'Identity Backend CRD', link: '/reference/keystone/identity-backend-crd' },
              { text: 'Controller Events', link: '/reference/keystone/keystone-events' },
              { text: 'Reconciler Architecture', link: '/reference/keystone/keystone-reconciler' },
              { text: 'Upgrade Flow', link: '/reference/keystone/keystone-upgrade-flow' },
              { text: 'Schema Drift Detection', link: '/reference/keystone/keystone-schema-drift-detection' },
              { text: 'Operator Metrics', link: '/reference/keystone-operator-metrics' },
              { text: 'Operator NetworkPolicy', link: '/reference/keystone/keystone-operator-networkpolicy' },
            ],
          },
          {
            text: 'Horizon',
            link: '/reference/horizon/',
            collapsed: true,
            items: [
              { text: 'Overview', link: '/reference/horizon/' },
              { text: 'CRD', link: '/reference/horizon/horizon-crd' },
              { text: 'Reconciler Architecture', link: '/reference/horizon/horizon-reconciler' },
            ],
          },
          {
            text: 'Glance',
            link: '/reference/glance/',
            collapsed: true,
            items: [
              { text: 'Overview', link: '/reference/glance/' },
              { text: 'CRD', link: '/reference/glance/glance-crd' },
              { text: 'Backend CRD', link: '/reference/glance/glance-backend-crd' },
              { text: 'Controller Events', link: '/reference/glance/glance-events' },
              { text: 'Reconciler Architecture', link: '/reference/glance/glance-reconciler' },
              { text: 'Upgrade Flow', link: '/reference/glance/glance-upgrade-flow' },
            ],
          },
          {
            text: 'Placement',
            link: '/reference/placement/',
            collapsed: true,
            items: [
              { text: 'Overview', link: '/reference/placement/' },
              { text: 'CRD', link: '/reference/placement/placement-crd' },
              { text: 'Controller Events', link: '/reference/placement/placement-events' },
              { text: 'Reconciler Architecture', link: '/reference/placement/placement-reconciler' },
            ],
          },
          {
            text: 'Barbican',
            link: '/reference/barbican/',
            collapsed: true,
            items: [
              { text: 'Overview', link: '/reference/barbican/' },
              { text: 'CRD', link: '/reference/barbican/barbican-crd' },
              { text: 'Secret Store CRD', link: '/reference/barbican/barbican-secret-store-crd' },
              { text: 'Controller Events', link: '/reference/barbican/barbican-events' },
              { text: 'Reconciler Architecture', link: '/reference/barbican/barbican-reconciler' },
            ],
          },
          {
            text: 'c5c3 (ControlPlane)',
            collapsed: true,
            items: [
              { text: 'ControlPlane CRD', link: '/reference/c5c3/controlplane-crd' },
              { text: 'Reconciler Architecture', link: '/reference/c5c3/controlplane-reconciler' },
              { text: 'KeystoneService CRD', link: '/reference/c5c3/keystoneservice-crd' },
              { text: 'KeystoneService Reconciler', link: '/reference/c5c3/keystoneservice-reconciler' },
            ],
          },
          {
            text: 'Testing',
            collapsed: true,
            items: [
              { text: 'Keystone E2E Tests', link: '/reference/testing/keystone-e2e-tests' },
              { text: 'ControlPlane E2E Tests', link: '/reference/testing/controlplane-e2e-tests' },
              { text: 'Operator Upgrade E2E Tests', link: '/reference/testing/operator-upgrade-e2e-tests' },
              { text: 'Chaos E2E Tests', link: '/reference/testing/chaos-e2e-tests' },
              { text: 'dizzy Chaos Testing', link: '/reference/testing/dizzy-chaos-testing' },
              { text: 'Tempest Test Infrastructure', link: '/reference/testing/tempest-test-infrastructure' },
              { text: 'Reconcile Performance Benchmark', link: '/reference/testing/reconcile-performance-benchmark' },
            ],
          },
          {
            text: 'CI/CD',
            collapsed: true,
            items: [
              { text: 'CI Workflow', link: '/reference/ci-cd/ci-workflow' },
              { text: 'Build Images Workflow', link: '/reference/ci-cd/build-images-workflow' },
              { text: 'Container Images', link: '/reference/ci-cd/container-images' },
            ],
          },
          {
            text: 'Backend',
            collapsed: true,
            items: [
              { text: 'Helm Values Schema', link: '/reference/backend/helm-values-schema' },
              { text: 'Keystone Operator Packaging', link: '/reference/backend/keystone-operator-packaging' },
              { text: 'Rotation Scripts', link: '/reference/backend/rotation-scripts' },
              { text: 'Kubernetes Packages', link: '/reference/backend/kubernetes-packages' },
            ],
          },
          {
            text: 'Infrastructure',
            collapsed: true,
            items: [
              { text: 'Manifests', link: '/reference/infrastructure/infrastructure-manifests' },
              { text: 'E2E Deployment', link: '/reference/infrastructure/e2e-deployment' },
              { text: 'OpenBao Bootstrap', link: '/reference/infrastructure/openbao-bootstrap' },
            ],
          },
          { text: 'Target Clusters', link: '/reference/target-clusters' },
        ],
      },
      {
        text: 'Future',
        items: [
          { text: 'Overview', link: '/future/' },
          { text: 'Brownfield Keystone Adoption', link: '/future/brownfield-keystone-adoption' },
          { text: 'Service Accounts on Application Credentials', link: '/future/service-account-application-credentials' },
          { text: 'Hypervisor Cluster', link: '/future/hypervisor-cluster' },
          { text: 'Storage Cluster', link: '/future/storage-cluster' },
          { text: 'Management Cluster', link: '/future/management-cluster' },
        ],
      },
      {
        text: 'Contributing',
        items: [
          { text: 'Adding a New Operator', link: '/contributing/adding-a-new-operator' },
          { text: 'Adding a New Release', link: '/contributing/adding-a-new-release' },
          { text: 'Guide Conventions', link: '/contributing/guide-conventions' },
          { text: 'Dependency Management', link: '/contributing/dependency-management' },
          { text: 'Nix Development Environment', link: '/contributing/nix-dev-environment' },
          { text: 'Claude Code Skills', link: '/contributing/claude-skills' },
        ],
      },
    ],
    socialLinks: [
      { icon: 'github', link: 'https://github.com/c5c3/cobaltcore' },
      {
        icon: {
          svg: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path fill="currentColor" d="M18 2H6c-1.1 0-1.99.9-1.99 2L4 20c0 1.1.89 2 1.99 2H18c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zM6 4h5v8l-2.5-1.5L6 12V4z"/></svg>',
        },
        link: 'https://deepwiki.com/C5C3/cobaltcore',
        ariaLabel: 'DeepWiki',
      },
    ],
  },
})
