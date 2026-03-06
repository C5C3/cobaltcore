# Review Pattern: Verify declarative config files are actually consumed by the build

**Review-Area**: architecture
**Detection-Hint**: When a YAML/config manifest declares packages or dependencies, trace whether any build step (Dockerfile, Makefile, CI script) actually reads and uses that file. If the build hardcodes the same information separately, the manifest is documentation-only and must be clearly marked as such.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check if the values in a config manifest (e.g., extra-packages.yaml listing pip extras) are consumed programmatically by the build, or if the Dockerfile independently hardcodes the same values. If they are decoupled, verify that (a) a prominent warning exists stating the file is documentation-only, and (b) a test or CI check asserts consistency between the manifest and the actual build instructions.

## Why it matters

A config file that looks authoritative but is silently ignored creates a drift trap: contributors will update the manifest and assume the change takes effect, when in fact a separate file (e.g., Dockerfile) also needs updating. Count-based tests (e.g., 'assert 3 pip items') won't catch content divergence.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `pip_packages` is **never read** by the build process — it is pure documentation. The risk is that someone adds a new extra to `pip_packages` and expects it to be installed, when in fact the Dockerfile also needs to be updated independently.
- **What was missed**: Check if the values in a config manifest (e.g., extra-packages.yaml listing pip extras) are consumed programmatically by the build, or if the Dockerfile independently hardcodes the same values. If they are decoupled, verify that (a) a prominent warning exists stating the file is documentation-only, and (b) a test or CI check asserts consistency between the manifest and the actual build instructions.
- **Fix**: Strengthened the warning comment to explicitly state that adding entries does NOT automatically install them and that the corresponding Dockerfile line must also be updated.
