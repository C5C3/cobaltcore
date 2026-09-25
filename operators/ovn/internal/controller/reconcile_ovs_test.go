// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// chassisOVSDaemonSet is the key of the Open vSwitch DaemonSet of the shared
// fixture.
var chassisOVSDaemonSet = chassisKey(testOVNChassisName + "-ovs")

// rollingOutDaemonSet builds one of the chassis DaemonSets as an earlier pass of
// this CR left it, carrying the status of a rollout that has reached one of the
// three nodes it selects. Both DaemonSet steps use it: a rollout in flight is
// the state their progressing arm exists for, and a DaemonSet the fake client
// has only just created carries no counters at all.
func rollingOutDaemonSet(t *testing.T, cr *ovnv1alpha1.OVNChassis, name string) *appsv1.DaemonSet {
	t.Helper()

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	if err := controllerutil.SetControllerReference(cr, ds, newTestScheme(t)); err != nil {
		t.Fatalf("setting the controller reference on DaemonSet %s: %v", name, err)
	}
	ds.Status = appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3,
		CurrentNumberScheduled: 3,
		UpdatedNumberScheduled: 1,
		NumberReady:            1,
	}
	return ds
}

// A rollout that has reached one of three nodes polls rather than failing, and
// names the two counters an operator watching the rollout wants.
func TestReconcileOVS_ProgressingUntilTheDaemonSetIsReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, rollingOutDaemonSet(t, cr, chassisOVSDaemonSet.Name))

	res, err := r.reconcileOVS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	var ds appsv1.DaemonSet
	g.Expect(r.Get(ctx, chassisOVSDaemonSet, &ds)).To(Succeed())
	g.Expect(ds.Spec.Template.Spec.Containers).To(HaveLen(2))

	cond := ovnChassisCondition(cr, conditionTypeOVSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(cond.Message).To(ContainSubstring("1 of 3 nodes"))
}

// Once every selected node runs a ready pod the step stops polling. It touches
// no status field of its own: the node counters and the installed image are the
// ovn-controller step's, which is the DaemonSet a chassis registration follows.
func TestReconcileOVS_ReadyWhenEveryNodeRunsAPod(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr, rollingOutDaemonSet(t, cr, chassisOVSDaemonSet.Name))

	_, err := r.reconcileOVS(ctx, r.Client, cr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, chassisOVSDaemonSet)).To(Succeed())

	res, err := r.reconcileOVS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a rolled-out DaemonSet is not polled")

	cond := ovnChassisCondition(cr, conditionTypeOVSReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetReady))
	g.Expect(cond.Message).To(ContainSubstring("on 3 nodes"))

	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(0))
	g.Expect(cr.Status.NumberReady).To(BeEquivalentTo(0))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty())
}

// A DaemonSet that selects no node is ready on zero nodes. Reporting the CR
// unready until somebody labels a node would make an empty node selection
// indistinguishable from a rollout that is stuck.
func TestReconcileOVS_EmptyNodeSelectionIsReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()
	r := newTestOVNChassisReconciler(t, cr)

	res, err := r.reconcileOVS(ctx, r.Client, cr)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeOVSReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetReady))
	g.Expect(cond.Message).To(ContainSubstring("on 0 nodes"))
}

// A target cluster that grants the operator no daemonsets verb fails here. The
// condition has to flip on that pass: the aggregate Ready is re-derived from the
// sub-conditions at the new observedGeneration, so a condition left untouched
// would report the failed pass as ready.
func TestReconcileOVS_ApplyErrorIsDaemonSetError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if kinded, ok := obj.(kindedApplyConfiguration); ok && kinded.GetKind() == "DaemonSet" {
					return apierrors.NewForbidden(appsv1.Resource("daemonsets"), chassisOVSDaemonSet.Name, nil)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileOVS(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring("ensuring ovs DaemonSet")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the API error must stay unwrappable")
	g.Expect(res.IsZero()).To(BeTrue())

	cond := ovnChassisCondition(cr, conditionTypeOVSReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetError))
}

// The step reads the DaemonSet back after the apply, both to judge the rollout
// and to quote the node counters in its condition. A failure of that read is
// reported like a failed apply, because a pass that cannot see the DaemonSet has
// nothing to say about it either way.
func TestReconcileOVS_ReadBackErrorIsDaemonSetError(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := testOVNChassis()

	c := ovnChassisFakeClientBuilder(t, cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.DaemonSet); ok {
					return apierrors.NewForbidden(appsv1.Resource("daemonsets"), key.Name, nil)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &OVNChassisReconciler{Client: c, Scheme: newTestScheme(t), Recorder: record.NewFakeRecorder(10)}

	_, err := r.reconcileOVS(ctx, r.Client, cr)

	g.Expect(err).To(MatchError(ContainSubstring(
		"ensuring ovs DaemonSet: getting DaemonSet " + chassisOVSDaemonSet.Namespace + "/" + chassisOVSDaemonSet.Name)))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
	g.Expect(ovnChassisCondition(cr, conditionTypeOVSReady).Reason).To(Equal(conditionReasonDaemonSetError))
}

// Both datapath daemons reach ovsdb-server over a socket the unprivileged user
// owns, and dropping ALL takes CAP_DAC_OVERRIDE with it, so uid 0 alone cannot
// open it. The shared group is what gets them in. A pin test would notice the
// field going missing only for whoever regenerates the golden; this states the
// requirement.
func TestBuildOVSDaemonSet_DatapathContainersJoinTheDatabaseGroup(t *testing.T) {
	g := NewWithT(t)
	ds := buildOVSDaemonSet(testOVNChassis())

	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == "ovsdb-server" {
			continue
		}
		g.Expect(c.SecurityContext.RunAsUser).To(HaveValue(BeEquivalentTo(0)), c.Name)
		g.Expect(c.SecurityContext.RunAsGroup).To(HaveValue(BeEquivalentTo(42424)), c.Name)
		g.Expect(c.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")), c.Name)
		g.Expect(c.SecurityContext.Capabilities.Add).NotTo(
			ContainElement(corev1.Capability("DAC_OVERRIDE")), c.Name)
	}
}

// ovsContainer returns the named container or init container of the OVS
// DaemonSet's pod template.
func ovsContainer(t *testing.T, ds *appsv1.DaemonSet, name string) corev1.Container {
	t.Helper()
	spec := ds.Spec.Template.Spec
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("container %q not rendered", name)
	return corev1.Container{}
}

// ovs-vswitchd receives the revalidator count it pins through its
// environment. An unset block or field renders the default of 2, so the count
// no longer follows the node's CPUs.
func TestBuildOVSDaemonSet_PinsTheRevalidatorThreads(t *testing.T) {
	resources := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	for _, tc := range []struct {
		name string
		ovs  *ovnv1alpha1.OVNChassisOVSSpec
		want string
	}{
		{name: "nil block", ovs: nil, want: "2"},
		{
			name: "block with only resources",
			ovs:  &ovnv1alpha1.OVNChassisOVSSpec{OVNChassisContainerSpec: ovnv1alpha1.OVNChassisContainerSpec{Resources: resources}},
			want: "2",
		},
		{name: "unset field", ovs: &ovnv1alpha1.OVNChassisOVSSpec{RevalidatorThreads: nil}, want: "2"},
		{name: "explicit count", ovs: &ovnv1alpha1.OVNChassisOVSSpec{RevalidatorThreads: ptr.To(int32(8))}, want: "8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			cr := testOVNChassis()
			cr.Spec.OVS = tc.ovs

			c := ovsContainer(t, buildOVSDaemonSet(cr), "ovs-vswitchd")

			g.Expect(c.Env).To(Equal([]corev1.EnvVar{{Name: "OVS_REVALIDATOR_THREADS", Value: tc.want}}))
			g.Expect(cr.Spec.OVS).To(Equal(tc.ovs), "the default must not be written into the CR")
		})
	}
}

// spec.ovs.resources reaches both containers it tunes, host-prepare and
// ovs-vswitchd, and an unset block renders none on either.
func TestBuildOVSDaemonSet_OVSResourcesReachBothContainers(t *testing.T) {
	g := NewWithT(t)
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}

	cr := testOVNChassis()
	cr.Spec.OVS = &ovnv1alpha1.OVNChassisOVSSpec{
		OVNChassisContainerSpec: ovnv1alpha1.OVNChassisContainerSpec{Resources: &resources},
		RevalidatorThreads:      ptr.To(int32(4)),
	}
	ds := buildOVSDaemonSet(cr)
	for _, name := range []string{"host-prepare", "ovs-vswitchd"} {
		g.Expect(ovsContainer(t, ds, name).Resources).To(Equal(resources), name)
	}

	cr.Spec.OVS = nil
	ds = buildOVSDaemonSet(cr)
	for _, name := range []string{"host-prepare", "ovs-vswitchd"} {
		g.Expect(ovsContainer(t, ds, name).Resources).To(Equal(corev1.ResourceRequirements{}), name)
	}
}
