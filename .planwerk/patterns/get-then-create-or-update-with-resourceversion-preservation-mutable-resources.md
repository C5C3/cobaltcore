# Pattern: Get-then-Create-or-Update with ResourceVersion preservation (mutable resources)

**Component**: internal/common/ K8s-interacting packages
**Category**: data-access
**Applies-When**: Ensuring Kubernetes resources that may need updates on reconciliation (Deployments, Services, CronJobs)

## Description

For resources that need updates when reconciled, the pattern is: Get the existing resource; if NotFound, Create; otherwise, copy ResourceVersion from existing to desired for optimistic concurrency, then Update. EnsureService additionally preserves the immutable ClusterIP field and rejects conflicting ClusterIP changes.

## Examples

### `internal/common/deployment/deployment.go:17-35`

```go
func EnsureDeployment(ctx context.Context, c client.Client, deployment *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(deployment), existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if createErr := c.Create(ctx, deployment); createErr != nil {
				return fmt.Errorf("creating Deployment %s/%s: %w", deployment.Namespace, deployment.Name, createErr)
			}
			return nil
		}
		return fmt.Errorf("getting Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	deployment.ResourceVersion = existing.ResourceVersion
	if err := c.Update(ctx, deployment); err != nil {
		return fmt.Errorf("updating Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	return nil
}
```

### `internal/common/job/job.go:43-60`

```go
func EnsureCronJob(ctx context.Context, c client.Client, cronJob *batchv1.CronJob) error {
	existing := &batchv1.CronJob{}
	err := c.Get(ctx, client.ObjectKeyFromObject(cronJob), existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if createErr := c.Create(ctx, cronJob); createErr != nil {
				return fmt.Errorf("creating CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, createErr)
			}
			return nil
		}
		return fmt.Errorf("getting CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}
	cronJob.ResourceVersion = existing.ResourceVersion
	if err := c.Update(ctx, cronJob); err != nil {
		return fmt.Errorf("updating CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}
	return nil
}
```

