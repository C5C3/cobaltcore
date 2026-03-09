# Pattern: Manual Get+Create/Update with SetControllerReference

**Component**: internal/common/*/
**Category**: data-access
**Applies-When**: Implementing an Ensure* or Run* function that creates or updates a Kubernetes resource owned by a controller; Writing an Ensure* function that updates an existing Kubernetes resource and then evaluates its readiness status (returns (bool, error) where bool indicates readiness); Writing an Ensure* function that creates-or-updates a Kubernetes resource by comparing desired spec against existing spec

## Description

All K8s-interacting functions follow a consistent Get+IsNotFound+Create/Update pattern instead of controllerutil.CreateOrUpdate. On not-found: set controller reference via SetControllerReference, then Create. On exists: overwrite existing.Spec with desired.Spec, then Update. Owner references are set only on create (not on update). The pattern returns (bool, error) where bool indicates readiness. Error wrapping includes resource namespace/name context.

After calling c.Update on an existing resource, the in-memory object retains the status from the initial c.Get (pre-update). If readiness is evaluated against this stale status, it can return false-positive ready=true when the controller has not yet processed the spec change. The fix is to c.Get the object again after c.Update, before calling the Is*Ready function. Functions that only return error (no readiness evaluation) do not need the re-fetch.

All Ensure* functions that perform create-or-update on Kubernetes resources guard the c.Update call with apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec). When specs are equal, the update is skipped to avoid unnecessary API churn, spurious watch events, and 409 Conflict errors. When specs differ, the update is performed and the object is re-fetched to avoid evaluating stale status. The apiequality package is imported with the named alias 'apiequality'.

## Examples

### `internal/common/deployment/deployment.go:25`

```go
func EnsureDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, deploy *appsv1.Deployment) (bool, error) {
	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(deploy), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, deploy, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
		}
		if err := c.Create(ctx, deploy); err != nil {
			return false, fmt.Errorf("creating Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}

	existing.Spec = deploy.Spec
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("updating Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}

	return IsDeploymentReady(existing), nil
}
```

### `internal/common/tls/tls.go:27`

```go
func EnsureCertificate(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, cert *certmanagerv1.Certificate) (bool, error) {
	existing := &certmanagerv1.Certificate{}
	err := c.Get(ctx, client.ObjectKeyFromObject(cert), existing)

	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(owner, cert, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		if err := c.Create(ctx, cert); err != nil {
			return false, fmt.Errorf("creating Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
	}

	existing.Spec = cert.Spec
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("updating Certificate %s/%s: %w", cert.Namespace, cert.Name, err)
	}

	return IsCertificateReady(existing), nil
}
```
### `internal/common/deployment/deployment.go:42`

```go
existing.Spec = deploy.Spec
if err := c.Update(ctx, existing); err != nil {
	return false, fmt.Errorf("updating Deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
}
// Re-fetch to get the server-assigned Generation after the update (CC-0005).
if err := c.Get(ctx, client.ObjectKeyFromObject(deploy), existing); err != nil {
	return false, fmt.Errorf("re-fetching Deployment %s/%s after update: %w", deploy.Namespace, deploy.Name, err)
}

return IsDeploymentReady(existing), nil
```

### `internal/common/database/database.go:45`

```go
existing.Spec = db.Spec
if err := c.Update(ctx, existing); err != nil {
	return false, fmt.Errorf("updating Database %s/%s: %w", db.Namespace, db.Name, err)
}
// Re-fetch to avoid evaluating stale status from before the spec
// update (CC-0005).
if err := c.Get(ctx, client.ObjectKeyFromObject(db), existing); err != nil {
	return false, fmt.Errorf("re-fetching Database %s/%s after update: %w", db.Namespace, db.Name, err)
}

return IsDatabaseReady(existing), nil
```
### `internal/common/database/database.go:46`

```go
if !apiequality.Semantic.DeepEqual(existing.Spec, db.Spec) {
	existing.Spec = db.Spec
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("updating Database %s/%s: %w", db.Namespace, db.Name, err)
	}
	// Re-fetch to avoid evaluating stale status from before the spec
	// update (CC-0005).
	if err := c.Get(ctx, client.ObjectKeyFromObject(db), existing); err != nil {
		return false, fmt.Errorf("re-fetching Database %s/%s after update: %w", db.Namespace, db.Name, err)
	}
}
```

### `internal/common/job/job.go:96`

```go
if !apiequality.Semantic.DeepEqual(existing.Spec, cronJob.Spec) {
	existing.Spec = cronJob.Spec
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}
}
```



