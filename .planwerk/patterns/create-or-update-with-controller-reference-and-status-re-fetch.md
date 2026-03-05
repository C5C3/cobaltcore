# Pattern: Create-or-update with controller reference and status re-fetch

**Component**: internal/common/*/
**Category**: data-access
**Applies-When**: Implementing a Kubernetes resource ensure/create function that sets owner references and checks readiness status after creation

## Description

All CC-0005 Ensure* and Run* functions follow a 4-step pattern: (1) create an empty typed object with Name/Namespace from desired spec, (2) call controllerutil.CreateOrUpdate with a mutate func that copies desired.Spec and calls controllerutil.SetControllerReference, (3) re-fetch via c.Get() to obtain current status, (4) check status conditions or replica counts for readiness. Error wrapping uses fmt.Errorf with %w including resource type, namespace, and name. Functions return (bool, error) for readiness or error for non-status resources.

## Examples

### `internal/common/database/database.go:25-47`

```go
func EnsureDatabase(ctx context.Context, c client.Client, owner client.Object, desired *mariadbv1alpha1.Database) (bool, error) {
	existing := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		existing.Spec = desired.Spec
		return controllerutil.SetControllerReference(owner, existing, c.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating Database %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		return false, fmt.Errorf("fetching Database status %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return isDatabaseReady(existing), nil
}
```

### `internal/common/job/job.go:26-48`

```go
func RunJob(ctx context.Context, c client.Client, owner client.Object, desired *batchv1.Job) (bool, error) {
	existing := &batchv1.Job{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c, existing, func() error {
		if err := controllerutil.SetControllerReference(owner, existing, c.Scheme()); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		existing.Spec = desired.Spec
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("creating or updating Job %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		return false, fmt.Errorf("fetching Job status %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return IsJobComplete(existing), nil
}
```

