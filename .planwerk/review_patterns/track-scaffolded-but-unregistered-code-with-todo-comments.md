# Review Pattern: Track scaffolded but unregistered code with TODO comments

**Review-Area**: documentation
**Detection-Hint**: When reviewing PRs that introduce new Setup/Register/Init functions, search main.go or the bootstrap entrypoint for a call to that function. If absent, confirm that a TODO comment with a tracking issue explains the deferral.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For every newly defined `SetupWebhookWithManager`, `SetupWithManager`, or similar registration function, verify it is either called in the entrypoint or has a tracked TODO explaining why it is deferred and which ticket will wire it up.

## Why it matters

Without a tracking comment, dead registration code creates a false sense of protection — developers assume validation is active when it is not. Future contributors may not realize the webhook is inert, leaving all runtime validation (cron parsing, duplicate detection, policy checks) silently disabled.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: `SetupWebhookWithManager` is defined but never called... all webhook validation is inactive. The only admission control in effect is the CRD XValidation CEL rules.
- **What was missed**: For every newly defined `SetupWebhookWithManager`, `SetupWithManager`, or similar registration function, verify it is either called in the entrypoint or has a tracked TODO explaining why it is deferred and which ticket will wire it up.
- **Fix**: Added `// TODO(CC-0012): Call (&keystonev1alpha1.KeystoneWebhook{}).SetupWebhookWithManager(mgr)` near the scheme registration in main.go to track the deferred webhook wiring.
