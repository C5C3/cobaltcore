// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitepress'
import sidebar from './sidebar'

export default defineConfig({
  title: 'CobaltCore',
  description: 'Deploy and operate OpenStack on Kubernetes with CobaltCore',
  base: '/cobaltcore/',
  // Port-forward URLs (e.g. http://localhost:9080 for Flux Web UI) are
  // documented for reader use and never resolve during the build.
  ignoreDeadLinks: [/^https?:\/\/localhost(:\d+)?/],
  themeConfig: {
    nav: [
      { text: 'Getting Started', link: '/quick-start' },
      { text: 'Deployment Guides', link: '/deployment/local-kubernetes' },
      { text: 'Tutorials', link: '/quick-start-controlplane' },
      { text: 'Reference', link: '/reference/keystone/' },
      { text: 'Future', link: '/future/' },
      { text: 'Contributing', link: '/contributing/adding-a-new-operator' },
    ],
    sidebar,
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
