# Review Pattern: Parallel test execution with shared mutable state

**Review-Area**: testing
**Detection-Hint**: When a test config sets parallel > 1, check whether the tests share any mutable infrastructure (databases, caches, pods). If any test action (e.g., pod kill, restart) affects resources used by other concurrent tests, parallelism is unsafe.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Look for parallel execution settings in test harness configs (e.g., chainsaw-config.yaml, pytest-xdist, Jest --workers). Then verify whether the tests under that config share namespaces, databases, or other stateful resources that one test's fault injection could disrupt for another.

## Why it matters

Concurrent tests mutating shared infrastructure produce non-deterministic failures — a pod-kill in one test causes cascading timeouts in another, making CI results unreliable and extremely hard to debug.

## Examples from external reviews

### CC-0047 — berendt
- **Feedback**: All three tests share the same MariaDB, Memcached, and OpenBao instances. With parallel: 2, any pair of concurrent tests will interfere because a pod-kill action against any infrastructure pod affects ALL Keystone CRs in the namespace, not just the CR owned by the test that injected the fault.
- **What was missed**: Look for parallel execution settings in test harness configs (e.g., chainsaw-config.yaml, pytest-xdist, Jest --workers). Then verify whether the tests under that config share namespaces, databases, or other stateful resources that one test's fault injection could disrupt for another.
- **Fix**: Changed parallel from 2 to 1 in chainsaw-config.yaml with updated comments explaining why serialization is required.