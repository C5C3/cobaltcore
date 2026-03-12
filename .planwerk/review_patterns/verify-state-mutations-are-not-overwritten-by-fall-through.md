# Review Pattern: Verify state mutations are not overwritten by fall-through

**Review-Area**: error-handling
**Detection-Hint**: When a conditional branch sets a status condition or state value, trace the control flow after that branch. If there is no early return or else-guard, check whether subsequent code unconditionally overwrites the value just set.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

After any block that sets a status condition (especially to a transient state like 'False' or 'Pending'), verify there is an early return or re-check before code that sets the same condition to a different value. Look for missing return/break statements after create-and-set-status blocks.

## Why it matters

The FernetKeysReady condition was set to False after secret creation, but without an early return, execution fell through to code that unconditionally set it to True. The False state was never observable and no requeue fired, defeating the purpose of the condition entirely.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: When the secret is not found, the code creates it and sets the `FernetKeysReady` condition to `False`. But because there is no early `return` after this block, execution falls straight through to step 4, which unconditionally sets `FernetKeysReady` back to `True`. The `False` state is never observable externally and no requeue is emitted.
- **What was missed**: After any block that sets a status condition (especially to a transient state like 'False' or 'Pending'), verify there is an early return or re-check before code that sets the same condition to a different value. Look for missing return/break statements after create-and-set-status blocks.
- **Fix**: Added `return ctrl.Result{Requeue: true}, nil` immediately after setting FernetKeysReady to False inside the IsNotFound branch, preventing fall-through to the True-setting code.
