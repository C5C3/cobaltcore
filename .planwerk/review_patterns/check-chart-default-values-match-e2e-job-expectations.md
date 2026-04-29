# Review Pattern: Check chart default values match e2e job expectations

**Review-Area**: validation
**Detection-Hint**: Compare chart default values against what each CI job's chainsaw/test assertions require. If a job asserts a resource exists, the deploy path for that job must enable it (either via flag override or differing default).
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a values key defaults to `false` (opt-in feature) and a CI job exists specifically to test that feature, confirm the deploy path for that job overrides the default. Cross-reference values.yaml defaults against chainsaw step assertions in the corresponding job.

## Why it matters

Opt-in defaults are correct for users but require explicit overrides in feature-specific test jobs. Missing the override means the test job validates the disabled state, not the feature, and gives false confidence.

## Examples from external reviews

### CC-0100 — berendt
- **Feedback**: With chart default `monitoring.serviceMonitor.enabled: false`, the gated template renders zero output... chainsaw step 2 (servicemonitor-exists) fails and step 3 (prometheus-target-up) cascades; the e2e-prometheus job has continue-on-error: false, so this is a hard fail.
- **What was missed**: When a values key defaults to `false` (opt-in feature) and a CI job exists specifically to test that feature, confirm the deploy path for that job overrides the default. Cross-reference values.yaml defaults against chainsaw step assertions in the corresponding job.
- **Fix**: Updated the e2e-prometheus job's Deploy operator step to export WITH_PROMETHEUS=true so the chart override is applied and the ServiceMonitor is rendered.
