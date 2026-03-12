# Review Pattern: Verify code compiles against actual API signatures

**Review-Area**: type-safety
**Detection-Hint**: When a struct field type or method call references an external package, verify the exact interface name and method signature exist on that type. Check that imports match the actual package used by the framework.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every external type reference and method call: (1) does the imported package export that exact type/method, (2) does the return type match the field it's assigned to, (3) is the method name spelled correctly including suffixes like 'For'.

## Why it matters

Two separate issues (wrong EventRecorder interface type and nonexistent method name) would have caused immediate compilation failure. These are not subtle runtime bugs — the code literally cannot build.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: `events.EventRecorder` is from `k8s.io/client-go/tools/events` (the newer structured-events API), while controller-runtime's `Manager.GetEventRecorderFor()` returns `record.EventRecorder` from `k8s.io/client-go/tools/record`. These are different, incompatible interfaces. Additionally, `main.go` line 47 calls `mgr.GetEventRecorder(...)` which does not exist on the `ctrl.Manager` interface — the correct method is `mgr.GetEventRecorderFor(name)`.
- **What was missed**: For every external type reference and method call: (1) does the imported package export that exact type/method, (2) does the return type match the field it's assigned to, (3) is the method name spelled correctly including suffixes like 'For'.
- **Fix**: Changed import from `k8s.io/client-go/tools/events` to `record "k8s.io/client-go/tools/record"`, changed field type to `record.EventRecorder`, and fixed method call to `mgr.GetEventRecorderFor("keystone-controller")`.
