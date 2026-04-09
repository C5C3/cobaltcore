# Review Pattern: Validate schema and format, not current cardinality

**Review-Area**: validation
**Detection-Hint**: When a validation script asserts a minimum item count (e.g., >= 1) on a list field, ask whether that count is a true invariant or just happens to hold for today's data. Look for assertions like 'length > 0', 'select(. != null)', or 'has at least one entry' on config fields that are inherently optional.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Does the validation enforce constraints beyond what the schema actually requires? Could a legitimate future input (e.g., a release with no extra packages) fail this check? Is the assertion checking type/format or business-level content?

## Why it matters

Overly strict validation creates false negatives — valid configurations are rejected, forcing maintainers to either add dummy entries or weaken the check later under time pressure.

## Examples from external reviews

### CC-0051 — sourcery-ai[bot]
- **Feedback**: In `verify_release_config.sh`, the extra-packages validation now enforces `keystone.pip_extras` and `keystone.apt_packages` to have at least one entry for every `releases/*/extra-packages.yaml`; if a future release legitimately has no extras or apt packages, this will cause false negatives, so you may want to allow empty lists and only assert schema/format.
- **What was missed**: Does the validation enforce constraints beyond what the schema actually requires? Could a legitimate future input (e.g., a release with no extra packages) fail this check? Is the assertion checking type/format or business-level content?
- **Fix**: Relaxed the pip_extras and apt_packages validations from enforcing >= 1 items to only asserting the YAML type is !!seq (array), allowing empty lists while still validating schema correctness.
