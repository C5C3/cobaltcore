// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package job

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// Feature: CC-0005

// helperCreateNamespace creates a namespace with the given name and returns it.
func helperCreateNamespace(ctx context.Context, g *GomegaWithT, c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	return ns
}

// helperCreateOwnerConfigMap creates a ConfigMap in the given namespace to use
// as an owner reference. The API server assigns a real UID.
func helperCreateOwnerConfigMap(ctx context.Context, g *GomegaWithT, c client.Client, name, namespace string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	g.Expect(c.Create(ctx, cm)).To(Succeed())
	return cm
}

// helperDesiredJob returns a minimal Job spec suitable for envtest.
func helperDesiredJob(name, namespace string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "worker",
							Image:   "busybox:latest",
							Command: []string{"echo", "hello"},
						},
					},
				},
			},
		},
	}
}

// helperDesiredCronJob returns a minimal CronJob spec suitable for envtest.
func helperDesiredCronJob(name, namespace, schedule, image string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{
								{
									Name:    "worker",
									Image:   image,
									Command: []string{"echo", "hello"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestIntegration_RunJob_CreatesJobInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace and owner ConfigMap.
	ns := helperCreateNamespace(ctx, g, c, "test-job-create-ns")
	owner := helperCreateOwnerConfigMap(ctx, g, c, "job-owner", ns.Name)

	// Call RunJob with a desired Job.
	desired := helperDesiredJob("test-job", ns.Name)
	completed, err := RunJob(ctx, c, owner, desired)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(completed).To(BeFalse(), "newly created Job should not be complete")

	// Verify the Job exists in the cluster.
	fetched := &batchv1.Job{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-job", Namespace: ns.Name}, fetched)).To(Succeed())

	// Verify owner reference points to the ConfigMap.
	g.Expect(fetched.OwnerReferences).NotTo(BeEmpty())
	g.Expect(fetched.OwnerReferences[0].UID).To(Equal(owner.UID))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal(owner.Name))
}

func TestIntegration_RunJob_DetectsCompletedJob(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace and owner ConfigMap.
	ns := helperCreateNamespace(ctx, g, c, "test-job-complete-ns")
	owner := helperCreateOwnerConfigMap(ctx, g, c, "job-owner", ns.Name)

	// Create a Job manually.
	job := helperDesiredJob("completed-job", ns.Name)
	g.Expect(c.Create(ctx, job)).To(Succeed())

	// Patch the Job status to Succeeded=1 via the status subresource.
	job.Status.Succeeded = 1
	g.Expect(c.Status().Update(ctx, job)).To(Succeed())

	// Call RunJob with the same Job name/namespace.
	desired := helperDesiredJob("completed-job", ns.Name)
	completed, err := RunJob(ctx, c, owner, desired)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(completed).To(BeTrue(), "Job with Succeeded=1 should be detected as complete")
}

func TestIntegration_EnsureCronJob_CreatesInCluster(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace and owner ConfigMap.
	ns := helperCreateNamespace(ctx, g, c, "test-cron-create-ns")
	owner := helperCreateOwnerConfigMap(ctx, g, c, "cron-owner", ns.Name)

	// Call EnsureCronJob.
	desired := helperDesiredCronJob("test-cronjob", ns.Name, "*/5 * * * *", "busybox:latest")
	g.Expect(EnsureCronJob(ctx, c, owner, desired)).To(Succeed())

	// Verify the CronJob exists in the cluster.
	fetched := &batchv1.CronJob{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "test-cronjob", Namespace: ns.Name}, fetched)).To(Succeed())

	// Verify schedule and container image.
	g.Expect(fetched.Spec.Schedule).To(Equal("*/5 * * * *"))
	g.Expect(fetched.Spec.JobTemplate.Spec.Template.Spec.Containers).NotTo(BeEmpty())
	g.Expect(fetched.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:latest"))

	// Verify owner reference.
	g.Expect(fetched.OwnerReferences).NotTo(BeEmpty())
	g.Expect(fetched.OwnerReferences[0].UID).To(Equal(owner.UID))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal(owner.Name))
}

func TestIntegration_EnsureCronJob_UpdatesExisting(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace and owner ConfigMap.
	ns := helperCreateNamespace(ctx, g, c, "test-cron-update-ns")
	owner := helperCreateOwnerConfigMap(ctx, g, c, "cron-owner", ns.Name)

	// Create CronJob with initial schedule.
	initial := helperDesiredCronJob("update-cronjob", ns.Name, "*/5 * * * *", "busybox:1.35")
	g.Expect(EnsureCronJob(ctx, c, owner, initial)).To(Succeed())

	// Update CronJob with new schedule and image.
	updated := helperDesiredCronJob("update-cronjob", ns.Name, "0 * * * *", "busybox:1.36")
	g.Expect(EnsureCronJob(ctx, c, owner, updated)).To(Succeed())

	// Verify the CronJob was updated.
	fetched := &batchv1.CronJob{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "update-cronjob", Namespace: ns.Name}, fetched)).To(Succeed())
	g.Expect(fetched.Spec.Schedule).To(Equal("0 * * * *"))
	g.Expect(fetched.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:1.36"))
}

func TestIntegration_IsJobComplete_WithRealJobStatus(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := helperCreateNamespace(ctx, g, c, "test-job-status-ns")

	// Create a Job.
	job := helperDesiredJob("status-job", ns.Name)
	g.Expect(c.Create(ctx, job)).To(Succeed())

	// Verify IsJobComplete returns false for the just-created Job.
	g.Expect(IsJobComplete(job)).To(BeFalse(), "newly created Job should not be complete")

	// Patch the Job status to Succeeded=1.
	job.Status.Succeeded = 1
	g.Expect(c.Status().Update(ctx, job)).To(Succeed())

	// Re-fetch the Job to get the updated status.
	fetched := &batchv1.Job{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "status-job", Namespace: ns.Name}, fetched)).To(Succeed())

	// Verify IsJobComplete returns true after status update.
	g.Expect(IsJobComplete(fetched)).To(BeTrue(), "Job with Succeeded=1 should be complete")
}
