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
          { text: 'Quick Start (Kind)', link: '/quick-start' },
        ],
      },
      {
        text: 'Guides',
        items: [
          { text: 'Observability & Diagnostics', link: '/guides/observability' },
          { text: 'Day 2 Operations', link: '/guides/day-2-operations' },
          { text: 'Advanced Configuration', link: '/guides/advanced-configuration' },
          { text: 'Rotate Keystone Keys', link: '/guides/keystone-key-rotation' },
          { text: 'Multi-Tenant Deployment', link: '/guides/multi-tenant-deployment' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Keystone CRD', link: '/reference/keystone-crd' },
          { text: 'Keystone Controller Events', link: '/reference/keystone-events' },
          { text: 'Keystone Reconciler Architecture', link: '/reference/keystone-reconciler' },
          { text: 'Keystone Upgrade Flow', link: '/reference/keystone-upgrade-flow' },
          { text: 'Keystone Schema Drift Detection', link: '/reference/keystone-schema-drift-detection' },
          { text: 'Keystone E2E Tests', link: '/reference/keystone-e2e-tests' },
          { text: 'Chaos E2E Tests', link: '/reference/chaos-e2e-tests' },
          { text: 'CI Workflow', link: '/reference/ci-workflow' },
          { text: 'Build Images Workflow', link: '/reference/build-images-workflow' },
          { text: 'Container Images', link: '/reference/container-images' },
          { text: 'Tempest Test Infrastructure', link: '/reference/tempest-test-infrastructure' },
          { text: 'Infrastructure Manifests', link: '/reference/infrastructure-manifests' },
          { text: 'Kubernetes Packages', link: '/reference/kubernetes-packages' },
          {
            text: 'Backend',
            collapsed: true,
            items: [
              { text: 'Helm Values Schema', link: '/reference/backend/helm-values-schema' },
              { text: 'Keystone Operator Packaging', link: '/reference/backend/keystone-operator-packaging' },
              { text: 'Rotation Scripts', link: '/reference/backend/rotation-scripts' },
            ],
          },
          {
            text: 'Infrastructure',
            collapsed: true,
            items: [
              { text: 'E2E Deployment', link: '/reference/infrastructure/e2e-deployment' },
              { text: 'OpenBao Bootstrap', link: '/reference/infrastructure/openbao-bootstrap' },
            ],
          },
        ],
      },
    ],
    socialLinks: [
      { icon: 'github', link: 'https://github.com/c5c3/forge' },
    ],
  },
})
