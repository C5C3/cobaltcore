# Review Pattern: Track scaffolded but unregistered code with TODO comments

**Review-Area**: documentation
**Detection-Hint**: When reviewing PRs that introduce new Setup/Register/Init functions, search main.go or the bootstrap entrypoint for a call to that function. If absent, confirm that a TODO comment with a tracking issue explains the deferral.
**Severity**: WARNING
**Occurrences**: 2

## What to check

For every newly defined `SetupWebhookWithManager`, `SetupWithManager`, or similar registration function, verify it is either called in the entrypoint or has a tracked TODO explaining why it is deferred and which ticket will wire it up.

## Why it matters

Without a tracking comment, dead registration code creates a false sense of protection — developers assume validation is active when it is not. Future contributors may not realize the webhook is inert, leaving all runtime validation (cron parsing, duplicate detection, policy checks) silently disabled.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: `SetupWebhookWithManager` is defined but never called... all webhook validation is inactive. The only admission control in effect is the CRD XValidation CEL rules.
- **What was missed**: For every newly defined `SetupWebhookWithManager`, `SetupWithManager`, or similar registration function, verify it is either called in the entrypoint or has a tracked TODO explaining why it is deferred and which ticket will wire it up.
- **Fix**: Added `// TODO(CC-0012): Call (&keystonev1alpha1.KeystoneWebhook{}).SetupWebhookWithManager(mgr)` near the scheme registration in main.go to track the deferred webhook wiring.

### CC-0017 — berendt
- **Feedback**: Also update the documentation dependency chain in docs/reference/backend/keystone-operator-packaging.md to include this dependency with its rationale.
- **What was missed**: After modifying dependency lists, feature flags, or architectural components, grep documentation for enumerations or diagrams of those items. Hardcoded counts like 'three infrastructure operators' and dependency tables must reflect the actual set.
- **Fix**: The HelmRelease dependsOn was updated but the documentation dependency chain section was NOT updated — this part of the fix was incomplete.
