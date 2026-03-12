# Review Pattern: Verify conditional config blocks are actually rendered

**Review-Area**: validation
**Detection-Hint**: When a configuration patch sets a flag to disable a feature (e.g., `enabled: false`) but also provides nested config under that same feature's key, check whether the tooling/chart actually renders the nested config when the feature is disabled.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For Helm value overrides and similar declarative config: confirm that every config block being set is within a code path that is active given the other flags in the same patch. Look for contradictions like `ha.enabled: false` paired with `ha.raft.config`.

## Why it matters

Silent misconfiguration — the system starts successfully but uses a different backend/mode than intended. No error is raised, so the bug is invisible until it causes data loss or unexpected behavior in production.

## Examples from external reviews

### CC-0010 — greptile-apps[bot]
- **Feedback**: `ha.raft.config` is ignored when `ha.enabled: false` — the chart generates the config from `server.standalone.config` instead. OpenBao will fall back to whatever `server.standalone.config` is set in the upstream production manifest.
- **What was missed**: For Helm value overrides and similar declarative config: confirm that every config block being set is within a code path that is active given the other flags in the same patch. Look for contradictions like `ha.enabled: false` paired with `ha.raft.config`.
- **Fix**: Changed `ha.enabled: false` to `ha.enabled: true` with `ha.replicas: 1` so the Helm chart renders the Raft storage config block.
