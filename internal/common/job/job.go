package job

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultFieldManager is the recommended server-side apply field manager name
// for controllers using this package. Callers may use their own value if they
// need controller-specific ownership tracking. (CC-0005)
const DefaultFieldManager = "cobaltcore-operator"

// IsJobComplete returns true if the given Job has completed successfully.
// It checks for the presence of a "Complete" condition with status "True"
// in the Job's status conditions. This is a pure function that inspects
// the Job's status fields without making any Kubernetes API calls.
// (CC-0005, REQ-006)
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

// RunJob creates the given batch/v1 Job. If ownerRefs are provided they are
// set on the Job before creation. The operation is idempotent: if the Job
// already exists the function returns nil. (CC-0005, REQ-006, REQ-009, REQ-010)
func RunJob(ctx context.Context, c client.Client, job *batchv1.Job, ownerRefs ...metav1.OwnerReference) error {
	if len(ownerRefs) > 0 {
		job.SetOwnerReferences(ownerRefs)
	}

	err := c.Create(ctx, job)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

// EnsureCronJob uses server-side apply to create or update the given
// batch/v1 CronJob. If ownerRefs are provided they are set on the object
// before applying. (CC-0005, REQ-006, REQ-009, REQ-010)
func EnsureCronJob(ctx context.Context, c client.Client, cronJob *batchv1.CronJob, fieldManager string, ownerRefs ...metav1.OwnerReference) error {
	if len(ownerRefs) > 0 {
		cronJob.SetOwnerReferences(ownerRefs)
	}

	cronJob.SetGroupVersionKind(batchv1.SchemeGroupVersion.WithKind("CronJob"))

	applyConfig, err := toApplyConfiguration(cronJob)
	if err != nil {
		return fmt.Errorf("converting CronJob to apply configuration: %w", err)
	}

	if err := c.Apply(ctx, applyConfig, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying CronJob %s/%s: %w", cronJob.Namespace, cronJob.Name, err)
	}
	return nil
}

// toApplyConfiguration converts a typed Kubernetes object into a
// runtime.ApplyConfiguration suitable for client.Client.Apply().
// The object must have its GVK set before calling this function.
func toApplyConfiguration(obj k8sruntime.Object) (k8sruntime.ApplyConfiguration, error) {
	data, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: data}), nil
}
