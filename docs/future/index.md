---
title: Future
---

# Future

This section collects **idea sketches** for the forge project: explorations of
where the operators could go next, written down early enough to be discussed,
challenged, and reshaped before anyone commits to an implementation.

Pages in this section are explicitly **not**:

- **Decided architecture.** Accepted designs and their rationale live in the
  architecture documentation, not here. A Future page graduates by being
  triaged through a GitHub issue and, if accepted, turned into a proper
  design — at which point the sketch here is replaced by a pointer or removed.
- **Reference documentation.** Nothing described here is guaranteed to exist
  in the codebase. Where a page describes current behavior, it does so only as
  the baseline for the proposed change.
- **A roadmap commitment.** Phases, API shapes, and field names are working
  material for discussion.

## Current sketches

- [Brownfield Keystone Adoption](./brownfield-keystone-adoption.md) — adopt an
  existing OpenStack Keystone installation incrementally. **Phase 1 has
  graduated**: the service-less, External-mode ControlPlane is implemented, and
  running it is documented in
  [Adopt an External Keystone](../guides/adopt-external-keystone.md). The page
  keeps the later phases (infrastructure attach, service takeover) as sketches.
