# Pattern: Kubernetes event emission alongside condition-setting with FakeRecorder test assertions

**Component**: operators/keystone/internal/controller
**Category**: logging
**Applies-When**: Adding a new sub-reconciler or lifecycle transition that sets a status condition on the Keystone CR (or future operator CRs following the same pattern)

## Description

Every significant lifecycle transition (success or failure) that sets a status condition via conditions.SetCondition also emits a Kubernetes event via r.Recorder.Event/Eventf. Normal type for success, Warning type for failure. No events for polling/in-progress states. Event reasons are stable PascalCase strings matching the condition Reason. Tests use record.NewFakeRecorder(10) and assert event emission with g.Expect(fakeRecorder.Events).To(Receive(ContainSubstring("Type Reason"))) for positive cases and g.Expect(fakeRecorder.Events).ToNot(Receive()) for negative cases (polling states).

## Examples

### `operators/keystone/internal/controller/reconcile_bootstrap.go:39`

```go
r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "BootstrapFailed", "Keystone bootstrap job failed: %v", err)
```

### `operators/keystone/internal/controller/reconcile_bootstrap_test.go:205-206`

```go
fakeRecorder := r.Recorder.(*record.FakeRecorder)
g.Expect(fakeRecorder.Events).To(Receive(ContainSubstring("Normal BootstrapComplete")))
```

