// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package job

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Feature: CC-0005

func TestIntegration_RunJob(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-job-run"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "job-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-job",
			Namespace: ns.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
					},
				},
			},
		},
	}

	// First call creates the Job.
	ready, err := RunJob(ctx, c, scheme, owner, job)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	// Verify the Job exists with owner reference.
	created := &batchv1.Job{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "integration-job", Namespace: ns.Name}, created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("job-owner"))

	// Second call is idempotent — returns false (not complete) without error.
	ready, err = RunJob(ctx, c, scheme, owner, job)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
}

func TestIntegration_EnsureCronJob(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cronjob-ensure"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cronjob-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-cronjob",
			Namespace: ns.Name,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * 0",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{
								{Name: "worker", Image: "busybox:latest", Command: []string{"echo", "hello"}},
							},
						},
					},
				},
			},
		},
	}

	// Create.
	g.Expect(EnsureCronJob(ctx, c, scheme, owner, cronJob)).To(Succeed())

	created := &batchv1.CronJob{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cronJob), created)).To(Succeed())
	g.Expect(created.Spec.Schedule).To(Equal("0 0 * * 0"))
	g.Expect(created.OwnerReferences).To(HaveLen(1))

	// Update schedule.
	updated := cronJob.DeepCopy()
	updated.Spec.Schedule = "0 12 * * *"
	g.Expect(EnsureCronJob(ctx, c, scheme, owner, updated)).To(Succeed())

	fetched := &batchv1.CronJob{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cronJob), fetched)).To(Succeed())
	g.Expect(fetched.Spec.Schedule).To(Equal("0 12 * * *"))
}

func TestIntegration_EnsureCronJob_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cronjob-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cronjob-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-cronjob",
			Namespace: ns.Name,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * 0",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{
								{Name: "worker", Image: "busybox:latest", Command: []string{"echo"}},
							},
						},
					},
				},
			},
		},
	}

	g.Expect(EnsureCronJob(ctx, c, scheme, owner, cronJob)).To(Succeed())
	g.Expect(EnsureCronJob(ctx, c, scheme, owner, cronJob)).To(Succeed())

	list := &batchv1.CronJobList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}
