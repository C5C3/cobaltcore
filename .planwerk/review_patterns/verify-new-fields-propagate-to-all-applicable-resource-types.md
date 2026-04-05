# Review Pattern: Verify new fields propagate to all applicable resource types

**Review-Area**: validation
**Detection-Hint**: When a PR adds a new spec field that affects one workload type (e.g., Deployment), check whether the controller also creates other workload types (e.g., Jobs, StatefulSets) that should receive the same field.
**Severity**: WARNING
**Occurrences**: 1

## What to check

List all Kubernetes resources the controller reconciles (Deployments, Jobs, CronJobs, etc.). For each new spec field, confirm it is applied to every resource type where it is semantically relevant, not just the most obvious one.

## Why it matters

Partial propagation means some workloads run with unbounded resources while others are constrained, leading to inconsistent behavior, OOM kills, or scheduling failures that only surface in production.

## Examples from external reviews

### CC-0042 — berendt
- **Feedback**: Resources not propagated to Jobs
- **What was missed**: List all Kubernetes resources the controller reconciles (Deployments, Jobs, CronJobs, etc.). For each new spec field, confirm it is applied to every resource type where it is semantically relevant, not just the most obvious one.
- **Fix**: Added TODO comments to reconcile_bootstrap.go, reconcile_credential.go, and reconcile_fernet.go documenting the known limitation and referencing the containerResources() pattern for future implementation.
