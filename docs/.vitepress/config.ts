// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'CobaltCore Forge',
  description: 'A Kubernetes-native OpenStack distribution — forge repository reference documentation',
  base: '/forge/',
  themeConfig: {
    nav: [
      { text: 'Home', link: '/' },
    ],
    sidebar: [
      {
        text: 'Reference',
        items: [
          { text: 'Keystone CRD', link: '/reference/keystone-crd' },
          { text: 'Keystone E2E Tests', link: '/reference/keystone-e2e-tests' },
          { text: 'CI Workflow', link: '/reference/ci-workflow' },
          { text: 'Build Images Workflow', link: '/reference/build-images-workflow' },
          { text: 'Container Images', link: '/reference/container-images' },
          { text: 'Infrastructure Manifests', link: '/reference/infrastructure-manifests' },
          { text: 'Kubernetes Packages', link: '/reference/kubernetes-packages' },
        ],
      },
    ],
    socialLinks: [
      { icon: 'github', link: 'https://github.com/c5c3/forge' },
    ],
  },
})
