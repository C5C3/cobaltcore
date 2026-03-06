# Review Pattern: Matrix job outputs silently keep only the last value

**Review-Area**: architecture
**Detection-Hint**: When a matrix job declares `outputs:` that a downstream job consumes via `needs.<job>.outputs.<key>`, flag it: GitHub Actions overwrites that output with whichever matrix leg finishes last. Check whether the downstream job needs results from ALL matrix entries or just one.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Verify that downstream jobs consuming matrix job outputs either (a) only need a single value that is identical across all matrix legs, or (b) use an aggregation mechanism (e.g., collecting into a JSON array, or giving the downstream job its own matrix). If neither applies, the downstream job silently validates only one arbitrary matrix entry.

## Why it matters

With a single-element matrix everything works, but expanding the matrix means most built artifacts go untested. The bug is invisible until a second matrix entry is added and a bad image ships because it was never smoke-tested.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The separate `smoke-test` job pulls `needs.build-service-images.outputs.image-ref`, which will silently be whichever matrix entry ran last. When the matrix grows to e.g. `service: [keystone, nova]`, both services are built but only one is smoke-tested on push events.
- **What was missed**: Verify that downstream jobs consuming matrix job outputs either (a) only need a single value that is identical across all matrix legs, or (b) use an aggregation mechanism (e.g., collecting into a JSON array, or giving the downstream job its own matrix). If neither applies, the downstream job silently validates only one arbitrary matrix entry.
- **Fix**: The smoke-test job was restructured with its own matrix strategy and independent image-ref derivation (checkout + tags step), eliminating the single-output limitation so each service is independently smoke-tested.
