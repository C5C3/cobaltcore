---
title: Publishing to cobaltcore.dev
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Publishing to cobaltcore.dev

The OpenStack pages configured for [cobaltcore.dev](https://cobaltcore.dev)
remain in this repository. The publishing contract is intended for the
separate [cobaltcore-dev/docs](https://github.com/cobaltcore-dev/docs)
repository, which builds the site with its own theme and navigation. That
repository does not yet contain `docs/.vitepress/overlay.ts`, so the
`docs-overlay` pull-request job in this repository currently skips its build.
The consumer repository deploys its site on pushes to `main`; its Pages
workflow has no nightly schedule. Meanwhile,
[c5c3.github.io/cobaltcore](https://c5c3.github.io/cobaltcore/) continues to
render all pages in this repository.

The planned publishing integration uses two files in `docs/.vitepress/`:

| File | What it holds | Read by |
| --- | --- | --- |
| `publish.json` | The patterns, canonical address, and repository for the planned publishing integration | cobaltcore.dev, when its overlay is enabled |
| `sidebar.ts` | The sidebar | this site and cobaltcore.dev, when its overlay is enabled |

## Publish patterns

`publish.json` lists the pages the integration will publish, using glob
patterns relative to `docs/`. A `*` matches within one path segment, a `**`
across segments:

| Pattern | Pages | Path on cobaltcore.dev |
| --- | --- | --- |
| `index.md` | The overview | `/openstack/` |
| `quick-start*.md` | The five quick starts | `/openstack/quick-start`, `/openstack/quick-start-extended`, … |
| `architecture/**` | The architecture pages | `/openstack/architecture/` |
| `guides/**` | Every guide | `/openstack/guides/` |
| `reference/*/index.md` | The overview page of each service operator | `/openstack/<service>` |

With the integration enabled, pages outside these patterns, including the
rest of `reference/`, `contributing/` and `future/`, stay on this site. A new
guide matches `guides/**`; a new top-level section needs a pattern of its own.

## Links from a published page

When the integration is enabled, write links as everywhere else, relative to
the page. The intended resolution for links from a published page is:

- A link to another published page stays on cobaltcore.dev.
- A link to a page that is not published goes to that page on this site, the
  `canonical` address in `publish.json`.
- A link to a file outside `docs/`, such as a Chainsaw test, goes to the file
  on GitHub.

Images and diagrams in this site's build resolve relative to the page. The
consumer build should preserve those paths. Its link resolver is not present
yet, so the pull-request job cannot currently verify the expected routing.

## Code imports

A code import names its file relative to the page, as the
[guide conventions](./guide-conventions.md#single-source-of-truth-for-manifests)
describe. The unit test below checks these paths. After the consumer adds its
overlay, the `docs-overlay` job also builds the published pages:

```md
<<< ../../../tests/e2e/keystone/<suite>/00-keystone-cr.yaml#<region>
```

A path that starts with `@/` is not portable between the two site roots. A
published page should import only from `docs/`, `tests/` and `deploy/`, the
directories the `docs-overlay` job checks out for the consumer build.

## The sidebar

When the consumer overlay is enabled, it imports `sidebar.ts`, keeps entries
whose pages are published, and drops groups left empty. A guide registered in
the `Guides` group will then appear in both sidebars. Keep the module free of
logic and of imports other than types so the consumer can load it with Node.

## Checks

| Check | What it catches |
| --- | --- |
| `tests/unit/docs/cobaltcore_dev_publish_test.sh`, part of `make test-shell` | A `publish.json` that does not parse, a pattern that matches no page, an `@/` import, and an import of a published page that resolves outside `docs/`, `tests/` and `deploy/` |
| The [`docs-overlay`](../reference/ci-cd/ci-workflow.md#docs-overlay) job on a pull request | When the consumer has `docs/.vitepress/overlay.ts`, the job builds cobaltcore-dev/docs with the pull request's docs. It does not block a merge. Until the overlay exists, the job reports a notice and skips the build |

## Preview a change on cobaltcore.dev

After cobaltcore-dev/docs adds its overlay, clone it next to this checkout,
copy the three directories into `docs/_upstream/cobaltcore/`, and start its
development server:

```bash
git clone https://github.com/cobaltcore-dev/docs.git cobaltcore-dev-docs
cd cobaltcore-dev-docs
mkdir -p docs/_upstream/cobaltcore
rsync -a --delete --exclude .vitepress/cache --exclude .vitepress/dist \
  ../cobaltcore/docs ../cobaltcore/tests ../cobaltcore/deploy \
  docs/_upstream/cobaltcore/
npm ci
npm run docs:dev
```

The OpenStack section is then at `http://localhost:5173/openstack/`. Run the
`rsync` command again after each change.
