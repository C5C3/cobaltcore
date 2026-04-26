# Review Pattern: Document intentional environment divergence in overlays

**Review-Area**: documentation
**Detection-Hint**: When a kustomize overlay patch changes multiple default behaviors (disables features, overrides resource limits, changes runtime settings), check whether the base configuration's defaults are intentional for non-overlayed environments and whether this divergence is documented. The strongest form of divergence is moving a component out of the production base entirely into a kind-only opt-in overlay — verify that the production omission and the opt-in flag are both documented.
**Severity**: WARNING
**Occurrences**: 2

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
