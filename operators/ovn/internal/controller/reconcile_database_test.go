// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// kindedApplyConfiguration is what an interceptor sees of an object the shared
// apply helper writes: the apply configuration is built from an unstructured
// object, whose kind is what tells the four objects of a database step apart.
type kindedApplyConfiguration interface {
	GetKind() string
}

// centralKey builds the key of a child of the shared fixture.
func centralKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: testNamespace, Name: name}
}

func TestReconcileRaftDatabase_CreatesServicesAndStatefulSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishOnNodePorts(testOVNCentral())
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileNorthbound(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait), "a cluster with no ready member is polled")

	var headless corev1.Service
	g.Expect(r.Get(ctx, centralKey("ovn-nb"), &headless)).To(Succeed())
	g.Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
	g.Expect(headless.Spec.PublishNotReadyAddresses).To(BeTrue(),
		"a member joins the cluster before it is ready and has to resolve its peers to do it")

	// One Service per member, on consecutive node ports from the Northbound
	// default base: a Raft client addresses the members individually.
	for ordinal := int32(0); ordinal < 3; ordinal++ {
		var member corev1.Service
		name := fmt.Sprintf("ovn-nb-%d", ordinal)
		g.Expect(r.Get(ctx, centralKey(name), &member)).To(Succeed())
		g.Expect(member.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
		g.Expect(member.Spec.Ports).To(HaveLen(1))
		g.Expect(member.Spec.Ports[0].NodePort).To(Equal(ovnv1alpha1.DefaultNorthboundNodePortBase + ordinal))
		g.Expect(member.Spec.Selector).To(HaveKeyWithValue(appsv1.StatefulSetPodNameLabel, name))
	}

	var sts appsv1.StatefulSet
	g.Expect(r.Get(ctx, centralKey("ovn-nb"), &sts)).To(Succeed())
	g.Expect(*sts.Spec.Replicas).To(BeEquivalentTo(3))
	g.Expect(sts.Spec.ServiceName).To(Equal("ovn-nb"))

	g.Expect(r.Get(ctx, centralKey("ovn-central-scripts"), &corev1.ConfigMap{})).To(Succeed())
}

// While the members are still coming up the step reports the count it is
// waiting on and polls, rather than failing: a Raft cluster takes an election to
// form, and the first pass always observes zero ready members.
func TestReconcileRaftDatabase_ProgressingUntilReadyReplicasMatch(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	res, err := r.reconcileNorthbound(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueRaftWait))

	cond := ovnCentralCondition(cr, conditionTypeNorthboundReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonStatefulSetProgressing))
	g.Expect(cond.Message).To(Equal("0 of 3 nb Raft members are ready"))
	g.Expect(cr.Status.Northbound.ReadyReplicas).To(BeEquivalentTo(0))
}

func TestReconcileRaftDatabase_ReadyMirrorsReadyReplicas(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	_, err := r.reconcileNorthbound(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.MarkStatefulSetReady(ctx, r.Client, centralKey("ovn-nb"))).To(Succeed())

	res, err := r.reconcileNorthbound(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a converged cluster is not polled")

	cond := ovnCentralCondition(cr, conditionTypeNorthboundReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonStatefulSetReady))
	g.Expect(cr.Status.Northbound.ReadyReplicas).To(BeEquivalentTo(3))
	g.Expect(cr.Status.Southbound.ReadyReplicas).To(BeEquivalentTo(0),
		"the Northbound step must not report on the other database")
}

// A target cluster that grants the operator no statefulsets verb fails here.
// The condition has to flip to False on that pass: the aggregate Ready is
// re-derived from the sub-conditions at the new observedGeneration, so a
// condition left untouched would report the failed pass as ready.
func TestReconcileRaftDatabase_ApplyErrorIsStatefulSetError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()

	c := ovnCentralFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "StatefulSet" {
					return apierrors.NewForbidden(appsv1.Resource("statefulsets"), "ovn-nb", nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNCentralReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileNorthbound(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring nb StatefulSet")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnCentralCondition(cr, conditionTypeNorthboundReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonStatefulSetError))

	// The Services of the step went in before the failure and stay: the next
	// pass re-applies them, and deleting them would take the addresses of the
	// members that are running away with it.
	g.Expect(r.Get(ctx, centralKey("ovn-nb-0"), &corev1.Service{})).To(Succeed())
}

func TestReconcileRaftDatabase_ScriptsConfigMapCarriesTheFiveKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNCentral()
	r := newTestOVNCentralReconciler(t, cr)

	// Both database steps apply the same ConfigMap; the second must not drop the
	// first one's scripts.
	_, err := r.reconcileNorthbound(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.reconcileSouthbound(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var scripts corev1.ConfigMap
	g.Expect(r.Get(ctx, centralKey("ovn-central-scripts"), &scripts)).To(Succeed())
	g.Expect(scripts.Data).To(HaveLen(5))
	g.Expect(scripts.Data).To(HaveKey("run-nb.sh"))
	g.Expect(scripts.Data).To(HaveKey("run-sb.sh"))
	g.Expect(scripts.Data).To(HaveKey("set-connection-nb.sh"))
	g.Expect(scripts.Data).To(HaveKey("set-connection-sb.sh"))
	// The backup CronJob mounts the same ConfigMap, so a database step that
	// dropped this key would leave every backup run without its script.
	g.Expect(scripts.Data).To(HaveKey(backupScriptKey))

	// Member 0 creates the cluster and every other member joins through it, so
	// the remote address is the one thing the run script must not carry
	// unconditionally.
	g.Expect(scripts.Data["run-nb.sh"]).To(ContainSubstring(`if [ "$ORD" != 0 ]; then`))
	g.Expect(scripts.Data["run-nb.sh"]).To(ContainSubstring(
		"--db-nb-cluster-remote-addr=ovn-nb-0.ovn-nb.${POD_NAMESPACE}.svc.cluster.local"))
}

func TestReconcileRaftDatabase_SouthboundUsesItsOwnPortsAndBase(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := publishOnNodePorts(testOVNCentral())
	r := newTestOVNCentralReconciler(t, cr)

	g.Expect(southboundDB(cr).schema).To(Equal("OVN_Southbound"))
	g.Expect(northboundDB(cr).schema).To(Equal("OVN_Northbound"))

	_, err := r.reconcileSouthbound(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())

	var headless corev1.Service
	g.Expect(r.Get(ctx, centralKey("ovn-sb"), &headless)).To(Succeed())
	g.Expect(headless.Spec.Ports[0].Port).To(BeEquivalentTo(6642))
	g.Expect(headless.Spec.Ports[1].Port).To(BeEquivalentTo(6644))

	for ordinal := int32(0); ordinal < 3; ordinal++ {
		var member corev1.Service
		g.Expect(r.Get(ctx, centralKey(fmt.Sprintf("ovn-sb-%d", ordinal)), &member)).To(Succeed())
		g.Expect(member.Spec.Ports[0].NodePort).To(Equal(ovnv1alpha1.DefaultSouthboundNodePortBase + ordinal))
	}

	cond := ovnCentralCondition(cr, conditionTypeSouthboundReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(ContainSubstring("sb Raft members"))
	g.Expect(ovnCentralCondition(cr, conditionTypeNorthboundReady)).To(BeNil(),
		"the Southbound step must set only its own condition")
}

// A spec that carries no storage size at all is only reachable when the CRD
// default was bypassed. The claim still has to name a size: an empty one is not
// a quantity, and the builder cannot report an error from where it runs.
func TestRaftDataClaim_EmptyStorageSizeFallsBackToTheCRDDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()
	cr.Spec.Northbound.Storage = ovnv1alpha1.OVNStorageSpec{}

	claim := raftDataClaim(northboundDB(cr))

	g.Expect(claim.Spec.Resources.Requests.Storage().String()).To(Equal(defaultStorageSize))
	g.Expect(claim.Spec.StorageClassName).To(BeNil(),
		"a CR that names no class takes the cluster default rather than an empty class name")
}

// The databases hold the whole logical network model, so nothing is published
// on the node network unless the CR asks for it. Without this default an
// OVNCentral with no opinion on the matter listens on 30641-30643 and
// 30651-30653 on every node IP of the cluster, which on any cluster whose nodes
// are reachable from a wider network puts the control plane on that network.
func TestRaftPerPodService_ClusterIPUnlessExternallyReachable(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := testOVNCentral()

	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		svc := raftPerPodService(cr, db, 0)
		g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP), db.suffix)
		g.Expect(svc.Spec.Ports).To(HaveLen(1), db.suffix)
		g.Expect(svc.Spec.Ports[0].NodePort).To(BeZero(), db.suffix)
	}

	// Publishing one database leaves the other cluster-internal: the case the
	// node port exists for is a chassis dialling the Southbound database.
	cr.Spec.Southbound.ExternallyReachable = true
	g.Expect(raftPerPodService(cr, northboundDB(cr), 0).Spec.Type).
		To(Equal(corev1.ServiceTypeClusterIP))
	sb := raftPerPodService(cr, southboundDB(cr), 1)
	g.Expect(sb.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
	g.Expect(sb.Spec.Ports[0].NodePort).To(Equal(ovnv1alpha1.DefaultSouthboundNodePortBase + 1))
}

// ovn-ctl's run_*_ovsdb keeps ovsdb-server as a background child and blocks in
// wait without installing a signal handler, so the container's PID 1 is a shell
// with no SIGTERM trap. PID 1 gets no default signal action from the kernel, so
// without a pre-stop hook the kubelet's TERM is ignored and every member is
// SIGKILLed when its grace expires — mid-snapshot, which is exactly what the
// 300s grace is there to prevent. The hook is what makes the container stop.
func TestRaftStatefulSet_PreStopAsksTheDatabaseToExit(t *testing.T) {
	g := NewWithT(t)
	cr := testOVNCentral()

	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		sts := raftStatefulSet(cr, db)
		c := sts.Spec.Template.Spec.Containers[0]
		g.Expect(c.Name).To(Equal("ovsdb"))
		g.Expect(c.Lifecycle).NotTo(BeNil(), db.suffix)
		g.Expect(c.Lifecycle.PreStop).NotTo(BeNil(),
			"without the hook the pod is SIGKILLed after the full grace period")
		g.Expect(c.Lifecycle.PreStop.Exec.Command).To(Equal([]string{
			"ovs-appctl", "-t", "/var/run/ovn/ovn" + db.suffix + "_db.ctl", "exit",
		}), db.suffix)
		g.Expect(sts.Spec.Template.Spec.TerminationGracePeriodSeconds).
			To(HaveValue(BeNumerically(">", 0)), db.suffix)
	}
}

// A block that asks for nothing gets the shared request floor and no limit, so
// each member runs Burstable instead of BestEffort. Empty maps count as unset,
// the same way DeploymentSpec.Default reads them.
func TestRaftResources_UnsetBlockGetsTheRequestFloor(t *testing.T) {
	cases := []struct {
		name string
		spec *corev1.ResourceRequirements
	}{
		{name: "nil", spec: nil},
		{name: "empty block", spec: &corev1.ResourceRequirements{}},
		{name: "empty maps", spec: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{},
			Limits:   corev1.ResourceList{},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got := commonv1.WithRequestFloor(tc.spec)

			g.Expect(got.Requests).To(HaveLen(2))
			g.Expect(got.Requests.Cpu().String()).To(Equal("100m"))
			g.Expect(got.Requests.Memory().String()).To(Equal("256Mi"))
			g.Expect(got.Limits).To(BeEmpty(), "the floor sets no limit")
		})
	}
}

// Each of CPU and memory is filled on its own. A limit alone must stay alone:
// the API server defaults an unset request to its limit, and a floor request
// added beside it would silently lower the request.
func TestRaftResources_FillsEachUnnamedResource(t *testing.T) {
	q := resource.MustParse
	cases := []struct {
		name string
		spec *corev1.ResourceRequirements
		want corev1.ResourceRequirements
	}{
		{
			name: "CPU request only gains the memory request",
			spec: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("1")}},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("256Mi")},
			},
		},
		{
			name: "memory limit only gains the CPU request",
			spec: &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q("1Gi")},
			},
		},
		{
			name: "memory request only gains the CPU request",
			spec: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: q("512Mi")}},
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q("512Mi"), corev1.ResourceCPU: q("100m")},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			g.Expect(commonv1.WithRequestFloor(tc.spec)).To(Equal(tc.want))
		})
	}
}

// A block that names both CPU and memory, as request or as limit, gets nothing
// added.
func TestRaftResources_BlockNamingCPUAndMemoryIsUsedAsWritten(t *testing.T) {
	q := resource.MustParse
	cases := []struct {
		name string
		spec *corev1.ResourceRequirements
	}{
		{name: "requests", spec: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("1Gi")},
		}},
		{name: "CPU request and memory limit", spec: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: q("2Gi")},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			g.Expect(commonv1.WithRequestFloor(tc.spec)).To(Equal(*tc.spec))
		})
	}
}

// The QoS class is computed from CPU and memory alone, so a block that names
// only other resources would still run the member BestEffort. It gets the floor
// beside what it sets, and the CR's block stays untouched.
func TestRaftResources_BlockWithoutCPUOrMemoryGetsTheFloorBesideIt(t *testing.T) {
	cases := []struct {
		name string
		spec *corev1.ResourceRequirements
	}{
		{name: "ephemeral-storage limit only", spec: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
		}},
		{name: "hugepages request and limit", spec: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"hugepages-2Mi": resource.MustParse("1Gi")},
			Limits:   corev1.ResourceList{"hugepages-2Mi": resource.MustParse("1Gi")},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			want := tc.spec.DeepCopy()

			got := commonv1.WithRequestFloor(tc.spec)

			g.Expect(got.Requests.Cpu().String()).To(Equal("100m"))
			g.Expect(got.Requests.Memory().String()).To(Equal("256Mi"))
			for name, q := range want.Requests {
				g.Expect(got.Requests).To(HaveKeyWithValue(name, q), "the block's own requests are kept")
			}
			g.Expect(got.Limits).To(Equal(want.Limits), "the floor adds no limit")
			g.Expect(tc.spec).To(Equal(want), "the CR's block must stay untouched")
		})
	}
}

// Claims name a DRA claim and neither request nor limit a resource, so a block
// that carries only claims still gets the floor, and keeps its claims.
func TestRaftResources_ClaimsAloneStillGetTheFloor(t *testing.T) {
	g := NewWithT(t)
	claims := []corev1.ResourceClaim{{Name: "gpu"}}

	got := commonv1.WithRequestFloor(&corev1.ResourceRequirements{Claims: claims})

	g.Expect(got.Requests.Cpu().String()).To(Equal("100m"))
	g.Expect(got.Requests.Memory().String()).To(Equal("256Mi"))
	g.Expect(got.Limits).To(BeEmpty())
	g.Expect(got.Claims).To(Equal(claims))
}

// The rendered block is the operator's to change: writing to it must reach
// neither the CR it was read from nor the floor the next member gets.
func TestRaftResources_ReturnsACopy(t *testing.T) {
	g := NewWithT(t)

	spec := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	got := commonv1.WithRequestFloor(spec)
	got.Requests[corev1.ResourceMemory] = resource.MustParse("2Gi")
	g.Expect(spec.Requests.Memory().String()).To(Equal("512Mi"), "the CR's block must stay untouched")

	floor := commonv1.WithRequestFloor(nil)
	floor.Requests[corev1.ResourceCPU] = resource.MustParse("4")
	floor.Requests[corev1.ResourceMemory] = resource.MustParse("8Gi")
	next := commonv1.WithRequestFloor(nil)
	g.Expect(next.Requests.Cpu().String()).To(Equal("100m"))
	g.Expect(next.Requests.Memory().String()).To(Equal("256Mi"))
}

// The shared fixture leaves resources unset on both databases, so both render
// their members with the floor, and the floor is resolved without writing it
// back into the CR.
func TestRaftStatefulSet_MembersRequestTheFloorWhenTheCRSetsNone(t *testing.T) {
	g := NewWithT(t)
	cr := testOVNCentral()

	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		c := raftStatefulSet(cr, db).Spec.Template.Spec.Containers[0]
		g.Expect(c.Name).To(Equal("ovsdb"))
		g.Expect(c.Resources.Requests.Cpu().String()).To(Equal("100m"), db.suffix)
		g.Expect(c.Resources.Requests.Memory().String()).To(Equal("256Mi"), db.suffix)
		g.Expect(c.Resources.Limits).To(BeEmpty(), db.suffix)
	}

	g.Expect(cr.Spec.Northbound.Resources).To(BeNil(), "the floor must not be written into the CR")
	g.Expect(cr.Spec.Southbound.Resources).To(BeNil(), "the floor must not be written into the CR")
}

// Each database block's placement and priority class reach its own Raft
// StatefulSet only.
func TestRaftStatefulSet_RendersPlacementAndPriority(t *testing.T) {
	g := NewWithT(t)
	cr := testOVNCentral()
	cr.Spec.Northbound.PriorityClassName = ptr.To("ovn-critical")
	cr.Spec.Northbound.NodePlacementSpec = commonv1.NodePlacementSpec{
		NodeSelector: map[string]string{"ovn-central": "true"},
		Tolerations:  []corev1.Toleration{{Key: "ovn", Operator: corev1.TolerationOpExists}},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{},
		}},
	}

	nb := raftStatefulSet(cr, northboundDB(cr)).Spec.Template.Spec
	g.Expect(nb.PriorityClassName).To(Equal("ovn-critical"))
	g.Expect(nb.NodeSelector).To(Equal(cr.Spec.Northbound.NodeSelector))
	g.Expect(nb.Tolerations).To(Equal(cr.Spec.Northbound.Tolerations))
	g.Expect(nb.Affinity).To(Equal(cr.Spec.Northbound.Affinity))

	sb := raftStatefulSet(cr, southboundDB(cr)).Spec.Template.Spec
	g.Expect(sb.PriorityClassName).To(BeEmpty())
	g.Expect(sb.NodeSelector).To(BeNil())
}

// Unset fields render nothing, and an empty priority class renders none, so a
// StatefulSet whose CR sets neither does not roll.
func TestRaftStatefulSet_RendersNoneWhenUnset(t *testing.T) {
	g := NewWithT(t)
	cr := testOVNCentral()
	cr.Spec.Southbound.PriorityClassName = ptr.To("")

	for _, db := range []raftDB{northboundDB(cr), southboundDB(cr)} {
		spec := raftStatefulSet(cr, db).Spec.Template.Spec
		g.Expect(spec.PriorityClassName).To(BeEmpty(), db.suffix)
		g.Expect(spec.NodeSelector).To(BeNil(), db.suffix)
		g.Expect(spec.Tolerations).To(BeNil(), db.suffix)
		g.Expect(spec.Affinity).To(BeNil(), db.suffix)
	}
}
