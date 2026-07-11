// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package job

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func migrationParams() MigrationJobParams {
	return MigrationJobParams{
		Name:              "keystone-db-sync",
		Namespace:         "openstack",
		Image:             "keystone:2025.2",
		ContainerName:     "db-sync",
		Command:           []string{"keystone-manage", "db_sync"},
		ConfigMapName:     "keystone-config",
		ConfigMountPath:   "/etc/keystone/keystone.conf.d/",
		Env:               []corev1.EnvVar{{Name: "OS_DATABASE__CONNECTION", Value: "mysql://x"}},
		PriorityClassName: "high",
		BackoffLimit:      4,
		SecurityContext:   &corev1.SecurityContext{RunAsNonRoot: ptr.To(true)},
	}
}

func TestBuildMigrationJob_BaseSpec(t *testing.T) {
	g := gomega.NewWithT(t)
	j := BuildMigrationJob(migrationParams())
	g.Expect(j.Name).To(gomega.Equal("keystone-db-sync"))
	g.Expect(*j.Spec.BackoffLimit).To(gomega.Equal(int32(4)))
	g.Expect(j.Spec.TTLSecondsAfterFinished).To(gomega.BeNil())
	spec := j.Spec.Template.Spec
	g.Expect(spec.RestartPolicy).To(gomega.Equal(corev1.RestartPolicyNever))
	g.Expect(spec.PriorityClassName).To(gomega.Equal("high"))
	g.Expect(spec.Containers).To(gomega.HaveLen(1))
	ctr := spec.Containers[0]
	g.Expect(ctr.Name).To(gomega.Equal("db-sync"))
	g.Expect(ctr.Command).To(gomega.Equal([]string{"keystone-manage", "db_sync"}))
	g.Expect(ctr.SecurityContext).NotTo(gomega.BeNil())
	g.Expect(ctr.Env).To(gomega.HaveLen(1))
	// The config ConfigMap is mounted read-only under the "config" volume.
	g.Expect(ctr.VolumeMounts).To(gomega.HaveLen(1))
	g.Expect(ctr.VolumeMounts[0].Name).To(gomega.Equal("config"))
	g.Expect(ctr.VolumeMounts[0].MountPath).To(gomega.Equal("/etc/keystone/keystone.conf.d/"))
	g.Expect(ctr.VolumeMounts[0].ReadOnly).To(gomega.BeTrue())
	g.Expect(spec.Volumes).To(gomega.HaveLen(1))
	g.Expect(spec.Volumes[0].ConfigMap.Name).To(gomega.Equal("keystone-config"))
}

func TestBuildMigrationJob_ExtrasAppendedAndOverrides(t *testing.T) {
	g := gomega.NewWithT(t)
	p := migrationParams()
	p.TTLSecondsAfterFinished = ptr.To(int32(300))
	p.BackoffLimit = 2
	p.ExtraVolumes = []corev1.Volume{{Name: "tls"}, {Name: "domains"}}
	p.ExtraVolumeMounts = []corev1.VolumeMount{{Name: "tls"}, {Name: "domains"}}
	j := BuildMigrationJob(p)
	g.Expect(*j.Spec.BackoffLimit).To(gomega.Equal(int32(2)))
	g.Expect(*j.Spec.TTLSecondsAfterFinished).To(gomega.Equal(int32(300)))
	// Config volume/mount stays first, extras appended in order.
	vols := j.Spec.Template.Spec.Volumes
	g.Expect(vols).To(gomega.HaveLen(3))
	g.Expect(vols[0].Name).To(gomega.Equal("config"))
	g.Expect(vols[1].Name).To(gomega.Equal("tls"))
	g.Expect(vols[2].Name).To(gomega.Equal("domains"))
	mounts := j.Spec.Template.Spec.Containers[0].VolumeMounts
	g.Expect(mounts).To(gomega.HaveLen(3))
	g.Expect(mounts[1].Name).To(gomega.Equal("tls"))
	g.Expect(mounts[2].Name).To(gomega.Equal("domains"))
}

func TestJobUIDAnnotationKey(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(JobUIDAnnotationKey("db-sync")).To(gomega.Equal("forge.c5c3.io/last-db-sync-job-uid"))
	g.Expect(JobUIDAnnotationKey("db-expand")).NotTo(gomega.Equal(JobUIDAnnotationKey("db-sync")))
}

func terminalScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	return s
}

func completedJob(uid string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-sync", Namespace: "ns", UID: apitypes.UID(uid),
			CreationTimestamp: metav1.NewTime(time.Unix(1_700_000_000, 0)),
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(time.Unix(1_700_000_030, 0)),
		}}},
	}
}

func TestRecordJobTerminalState_NilObservedNoOp(t *testing.T) {
	g := gomega.NewWithT(t)
	s := terminalScheme(t)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	called := false
	RecordJobTerminalState(context.Background(), c, nil, owner, "db-sync", nil, "R",
		func(string, time.Duration) { called = true })
	g.Expect(called).To(gomega.BeFalse())
}

func TestRecordJobTerminalState_AtMostOncePerUID(t *testing.T) {
	g := gomega.NewWithT(t)
	s := terminalScheme(t)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	job := completedJob("uid-1")

	var results []string
	recordFn := func(result string, d time.Duration) { results = append(results, result) }

	RecordJobTerminalState(context.Background(), c, nil, owner, "db-sync", job, "R", recordFn)
	g.Expect(results).To(gomega.Equal([]string{"succeeded"}))
	g.Expect(owner.Annotations).To(gomega.HaveKeyWithValue(JobUIDAnnotationKey("db-sync"), "uid-1"))

	// Re-observing the same UID must not re-emit.
	RecordJobTerminalState(context.Background(), c, nil, owner, "db-sync", job, "R", recordFn)
	g.Expect(results).To(gomega.HaveLen(1), "same UID must emit at most once")

	// A recreated Job (fresh UID) drives a fresh emission.
	RecordJobTerminalState(context.Background(), c, nil, owner, "db-sync", completedJob("uid-2"), "R", recordFn)
	g.Expect(results).To(gomega.HaveLen(2))
}

func TestRecordJobTerminalState_PatchFailureDefersAndEvents(t *testing.T) {
	g := gomega.NewWithT(t)
	s := terminalScheme(t)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return context.DeadlineExceeded
			},
		}).Build()
	rec := record.NewFakeRecorder(4)
	called := false
	RecordJobTerminalState(context.Background(), c, rec, owner, "db-sync", completedJob("uid-1"), "DeferReason",
		func(string, time.Duration) { called = true })

	g.Expect(called).To(gomega.BeFalse(), "record must be deferred when the UID patch fails")
	g.Expect(owner.Annotations).NotTo(gomega.HaveKey(JobUIDAnnotationKey("db-sync")))
	var ev string
	g.Eventually(rec.Events).Should(gomega.Receive(&ev))
	g.Expect(ev).To(gomega.ContainSubstring("Warning DeferReason"))
}
