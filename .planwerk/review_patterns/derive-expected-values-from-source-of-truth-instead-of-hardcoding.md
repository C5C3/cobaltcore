# Review Pattern: Derive expected values from source of truth instead of hardcoding

**Review-Area**: validation
**Detection-Hint**: When a test asserts a numeric value (replica count, resource limit, port number, etc.), check whether that value is already declared in a spec, config file, or variable. If so, the test should read from that source rather than duplicating the literal.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Look for hardcoded numeric literals in test assertions that correspond to values defined in resource specs (e.g. Deployment .spec.replicas, ConfigMap entries). The test should read the expected value from the authoritative source so it stays correct when the configuration changes.

## Why it matters

Hardcoded expected values create a hidden coupling: when someone changes the Deployment replica count, the test silently becomes wrong, passing or failing for the wrong reason. Reading from the spec makes the test self-maintaining.

## Examples from external reviews

### CC-0076 — sourcery-ai[bot]
- **Feedback**: The script currently hardcodes the expected replica count as 3 when checking readyReplicas; consider reading the desired replica count from the Deployment spec (e.g. `.spec.replicas`) so the test remains valid if the memcached replica count changes.
- **What was missed**: Look for hardcoded numeric literals in test assertions that correspond to values defined in resource specs (e.g. Deployment .spec.replicas, ConfigMap entries). The test should read the expected value from the authoritative source so it stays correct when the configuration changes.
- **Fix**: Replaced the hardcoded replica count of 3 with a DESIRED variable read from .spec.replicas of the Deployment, plus a guard clause validating the value is non-empty.
