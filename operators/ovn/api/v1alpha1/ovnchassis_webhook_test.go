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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validOVNChassis returns a minimal OVNChassis CR that passes every validation
// rule: the OVNCentral it registers with and the one node label that keeps the
// DaemonSets off every other node. Tests mutate single fields to exercise
// individual rules.
func validOVNChassis() *OVNChassis {
	return &OVNChassis{
		ObjectMeta: metav1.ObjectMeta{Name: "test-chassis", Namespace: "openstack"},
		Spec: OVNChassisSpec{
			CentralRef:   OVNCentralRef{Name: "test-ovn"},
			NodeSelector: map[string]string{"openstack.c5c3.io/network-node": "true"},
		},
	}
}

func TestOVNChassisValidateCreate_AcceptedShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OVNChassis)
	}{
		{
			name:   "minimal",
			mutate: func(*OVNChassis) {},
		},
		{
			name: "every optional block set",
			mutate: func(o *OVNChassis) {
				o.Spec.Image = &commonv1.ImageSpec{Repository: "ghcr.io/c5c3/ovn", Tag: "25.03"}
				o.Spec.Tolerations = []corev1.Toleration{{
					Key:      "openstack.c5c3.io/network-node",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}}
				o.Spec.Gateway = &OVNGatewaySpec{
					NodeSelector: map[string]string{"openstack.c5c3.io/gateway": "true"},
				}
				o.Spec.BridgeMappings = []OVNBridgeMapping{
					{PhysicalNetwork: "physnet1", Bridge: "br-ex"},
					{PhysicalNetwork: "physnet2", Bridge: "br-ex2"},
				}
				o.Spec.EncapType = "vxlan"
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{Type: "OnDelete"}
				o.Spec.RemoteProbeIntervalMs = 0
				o.Spec.OVS = &OVNChassisContainerSpec{Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				}}
				o.Spec.Controller = &OVNChassisContainerSpec{Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}}
				o.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
			},
		},
		{
			name: "rolling update paced by a percentage",
			mutate: func(o *OVNChassis) {
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{
					Type:           "RollingUpdate",
					MaxUnavailable: ptr.To(intstr.FromString("10%")),
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNChassisWebhook{}

			obj := validOVNChassis()
			tt.mutate(obj)

			warnings, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(warnings).To(gomega.BeEmpty())
		})
	}
}

// Each case violates exactly one rule, so the assertion pins which rule fired
// rather than merely that something did.
func TestOVNChassisValidateCreate_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*OVNChassis)
		wantMsg string
	}{
		{
			name: "centralRef has no name",
			mutate: func(o *OVNChassis) {
				o.Spec.CentralRef.Name = ""
			},
			wantMsg: "centralRef.name must be set",
		},
		{
			name: "nodeSelector is empty",
			mutate: func(o *OVNChassis) {
				o.Spec.NodeSelector = nil
			},
			wantMsg: "nodeSelector must carry at least one label",
		},
		{
			name: "gateway is set with an empty nodeSelector",
			mutate: func(o *OVNChassis) {
				o.Spec.Gateway = &OVNGatewaySpec{}
			},
			wantMsg: "gateway.nodeSelector must carry at least one label",
		},
		{
			name: "two physical networks map to the same bridge",
			mutate: func(o *OVNChassis) {
				o.Spec.BridgeMappings = []OVNBridgeMapping{
					{PhysicalNetwork: "physnet1", Bridge: "br-ex"},
					{PhysicalNetwork: "physnet2", Bridge: "br-ex"},
				}
			},
			wantMsg: "spec.bridgeMappings[1].bridge",
		},
		{
			name: "one physical network is mapped twice",
			mutate: func(o *OVNChassis) {
				o.Spec.BridgeMappings = []OVNBridgeMapping{
					{PhysicalNetwork: "physnet1", Bridge: "br-ex"},
					{PhysicalNetwork: "physnet1", Bridge: "br-ex2"},
				}
			},
			wantMsg: "spec.bridgeMappings[1].physicalNetwork",
		},
		{
			name: "maxUnavailable paired with OnDelete",
			mutate: func(o *OVNChassis) {
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{
					Type:           "OnDelete",
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				}
			},
			wantMsg: "maxUnavailable applies to RollingUpdate only",
		},
		{
			name: "maxUnavailable of zero stalls the rollout",
			mutate: func(o *OVNChassis) {
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{
					Type:           "RollingUpdate",
					MaxUnavailable: ptr.To(intstr.FromInt32(0)),
				}
			},
			wantMsg: "maxUnavailable must resolve to at least 1 for RollingUpdate",
		},
		{
			// The percentage form walks past an integer-only check, and the
			// DaemonSet the operator would render with it is rejected by the API
			// server on every pass with no field left to report it on.
			name: "maxUnavailable of zero percent stalls the rollout",
			mutate: func(o *OVNChassis) {
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{
					Type:           "RollingUpdate",
					MaxUnavailable: ptr.To(intstr.FromString("0%")),
				}
			},
			wantMsg: "maxUnavailable must resolve to at least 1 for RollingUpdate",
		},
		{
			name: "maxUnavailable is negative",
			mutate: func(o *OVNChassis) {
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{
					Type:           "RollingUpdate",
					MaxUnavailable: ptr.To(intstr.FromInt32(-1)),
				}
			},
			wantMsg: "maxUnavailable must resolve to at least 1 for RollingUpdate",
		},
		{
			name: "maxUnavailable is a malformed percentage",
			mutate: func(o *OVNChassis) {
				o.Spec.UpdateStrategy = OVNChassisUpdateStrategy{
					Type:           "RollingUpdate",
					MaxUnavailable: ptr.To(intstr.FromString("10 %")),
				}
			},
			wantMsg: `maxUnavailable must be an integer or a percentage such as "25%"`,
		},
		{
			name: "image pins both a tag and a digest",
			mutate: func(o *OVNChassis) {
				o.Spec.Image = &commonv1.ImageSpec{
					Repository: "ghcr.io/c5c3/ovn",
					Tag:        "25.03",
					Digest:     strings.Repeat("a", 64),
				}
			},
			wantMsg: "exactly one of image.tag or image.digest must be set",
		},
		{
			name: "targetClusterRef has an empty name",
			mutate: func(o *OVNChassis) {
				o.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{}
			},
			wantMsg: "target cluster name must be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNChassisWebhook{}

			obj := validOVNChassis()
			tt.mutate(obj)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.wantMsg)))
		})
	}
}

// Repointing a live chassis leaves its registration behind in the old Southbound
// database, where it keeps claiming the ports of workloads that have moved on.
// The CEL transition rule on OVNChassisSpec says the same thing at the schema
// layer; this is the webhook half.
func TestOVNChassisValidateUpdate_RejectsCentralRefChange(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNChassisWebhook{}

	oldObj := validOVNChassis()
	newObj := validOVNChassis()
	newObj.Spec.CentralRef.Name = "other-ovn"

	_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("centralRef is immutable")))
}

// The DaemonSets already exist on the cluster the ref named at creation, so
// every transition away from it strands them.
func TestOVNChassisValidateUpdate_RejectsTargetClusterRefTransitions(t *testing.T) {
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
			w := &OVNChassisWebhook{}

			oldObj := validOVNChassis()
			oldObj.Spec.TargetClusterRef = tt.old
			newObj := validOVNChassis()
			newObj.Spec.TargetClusterRef = tt.new

			_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("targetClusterRef is immutable")))
		})
	}
}

// metadata.name is bounded by the child object with the tightest name budget,
// the per-node chassis-deletion Job. Nothing else in the CRD or the webhook
// bounds it, so without this rule a name whose deletion Job the API server would
// refuse admits cleanly, and the stale chassis registration it was meant to
// remove stays in the Southbound database.
func TestOVNChassisValidateCreate_NameLengthBoundedByChassisDelJob(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNChassisWebhook{}

	atLimit := validOVNChassis()
	atLimit.Name = strings.Repeat("c", MaxOVNChassisNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still leaves the Jobs their 21 characters must be accepted")

	tooLong := validOVNChassis()
	tooLong.Name = strings.Repeat("c", MaxOVNChassisNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("at most 42 characters")))
}

// The bound is create-only, for the same reason it is on OVNCentral: on update
// it could only fire against a CR a pre-upgrade operator already admitted, and
// the finalizer-removal update reconcileDelete issues would then wedge it in
// Terminating.
func TestOVNChassisValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNChassisWebhook{}

	grandfathered := validOVNChassis()
	grandfathered.Name = strings.Repeat("c", MaxOVNChassisNameLength+1)
	grandfathered.Finalizers = []string{"ovn.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// The mutating webhook is registered but resolves nothing, so a CR that round
// trips through it has to come back byte-identical. A default materialized here
// by accident would freeze today's value into stored state and stop tracking the
// operator default across upgrades.
func TestOVNChassisDefault_LeavesTheObjectUnchanged(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &OVNChassisWebhook{}

	obj := validOVNChassis()
	obj.Spec.UpdateStrategy = OVNChassisUpdateStrategy{}
	before := obj.DeepCopy()

	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj).To(gomega.Equal(before))
}
