// In the cluster's hum, where containers dream,
// a Job awakes beside a byte-lit stream.
// It mounts its volumes, secrets tightly pressed,
// and writes its purpose on the scheduler's chest.
//
// Through reconcile loops the controller flies,
// past CronJob tides beneath Kubernetes skies.
// Each pod a verse in YAML's careful rhyme,
// converging state—one spec at a time.
//
// When conditions bloom and status turns to True,
// the finalizer bows; its work is through.
// So rest, dear Job, your exit code is zero—
// in the land of orchestration, you're the hero.

package job

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsJobComplete returns true if the given Job has a "Complete" condition with
// status "True". This indicates the Job has successfully finished all of its
// work. (CC-0005, REQ-006)
func IsJobComplete(j *batchv1.Job) bool {
	if j == nil {
		return false
	}
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// RunJob creates the given Job if it does not already exist. Jobs are immutable
// once created, so an AlreadyExists error is treated as success. (CC-0005, REQ-009)
func RunJob(ctx context.Context, c client.Client, job *batchv1.Job) error {
	if err := c.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

// EnsureCronJob creates or updates the given CronJob. If the CronJob does not
// exist it is created; otherwise the existing resource is updated in place.
// (CC-0005, REQ-010)
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
