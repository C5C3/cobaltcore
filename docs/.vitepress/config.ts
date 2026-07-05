// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'CobaltCore Forge',
  description: 'A Kubernetes-native OpenStack distribution — forge repository reference documentation',
  base: '/forge/',
  // Port-forward URLs (e.g. http://localhost:9080 for Flux Web UI) are
  // documented for reader use and never resolve during the build.
  ignoreDeadLinks: [/^https?:\/\/localhost(:\d+)?/],
  themeConfig: {
    nav: [
      { text: 'Home', link: '/' },
    ],
    sidebar: [
      {
        text: 'Getting Started',
        items: [
          { text: 'Quick Start', link: '/quick-start' },
          { text: 'Quick Start (Extended)', link: '/quick-start-extended' },
          { text: 'Quick Start (ControlPlane)', link: '/quick-start-controlplane' },
        ],
      },
      {
        text: 'Guides',
        items: [
          { text: 'Observability & Diagnostics', link: '/guides/observability' },
          { text: 'Day 2 Operations', link: '/guides/day-2-operations' },
          { text: 'Advanced Configuration', link: '/guides/advanced-configuration' },
          { text: 'Rotate Keystone Keys', link: '/guides/keystone-key-rotation' },
          { text: 'Rotate Keystone Admin Password', link: '/guides/keystone-admin-password-rotation' },
          { text: 'Schedule Keystone Admin Password Rotation', link: '/guides/keystone-admin-password-scheduled-rotation' },
          { text: 'Multi-Tenant Deployment', link: '/guides/multi-tenant-deployment' },
          { text: 'Enable Keystone Database TLS', link: '/guides/enable-keystone-database-tls' },
          { text: 'Enable Keystone Operator Metrics', link: '/guides/enable-keystone-operator-metrics' },
          { text: 'Enable Keystone Operator NetworkPolicy', link: '/guides/enable-keystone-operator-networkpolicy' },
          { text: 'Migrate Keystone DB to Dynamic Credentials', link: '/guides/migrate-keystone-db-to-dynamic-credentials' },
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
              { text: 'Controller Events', link: '/reference/keystone/keystone-events' },
              { text: 'Reconciler Architecture', link: '/reference/keystone/keystone-reconciler' },
              { text: 'Upgrade Flow', link: '/reference/keystone/keystone-upgrade-flow' },
              { text: 'Schema Drift Detection', link: '/reference/keystone/keystone-schema-drift-detection' },
              { text: 'Operator Metrics', link: '/reference/keystone-operator-metrics' },
              { text: 'Operator NetworkPolicy', link: '/reference/keystone/keystone-operator-networkpolicy' },
            ],
          },
          {
            text: 'c5c3 (ControlPlane)',
            collapsed: true,
            items: [
              { text: 'ControlPlane CRD', link: '/reference/c5c3/controlplane-crd' },
              { text: 'Reconciler Architecture', link: '/reference/c5c3/controlplane-reconciler' },
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
        ],
      },
      {
        text: 'Future',
        items: [
          { text: 'Overview', link: '/future/' },
          { text: 'Brownfield Keystone Adoption', link: '/future/brownfield-keystone-adoption' },
        ],
      },
      {
        text: 'Contributing',
        items: [
          { text: 'Adding a New Operator', link: '/contributing/adding-a-new-operator' },
          { text: 'Dependency Management', link: '/contributing/dependency-management' },
          { text: 'Claude Code Skills', link: '/contributing/claude-skills' },
        ],
      },
    ],
    socialLinks: [
      { icon: 'github', link: 'https://github.com/c5c3/forge' },
    ],
  },
})
