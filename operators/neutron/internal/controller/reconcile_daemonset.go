// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"path"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// conditionTypeDaemonSetReady is the condition the DaemonSet step reports under.
// It is the last gate of the agent pipeline: everything the pods mount has been
// resolved by the time it runs.
const conditionTypeDaemonSetReady = "DaemonSetReady"

// The condition reasons of the DaemonSet step.
const (
	conditionReasonDaemonSetReady       = "DaemonSetReady"
	conditionReasonDaemonSetProgressing = "DaemonSetProgressing"
	conditionReasonDaemonSetError       = "DaemonSetError"
)

// The host directories the agent pod shares with the OVNChassis pods on the same
// node: the Open vSwitch runtime directory holding the local database socket,
// and the network-namespace directory the agent creates its proxy namespaces in.
const (
	ovsRunDir   = "/run/openvswitch"
	netnsRunDir = "/run/netns"
)

// The pod volume names the agent adds to the ones the API pods already carry
// (configVolumeName, ovnTLSVolumeName, stateVolumeName).
const (
	runOVSVolumeName   = "run-ovs"
	runNetnsVolumeName = "run-netns"
	tmpVolumeName      = "tmp"
)

// metadataProxySocketPath is the UNIX socket the agent listens on once it is
// serving. It is the [DEFAULT] metadata_proxy_socket default, $state_path
// relative, and the readiness probe tests for it rather than for the process:
// an agent that has not opened it answers no instance.
const metadataProxySocketPath = neutronStatePath + "/metadata_proxy"

// metadataProxySharedSecretEnvVarName is the oslo.config env override for
// [DEFAULT].metadata_proxy_shared_secret. The OS_<GROUP>__<OPTION> form wins
// over the rendered file at runtime, so the secret the agent signs forwarded
// requests with reaches the process from the referenced Secret instead of from
// the ConfigMap.
// #nosec G101 -- an oslo.config env override key, not a credential.
const metadataProxySharedSecretEnvVarName = "OS_DEFAULT__METADATA_PROXY_SHARED_SECRET"

// waitForChassisScript blocks until the OVNChassis on this node has registered
// itself: its apply-node init container writes external_ids:system-id into the
// local Open vSwitch database, and until that row exists the agent has no
// chassis to read port bindings for. Both workloads select the same nodes, but
// nothing orders the two DaemonSets, so the gate is per node rather than per
// cluster.
//
// The query goes through ovsdb-client rather than ovs-vsctl: the neutron image
// ships the OVS Python client and not the ovs-vsctl binary.
const waitForChassisScript = `until ovsdb-client --timeout=5 transact unix:/run/openvswitch/db.sock ` +
	`'["Open_vSwitch",{"op":"select","table":"Open_vSwitch","where":[],"columns":["external_ids"]}]' ` +
	`2>/dev/null | grep -q system-id; do sleep 2; done`

// reconcileDaemonSet projects the metadata-agent DaemonSet onto the chassis's
// nodes and mirrors its node counters into status.
func (r *NeutronMetadataAgentReconciler) reconcileDaemonSet(ctx context.Context, children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent, chassis resolvedChassis, configMapName, transportDigest string,
) (ctrl.Result, error) {
	live, ready, err := deployment.EnsureDaemonSet(ctx, children, r.Scheme, cr,
		buildAgentDaemonSet(cr, chassis, configMapName, transportDigest))
	if err != nil {
		err = fmt.Errorf("ensuring metadata-agent DaemonSet: %w", err)
		agentSkeleton.MarkFailed(cr, conditionTypeDaemonSetReady, conditionReasonDaemonSetError, err)
		return ctrl.Result{}, err
	}

	cr.Status.DesiredNumberScheduled = live.Status.DesiredNumberScheduled
	cr.Status.NumberReady = live.Status.NumberReady

	if !ready {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDaemonSetReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonDaemonSetProgressing,
			Message: fmt.Sprintf("Waiting for the metadata-agent DaemonSet: %d of %d nodes run a ready pod",
				live.Status.NumberReady, live.Status.DesiredNumberScheduled),
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// The installed image records what runs rather than what was applied, which
	// is why it is stamped on this arm only. A rollout that has reached no node
	// yet leaves the previous value in place, and that is what tells the two
	// apart.
	cr.Status.InstalledImage = cr.Spec.Image.Reference()

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDaemonSetReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonDaemonSetReady,
		Message: fmt.Sprintf("The metadata-agent DaemonSet runs a ready pod on %d nodes",
			live.Status.DesiredNumberScheduled),
	})
	return ctrl.Result{}, nil
}

// buildAgentDaemonSet builds the metadata-agent DaemonSet: the init container
// that waits for this node's chassis registration and the agent that answers the
// 169.254.169.254 requests of the instances on that node.
//
// There is no liveness probe. An agent that lost its Southbound connection keeps
// serving the proxies it already created, and restarting it would drop them for
// the fault the readiness probe already reports.
func buildAgentDaemonSet(cr *neutronv1alpha1.NeutronMetadataAgent, chassis resolvedChassis,
	configMapName, transportDigest string,
) *appsv1.DaemonSet {
	image := cr.Spec.Image.Reference()
	resources := effectiveAgentResources(cr)

	var podAnnotations map[string]string
	if transportDigest != "" {
		podAnnotations = map[string]string{transportURLHashAnnotation: transportDigest}
	}

	initContainers := []corev1.Container{{
		Name:            "wait-for-chassis",
		Image:           image,
		Command:         []string{"/bin/sh", "-c", waitForChassisScript},
		SecurityContext: deployment.RestrictedSecurityContext(),
		Resources:       resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: runOVSVolumeName, MountPath: ovsRunDir},
		},
	}}

	containers := []corev1.Container{{
		Name:  metadataAgentComponent,
		Image: image,
		// The agent creates network namespaces, moves interfaces into them and
		// starts a haproxy per network through privsep. That needs the full
		// capability set and uid 0, which the Restricted profile denies and a
		// named capability does not cover.
		SecurityContext: agentSecurityContext(),
		Resources:       resources,
		Command: []string{
			"neutron-ovn-metadata-agent",
			"--config-file", path.Join(neutronConfigMountPath, metadataAgentConfigFile),
		},
		Env: agentEnv(cr),
		// Readiness is the proxy socket rather than the process: an agent that has
		// not opened it yet answers no instance, and must not count as a node the
		// rollout may move on from.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"test", "-S", metadataProxySocketPath}},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: runOVSVolumeName, MountPath: ovsRunDir},
			// Bidirectional: the namespaces the agent creates here have to be
			// visible to the node, which is what lets the datapath reach the
			// proxies running inside them.
			{
				Name:             runNetnsVolumeName,
				MountPath:        netnsRunDir,
				MountPropagation: ptr.To(corev1.MountPropagationBidirectional),
			},
			{Name: configVolumeName, MountPath: neutronConfigMountPath, ReadOnly: true},
			{Name: ovnTLSVolumeName, MountPath: ovnTLSMountPath, ReadOnly: true},
			{Name: stateVolumeName, MountPath: neutronStatePath},
			{Name: tmpVolumeName, MountPath: "/tmp"},
		},
	}}

	volumes := []corev1.Volume{
		// The two host directories the pod shares with the chassis pods. The
		// sockets in them are how the two workloads reach each other, which is why
		// neither may be an emptyDir. DirectoryOrCreate covers the first boot of a
		// node that has never run Open vSwitch.
		{Name: runOVSVolumeName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: ovsRunDir,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		}},
		{Name: runNetnsVolumeName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: netnsRunDir,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		}},
		{Name: configVolumeName, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		}},
		// The client keypair the OVNCentral publishes, mounted under the name the
		// central chose: the two CRs carry unrelated names, and an agent that
		// guessed would wait on a volume that never mounts.
		{Name: ovnTLSVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: chassis.clientSecretName},
		}},
		// state_path and /tmp are the two directories the process writes to, and
		// the container root filesystem is read-only. Their contents are per-pod
		// scratch: the logical model lives in the OVN databases.
		{Name: stateVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
	}

	return deployment.BuildDaemonSet(deployment.DaemonSetParams{
		Namespace:      cr.Namespace,
		Name:           agentDaemonSetName(cr),
		Labels:         naming.ComponentLabels(metadataAgentAppName, cr.Name, metadataAgentComponent),
		SelectorLabels: agentSelectorLabels(cr),
		PodAnnotations: podAnnotations,
		NodeSelector:   chassis.nodeSelector,
		Tolerations:    chassis.tolerations,
		// The pod runs in the node's network namespace: it answers the
		// 169.254.169.254 requests arriving on the node's own interfaces.
		HostNetwork: true,
		// The pod-level context carries the seccomp profile and nothing else. The
		// two containers run under different postures and pin their own users, and
		// an fsGroup would be applied to the host directories the pod mounts,
		// where the ownership is the node's business rather than the pod's.
		PodSecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: initContainers,
		Containers:     containers,
		Volumes:        volumes,
	})
}

// agentSecurityContext is the posture of the agent container: the privileged
// profile, pinned to uid 0.
//
// RunAsUser and RunAsNonRoot are spelled out because PrivilegedSecurityContext
// leaves both unset, which would leave the container on the image's own
// openstack user; privsep-helper is invoked through sudo and the namespaces the
// agent creates are the node's, so neither works unprivileged.
func agentSecurityContext() *corev1.SecurityContext {
	sc := deployment.PrivilegedSecurityContext()
	sc.RunAsUser = ptr.To(int64(0))
	sc.RunAsNonRoot = ptr.To(false)
	return sc
}

// agentEnv is the environment of the agent container: the two credentials that
// reach the process as oslo.config overrides rather than through the rendered
// file, each one rendered only for the spec block that names its Secret.
func agentEnv(cr *neutronv1alpha1.NeutronMetadataAgent) []corev1.EnvVar {
	var env []corev1.EnvVar
	if cr.Spec.Messaging != nil {
		env = append(env, messaging.TransportURLEnvVar(cr.Name))
	}
	if ref := agentSharedSecretRef(cr); ref != nil {
		env = append(env, corev1.EnvVar{
			Name: metadataProxySharedSecretEnvVarName,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
					Key:                  agentSharedSecretKey(cr),
				},
			},
		})
	}
	return env
}

// agentSelectorLabels is the pod selector of the metadata-agent DaemonSet: the
// shared selector labels narrowed by the component, so it selects its own pods
// rather than the chassis pods sharing the node.
func agentSelectorLabels(cr *neutronv1alpha1.NeutronMetadataAgent) map[string]string {
	labels := naming.SelectorLabels(metadataAgentAppName, cr.Name)
	labels[naming.LabelKeyComponent] = metadataAgentComponent
	return labels
}

// agentDaemonSetName names the metadata-agent DaemonSet.
func agentDaemonSetName(cr *neutronv1alpha1.NeutronMetadataAgent) string {
	return cr.Name + "-" + metadataAgentComponent
}

// effectiveAgentResources resolves the requests and limits of both agent
// containers. An empty spec.resources falls back to the shared container
// defaults, the ones a defaulted DeploymentSpec carries, so a CR that named none
// still lands in the Burstable QoS class rather than in BestEffort.
func effectiveAgentResources(cr *neutronv1alpha1.NeutronMetadataAgent) corev1.ResourceRequirements {
	if len(cr.Spec.Resources.Requests) > 0 || len(cr.Spec.Resources.Limits) > 0 {
		return cr.Spec.Resources
	}
	var defaulted commonv1.DeploymentSpec
	defaulted.Default()
	return deployment.ContainerResources(&defaulted)
}
