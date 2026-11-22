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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
				o.Spec.OVS = &OVNChassisOVSSpec{OVNChassisContainerSpec: OVNChassisContainerSpec{Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				}}}
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
		{
			name: "one revalidator thread",
			mutate: func(o *OVNChassis) {
				o.Spec.OVS = &OVNChassisOVSSpec{RevalidatorThreads: ptr.To(int32(1))}
			},
		},
		{
			name: "ovs block without a revalidator count",
			mutate: func(o *OVNChassis) {
				o.Spec.OVS = &OVNChassisOVSSpec{RevalidatorThreads: nil}
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
		{
			name: "revalidatorThreads is zero",
			mutate: func(o *OVNChassis) {
				o.Spec.OVS = &OVNChassisOVSSpec{RevalidatorThreads: ptr.To(int32(0))}
			},
			wantMsg: "spec.ovs.revalidatorThreads: Invalid value: 0: revalidatorThreads must be at least 1",
		},
		{
			name: "verticalAutoscaling foreign resource rejected",
			mutate: func(o *OVNChassis) {
				o.Spec.VerticalAutoscaling = &VerticalAutoscalingSpec{
					UpdateMode: "Off",
					MinAllowed: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
				}
			},
			wantMsg: `spec.verticalAutoscaling.minAllowed[ephemeral-storage]: Unsupported value: "ephemeral-storage"`,
		},
		{
			name: "verticalAutoscaling minAllowed above maxAllowed rejected",
			mutate: func(o *OVNChassis) {
				o.Spec.VerticalAutoscaling = &VerticalAutoscalingSpec{
					UpdateMode: "Off",
					MinAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					MaxAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}
			},
			wantMsg: `spec.verticalAutoscaling.minAllowed[cpu]: Invalid value: "2": must not exceed maxAllowed`,
		},
		{
			name: "verticalAutoscaling updateMode outside the enum rejected",
			mutate: func(o *OVNChassis) {
				o.Spec.VerticalAutoscaling = &VerticalAutoscalingSpec{UpdateMode: "Sometimes"}
			},
			wantMsg: `spec.verticalAutoscaling.updateMode: Unsupported value: "Sometimes"`,
		},
		{
			name: "verticalAutoscaling minReplicas zero rejected",
			mutate: func(o *OVNChassis) {
				o.Spec.VerticalAutoscaling = &VerticalAutoscalingSpec{UpdateMode: "Auto", MinReplicas: ptr.To(int32(0))}
			},
			wantMsg: "spec.verticalAutoscaling.minReplicas: Invalid value: 0: must be at least 1",
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

// The finalizer removal reconcileDelete issues is an update, and the validating
// webhook sees it. A spec.jobs.priorityClassName admitted earlier can name a
// PriorityClass deleted since, and rejecting the removal would hold the CR in
// Terminating. A deleting CR whose spec changes is still validated.
func TestOVNChassisValidateUpdate_FinalizerRemovalOnADeletingCRSkipsValidation(t *testing.T) {
	g := gomega.NewWithT(t)
	ctx := context.Background()
	w := &OVNChassisWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}

	stale := validOVNChassis()
	stale.Spec.Jobs = &commonv1.JobBaseSpec{PriorityClassName: ptr.To("deleted-class")}
	stale.Finalizers = []string{"openstack.c5c3.io/remote-children"}
	stale.DeletionTimestamp = ptr.To(metav1.Now())

	released := stale.DeepCopy()
	released.Finalizers = nil
	_, err := w.ValidateUpdate(ctx, stale, released)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"the finalizer removal must pass however the unchanged spec fares against today's rules")

	edited := released.DeepCopy()
	edited.Spec.NodeSelector = map[string]string{"node-role.kubernetes.io/compute": "true"}
	_, err = w.ValidateUpdate(ctx, stale, edited)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.jobs.priorityClassName: Not found")),
		"a spec edit on a deleting CR is validated like any other")
}

// spec.jobs carries no placement, so the webhook checks what it does carry: an
// existing priority class and requests within limits.
func TestOVNChassisValidate_Jobs(t *testing.T) {
	w := &OVNChassisWebhook{Client: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()}
	for _, tc := range []struct {
		name string
		jobs *commonv1.JobBaseSpec
		want string
	}{
		{name: "empty block accepted", jobs: &commonv1.JobBaseSpec{}},
		{
			name: "unknown priority class",
			jobs: &commonv1.JobBaseSpec{PriorityClassName: ptr.To("typo")},
			want: "spec.jobs.priorityClassName: Not found",
		},
		{
			name: "cpu request above limit",
			jobs: &commonv1.JobBaseSpec{Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}},
			want: "spec.jobs.resources.requests.cpu: Invalid value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			o := validOVNChassis()
			o.Spec.Jobs = tc.jobs

			_, err := w.ValidateCreate(context.Background(), o)
			if tc.want == "" {
				g.Expect(err).NotTo(gomega.HaveOccurred())
				return
			}
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.want))
		})
	}
}

// validFabric returns a spec.fabric block that passes every rule, in the shape
// the CRD schema would have defaulted it: two uplinks, one node row, one
// announceable range per family and one VRF id on each side. The webhook tests
// and the CRD-only envtest mutate one field of it per case.
func validFabric() *OVNFabricSpec {
	return &OVNFabricSpec{
		VRF: OVNFabricVRFSpec{Name: "fabric", Table: 100},
		Uplinks: []OVNFabricUplink{
			{Interface: "fab1", Bridge: "br-fab1"},
			{Interface: "fab2", Bridge: "br-fab2"},
		},
		MTU: 9000,
		Nodes: []OVNFabricNode{{
			Name: "node-1", ASN: 4200000001, LoopbackIPv4: "192.0.2.1", LoopbackIPv6: "2001:db8::1",
		}},
		AnnounceableRanges: []string{"198.51.100.0/24", "2001:db8::/64"},
		AdvertiseVRFIDs:    []int64{42},
		LearnVRFIDs:        []int64{10},
		BFD:                OVNFabricBFDSpec{ReceiveIntervalMs: 300, TransmitIntervalMs: 300, DetectMultiplier: 3},
	}
}

// secondFabricNode is a node row that collides with nothing in validFabric.
func secondFabricNode() OVNFabricNode {
	return OVNFabricNode{Name: "node-2", ASN: 4200000002, LoopbackIPv4: "192.0.2.2", LoopbackIPv6: "2001:db8::2"}
}

// Each case breaks exactly one rule of validateFabric, so the assertion pins
// the field path and the message of the rule that fired rather than merely that
// something did.
func TestOVNChassisValidateCreate_FabricRejections(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*OVNFabricSpec)
		wantPath string
		wantMsg  string
	}{
		{
			name: "a bridge named twice",
			mutate: func(f *OVNFabricSpec) {
				f.Uplinks[1].Bridge = "br-fab1"
			},
			wantPath: "spec.fabric.uplinks[1].bridge",
			wantMsg:  `Duplicate value: "br-fab1"`,
		},
		{
			name: "a bridge that is also an uplink interface",
			mutate: func(f *OVNFabricSpec) {
				f.Uplinks[1].Bridge = "fab1"
			},
			wantPath: "spec.fabric.uplinks[1].bridge",
			wantMsg:  "a peer bridge cannot carry the name of an uplink interface",
		},
		{
			name: "a bridge named br-int",
			mutate: func(f *OVNFabricSpec) {
				f.Uplinks[0].Bridge = "br-int"
			},
			wantPath: "spec.fabric.uplinks[0].bridge",
			wantMsg:  "br-int is the integration bridge of ovn-controller",
		},
		{
			name: "a bridge named as the VRF",
			mutate: func(f *OVNFabricSpec) {
				f.Uplinks[0].Bridge = "fabric"
			},
			wantPath: "spec.fabric.uplinks[0].bridge",
			wantMsg:  "the name is taken by the fabric VRF device",
		},
		{
			name: "an interface named as the VRF",
			mutate: func(f *OVNFabricSpec) {
				f.Uplinks[1].Interface = "fabric"
			},
			wantPath: "spec.fabric.uplinks[1].interface",
			wantMsg:  "the name is taken by the fabric VRF device",
		},
		{
			name: "a VRF name with the ovnvrf prefix",
			mutate: func(f *OVNFabricSpec) {
				f.VRF.Name = "ovnvrf7"
			},
			wantPath: "spec.fabric.vrf.name",
			wantMsg:  "the prefix ovnvrf is reserved for the VRFs ovn-controller maintains",
		},
		{
			name: "the kernel's main table",
			mutate: func(f *OVNFabricSpec) {
				f.VRF.Table = 254
			},
			wantPath: "spec.fabric.vrf.table",
			wantMsg:  "tables 253, 254 and 255 are the kernel's default, main and local tables",
		},
		{
			name: "the kernel's default table",
			mutate: func(f *OVNFabricSpec) {
				f.VRF.Table = 253
			},
			wantPath: "spec.fabric.vrf.table",
			wantMsg:  "tables 253, 254 and 255 are the kernel's default, main and local tables",
		},
		{
			name: "the kernel's local table",
			mutate: func(f *OVNFabricSpec) {
				f.VRF.Table = 255
			},
			wantPath: "spec.fabric.vrf.table",
			wantMsg:  "tables 253, 254 and 255 are the kernel's default, main and local tables",
		},
		{
			name: "a table that is an advertised VRF id",
			mutate: func(f *OVNFabricSpec) {
				f.VRF.Table = 42
			},
			wantPath: "spec.fabric.vrf.table",
			wantMsg:  "ovn-controller uses a VRF id as the table of ovnvrf<id>",
		},
		{
			name: "a table that is a learned VRF id",
			mutate: func(f *OVNFabricSpec) {
				f.VRF.Table = 10
			},
			wantPath: "spec.fabric.vrf.table",
			wantMsg:  "ovn-controller uses a VRF id as the table of ovnvrf<id>",
		},
		{
			name: "an id in both lists",
			mutate: func(f *OVNFabricSpec) {
				f.LearnVRFIDs = []int64{10, 42}
			},
			wantPath: "spec.fabric.learnVRFIDs[1]",
			wantMsg:  "a VRF id is either advertised from or learned into, not both",
		},
		{
			name: "an ASN named twice",
			mutate: func(f *OVNFabricSpec) {
				node := secondFabricNode()
				node.ASN = f.Nodes[0].ASN
				f.Nodes = append(f.Nodes, node)
			},
			wantPath: "spec.fabric.nodes[1].asn",
			wantMsg:  "Duplicate value: 4200000001",
		},
		{
			name: "an IPv4 loopback named twice",
			mutate: func(f *OVNFabricSpec) {
				node := secondFabricNode()
				node.LoopbackIPv4 = f.Nodes[0].LoopbackIPv4
				f.Nodes = append(f.Nodes, node)
			},
			wantPath: "spec.fabric.nodes[1].loopbackIPv4",
			wantMsg:  `Duplicate value: "192.0.2.1"`,
		},
		{
			// The second spelling is the same address, so the comparison is on
			// the parsed value rather than on the string.
			name: "an IPv6 loopback named twice in another spelling",
			mutate: func(f *OVNFabricSpec) {
				node := secondFabricNode()
				node.LoopbackIPv6 = "2001:DB8:0::1"
				f.Nodes = append(f.Nodes, node)
			},
			wantPath: "spec.fabric.nodes[1].loopbackIPv6",
			wantMsg:  `Duplicate value: "2001:DB8:0::1"`,
		},
		{
			name: "a range with host bits set",
			mutate: func(f *OVNFabricSpec) {
				f.AnnounceableRanges = []string{"198.51.100.7/24", "2001:db8::/64"}
			},
			wantPath: "spec.fabric.announceableRanges[0]",
			wantMsg:  "must be a network address; did you mean 198.51.100.0/24",
		},
		{
			name: "advertised ids without an announceable range",
			mutate: func(f *OVNFabricSpec) {
				f.AnnounceableRanges = nil
			},
			wantPath: "spec.fabric.announceableRanges",
			wantMsg:  "advertiseVRFIDs needs at least one announceable range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNChassisWebhook{}

			obj := validOVNChassis()
			obj.Spec.Fabric = validFabric()
			tt.mutate(obj.Spec.Fabric)

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tt.wantPath + ": "))
			g.Expect(err.Error()).To(gomega.ContainSubstring(tt.wantMsg))
		})
	}
}

// The accepted shapes: the full fixture, a block without the block itself, a
// block that advertises nothing and so needs no range, and values the schema
// answers for (an unparsable loopback or range), which the webhook leaves to it
// rather than reporting a second time.
func TestOVNChassisValidateCreate_FabricAccepted(t *testing.T) {
	tests := []struct {
		name   string
		fabric func() *OVNFabricSpec
	}{
		{name: "nil block", fabric: func() *OVNFabricSpec { return nil }},
		{name: "valid block", fabric: validFabric},
		{
			name: "two distinct node rows",
			fabric: func() *OVNFabricSpec {
				f := validFabric()
				f.Nodes = append(f.Nodes, secondFabricNode())
				return f
			},
		},
		{
			name: "no advertised id and no range",
			fabric: func() *OVNFabricSpec {
				f := validFabric()
				f.AdvertiseVRFIDs = nil
				f.AnnounceableRanges = nil
				return f
			},
		},
		{
			name: "unparsable values are left to the schema",
			fabric: func() *OVNFabricSpec {
				f := validFabric()
				second := secondFabricNode()
				second.LoopbackIPv4 = "not-an-address"
				f.Nodes = append(f.Nodes, second)
				f.Nodes[0].LoopbackIPv4 = "not-an-address"
				f.AnnounceableRanges = []string{"not-a-cidr"}
				return f
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &OVNChassisWebhook{}

			obj := validOVNChassis()
			obj.Spec.Fabric = tt.fabric()

			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(validateFabric(nil, obj.Spec.Fabric)).To(gomega.BeEmpty())
		})
	}
}
