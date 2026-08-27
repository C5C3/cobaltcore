// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// rollingOutAgentDaemonSet is the DaemonSet of the shared fixture mid-rollout:
// three nodes selected, two of them running a ready pod of the current
// template.
func rollingOutAgentDaemonSet(t *testing.T, cr *neutronv1alpha1.NeutronMetadataAgent) *appsv1.DaemonSet {
	t.Helper()

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:      agentDaemonSetKey.Name,
		Namespace: agentDaemonSetKey.Namespace,
	}}
	if err := controllerutil.SetControllerReference(cr, ds, testScheme()); err != nil {
		t.Fatalf("setting the controller reference on DaemonSet %s: %v", ds.Name, err)
	}
	ds.Status = appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3,
		CurrentNumberScheduled: 3,
		UpdatedNumberScheduled: 2,
		NumberReady:            2,
	}
	return ds
}

// A DaemonSet that selects no node is ready: its rollout has nothing left to do,
// and reporting the agent unready until somebody labels a node would make an
// empty selection indistinguishable from a stuck one.
func TestReconcileDaemonSet_EmptySelectionIsReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validAgent()
	r := newAgentTestReconciler(cr)

	res, err := r.reconcileDaemonSet(ctx, r.Client, cr, resolvedForAgentConfig(), "agent-config", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a rolled-out DaemonSet is not polled")

	var ds appsv1.DaemonSet
	g.Expect(r.Get(ctx, agentDaemonSetKey, &ds)).To(Succeed())
	g.Expect(ds.Spec.Template.Spec.Volumes).To(ContainElement(HaveField("Name", configVolumeName)))

	cond := agentCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetReady))
	g.Expect(cr.Status.InstalledImage).To(Equal("ghcr.io/c5c3/neutron:2026.1"))
}

// A rollout in flight polls, mirrors the counters it found, and leaves the
// installed image alone: the image belongs to the pods that run, and on one of
// the three nodes the previous one still does.
func TestReconcileDaemonSet_ProgressingMirrorsTheCounters(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validAgent()
	r := newAgentTestReconciler(cr, rollingOutAgentDaemonSet(t, cr))

	res, err := r.reconcileDaemonSet(ctx, r.Client, cr, resolvedForAgentConfig(), "agent-config", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	cond := agentCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(cond.Message).To(ContainSubstring("2 of 3 nodes"))

	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(3))
	g.Expect(cr.Status.NumberReady).To(BeEquivalentTo(2))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty(),
		"the installed image is the image the nodes run, and one of them still runs the previous one")
}

// The counters and the installed image both follow the live object: once every
// selected node runs a ready pod, the image those pods run is recorded.
func TestReconcileDaemonSet_ReadyRecordsTheInstalledImage(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validAgent()
	r := newAgentTestReconciler(cr, rollingOutAgentDaemonSet(t, cr))

	_, err := r.reconcileDaemonSet(ctx, r.Client, cr, resolvedForAgentConfig(), "agent-config", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cr.Status.InstalledImage).To(BeEmpty(), "the rollout has reached two of three nodes")
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, agentDaemonSetKey)).To(Succeed())

	res, err := r.reconcileDaemonSet(ctx, r.Client, cr, resolvedForAgentConfig(), "agent-config", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(agentCondition(cr, conditionTypeDaemonSetReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(3))
	g.Expect(cr.Status.NumberReady).To(BeEquivalentTo(3))
	g.Expect(cr.Status.InstalledImage).To(Equal(cr.Spec.Image.Reference()))
}

// A target cluster that grants the operator no daemonsets verb fails here. The
// condition has to flip on that pass: the aggregate Ready is re-derived from the
// sub-conditions at the new observedGeneration, so a condition left untouched
// would report the failed pass as ready.
func TestReconcileDaemonSet_ApplyErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validAgent()

	boom := errors.New("daemonsets is forbidden")
	c := neutronFakeClientBuilder(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if co, ok := obj.(client.Object); ok && co.GetObjectKind().GroupVersionKind().Kind == "DaemonSet" {
					return boom
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &NeutronMetadataAgentReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileDaemonSet(context.Background(), r.Client, cr,
		resolvedForAgentConfig(), "agent-config", "")

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("ensuring metadata-agent DaemonSet:")))
	g.Expect(res.IsZero()).To(BeTrue())

	cond := agentCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetError))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(0),
		"a pass that could not apply the DaemonSet has no counters to mirror")
	g.Expect(cr.Status.InstalledImage).To(BeEmpty())
}

// The pod runs where the chassis runs. The selection is not derivable from a
// label constant, so it is copied out of the OVNChassis and onto the DaemonSet.
func TestBuildAgentDaemonSet_RunsOnTheChassisNodes(t *testing.T) {
	g := NewGomegaWithT(t)
	chassis := resolvedForAgentConfig()
	chassis.nodeSelector = map[string]string{"openstack.c5c3.io/gateway": "true"}
	chassis.tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}

	ds := buildAgentDaemonSet(validAgent(), chassis, "agent-config", "")

	g.Expect(ds.Spec.Template.Spec.NodeSelector).To(Equal(chassis.nodeSelector))
	g.Expect(ds.Spec.Template.Spec.Tolerations).To(Equal(chassis.tolerations))
	g.Expect(ds.Spec.Template.Spec.HostNetwork).To(BeTrue())
	g.Expect(ds.Spec.Template.Spec.DNSPolicy).To(Equal(corev1.DNSClusterFirstWithHostNet),
		"a host-network pod without it loses cluster DNS")
}

// The two postures of the pod: the gate runs unprivileged as the image's own
// user, the agent runs privileged as root, and neither may drift into the
// other's.
func TestBuildAgentDaemonSet_ContainerPostures(t *testing.T) {
	g := NewGomegaWithT(t)

	ds := buildAgentDaemonSet(validAgent(), resolvedForAgentConfig(), "agent-config", "")

	init := ds.Spec.Template.Spec.InitContainers[0]
	g.Expect(init.Name).To(Equal("wait-for-chassis"))
	g.Expect(init.SecurityContext).To(Equal(deployment.RestrictedSecurityContext()))
	g.Expect(init.Command).To(Equal([]string{"/bin/sh", "-c", waitForChassisScript}))
	g.Expect(init.Command[2]).To(ContainSubstring("ovsdb-client --timeout=5 transact"),
		"ovs-vsctl is not in the neutron image")
	g.Expect(init.Command[2]).To(ContainSubstring("grep -q system-id"))
	g.Expect(init.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: runOVSVolumeName, MountPath: ovsRunDir}))

	agent := ds.Spec.Template.Spec.Containers[0]
	g.Expect(agent.Name).To(Equal(metadataAgentComponent))
	g.Expect(*agent.SecurityContext.Privileged).To(BeTrue())
	g.Expect(*agent.SecurityContext.RunAsUser).To(BeEquivalentTo(0))
	g.Expect(*agent.SecurityContext.RunAsNonRoot).To(BeFalse())
	g.Expect(agent.Command).To(Equal([]string{
		"neutron-ovn-metadata-agent", "--config-file", "/etc/neutron/neutron_ovn_metadata_agent.ini",
	}))
	g.Expect(agent.ReadinessProbe.ProbeHandler.Exec.Command).
		To(Equal([]string{"test", "-S", "/var/lib/neutron/metadata_proxy"}))
	g.Expect(agent.LivenessProbe).To(BeNil(),
		"an agent that lost its Southbound connection keeps serving the proxies it has")
}

// The namespaces the agent creates have to be visible to the node, which is what
// lets the datapath reach the proxies running inside them. Without the
// propagation they stay inside the container's mount namespace.
func TestBuildAgentDaemonSet_NetnsMountIsBidirectional(t *testing.T) {
	g := NewGomegaWithT(t)

	ds := buildAgentDaemonSet(validAgent(), resolvedForAgentConfig(), "agent-config", "")

	var netns *corev1.VolumeMount
	for i, mount := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name == runNetnsVolumeName {
			netns = &ds.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	g.Expect(netns).NotTo(BeNil())
	g.Expect(netns.MountPath).To(Equal(netnsRunDir))
	g.Expect(*netns.MountPropagation).To(Equal(corev1.MountPropagationBidirectional))
}

// The client Secret is mounted under the name the central published rather than
// one the agent derives: the CRs carry unrelated names, and an agent that
// guessed would wait on a volume that never mounts.
func TestBuildAgentDaemonSet_MountsTheCentralsClientSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	chassis := resolvedForAgentConfig()
	chassis.clientSecretName = "some-other-client"

	ds := buildAgentDaemonSet(validAgent(), chassis, "rendered-config", "")

	var secretName, configMapName string
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		switch volume.Name {
		case ovnTLSVolumeName:
			secretName = volume.Secret.SecretName
		case configVolumeName:
			configMapName = volume.ConfigMap.Name
		}
	}
	g.Expect(secretName).To(Equal("some-other-client"))
	g.Expect(configMapName).To(Equal("rendered-config"))
}

// Each env var follows the spec block that names its Secret, so a CR without a
// bus or without a Nova metadata API starts a container that sources neither.
func TestAgentEnv_FollowsTheOptionalBlocks(t *testing.T) {
	t.Run("neither block renders no environment", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(agentEnv(validAgent())).To(BeEmpty())
	})

	t.Run("both blocks render both overrides", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := withNovaMetadata("shared_secret")
		cr.Spec.Messaging = &commonv1.MessagingSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
		}

		env := agentEnv(cr)

		g.Expect(env).To(HaveLen(2))
		g.Expect(env[0].Name).To(Equal("OS_DEFAULT__TRANSPORT_URL"))
		g.Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal(testAgentName + "-transport-url"))
		g.Expect(env[1].Name).To(Equal("OS_DEFAULT__METADATA_PROXY_SHARED_SECRET"))
		g.Expect(env[1].ValueFrom.SecretKeyRef.Name).To(Equal(testAgentSharedSecretName))
		g.Expect(env[1].ValueFrom.SecretKeyRef.Key).To(Equal("shared_secret"))
	})

	t.Run("a shared secret without a key sources the default one", func(t *testing.T) {
		g := NewGomegaWithT(t)

		env := agentEnv(withNovaMetadata(""))

		g.Expect(env).To(HaveLen(1))
		g.Expect(env[0].ValueFrom.SecretKeyRef.Key).To(Equal(agentSharedSecretDefaultKey))
	})
}

// The annotation is what rolls the pods when the broker credential rotates: the
// URL is env-var-consumed, so it only takes effect on a restart. Without a
// digest there is nothing to roll on and the template stays annotation-free.
func TestBuildAgentDaemonSet_TransportDigestAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)

	without := buildAgentDaemonSet(validAgent(), resolvedForAgentConfig(), "agent-config", "")
	g.Expect(without.Spec.Template.Annotations).To(BeEmpty())

	with := buildAgentDaemonSet(validAgent(), resolvedForAgentConfig(), "agent-config", "deadbeef")
	g.Expect(with.Spec.Template.Annotations).To(HaveKeyWithValue(transportURLHashAnnotation, "deadbeef"))
}

// A CR that names no resources still lands in the Burstable QoS class: an
// unbounded agent on a compute node competes with the instances it serves.
func TestEffectiveAgentResources_FallsBackToTheSharedDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	defaults := effectiveAgentResources(validAgent())
	g.Expect(defaults.Requests).To(HaveKey(corev1.ResourceCPU))
	g.Expect(defaults.Limits).To(HaveKey(corev1.ResourceMemory))

	cr := validAgent()
	cr.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: defaults.Limits[corev1.ResourceCPU]},
	}
	g.Expect(effectiveAgentResources(cr)).To(Equal(cr.Spec.Resources))
}

// The DaemonSet must select its own pods rather than the chassis pods sharing
// the node, and a selector is immutable once applied.
func TestAgentSelectorLabels_NarrowByComponent(t *testing.T) {
	g := NewGomegaWithT(t)

	labels := agentSelectorLabels(validAgent())

	g.Expect(labels).To(HaveKeyWithValue("app.kubernetes.io/name", metadataAgentAppName))
	g.Expect(labels).To(HaveKeyWithValue("app.kubernetes.io/instance", testAgentName))
	g.Expect(labels).To(HaveKeyWithValue("app.kubernetes.io/component", metadataAgentComponent))
	g.Expect(agentDaemonSetName(validAgent())).To(Equal(testAgentName + "-metadata-agent"))
}
