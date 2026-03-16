# Review Pattern: Test fixtures must mirror real repository structure

**Review-Area**: testing
**Detection-Hint**: When reviewing test setup code that creates temporary directories and files, compare the directory structure created in the test workdir against the actual repository layout. If the test places files at different relative paths than production, it will mask path-related bugs.
**Severity**: BLOCKING
**Occurrences**: 3

## What to check

Check that test workdir file placement (e.g., '$workdir/upper-constraints.txt') matches where the file lives in the real repo (e.g., '$workdir/releases/2025.2/upper-constraints.txt'). A test that passes with a flat structure but would fail against the real repo tree is a false-positive test.

## Why it matters

Tests that use a simplified directory structure give false confidence — they pass even though the code under test would fail in every real invocation. The bug in the script went undetected precisely because the tests accommodated the wrong path.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Each test workdir places `upper-constraints.txt` directly in the temp dir root rather than under `releases/2025.2/upper-constraints.txt`. Once `CONSTRAINTS` is corrected, the test setup should be updated to stay consistent with the real repository structure.
- **What was missed**: Check that test workdir file placement (e.g., '$workdir/upper-constraints.txt') matches where the file lives in the real repo (e.g., '$workdir/releases/2025.2/upper-constraints.txt'). A test that passes with a flat structure but would fail against the real repo tree is a false-positive test.
- **Fix**: Updated test setup to create files under 'releases/2025.2/' subdirectory in the test workdir, matching the real repository layout.

### CC-0014 — greptile-apps[bot]
- **Feedback**: The function only sets the `Available=True` condition, but a real Kubernetes deployment controller also sets `Progressing=True (reason: NewReplicaSetAvailable)` when a rollout completes. ... if `IsDeploymentReady` is ever updated to also guard on `Progressing`, every integration test that calls `SimulateDeploymentReady` would silently fail.
- **What was missed**: Does the test simulator set ALL status conditions and fields that the real system sets in that state, not just the minimum subset the current readiness check inspects?
- **Fix**: Added the `DeploymentProgressing` condition with `Reason: NewReplicaSetAvailable` alongside the existing `DeploymentAvailable` condition in `SimulateDeploymentReady`. Same pattern applied to `SimulateJobComplete` which was missing `StartTime` and `JobSuccessCriteriaMet`.

### CC-0014 — greptile-apps[bot]
- **Feedback**: `driveFullReconciliation` hardcodes `3` for `SimulateDeploymentReady`. `IsDeploymentReady` checks `Status.ReadyReplicas >= *Spec.Replicas` — so if the `integrationBrownfieldKeystone` fixture is ever changed to a replica count **greater than 3** (e.g., 5), `SimulateDeploymentReady(..., 3)` would set `ReadyReplicas=3`, which is less than `desired=5`, causing `IsDeploymentReady` to return `false`. The test would then silently hang at `waitForCondition`.
- **What was missed**: Look for numeric literals passed to test simulation/assertion helpers. Trace whether the same value is defined or implied by the object under test (e.g., Spec.Replicas). If so, the helper should read the value from the object rather than hardcoding it.
- **Fix**: Read the desired replica count from the already-created Deployment spec instead of hardcoding it: `deploy := &appsv1.Deployment{}; c.Get(ctx, deployKey, deploy); replicas := *deploy.Spec.Replicas; SimulateDeploymentReady(ctx, c, deployKey, replicas)`.
