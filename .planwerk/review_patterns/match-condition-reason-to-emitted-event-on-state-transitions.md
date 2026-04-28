# Review Pattern: Match condition Reason to emitted event on state transitions

**Review-Area**: architecture
**Detection-Hint**: When a reconciler sets a Status condition after performing a distinct action (e.g. rotation applied vs steady-state), check that the Reason and Message differ from the steady-state branch and align with any event emitted on that branch.
**Severity**: WARNING
**Occurrences**: 2

## What to check

On early-return branches after applying a change, verify the condition Reason/Message reflect the transition that just occurred rather than being copy-pasted from the steady-state path. Cross-check against the event emitted on the same branch.

## Why it matters

Stale or copy-pasted condition messages make `kubectl describe` misleading during operational debugging — users cannot distinguish 'just rotated' from 'steady state' even though the controller emitted a rotation event.

## Examples from external reviews

### CC-0081 — berendt
- **Feedback**: On the apply-success early return, the condition is set with Reason 'CredentialKeysAvailable' and a message copy-pasted from the steady-state path at step 8. It does not reflect that a rotation was just applied, and the reconciler short-circuits before re-running steps 4-7.
- **What was missed**: On early-return branches after applying a change, verify the condition Reason/Message reflect the transition that just occurred rather than being copy-pasted from the steady-state path. Cross-check against the event emitted on the same branch.
- **Fix**: Changed Reason to 'CredentialKeysRotated'/'FernetKeysRotated' on the apply-success branch and updated the message to 'rotation applied; staging secret cleared' to match the emitted event.

### CC-0098 — berendt
- **Feedback**: LoggingStderrDisabled event fires on every reconcile instead of on state transition
- **What was missed**: Warning/Normal events emitted from reconcile loops must be gated on an actual state transition — typically by maintaining a status condition and only recording the event when the condition's status flips. Otherwise reconcile loops spam the event stream.
- **Fix**: Refactored into `recordLoggingHealth` helper that maintains a `LoggingHealthy` status condition and only emits the Warning event when the condition transitions.
