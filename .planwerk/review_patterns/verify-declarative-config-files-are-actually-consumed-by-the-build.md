# Review Pattern: Verify declarative config files are actually consumed by the build

**Review-Area**: architecture
**Detection-Hint**: When a YAML/config manifest declares packages or dependencies, trace whether any build step (Dockerfile, Makefile, CI script) actually reads and uses that file. If the build hardcodes the same information separately, the manifest is documentation-only and must be clearly marked as such.
**Severity**: WARNING
**Occurrences**: 3

## What to check

Check if the values in a config manifest (e.g., extra-packages.yaml listing pip extras) are consumed programmatically by the build, or if the Dockerfile independently hardcodes the same values. If they are decoupled, verify that (a) a prominent warning exists stating the file is documentation-only, and (b) a test or CI check asserts consistency between the manifest and the actual build instructions.

## Why it matters

A config file that looks authoritative but is silently ignored creates a drift trap: contributors will update the manifest and assume the change takes effect, when in fact a separate file (e.g., Dockerfile) also needs updating. Count-based tests (e.g., 'assert 3 pip items') won't catch content divergence.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `pip_packages` is **never read** by the build process — it is pure documentation. The risk is that someone adds a new extra to `pip_packages` and expects it to be installed, when in fact the Dockerfile also needs to be updated independently.
- **What was missed**: Check if the values in a config manifest (e.g., extra-packages.yaml listing pip extras) are consumed programmatically by the build, or if the Dockerfile independently hardcodes the same values. If they are decoupled, verify that (a) a prominent warning exists stating the file is documentation-only, and (b) a test or CI check asserts consistency between the manifest and the actual build instructions.
- **Fix**: Strengthened the warning comment to explicitly state that adding entries does NOT automatically install them and that the corresponding Dockerfile line must also be updated.

### CC-0011 — greptile-apps[bot]
- **Feedback**: The Makefile's generate target loops over $(OPERATORS) and skips internal/common entirely, so there is currently no make generate path that regenerates this file with the proper header.
- **What was missed**: Compare the generation invocation (Makefile target, go:generate directive, or script) for every generated file touched in the PR against existing generation targets for the same tool. Verify all required flags (e.g., headerFile for controller-gen) are present and that the Makefile actually has a target that regenerates the file.
- **Fix**: Added a dedicated generate-common Makefile target that runs controller-gen with the headerFile argument, and regenerated zz_generated.deepcopy.go with the proper SPDX header.

### CC-0013 — greptile-apps[bot]
- **Feedback**: The CronJob completes without error. On next pod restart, kubelet re-populates the tmpfs from the unchanged Kubernetes object, reverting all keys to their original values. Fernet tokens signed under the "rotated" key become invalid after the pod is recycled, producing spurious 401 responses for active sessions.
- **What was missed**: If a CronJob or container runs a command that writes/rotates files on a Secret-backed volume mount (e.g., fernet_rotate writing to /etc/keystone/fernet-keys), verify there is an explicit step that writes updated content back to the Kubernetes API (e.g., kubectl patch, client-go update). Without this, changes are lost on pod restart.
- **Fix**: The Fernet rotation CronJob was redesigned to use an init container copying keys to an emptyDir, then a Python script PATCHes the updated keys back to the K8s Secret via the API. Additional RBAC resources (ServiceAccount, Role, RoleBinding) were added to support this.
