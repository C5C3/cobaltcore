# Pattern: Symmetric DeepCopy-then-normalize before spec comparison

**Component**: internal/common/deployment, internal/common/job
**Category**: data-access
**Applies-When**: Comparing a desired Kubernetes resource spec against the existing server-returned spec to decide whether an update is needed; Implementing create-or-update logic for mutable Kubernetes resources (Deployments, Services, CronJobs) where the API server applies defaulting that makes comparison unreliable without a normalization layer

## Description

Both the desired and existing specs are DeepCopied, then normalized via the appropriate normalize.*Defaults function, before being compared with apiequality.Semantic.DeepEqual. This prevents both false positives (spurious updates from API-server-injected defaults) and ensures neither the caller's object nor the cached existing object is mutated.

For mutable resources (Deployments, Services, CronJobs), the Ensure* functions always update the spec to the desired state on every reconciliation, rather than comparing existing vs desired specs. This avoids the maintenance burden of a normalization layer that mirrors API-server defaulting logic. The approach trades a small number of no-op API writes for complete elimination of false-positive and false-negative spec comparisons. Server-assigned immutable fields (ClusterIP, ClusterIPs, IPFamilies on Services) are explicitly preserved before the update.

## Examples

### `internal/common/deployment/deployment.go:49`

```go
desiredSpec := deploy.Spec.DeepCopy()
normalize.DeploymentSpecDefaults(desiredSpec)
existingSpec := existing.Spec.DeepCopy()
normalize.DeploymentSpecDefaults(existingSpec)

if !apiequality.Semantic.DeepEqual(*existingSpec, *desiredSpec) {
```

### `internal/common/job/job.go:110`

```go
desiredSpec := cronJob.Spec.DeepCopy()
normalize.CronJobSpecDefaults(desiredSpec)
existingSpec := existing.Spec.DeepCopy()
normalize.CronJobSpecDefaults(existingSpec)

if !apiequality.Semantic.DeepEqual(*existingSpec, *desiredSpec) {
```
### `internal/common/deployment/deployment.go:43-53`

```go
	// Always update the spec to the desired state. This avoids maintaining
	// a normalization layer to replicate API-server defaulting logic, which
	// would be an unmanageable maintenance burden (CC-0005).
	existing.Spec = deploy.Spec
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("updating Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}
```

### `internal/common/job/job.go:128-134`

```go
	// Always update the spec to the desired state. This avoids maintaining
	// a normalization layer to replicate API-server defaulting logic
	// (CC-0005).
	existing.Spec = cronJob.Spec
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}
```


