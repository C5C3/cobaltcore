# Review Pattern: Ensure test error-capture blocks collect sufficient diagnostic context

**Review-Area**: error-handling
**Detection-Hint**: When reviewing test catch/error blocks, ask: if this test fails at 3am, does the captured output include enough resources to diagnose root cause? If a step tests a running service, the catch block should capture not just the service object but also pod status and events. For auth/permission failures (403/401), the catch must capture logs from the server enforcing the policy (the authoritative source), not just the client that reported the error.
**Severity**: WARNING
**Occurrences**: 2

## What to check

For each catch or on-failure block in a test, check whether it captures the resources that could explain the failure. If the test validates API accessibility, the catch should include pod status (to detect CrashLoopBackOff/OOM) and namespace events, not just Service and Endpoints. For auth/permission failures, include logs from the component enforcing the policy (e.g., OpenBao audit/server logs for a PushSecret 403), since only that component records the denied path and the policy decision.

## Why it matters

Minimal catch blocks force engineers to manually reproduce failures to diagnose them. Capturing pod state and events in the error path drastically reduces mean-time-to-diagnosis for CI failures.

## Examples from external reviews

### CC-0060 — berendt
- **Feedback**: If the API test fails because the Keystone pod crashed (e.g., OOM after initial readiness) or is in CrashLoopBackOff, the catch block provides no pod-level signal.
- **What was missed**: For each catch or on-failure block in a test, check whether it captures the resources that could explain the failure. If the test validates API accessibility, the catch should include pod status (to detect CrashLoopBackOff/OOM) and namespace events, not just Service and Endpoints.
- **Fix**: Added pod status and namespace events to the catch blocks of both API accessibility test steps.

### CC-0083 — berendt
- **Feedback**: A 403 on PushSecret is most authoritatively diagnosed from OpenBao's audit log (which records the denied path + policy) rather than the ESO controller log (which only reports the HTTP status). Without OpenBao logs in the catch block, operators need a second CI run with more logging to diagnose failures.
- **What was missed**: In test failure handlers (catch blocks, teardown scripts), verify that diagnostic output includes logs from every component whose behavior could explain the failure. For auth/permission failures, the authoritative source (e.g., the server enforcing the policy) must be captured, not just the client reporting the error.
- **Fix**: Added `kubectl -n openbao logs -l app.kubernetes.io/name=openbao --tail=100` to the chainsaw catch script alongside the existing PushSecret status dump.
