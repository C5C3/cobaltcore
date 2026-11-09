---
title: Publishing to cobaltcore.dev
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Publishing to cobaltcore.dev

[cobaltcore.dev](https://cobaltcore.dev) builds its OpenStack section from the
pages in this directory. The build of
[cobaltcore-dev/docs](https://github.com/cobaltcore-dev/docs) checks out
`docs/`, `tests/` and `deploy/` of this repository and renders a subset of the
pages below `/openstack/`, with the theme and navigation of cobaltcore.dev. It
rebuilds every night from `main`. The pages stay here, and
[c5c3.github.io/cobaltcore](https://c5c3.github.io/cobaltcore/) keeps rendering
all of them.

Two files in `docs/.vitepress/` are the contract between the two sites:

| File | What it holds | Read by |
| --- | --- | --- |
| `publish.json` | The patterns of the pages cobaltcore.dev publishes, the canonical address of this site, and the repository | cobaltcore.dev |
| `sidebar.ts` | The sidebar | this site and cobaltcore.dev |

## What is published

`publish.json` lists glob patterns relative to `docs/`. A `*` matches within
one path segment, a `**` across segments:

| Pattern | Pages | Path on cobaltcore.dev |
| --- | --- | --- |
| `index.md` | The overview | `/openstack/` |
| `quick-start*.md` | The five quick starts | `/openstack/quick-start`, `/openstack/quick-start-extended`, … |
| `architecture/**` | The architecture pages | `/openstack/architecture/` |
| `guides/**` | Every guide | `/openstack/guides/` |
| `reference/*/index.md` | The overview page of each service operator | `/openstack/<service>` |

No other page is published: the rest of `reference/`, `contributing/` and
`future/` stays on this site. A new guide appears on cobaltcore.dev with the
next nightly build. A new top-level section does not appear until a pattern
covers it.

## Links from a published page

Write links as everywhere else, relative to the page. cobaltcore.dev resolves
each link of a published page against the page's file in this repository:

- A link to another published page stays on cobaltcore.dev.
- A link to a page that is not published goes to that page on this site, the
  `canonical` address in `publish.json`.
- A link to a file outside `docs/`, such as a Chainsaw test, goes to the file
  on GitHub.

Images and diagrams need no rule, because both builds resolve them relative to
the page.

## Code imports

A code import names its file relative to the page, as the
[guide conventions](./guide-conventions.md#single-source-of-truth-for-manifests)
describe:

```md
<<< ../../../tests/e2e/keystone/<suite>/00-keystone-cr.yaml#<region>
```

A path that starts with `@/` breaks the cobaltcore.dev build. VitePress
resolves `@` against the source root of the site that renders the page, and on
cobaltcore.dev that is a different directory. A published page imports only
from `docs/`, `tests/` and `deploy/`, because cobaltcore.dev checks out no
other directory.

## The sidebar

cobaltcore.dev imports `sidebar.ts`, keeps the entries whose page is published,
and drops the groups that are left empty. A guide registered in the `Guides`
group therefore appears in both sidebars. Keep the module free of logic and of
imports other than types: cobaltcore.dev loads it with Node, outside the build
of this site.

## Checks

| Check | What it catches |
| --- | --- |
| `tests/unit/docs/cobaltcore_dev_publish_test.sh`, part of `make test-shell` | A `publish.json` that does not parse, a pattern that matches no page, an `@/` import, and an import of a published page that resolves outside `docs/`, `tests/` and `deploy/` |

## Preview a change on cobaltcore.dev

Clone cobaltcore-dev/docs next to this checkout, copy the three directories into
`docs/_upstream/cobaltcore/`, and start its development server:

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
