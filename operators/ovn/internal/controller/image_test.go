// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// dockerfileOVNVersionPattern captures the version off the single
// ARG OVN_VERSION line of images/ovn/Dockerfile. The image records the tag with
// its upstream "v" prefix, the operator constant without it, so the prefix is
// matched rather than captured.
var dockerfileOVNVersionPattern = regexp.MustCompile(`(?m)^ARG OVN_VERSION=v(.+)$`)

func TestEffectiveImage_NilResolvesDefault(t *testing.T) {
	g := NewWithT(t)

	image := effectiveImage(nil)

	g.Expect(image.Repository).To(Equal(defaultOVNRepository))
	g.Expect(image.Tag).To(Equal(defaultOVNVersion))
	g.Expect(image.Digest).To(BeEmpty())
	g.Expect(image.Reference()).To(Equal("ghcr.io/c5c3/ovn:" + defaultOVNVersion))
}

// An override reaches the workloads unchanged, digest included: a CR that pins a
// digest has closed the supply-chain gap a mutable tag leaves open, and a
// resolver that re-attached the default tag would reopen it.
func TestEffectiveImage_DigestPreserved(t *testing.T) {
	g := NewWithT(t)
	digest := "sha256:" + strings.Repeat("a", 64)

	image := effectiveImage(&commonv1.ImageSpec{Repository: "registry.example.com/ovn", Digest: digest})

	g.Expect(image.Tag).To(BeEmpty(), "a digest-pinned override must not gain the default tag")
	g.Expect(image.Digest).To(Equal(digest))
	g.Expect(image.Reference()).To(Equal("registry.example.com/ovn@" + digest))
}

func TestEffectiveShifterImage_NilResolvesLatest(t *testing.T) {
	g := NewWithT(t)

	image := effectiveShifterImage(nil)

	g.Expect(image.Repository).To(Equal(defaultBackupShifterRepository))
	g.Expect(image.Tag).To(Equal("latest"))
	g.Expect(image.Reference()).To(Equal("ghcr.io/c5c3/backup-shifter:latest"))
}

func TestEffectiveShifterImage_OverrideVerbatim(t *testing.T) {
	g := NewWithT(t)
	override := commonv1.ImageSpec{Repository: "registry.example.com/shifter", Tag: "2026.1"}

	g.Expect(effectiveShifterImage(&override)).To(Equal(override))
	g.Expect(effectiveShifterImage(&override).Reference()).To(Equal("registry.example.com/shifter:2026.1"))
}

// The operator default and the image it runs are two records of one version. A
// bump that lands in only one of them ships an operator whose default image tag
// names a tag the build never pushed, so the two are compared here rather than
// left to a reviewer.
func TestDefaultOVNVersionMatchesDockerfilePin(t *testing.T) {
	g := NewWithT(t)

	_, thisFile, _, ok := runtime.Caller(0)
	g.Expect(ok).To(BeTrue(), "runtime.Caller must resolve this test file to locate the Dockerfile")
	dockerfile := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "images", "ovn", "Dockerfile")

	content, err := os.ReadFile(filepath.Clean(dockerfile))
	g.Expect(err).NotTo(HaveOccurred(), "images/ovn/Dockerfile must be readable at %s", dockerfile)

	match := dockerfileOVNVersionPattern.FindSubmatch(content)
	g.Expect(match).NotTo(BeNil(),
		"images/ovn/Dockerfile carries no line matching %q; defaultOVNVersion has nothing to be checked against",
		dockerfileOVNVersionPattern.String())
	g.Expect(string(match[1])).To(Equal(defaultOVNVersion),
		"defaultOVNVersion and ARG OVN_VERSION in images/ovn/Dockerfile must name the same OVN version")
}

// pullPolicyCase is one row of the OVN pull-policy tables: the image override
// a CR names (nil resolves the operator default) and the policy every container
// that runs it must carry.
type pullPolicyCase struct {
	name  string
	image func(repository string) *commonv1.ImageSpec
	want  corev1.PullPolicy
}

// pullPolicyCases covers the three ways a policy resolves: a nil override
// resolves the default tag, which names no pullPolicy (Always); a digest
// override without pullPolicy (IfNotPresent); and an explicit pullPolicy.
var pullPolicyCases = []pullPolicyCase{
	{name: "nil image resolves the default tag", image: func(string) *commonv1.ImageSpec { return nil }, want: corev1.PullAlways},
	{
		name: "digest without pullPolicy",
		image: func(repository string) *commonv1.ImageSpec {
			return &commonv1.ImageSpec{Repository: repository, Digest: "sha256:" + strings.Repeat("a", 64)}
		},
		want: corev1.PullIfNotPresent,
	},
	{
		name: "explicit pullPolicy Never",
		image: func(repository string) *commonv1.ImageSpec {
			return &commonv1.ImageSpec{Repository: repository, Tag: "custom", PullPolicy: corev1.PullNever}
		},
		want: corev1.PullNever,
	},
}

// expectPullPolicy checks every init container and container of every pod.
func expectPullPolicy(g *WithT, specs map[string]corev1.PodSpec, want corev1.PullPolicy) {
	for name, spec := range specs {
		containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
		g.Expect(containers).NotTo(BeEmpty(), name)
		for _, c := range containers {
			g.Expect(c.ImagePullPolicy).To(Equal(want), "%s/%s", name, c.Name)
		}
	}
}

// TestOVNCentralWorkloads_ImagePullPolicy pins the pull policy of every
// container of every OVNCentral workload: the two Raft StatefulSets, northd,
// the relay and the backup CronJob, whose snapshot init container runs the OVN
// image and whose upload container runs the backup-shifter image.
func TestOVNCentralWorkloads_ImagePullPolicy(t *testing.T) {
	for _, tc := range pullPolicyCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := pinS3BackupOVNCentral()
			cr.Spec.Relay = &ovnv1alpha1.OVNRelaySpec{Replicas: 2}
			cr.Spec.Image = tc.image(defaultOVNRepository)
			cr.Spec.Backup.S3.Image = tc.image(defaultBackupShifterRepository)

			specs := map[string]corev1.PodSpec{
				"ovsdb-nb": raftStatefulSet(cr, northboundDB(cr)).Spec.Template.Spec,
				"ovsdb-sb": raftStatefulSet(cr, southboundDB(cr)).Spec.Template.Spec,
				"northd":   buildNorthdDeployment(cr).Spec.Template.Spec,
				"relay":    buildRelayDeployment(cr).Spec.Template.Spec,
				"backup":   backupCronJob(cr, effectiveBackup(cr)).Spec.JobTemplate.Spec.Template.Spec,
			}
			expectPullPolicy(g, specs, tc.want)

			backup := specs["backup"]
			g.Expect(backup.InitContainers).To(HaveLen(1), "the snapshot runs before the upload")
			g.Expect(backup.Containers).To(HaveLen(1))
			g.Expect(backup.Containers[0].Name).To(Equal("shifter"))
			g.Expect(backup.Containers[0].Image).To(HavePrefix(defaultBackupShifterRepository))
		})
	}
}

// TestOVNChassisWorkloads_ImagePullPolicy pins the pull policy of every
// container of every OVNChassis workload: the Open vSwitch and ovn-controller
// DaemonSets and the maintenance Jobs.
func TestOVNChassisWorkloads_ImagePullPolicy(t *testing.T) {
	for _, tc := range pullPolicyCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := testOVNChassis()
			cr.Spec.Image = tc.image(defaultOVNRepository)
			central := pinChassisCentral()

			expectPullPolicy(g, map[string]corev1.PodSpec{
				"ovs":            buildOVSDaemonSet(cr).Spec.Template.Spec,
				"ovn-controller": buildControllerDaemonSet(cr, central).Spec.Template.Spec,
				"apply":          applyJob(cr, central, "node-a").Spec.Template.Spec,
				"chassis-del":    chassisDelJob(cr, central, "node-a", nodeEntry{systemID: "system-id"}).Spec.Template.Spec,
			}, tc.want)
		})
	}
}
