# Review Pattern: Ensure test error-capture blocks collect sufficient diagnostic context

**Review-Area**: error-handling
**Detection-Hint**: When reviewing test catch/error blocks, ask: if this test fails at 3am, does the captured output include enough resources to diagnose root cause? If a step tests a running service, the catch block should capture not just the service object but also pod status and events.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For each catch or on-failure block in a test, check whether it captures the resources that could explain the failure. If the test validates API accessibility, the catch should include pod status (to detect CrashLoopBackOff/OOM) and namespace events, not just Service and Endpoints.

## Why it matters

Minimal catch blocks force engineers to manually reproduce failures to diagnose them. Capturing pod state and events in the error path drastically reduces mean-time-to-diagnosis for CI failures.

## Examples from external reviews

### CC-0060 — berendt
- **Feedback**: If the API test fails because the Keystone pod crashed (e.g., OOM after initial readiness) or is in CrashLoopBackOff, the catch block provides no pod-level signal.
- **What was missed**: For each catch or on-failure block in a test, check whether it captures the resources that could explain the failure. If the test validates API accessibility, the catch should include pod status (to detect CrashLoopBackOff/OOM) and namespace events, not just Service and Endpoints.
- **Fix**: Added pod status and namespace events to the catch blocks of both API accessibility test steps.
