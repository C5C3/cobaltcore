# Pattern: Get-or-Create pattern for immutable Kubernetes resources

**Component**: internal/common/job
**Category**: data-access
**Applies-When**: Writing a helper function that manages Kubernetes resources with immutable spec fields (e.g., Job selector/template). Do NOT use controllerutil.CreateOrUpdate for these resources.

## Description

For Kubernetes resources with immutable spec fields (Job, potentially others), use an explicit Get-then-Create pattern instead of controllerutil.CreateOrUpdate. The function calls client.Get first; if IsNotFound, it sets the controller reference and calls client.Create; if found, it reads the existing status as-is without modifying the spec. This avoids 'field is immutable' errors on re-reconcile when the API server has defaulted spec fields that differ from the caller-supplied spec.

## Examples

### `internal/common/job/job.go:30`

```go
func RunJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.Job) (bool, error) {
	existing := &batchv1.Job{}
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, desired, c.Scheme()); err != nil {
			return false, fmt.Errorf("setting controller reference: %w", err)
		}
		if err := c.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("creating Job %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Job %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	return IsJobComplete(existing), nil
}
```

