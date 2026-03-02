package job

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const fieldManager = "cobaltcore-operator"

// IsJobComplete returns true when the given Job has a Complete condition with
// status True. It is a pure function that inspects the in-memory object without
// making any API calls. (CC-0005 / REQ-006)
func IsJobComplete(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// RunJob creates a Job using the create-or-get pattern for idempotency.
// If the Job already exists (by name), it is treated as idempotent — the
// existing Job is left as-is and the name is returned.
// Sets owner references for garbage collection. (CC-0005 / REQ-006)
func RunJob(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, job *batchv1.Job) (string, error) {
	if err := controllerutil.SetControllerReference(owner, job, scheme); err != nil {
		return "", fmt.Errorf("setting controller reference on Job %s/%s: %w", job.Namespace, job.Name, err)
	}

	err := c.Create(ctx, job)
	if apierrors.IsAlreadyExists(err) {
		return job.Name, nil
	}
	if err != nil {
		return "", fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
	}

	return job.Name, nil
}

// toUnstructuredApply converts a typed K8s object to an unstructured apply
// configuration for use with client.Apply().
func toUnstructuredApply(obj k8sruntime.Object) (k8sruntime.ApplyConfiguration, error) {
	data, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("converting to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: data}
	return client.ApplyConfigurationFromUnstructured(u), nil
}

// EnsureCronJob applies a CronJob via server-side apply with field manager.
// Sets owner references for garbage collection. (CC-0005 / REQ-006)
func EnsureCronJob(ctx context.Context, c client.Client, owner client.Object, scheme *k8sruntime.Scheme, cronJob *batchv1.CronJob) (string, error) {
	if err := controllerutil.SetControllerReference(owner, cronJob, scheme); err != nil {
		return "", fmt.Errorf("setting controller reference on CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}

	// Server-side apply requires TypeMeta to be set.
	cronJob.APIVersion = "batch/v1"
	cronJob.Kind = "CronJob"

	ac, err := toUnstructuredApply(cronJob)
	if err != nil {
		return "", fmt.Errorf("preparing CronJob %s/%s for apply: %w", cronJob.Namespace, cronJob.Name, err)
	}

	force := true
	if err := c.Apply(ctx, ac, &client.ApplyOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		return "", fmt.Errorf("applying CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}

	return cronJob.Name, nil
}
