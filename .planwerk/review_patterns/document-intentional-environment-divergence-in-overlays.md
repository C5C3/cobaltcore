# Review Pattern: Document intentional environment divergence in overlays

**Review-Area**: documentation
**Detection-Hint**: When a kustomize overlay patch changes multiple default behaviors (disables features, overrides resource limits, changes runtime settings), check whether the base configuration's defaults are intentional for non-overlayed environments and whether this divergence is documented. The strongest form of divergence is moving a component out of the production base entirely into a kind-only opt-in overlay — verify that the production omission and the opt-in flag are both documented.
**Severity**: WARNING
**Occurrences**: 3

## What to check

When an overlay significantly alters the base HelmRelease values (e.g., disabling dashboard, changing container runtime, reducing resources), verify that (1) the base defaults are intentional for production/non-kind environments and (2) the divergence is documented so operators understand what differs between environments. When a component is moved out of the production base entirely (kind-only opt-in overlay, gated by a flag like `WITH_CHAOS_MESH=true`), verify that (3) the production overlay's omission is explicit and intentional, (4) the opt-in flag is documented in the Quick Start and reference docs, and (5) any kind-tuning patch that previously lived in `deploy/kind/base/` is co-located with the new overlay so the patch target lives in the same kustomization root.

## Why it matters

Undocumented divergence between environments leads to surprises during incidents or environment promotion. For example, the dashboard being enabled in production but disabled in kind may be intentional, but without documentation a future contributor may assume the kind overlay represents the desired state everywhere, or may not realize production runs with higher resource usage. When a component moves out of the production base (e.g., chaos-mesh becoming opt-in under CC-0097), the risk inverts: a future contributor may assume the component is still always-on, write tests that depend on it, and only discover the divergence when CI fails on the default `make deploy-infra` path.

## Examples from external reviews

### CC-0046 — sourcery-ai[bot]
- **Feedback**: The kind overlay patches Chaos Mesh to use containerd, disable the dashboard, and reduce resources, but the base HelmRelease uses upstream defaults; double-check that the divergence between kind and non-kind environments is intentional and documented where needed.
- **What was missed**: When an overlay significantly alters the base HelmRelease values (e.g., disabling dashboard, changing container runtime, reducing resources), verify that (1) the base defaults are intentional for production/non-kind environments and (2) the divergence is documented so operators understand what differs between environments.
- **Fix**: Added comments in the base HelmRelease and/or overlay documenting the intentional differences between kind (dev) and non-kind (production) configurations.

## CC-0097 follow-up

CC-0097 escalated the Chaos Mesh divergence from "kind patches the base
HelmRelease" to "production explicitly does not install Chaos Mesh at all".
The kind-tuning HelmRelease patch (containerd runtime, dashboard disabled,
reduced resource requests) that the CC-0046 example references **no longer
lives in `deploy/kind/base/kustomization.yaml`** — it has moved to the
opt-in kind overlay at `deploy/kind/chaos-mesh/kustomization.yaml`, which
is only applied when `WITH_CHAOS_MESH=true make deploy-infra` is used.

The original CC-0046 example block above is preserved as historical record
of the divergence-documentation review pattern. When applying this pattern
to chaos-mesh today, look in:

- `deploy/kind/chaos-mesh/kustomization.yaml` — overlay + kind-tuning patch
- `deploy/flux-system/kustomization.yaml` — production overlay (chaos-mesh entries removed)
- `docs/quick-start.md` — `::: tip Enabling Chaos Mesh` block
- `docs/reference/infrastructure-manifests.md` — `### Chaos Mesh (kind-only opt-in)` subsection
- `docs/reference/chaos-e2e-tests.md` — `WITH_CHAOS_MESH=true make deploy-infra` prerequisite

The pattern still applies to any future component where the kind overlay
diverges from production — and now, even more strongly, to any component
that is moved out of the production base entirely.

## CC-0100 follow-up

CC-0100 added a third worked example of this pattern: the kube-prometheus-stack
is **never** installed in production overlays (production clusters are
expected to run their own Prometheus with `serviceMonitorSelector` widened
to pick up the operator's `ServiceMonitor`), but the kind Quick Start can
opt in to a tuned-down stack via `WITH_PROMETHEUS=true make deploy-infra`.
This sits alongside the chaos-mesh opt-in (CC-0097, `WITH_CHAOS_MESH=true`),
the kind-only Envoy demo settings on the Envoy Gateway, and the kind-only
OpenBao Web UI (`ui = true` in the Raft config) — each of those is a
slightly different shape of the same divergence:

- **CC-0097 (chaos-mesh):** component moved out of production base into a
  `WITH_CHAOS_MESH=true` overlay.
- **CC-0100 (kube-prometheus-stack):** new component added kind-only via
  `WITH_PROMETHEUS=true` overlay; production overlay contains no reference
  at all (deliberately).
- **OpenBao UI:** kind overlay flips `ui = true` in the standalone Raft
  config; production overlay keeps `ui = false`.
- **Envoy Gateway demo settings:** kind overlay relaxes settings for the
  shared `openstack-gw`; production overlay keeps tighter defaults.

When applying this pattern to the CC-0100 prometheus stack today, look in:

- `deploy/kind/prometheus/kustomization.yaml` — overlay + kind-tuned values
  (alertmanager off, node-exporter off, kube-state-metrics off, 6h
  retention, ≤100m CPU / ≤256Mi memory caps) + dashboard configMapGenerator
- `deploy/flux-system/kustomization.yaml` — production overlay (no
  kube-prometheus-stack entry; deliberately omitted)
- `hack/deploy-infra.sh` — `WITH_PROMETHEUS=true` flag, opt-in `kubectl
  apply -k`, and the post-Ready `kubectl patch` that flips
  `monitoring.serviceMonitor.enabled=true` on the keystone-operator
  HelmRelease
- `docs/quick-start-extended.md` — `::: tip Enabling Prometheus & Grafana
  (CC-0100)` block in Step 3 and the new
  `## Step 4c — Open the Grafana UI` section
- `docs/quick-start.md` — single-line pointer back to Step 4c
- `docs/guides/enable-keystone-operator-metrics.md` — `::: tip On kind`
  callout reframing the rest of the guide as the canonical non-kind path
- `docs/reference/infrastructure/infrastructure-manifests.md` —
  `### kube-prometheus-stack (kind-only opt-in, CC-0100)` subsection
  under `## Kind Overlay Demo Addons`, sitting alongside the existing
  `### Chaos Mesh (kind-only opt-in)` example

Reviewers checking new kind-only opt-ins should confirm the production
omission is explicit, the opt-in flag has a single name documented in both
the Quick Start callout and the reference doc set, and the kind overlay
does not silently re-introduce the component into the production
kustomization root.
