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

// A service whose whole configuration document is credential-bearing renders it
// into a Secret, so the config volume has to source from one. The Secret wins
// over a ConfigMap name left in the params: a Job mounting an empty or wrong
// ConfigMap would start a pod with no configuration at all.
func TestBuildMigrationJob_ConfigSecretReplacesTheConfigMapVolume(t *testing.T) {
	g := gomega.NewWithT(t)
	p := migrationParams()
	p.ConfigSecretName = "barbican-config-abc"

	vols := BuildMigrationJob(p).Spec.Template.Spec.Volumes
	g.Expect(vols).To(gomega.HaveLen(1))
	g.Expect(vols[0].Name).To(gomega.Equal("config"))
	g.Expect(vols[0].ConfigMap).To(gomega.BeNil())
	g.Expect(vols[0].Secret).NotTo(gomega.BeNil())
	g.Expect(vols[0].Secret.SecretName).To(gomega.Equal("barbican-config-abc"))
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

// Once a CR names a target cluster, the owner and its Job live on two different
// clusters: the CR stays on the management cluster while the Job runs on the
// children cluster. The single client this helper takes is the owner's, and the
// split has to hold in both directions — the dedupe annotation lands on the
// management cluster, and the children cluster the Job was read from is never
// written to.
func TestRecordJobTerminalState_WritesOnlyToTheOwnerCluster(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()

	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "ns"}}
	ownerCluster := fake.NewClientBuilder().WithScheme(terminalScheme(t)).WithObjects(owner).Build()

	childrenScheme := terminalScheme(t)
	if err := batchv1.AddToScheme(childrenScheme); err != nil {
		t.Fatalf("batchv1: %v", err)
	}
	childrenWrites := 0
	childrenCluster := fake.NewClientBuilder().WithScheme(childrenScheme).
		WithObjects(completedJob("uid-1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				childrenWrites++
				return nil
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				childrenWrites++
				return nil
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				childrenWrites++
				return nil
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				childrenWrites++
				return nil
			},
		}).Build()

	// The Job the caller observed comes off the children cluster.
	var observed batchv1.Job
	g.Expect(childrenCluster.Get(ctx, apitypes.NamespacedName{Name: "db-sync", Namespace: "ns"}, &observed)).
		To(gomega.Succeed())

	var results []string
	recordFn := func(result string, _ time.Duration) { results = append(results, result) }
	RecordJobTerminalState(ctx, ownerCluster, nil, owner, "db-sync", &observed, "R", recordFn)

	g.Expect(results).To(gomega.Equal([]string{"succeeded"}))
	var storedOwner corev1.ConfigMap
	g.Expect(ownerCluster.Get(ctx, client.ObjectKeyFromObject(owner), &storedOwner)).To(gomega.Succeed())
	g.Expect(storedOwner.Annotations).To(gomega.HaveKeyWithValue(JobUIDAnnotationKey("db-sync"), "uid-1"),
		"the dedupe annotation belongs on the cluster that holds the owner")
	g.Expect(childrenWrites).To(gomega.BeZero(), "the children cluster must see no write")

	// The dedupe state lives on the owner cluster, so re-observing the same Job
	// is a no-op there too — and still writes nothing to the children cluster.
	RecordJobTerminalState(ctx, ownerCluster, nil, owner, "db-sync", &observed, "R", recordFn)
	g.Expect(results).To(gomega.HaveLen(1), "same UID must emit at most once")
	g.Expect(childrenWrites).To(gomega.BeZero(), "the children cluster must see no write")
}

// The dedupe patch must not land blindly on an object that moved on since the
// caller read it: the caller mirrors the response version back and hands it to a
// trailing Status().Update, so a blind patch would launder its stale snapshot
// into a write the API server accepts, silently overwriting whatever landed in
// between. The optimistic lock turns that into a deferral instead.
func TestRecordJobTerminalState_StaleOwnerVersionDefersInsteadOfPatchingBlindly(t *testing.T) {
	g := gomega.NewWithT(t)
	s := terminalScheme(t)
	stored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(stored).Build()

	// A concurrent write the reconcile never saw: the stored object advances
	// while the caller still holds the version it read.
	stale := stored.DeepCopy()
	stored.Data = map[string]string{"written": "concurrently"}
	g.Expect(c.Update(context.Background(), stored)).To(gomega.Succeed())
	g.Expect(stored.GetResourceVersion()).NotTo(gomega.Equal(stale.GetResourceVersion()))

	rec := record.NewFakeRecorder(4)
	called := false
	RecordJobTerminalState(context.Background(), c, rec, stale, "db-sync", completedJob("uid-1"), "DeferReason",
		func(string, time.Duration) { called = true })

	g.Expect(called).To(gomega.BeFalse(), "a stale snapshot must defer, not overwrite")
	g.Expect(stale.Annotations).NotTo(gomega.HaveKey(JobUIDAnnotationKey("db-sync")))
	g.Expect(stale.GetResourceVersion()).NotTo(gomega.Equal(stored.GetResourceVersion()),
		"the caller must not adopt a version its status was not computed from")

	// The concurrent write survives — the deferred patch never reached the object.
	var live corev1.ConfigMap
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(stored), &live)).To(gomega.Succeed())
	g.Expect(live.Data).To(gomega.HaveKeyWithValue("written", "concurrently"))
	g.Expect(live.Annotations).NotTo(gomega.HaveKey(JobUIDAnnotationKey("db-sync")))
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
