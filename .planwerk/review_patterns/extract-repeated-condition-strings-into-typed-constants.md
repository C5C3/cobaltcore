# Review Pattern: Extract repeated condition strings into typed constants

**Review-Area**: type-safety
**Detection-Hint**: Search for string literals that appear more than once and represent status condition types, reasons, or similar enum-like values. Grep for quoted strings used in condition-setting helpers and status updates.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a reconciler sets status conditions, verify that condition type and reason strings are defined as named constants (ideally typed) rather than repeated inline string literals. Check that every call site references the constant, not a duplicated literal.

## Why it matters

Repeated string literals are fragile: a typo in one location silently creates a different condition type or reason, breaking status consumers and making bugs hard to trace. Constants give compile-time safety and make refactors mechanical rather than error-prone.

## Examples from external reviews

### CC-0039 — sourcery-ai[bot]
- **Feedback**: The NetworkPolicy reconciliation logic uses hard-coded condition type/reason strings (e.g., "NetworkPolicyReady", "NetworkPolicyNotRequired") in multiple places; consider centralizing these as typed constants to avoid typos and make future refactors less error-prone.
- **What was missed**: When a reconciler sets status conditions, verify that condition type and reason strings are defined as named constants (ideally typed) rather than repeated inline string literals. Check that every call site references the constant, not a duplicated literal.
- **Fix**: Extracted conditionTypeNetworkPolicyReady, conditionReasonNetworkPolicyReady, and conditionReasonNetworkPolicyNotRequired as typed constants at lines 29-33 and replaced all inline string literals with references to them.
