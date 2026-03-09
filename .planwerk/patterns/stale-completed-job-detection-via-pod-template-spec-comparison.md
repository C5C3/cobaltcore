# Pattern: Stale completed Job detection via pod template spec comparison

**Component**: internal/common/job
**Category**: data-access
**Applies-When**: Implementing idempotent Job execution that must re-run when the Job spec changes (e.g., operator upgrade with new container image); Implementing create-or-update logic for Kubernetes resources whose specs are mutated by API-server defaulting (e.g., Jobs where PodSpec fields like ImagePullPolicy are filled in after creation), and you need to detect whether the desired spec has changed from what was originally created

## Description

RunJob guards against stale completed Jobs by comparing existing.Spec.Template.Spec against the desired job.Spec.Template.Spec using apiequality.Semantic.DeepEqual after confirming IsJobComplete. When specs differ (e.g., new container image from an operator upgrade), the old Job is deleted and a new one is created with the updated spec, returning (false, nil) so the reconciler requeues. This prevents silently skipping migrations when the operator binary changes.

Instead of maintaining a normalization layer that mirrors API-server defaulting logic (fragile and requires updates with every Kubernetes version), a SHA-256 hash of the desired spec is stored as an annotation at creation time. On subsequent reconciliations, the hash of the current desired spec is compared against the stored annotation. This is used specifically for Jobs because they are immutable after creation — if the spec changes, the old Job must be deleted and a new one created. The annotation key follows the pattern 'forge.c5c3.io/<spec-type>-hash'.

## Examples

### `internal/common/job/job.go:53`

```go
if IsJobComplete(existing) {
	// Guard against stale completed Jobs: if the pod spec has changed
	// (e.g. operator upgrade with a new container image), delete the
	// old Job and create a new one so the updated migration runs
	// (CC-0005).
	if !apiequality.Semantic.DeepEqual(existing.Spec.Template.Spec, job.Spec.Template.Spec) {
		if err := c.Delete(ctx, existing); err != nil {
			return false, fmt.Errorf("deleting stale Job %s/%s: %w", existing.Namespace, existing.Name, err)
		}
		if err := controllerutil.SetControllerReference(owner, job, scheme); err != nil {
			return false, fmt.Errorf("setting owner reference on Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		if err := c.Create(ctx, job); err != nil {
			return false, fmt.Errorf("creating replacement Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return false, nil
	}
	return true, nil
}
```

### `internal/common/job/job_test.go:178`

```go
func TestRunJob_existingComplete_specChanged(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	now := metav1.Now()
	oldJob := testJob()
	oldJob.Status.Succeeded = 1
	oldJob.Status.CompletionTime = &now
	oldJob.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner, oldJob).
		WithStatusSubresource(oldJob).
		Build()

	newJob := testJob()
	newJob.Spec.Template.Spec.Containers[0].Image = "busybox:v2"

	ready, err := RunJob(context.Background(), c, s, owner, newJob)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "should recreate Job when spec changes after completion")
}
```
### `internal/common/job/job.go:23-34`

```go
const PodSpecHashAnnotation = "forge.c5c3.io/pod-spec-hash"

func PodSpecHash(spec *corev1.PodSpec) string {
	data, _ := json.Marshal(spec)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
```

### `internal/common/job/job.go:69-79`

```go
		desiredHash := PodSpecHash(&job.Spec.Template.Spec)
		existingHash := existing.Annotations[PodSpecHashAnnotation]
		if desiredHash != existingHash {
			if err := c.Delete(ctx, existing); err != nil {
				return false, fmt.Errorf("deleting stale Job %s/%s: %w", existing.Namespace, existing.Name, err)
			}
			if err := createJobWithHash(ctx, c, scheme, owner, job); err != nil {
				return false, err
			}
			return false, nil
		}
```


