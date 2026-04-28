# Pattern: Kubernetes event emission alongside condition-setting with FakeRecorder test assertions

**Component**: operators/keystone/internal/controller
**Category**: logging
**Applies-When**: Adding a new sub-reconciler or lifecycle transition that sets a status condition on the Keystone CR (or future operator CRs following the same pattern); Emitting a Warning/Normal event for a steady-state misconfiguration that is purely a function of user spec (so reconcile polling would otherwise re-emit the event indefinitely). Use an informational status condition (not Ready-blocking) as the transition gate.

## Description

Every significant lifecycle transition (success or failure) that sets a status condition via conditions.SetCondition also emits a Kubernetes event via r.Recorder.Event/Eventf. Normal type for success, Warning type for failure. No events for polling/in-progress states. Event reasons are stable PascalCase strings matching the condition Reason. Tests use record.NewFakeRecorder(10) and assert event emission with g.Expect(fakeRecorder.Events).To(Receive(ContainSubstring("Type Reason"))) for positive cases and g.Expect(fakeRecorder.Events).ToNot(Receive()) for negative cases (polling states).

Maintain a status condition that mirrors the misconfiguration state (Healthy=True/False with stable Reason strings), upsert it on every reconcile so status reflects current state, and only emit the corresponding event when the condition transitions into the failure state (prev==nil || prev.Status!=False || prev.Reason!=expectedReason). The condition is intentionally excluded from subConditionTypes/Ready aggregation so it surfaces the issue without flipping the CR Ready=False — appropriate for explicit operator overrides where the controller honours the override but warns about its impact. Tests must cover four cases: (a) initial transition emits, (b) repeat reconcile suppresses, (c) back-transition to healthy does not emit a recovery event, (d) happy-path reconcile writes True with the matching Reason. Same pattern as the existing CredentialKeysRotated/FernetKeysRotated event-condition coupling.

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
### `operators/keystone/internal/controller/reconcile_config.go:251-283`

```go
func (r *KeystoneReconciler) recordLoggingHealth(
	keystone *keystonev1alpha1.Keystone,
	merged map[string]map[string]string,
) {
	prev := conditions.GetCondition(keystone.Status.Conditions, conditionTypeLoggingHealthy)

	if merged["DEFAULT"] != nil && merged["DEFAULT"]["use_stderr"] != "true" {
		useStderr := merged["DEFAULT"]["use_stderr"]
		if prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != conditionReasonStderrDisabled {
			r.Recorder.Eventf(keystone, corev1.EventTypeWarning, "LoggingStderrDisabled",
				"spec.extraConfig overrode [DEFAULT].use_stderr to %q; container logs will not reach kubectl logs (CC-0098)",
				useStderr)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type:               conditionTypeLoggingHealthy,
			Status:             metav1.ConditionFalse,
			Reason:             conditionReasonStderrDisabled,
			ObservedGeneration: keystone.Generation,
			Message: fmt.Sprintf(
				"spec.extraConfig set [DEFAULT].use_stderr=%q; container logs will not reach kubectl logs (CC-0098)",
				useStderr),
		})
		return
	}

	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type:               conditionTypeLoggingHealthy,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonStderrEnabled,
		ObservedGeneration: keystone.Generation,
		Message:            "[DEFAULT].use_stderr is true; oslo.log records reach container stderr (CC-0098)",
	})
}
```

### `operators/keystone/internal/controller/reconcile_config_test.go:986-1019`

```go
func TestReconcileConfig_LoggingExtraConfigUseStderrFalseEventGatedOnTransition(t *testing.T) {
	g := NewGomegaWithT(t)
	s := configTestScheme()

	ks := configTestKeystone()
	ks.Spec.Logging = &keystonev1alpha1.LoggingSpec{Format: "text", Level: "INFO"}
	ks.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"use_stderr": "false"},
	}
	secret := dbCredentialsSecret("default", "keystone-db-credentials", "keystone", "pass")
	r := newConfigTestReconciler(s, ks, secret)

	// First reconcile: transition into Status=False/Reason=StderrDisabled fires.
	_, err := r.reconcileConfig(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	expectEvent(g, r, "Warning LoggingStderrDisabled")

	cond := meta.FindStatusCondition(ks.Status.Conditions, "LoggingHealthy")
	g.Expect(cond).NotTo(BeNil(), "first reconcile must set LoggingHealthy")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("StderrDisabled"))

	// Second reconcile: gate suppresses; FakeRecorder channel must remain empty.
	_, err = r.reconcileConfig(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	expectNoEvent(g, r)
}
```


