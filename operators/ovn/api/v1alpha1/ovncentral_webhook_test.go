// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validOVNCentral returns a minimal OVNCentral CR that passes every validation
// rule. Only the issuer name is set: every other knob is either resolved by the
// operator or defaulted by the API server from the CRD schema, and the webhook
// resolves the same values so it stays callable on a spec that never passed
// through API-server defaulting. Tests mutate single fields to exercise
// individual rules.
func validOVNCentral() *OVNCentral {
	return &OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ovn", Namespace: "openstack"},
		Spec: OVNCentralSpec{
			TLS: OVNTLSSpec{IssuerRef: OVNIssuerRef{Name: "ovn-ca"}},
		},
	}
}

func TestOVNCentralValidateCreate_AcceptsMinimalSpec(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{}

	warnings, err := w.ValidateCreate(context.Background(), validOVNCentral())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.BeEmpty())
}

// The full shape exercises every optional block at once, so a rule that rejects
// a legitimate combination (the two custom nodePort ranges above all, which the
// disjointness check sees together) fails here rather than in production.
func TestOVNCentralValidateCreate_AcceptsFullCustomSpec(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{}

	obj := validOVNCentral()
	obj.Spec.Image = &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/ovn", Tag: "25.03"}
	obj.Spec.Northbound = OVNDatabaseSpec{
		Replicas:          5,
		Storage:           OVNStorageSpec{Size: "10Gi", StorageClassName: ptr.To("fast")},
		NodePortBase:      ptr.To(int32(31000)),
		ElectionTimerMs:   5000,
		InactivityProbeMs: 0,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		},
	}
	obj.Spec.Southbound = OVNDatabaseSpec{
		Replicas:     3,
		Storage:      OVNStorageSpec{Size: "20Gi"},
		NodePortBase: ptr.To(int32(31010)),
	}
	obj.Spec.Northd = OVNNorthdSpec{
		Deployment: commonv1.DeploymentSpec{Replicas: 2},
		Threads:    4,
	}
	obj.Spec.Relay = &OVNRelaySpec{Replicas: 2}
	obj.Spec.TLS.IssuerRef.Kind = "Issuer"
	obj.Spec.Backup = &OVNBackupSpec{
		Schedule:      "30 1 * * *",
		RetentionDays: ptr.To(int32(30)),
		Storage:       OVNStorageSpec{Size: "50Gi"},
		S3: &OVNBackupS3Spec{
			Bucket:               "ovn-backups",
			Prefix:               "prod/",
			Endpoint:             "https://s3.example.com",
			Region:               "eu-central-1",
			CredentialsSecretRef: commonv1.SecretRefSpec{Name: "ovn-backup-s3"},
			Image:                &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/backup-shifter", Tag: "latest"},
		},
	}
	obj.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}

	warnings, err := w.ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(warnings).To(gomega.BeEmpty())
}

// Each case violates exactly one rule, so the assertion pins which rule fired
// rather than merely that something did.
func TestOVNCentralValidateCreate_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OVNCentral)
		wantMsg string
	}{
		{
			name: "nodePortBase leaves no room for the replicas",
			mutate: func(o *OVNCentral) {
				o.Spec.Northbound.NodePortBase = ptr.To(int32(32766))
			},
			wantMsg: "leaves no room for 3 replicas below 32767",
		},
		{
			name: "the two nodePort ranges overlap",
			mutate: func(o *OVNCentral) {
				o.Spec.Northbound.NodePortBase = ptr.To(int32(31000))
				o.Spec.Southbound.NodePortBase = ptr.To(int32(31001))
			},
			wantMsg: "northbound and southbound nodePort ranges overlap",
		},
		{
			name: "backup retention of zero days",
			mutate: func(o *OVNCentral) {
				o.Spec.Backup = &OVNBackupSpec{RetentionDays: ptr.To(int32(0))}
			},
			wantMsg: "retentionDays must be at least 1",
		},
		{
			name: "backup schedule is not a cron expression",
			mutate: func(o *OVNCentral) {
				o.Spec.Backup = &OVNBackupSpec{Schedule: "every other tuesday"}
			},
			wantMsg: "invalid cron expression",
		},
		{
			name: "S3 credentials Secret has no name",
			mutate: func(o *OVNCentral) {
				o.Spec.Backup = &OVNBackupSpec{S3: &OVNBackupS3Spec{
					Bucket:   "ovn-backups",
					Endpoint: "https://s3.example.com",
				}}
			},
			wantMsg: "credentialsSecretRef.name must be set",
		},
		{
			name: "image pins both a tag and a digest",
			mutate: func(o *OVNCentral) {
				o.Spec.Image = &commonv1.ImageSpec{
					Repository: "ghcr.io/c5c3/ovn",
					Tag:        "25.03",
					Digest:     strings.Repeat("a", 64),
				}
			},
			wantMsg: "exactly one of image.tag or image.digest must be set",
		},
		{
			name: "image pins neither a tag nor a digest",
			mutate: func(o *OVNCentral) {
				o.Spec.Image = &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/ovn"}
			},
			wantMsg: "exactly one of image.tag or image.digest must be set",
		},
		{
			name: "image has no repository",
			mutate: func(o *OVNCentral) {
				o.Spec.Image = &commonv1.ImageSpec{Tag: "25.03"}
			},
			wantMsg: "repository must be set",
		},
		{
			name: "TLS issuer has no name",
			mutate: func(o *OVNCentral) {
				o.Spec.TLS.IssuerRef.Name = ""
			},
			wantMsg: "issuerRef.name must be set",
		},
		{
			name: "targetClusterRef has an empty name",
			mutate: func(o *OVNCentral) {
				o.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{}
			},
			wantMsg: "target cluster name must be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNCentralWebhook{}

			obj := validOVNCentral()
			tt.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.wantMsg)))
		})
	}
}

// The children already exist on the cluster the ref named at creation, so every
// transition away from it strands them. The CEL transition rules on
// OVNCentralSpec say the same thing at the schema layer; this is the webhook
// half, which also covers a cluster whose CRD predates those rules.
func TestOVNCentralValidateUpdate_RejectsTargetClusterRefTransitions(t *testing.T) {
	tests := []struct {
		name     string
		old, new *commonv1.TargetClusterRefSpec
	}{
		{
			name: "renamed",
			old:  &commonv1.TargetClusterRefSpec{Name: "edge-1"},
			new:  &commonv1.TargetClusterRefSpec{Name: "edge-2"},
		},
		{
			name: "added",
			old:  nil,
			new:  &commonv1.TargetClusterRefSpec{Name: "edge-1"},
		},
		{
			name: "removed",
			old:  &commonv1.TargetClusterRefSpec{Name: "edge-1"},
			new:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNCentralWebhook{}

			oldObj := validOVNCentral()
			oldObj.Spec.TargetClusterRef = tt.old
			newObj := validOVNCentral()
			newObj.Spec.TargetClusterRef = tt.new

			_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("targetClusterRef is immutable")))
		})
	}
}

// metadata.name is bounded by the child object with the tightest name budget,
// the "{name}-backup" CronJob. Nothing else in the CRD or the webhook bounds it,
// so without this rule a name the API server would refuse as a CronJob admits
// cleanly and the backup silently never gets created.
func TestOVNCentralValidateCreate_NameLengthBoundedByBackupCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{}

	atLimit := validOVNCentral()
	atLimit.Name = strings.Repeat("o", MaxOVNCentralNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits the 52-character CronJob budget must be accepted")

	tooLong := validOVNCentral()
	tooLong.Name = strings.Repeat("o", MaxOVNCentralNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("-backup")))
}

// The bound is create-only. metadata.name is immutable, so on update it could
// only ever fire against a CR a pre-upgrade operator already admitted, and the
// validating webhook registers the update verb, so it also sees the
// finalizer-removal update reconcileDelete issues. Rejecting that would wedge
// the grandfathered CR in Terminating forever, with no field left to edit to
// repair it.
func TestOVNCentralValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{}

	grandfathered := validOVNCentral()
	grandfathered.Name = strings.Repeat("o", MaxOVNCentralNameLength+1)
	grandfathered.Finalizers = []string{"ovn.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// Shortening the retention window is the one edit on this CR that destroys data
// at the next firing with no undo, and a typo is indistinguishable from an
// intended change at admission time. The warning is what echoes it back.
func TestOVNCentralValidateUpdate_WarnsOnReducedBackupRetention(t *testing.T) {
	objWith := func(b *OVNBackupSpec) *OVNCentral {
		o := validOVNCentral()
		o.Spec.Backup = b
		return o
	}
	tests := []struct {
		name     string
		old, new *OVNBackupSpec
		wantWarn bool
	}{
		{
			name:     "window shortened",
			old:      &OVNBackupSpec{RetentionDays: ptr.To(int32(14))},
			new:      &OVNBackupSpec{RetentionDays: ptr.To(int32(7))},
			wantWarn: true,
		},
		{
			name: "window lengthened",
			old:  &OVNBackupSpec{RetentionDays: ptr.To(int32(7))},
			new:  &OVNBackupSpec{RetentionDays: ptr.To(int32(14))},
		},
		{
			name: "both sides unset, so both resolve the operator default",
			old:  nil,
			new:  nil,
		},
		{
			name:     "block dropped below an explicit longer window",
			old:      &OVNBackupSpec{RetentionDays: ptr.To(int32(30))},
			new:      nil,
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNCentralWebhook{}

			warnings, err := w.ValidateUpdate(context.Background(), objWith(tt.old), objWith(tt.new))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			if tt.wantWarn {
				g.Expect(warnings).To(gomega.ContainElement(
					gomega.ContainSubstring("spec.backup.retentionDays reduced")))
				return
			}
			g.Expect(warnings).To(gomega.BeEmpty())
		})
	}
}

// The mutating webhook is registered but resolves nothing, so a CR that round
// trips through it has to come back byte-identical. A default materialized here
// by accident would freeze today's value into stored state and stop tracking the
// operator default across upgrades.
func TestOVNCentralDefault_LeavesTheObjectUnchanged(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{}

	obj := validOVNCentral()
	obj.Spec.Backup = &OVNBackupSpec{}
	before := obj.DeepCopy()

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj).To(gomega.Equal(before))
}

// --- Node placement, priority class and spec.jobs validation ---

// Every OVN central workload that carries a placement rejects a node selector
// the API server would refuse, at the path of the block that set it.
func TestOVNCentralValidate_NodePlacementRejected(t *testing.T) {
	for _, tc := range []struct {
		name      string
		placement func(o *OVNCentral) *commonv1.NodePlacementSpec
		path      string
	}{
		{"northd deployment", func(o *OVNCentral) *commonv1.NodePlacementSpec {
			return &o.Spec.Northd.Deployment.NodePlacementSpec
		}, "spec.northd.deployment"},
		{"relay", func(o *OVNCentral) *commonv1.NodePlacementSpec {
			o.Spec.Relay = &OVNRelaySpec{Replicas: 1}
			return &o.Spec.Relay.NodePlacementSpec
		}, "spec.relay"},
		{"northbound", func(o *OVNCentral) *commonv1.NodePlacementSpec { return &o.Spec.Northbound.NodePlacementSpec }, "spec.northbound"},
		{"southbound", func(o *OVNCentral) *commonv1.NodePlacementSpec { return &o.Spec.Southbound.NodePlacementSpec }, "spec.southbound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			o := validOVNCentral()
			p := tc.placement(o)
			p.NodeSelector = map[string]string{"bad key": "x"}
			p.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpEqual}}

			_, err := (&OVNCentralWebhook{}).ValidateCreate(context.Background(), o)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.path + ".nodeSelector: Invalid value"))
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.path + ".tolerations[0].operator: Invalid value"))
		})
	}
}

func TestOVNCentralValidate_DatabasePriorityClassNotFound(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
	o := validOVNCentral()
	o.Spec.Northbound.PriorityClassName = ptr.To("typo")

	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("spec.northbound.priorityClassName: Not found"))

	// Without a reader the lookup is skipped, and an empty class is no lookup.
	_, err = (&OVNCentralWebhook{}).ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	o.Spec.Northbound.PriorityClassName = ptr.To("")
	_, err = w.ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestOVNCentralValidate_JobsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		jobs *commonv1.JobSpec
		want string
	}{
		{
			name: "unknown priority class",
			jobs: &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("typo")}},
			want: "spec.jobs.priorityClassName: Not found",
		},
		{
			name: "memory request above limit",
			jobs: &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}}},
			want: "spec.jobs.resources.requests.memory: Invalid value",
		},
		{
			name: "node selector key",
			jobs: &commonv1.JobSpec{NodePlacementSpec: commonv1.NodePlacementSpec{NodeSelector: map[string]string{"bad key": "x"}}},
			want: "spec.jobs.nodeSelector: Invalid value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNCentralWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
			o := validOVNCentral()
			o.Spec.Jobs = tc.jobs

			_, err := w.ValidateCreate(context.Background(), o)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
		})
	}
}

// The finalizer removal reconcileDelete issues is an update, and the validating
// webhook sees it. A priorityClassName admitted earlier can name a PriorityClass
// deleted since, and rejecting the removal would hold the CR in Terminating. A
// deleting CR whose spec changes is still validated.
func TestOVNCentralValidateUpdate_FinalizerRemovalOnADeletingCRSkipsValidation(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	w := &OVNCentralWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}

	stale := validOVNCentral()
	stale.Spec.Southbound.PriorityClassName = ptr.To("deleted-class")
	stale.Spec.Jobs = &commonv1.JobSpec{JobBaseSpec: commonv1.JobBaseSpec{PriorityClassName: ptr.To("deleted-class")}}
	stale.Finalizers = []string{"openstack.c5c3.io/remote-children"}
	stale.DeletionTimestamp = ptr.To(metav1.Now())

	released := stale.DeepCopy()
	released.Finalizers = nil
	_, err := w.ValidateUpdate(ctx, stale, released)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"the finalizer removal must pass however the unchanged spec fares against today's rules")

	edited := released.DeepCopy()
	edited.Spec.Northd.Deployment.Replicas = 3
	_, err = w.ValidateUpdate(ctx, stale, edited)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.jobs.priorityClassName: Not found")),
		"a spec edit on a deleting CR is validated like any other")
}

func TestOVNCentralValidate_EmptyJobsAccepted(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNCentralWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
	o := validOVNCentral()
	o.Spec.Jobs = &commonv1.JobSpec{}

	_, err := w.ValidateCreate(context.Background(), o)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}
