# Review Pattern: Verify tool output format matches downstream parsing

**Review-Area**: testing
**Detection-Hint**: When a command pipeline pipes tool output (e.g., yq, jq) into grep/awk, check whether the grep pattern matches the actual output format. If yq outputs YAML by default but grep looks for JSON-quoted keys like '"uses":', the count will always be 0 and assertions will silently produce wrong results.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

In any shell test that chains a data-extraction tool (yq, jq, xmllint) with grep -c on a specific string pattern, confirm the output format of the upstream tool matches the pattern being grepped. Look for quoted JSON keys in grep when the upstream tool emits YAML, or vice versa.

## Why it matters

The test appears to run without error but always evaluates to 0, meaning it never actually validates the condition it claims to test. This is a false-passing test that provides zero coverage while giving false confidence.

## Examples from external reviews

### CC-0029 — greptile-apps[bot]
- **Feedback**: `test_sbom_attestation_steps_exist` has the same root problem as `test_sbom_generation_steps_exist` above: it greps for the JSON-format key `"uses":` in what is likely YAML output from `yq`. Both `base_attest_count` and `service_attest_count` will evaluate to `"0"` and fail their assertions.
- **What was missed**: In any shell test that chains a data-extraction tool (yq, jq, xmllint) with grep -c on a specific string pattern, confirm the output format of the upstream tool matches the pattern being grepped. Look for quoted JSON keys in grep when the upstream tool emits YAML, or vice versa.
- **Fix**: Changed from `grep -c '"uses":'` on raw yq YAML output to using `yq_raw` to extract `.uses` values and `grep -c .` to count non-empty lines, applied across 4 locations in 2 test functions.
