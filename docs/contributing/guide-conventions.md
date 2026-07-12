---
title: Guide Conventions
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Guide Conventions

The how-to guides under `docs/guides/` all lead an operator through a task on a
running cluster. To keep them copy-pasteable, every guide must agree on the same
few structural rules. Without them each guide re-derives its structure from a
neighbour and inherits that neighbour's inconsistencies — most visibly, example
names that name a resource the reader's cluster never created.

This page is the contract. New guides follow it from the start; existing guides
are being brought into conformance.

The `prepare-new-guide` Claude Code skill applies this contract: its
`scaffold-guide.sh` prints a conforming skeleton for a chosen devstack anchor,
and its `validate-guide.sh` checks a draft against the conventions the docs
gate does not cover (see
[Claude Code skills](./claude-skills.md) for how to invoke it, and
`.claude/skills/prepare-new-guide/` for the scripts).

## One devstack per guide

A reader arriving at a guide has to know exactly which cluster to build before
the first command runs. Every guide therefore opens with a standardized
prerequisites block that names **exactly one** Getting-Started tutorial and its
verbatim bring-up command.

The block is machine-checkable. It must be:

1. A `## Prerequisites` heading.
2. Whose first element is a `::: info Devstack` container that holds, in order:
   - exactly **one** link to a Getting-Started tutorial
     (`../quick-start.md`, `../quick-start-extended.md`, or
     `../quick-start-controlplane.md`);
   - exactly **one** fenced ` ```bash ` block with the tutorial's verbatim
     bring-up command, including every `WITH_*` opt-in flag the guide depends
     on (a guide that needs Prometheus names `WITH_PROMETHEUS=true`, a guide
     that needs a ControlPlane names `WITH_CONTROLPLANE=true`, and so on);
   - a completion pointer — how far the reader follows the tutorial (its final
     **Verify** step, so the devstack is running).
3. Followed by any guide-specific prerequisite bullets (an external LDAP server,
   a Keycloak realm, a CNI that enforces NetworkPolicy, ...).

The three devstacks and the names each produces:

| Devstack | Verbatim bring-up | Names the examples may use |
| --- | --- | --- |
| [Quick Start](../quick-start.md) | `KIND_HOST_PORT=8443 make deploy-infra` (then the operator, image, and CR steps) | Keystone CR `keystone` in `openstack`; admin Secret `keystone-admin`; DB Secret `keystone-db`; gateway `openstack-gw`; endpoint `https://keystone.127-0-0-1.nip.io:8443/v3` |
| [Quick Start (Extended)](../quick-start-extended.md) | `kind create cluster --name forge --config hack/kind-config.yaml` then `make deploy-infra` (then the operator, image, and CR steps) | Same standalone names as the base Quick Start (`keystone`, `keystone-admin`, `keystone-db`) |
| [Quick Start (ControlPlane)](../quick-start-controlplane.md) | `KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra` | ControlPlane CR `controlplane` in `openstack`; projected children `controlplane-keystone` / `controlplane-horizon`; admin Secret and ExternalSecret `controlplane-keystone-admin-credentials`; DB ExternalSecret `controlplane-keystone-db-credentials`; shared `openstack-db` / `openstack-memcached` / `openstack-gw` |

Opt-in flags compose, so a guide that needs both a ControlPlane and Prometheus
names `KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_PROMETHEUS=true make deploy-infra`.

## ControlPlane-first naming

The primary path of a guide uses the resource names its declared devstack
actually produces — never a placeholder name that no tutorial creates. A reader
on that devstack must be able to copy a command and have it hit a resource that
exists.

When a guide's devstack is the ControlPlane Quick Start, the primary path uses
the ControlPlane devstack's real names: the projected Keystone child is
`controlplane-keystone`, its admin credential lives in
`controlplane-keystone-admin-credentials`, and so on. Where a standalone
(non-ControlPlane) installation differs, the differences go in a separate
`## Standalone Keystone, without a ControlPlane` section at the end, modeled on
[End-to-End SSO](../guides/end-to-end-sso.md). Do not interleave the two naming
worlds in the primary walkthrough.

Placeholder example names that no tutorial produces are banned. If a value
genuinely varies per reader, express it as a substitution rule anchored on a
concrete devstack name, not as an invented literal.

## Never edit operator-projected children

On a ControlPlane devstack the Keystone and Horizon CRs are **projected** by the
ControlPlane operator; editing them by hand is reverted on the next reconcile.
A guide sets a knob on the `ControlPlane` CR, and lets the operator project it
down. A knob the `ControlPlane` CRD does not expose is documented as
standalone-only — in the guide's `## Standalone Keystone, without a ControlPlane`
section, applied to a Keystone CR the reader owns.

## Every guide is testable

A guide describes behaviour the project also asserts in an end-to-end suite.
Every guide closes with a terminal section titled exactly `## Tested by` that
names its mirroring suite(s) and the local invocation, so a reader can run the
same flow the guide walks through. The invocation uses the `--test-dir` form,
one line per suite:

```bash
chainsaw test --test-dir tests/e2e/keystone/<suite>
```

`tests/unit/docs/guide_devstack_and_tested_by_test.sh` enforces this per guide:
the `## Tested by` section must exist, and every path it names with
`chainsaw test --test-dir` must resolve to a real `chainsaw-test.yaml`.

## Single source of truth for manifests

Where a guide mirrors an e2e suite, embed the fixture the suite applies rather
than hand-maintaining a second copy that drifts. A VitePress snippet import
keeps the rendered YAML in lockstep with the tested fixture; mark the region to
import with column-0 `# region <name>` / `# endregion <name>` YAML comments
around the CR document in the fixture:

```md
<<< @/../tests/e2e/keystone/<suite>/00-keystone-cr.yaml#<region>
```

The fixture and the walkthrough serve different masters, so they carry different
names. A suite fixture is **isolation-named** — a distinct CR name, its own
logical database, `deletionPolicy: Delete`, dev-tag image pins — so the suite
runs safely in the parallel suite pool. The walkthrough is **devstack-named** —
it uses the resource names the guide's declared devstack actually produces (see
[ControlPlane-first naming](#controlplane-first-naming)), so a reader can copy a
command and have it hit a resource that exists. Reconcile the two by keeping the
walkthrough YAML devstack-named and embedding the isolation-named fixture as a
labelled `::: details` exhibit inside `## Tested by`, prefaced by a sentence
that states which names the exhibit uses and why. The import is real, so the
exhibit cannot drift from the tested fixture, and the walkthrough keeps the
names the reader's devstack can actually copy.

## Housekeeping

- Frontmatter (`title`, `quadrant: operator`) starts on **line 1**. VitePress
  renders anything above the frontmatter as body text.
- Register a new guide in the `Guides` sidebar in `docs/.vitepress/config.ts`.
- Source describes behaviour, not internal tracking. Do not put internal
  feature or requirement IDs, or issue references, in a guide.
- Guides are written in English.
