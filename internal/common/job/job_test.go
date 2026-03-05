// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Feature: CC-0005

func newFakeClient(objs ...client.Object) client.Client {
	s := clientgoscheme.Scheme
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&batchv1.Job{}).
		Build()
}

// owner returns a ConfigMap with UID and GVK set to use as the owner in tests.
func owner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       types.UID("owner-uid-1234"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
	}
}

// --- IsJobComplete ---

func TestIsJobComplete(t *testing.T) {
	tests := []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{
			name: "job with 0 succeeded returns false",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Succeeded: 0,
				},
			},
			want: false,
		},
		{
			name: "job with 1 succeeded returns true",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Succeeded: 1,
				},
			},
			want: true,
		},
		{
			name: "job with multiple succeeded returns true",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Succeeded: 3,
				},
			},
			want: true,
		},
		{
			name: "job with status not set returns false",
			job:  &batchv1.Job{},
			want: false,
		},
		{
			name: "nil job returns false",
			job:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(IsJobComplete(tt.job)).To(Equal(tt.want))
		})
	}
}

// --- RunJob ---

func TestRunJob_createsJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	_, err := RunJob(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the Job was created.
	var created batchv1.Job
	err = c.Get(ctx, types.NamespacedName{Name: "test-job", Namespace: "default"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.Template.Spec.Containers).To(HaveLen(1))
	g.Expect(created.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox"))
}

func TestRunJob_returnsTrue_whenComplete(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	// Pre-create a Job that has already succeeded.
	existing := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done-job",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}
	c := newFakeClient(existing)

	// Update the status on the fake client (status subresource).
	err := c.Status().Update(ctx, existing)
	g.Expect(err).NotTo(HaveOccurred())

	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done-job",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	complete, err := RunJob(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(complete).To(BeTrue())
}

func TestRunJob_returnsFalse_whenRunning(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-job",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	complete, err := RunJob(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(complete).To(BeFalse())
}

func TestRunJob_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	o := owner()
	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-job",
			Namespace: "default",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "worker", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	_, err := RunJob(ctx, c, o, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Fetch the created Job and verify owner reference.
	var created batchv1.Job
	err = c.Get(ctx, types.NamespacedName{Name: "owned-job", Namespace: "default"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}

// --- EnsureCronJob ---

func TestEnsureCronJob_createsCronJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	desired := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cronjob",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron-worker", Image: "busybox"},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	err := EnsureCronJob(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the CronJob was created.
	var created batchv1.CronJob
	err = c.Get(ctx, types.NamespacedName{Name: "test-cronjob", Namespace: "default"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.Schedule).To(Equal("*/5 * * * *"))
	g.Expect(created.Spec.JobTemplate.Spec.Template.Spec.Containers).To(HaveLen(1))
	g.Expect(created.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox"))
}

func TestEnsureCronJob_updatesCronJob(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	// Pre-create a CronJob with the old schedule.
	existing := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "update-cronjob",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron-worker", Image: "busybox:1.0"},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}
	c := newFakeClient(existing)

	// Update with a new schedule and image.
	desired := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "update-cronjob",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron-worker", Image: "busybox:2.0"},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	err := EnsureCronJob(ctx, c, owner(), desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Verify the CronJob was updated.
	var updated batchv1.CronJob
	err = c.Get(ctx, types.NamespacedName{Name: "update-cronjob", Namespace: "default"}, &updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updated.Spec.Schedule).To(Equal("0 * * * *"))
	g.Expect(updated.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:2.0"))
}

func TestEnsureCronJob_setsOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	c := newFakeClient()

	o := owner()
	desired := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "owned-cronjob",
			Namespace: "default",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/10 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "cron-worker", Image: "busybox"},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}

	err := EnsureCronJob(ctx, c, o, desired)
	g.Expect(err).NotTo(HaveOccurred())

	// Fetch the created CronJob and verify owner reference.
	var created batchv1.CronJob
	err = c.Get(ctx, types.NamespacedName{Name: "owned-cronjob", Namespace: "default"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].UID).To(Equal(types.UID("owner-uid-1234")))
}
