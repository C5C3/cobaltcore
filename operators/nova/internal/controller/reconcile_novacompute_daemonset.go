// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The reasons of the DaemonSetReady condition.
const (
	conditionReasonDaemonSetReady       = "DaemonSetReady"
	conditionReasonDaemonSetProgressing = "DaemonSetProgressing"
	conditionReasonDaemonSetError       = "DaemonSetError"
)

// waitForChassisScript blocks until the OVNChassis on this node has registered
// it: the chassis writes external_ids:system-id into the local Open vSwitch
// database, and until that row exists an instance port has no chassis to be
// bound to. It is the gate the metadata agent runs, written against the
// OVSDB JSON-RPC protocol in the Python standard library, because the
// nova-compute image ships no ovsdb-client.
//
// Every failure (no socket yet, a refused or reset connection, a reply that is
// not the one asked for, an error reply, a row without the key) reads as "not
// yet". A message whose id is not 0, such as an echo request the server sends
// on its own, is skipped.
const waitForChassisScript = `import json, os, socket, time
path = os.environ.get("OVSDB_SOCKET", "/run/openvswitch/db.sock")
query = {"id": 0, "method": "transact", "params": ["Open_vSwitch", {"op": "select", "table": "Open_vSwitch", "where": [], "columns": ["external_ids"]}]}
def registered():
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect(path)
        s.sendall(json.dumps(query).encode())
        decoder, buf = json.JSONDecoder(), ""
        while True:
            chunk = s.recv(65536)
            if not chunk:
                return False
            buf += chunk.decode()
            while buf.strip():
                try:
                    msg, end = decoder.raw_decode(buf.lstrip())
                except ValueError:
                    break
                buf = buf.lstrip()[end:]
                if msg.get("id") != 0:
                    continue
                for row in msg["result"][0]["rows"]:
                    if any(k == "system-id" for k, _ in row["external_ids"][1]):
                        return True
                return False
    finally:
        s.close()
while True:
    try:
        if registered():
            break
    except (OSError, ValueError, KeyError, IndexError, TypeError):
        pass
    time.sleep(2)
`

// The pod volume names.
const (
	computeContractVolume = "compute-config"
	poolConfigVolume      = "pool-config"
	runLibvirtVolume      = "run-libvirt"
	varLibNovaVolume      = "var-lib-nova"
	runOVSVolume          = "run-openvswitch"
	devVolume             = "dev"
	cgroupVolume          = "sys-fs-cgroup"
	modulesVolume         = "lib-modules"
	etcISCSIVolume        = "etc-iscsi"
	etcNVMeVolume         = "etc-nvme"
	etcMultipathVolume    = "etc-multipath"
	multipathConfVolume   = "etc-multipath-conf"
	novaComputeTmpVolume  = "tmp"
)

// reconcileNovaComputeDaemonSet projects the nova-compute DaemonSet, mirrors its
// counters into status and sets DaemonSetReady.
//
// When the affinity has no term (the CR is being deleted and holds no draining
// node) there is no node left for a pod, so the DaemonSet is deleted rather
// than applied. That is what releases the pod of the last Releasing node of a
// deleting CR, whose compute service is deleted only once the pod is gone.
func (r *NovaComputeReconciler) reconcileNovaComputeDaemonSet(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute, pass *novaComputePass,
) (ctrl.Result, error) {
	affinity := novaComputeAffinity(cr, pass.excluded, pass.keepPods)
	if affinity == nil {
		if err := r.deleteNovaComputeDaemonSet(ctx, children, cr); err != nil {
			err = fmt.Errorf("deleting nova-compute DaemonSet: %w", err)
			novaComputeSkeleton.MarkFailed(cr, conditionTypeDaemonSetReady, conditionReasonDaemonSetError, err)
			return ctrl.Result{}, err
		}
		cr.Status.DesiredNumberScheduled = 0
		cr.Status.NumberReady = 0
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDaemonSetReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonDaemonSetReady,
			Message:            "No node is selected or held; the nova-compute DaemonSet is removed",
		})
		return ctrl.Result{}, nil
	}

	ds := buildNovaComputeDaemonSet(cr, pass.image, pass.secretName, pass.configMapName, pass.configHash, affinity)
	live, ready, err := deployment.EnsureDaemonSet(ctx, children, r.Scheme, cr, ds)
	if err != nil {
		err = fmt.Errorf("ensuring nova-compute DaemonSet: %w", err)
		novaComputeSkeleton.MarkFailed(cr, conditionTypeDaemonSetReady, conditionReasonDaemonSetError, err)
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
			Message: fmt.Sprintf("Waiting for the nova-compute DaemonSet: %d of %d nodes run a ready pod",
				live.Status.NumberReady, live.Status.DesiredNumberScheduled),
		})
		// Not a wait: the aggregates and the drain do not depend on the rollout,
		// and a node whose pod never gets ready, such as the NotReady hypervisor
		// being taken out of the pool, must not hold them up. The DaemonSet's
		// status events and the Services step's poll bring the next pass.
		return ctrl.Result{}, nil
	}

	// The installed image records what runs rather than what was applied, so it
	// is stamped on this arm only.
	cr.Status.InstalledImage = pass.image.Reference()

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDaemonSetReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonDaemonSetReady,
		Message: fmt.Sprintf("The nova-compute DaemonSet runs a ready pod on %d nodes",
			live.Status.DesiredNumberScheduled),
	})
	return ctrl.Result{}, nil
}

// deleteNovaComputeDaemonSet deletes the CR's DaemonSet if the CR owns it. The
// read is live, so a DaemonSet created moments ago is not missed.
func (r *NovaComputeReconciler) deleteNovaComputeDaemonSet(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute,
) error {
	live := &appsv1.DaemonSet{}
	key := types.NamespacedName{Namespace: cr.Namespace, Name: novaComputeDaemonSetName(cr)}
	if err := commonmulticluster.LiveReader(children).Get(ctx, key, live); err != nil {
		return client.IgnoreNotFound(err)
	}
	owned, err := commonmulticluster.Controls(r.Scheme, cr, live)
	if err != nil || !owned {
		return err
	}
	return client.IgnoreNotFound(children.Delete(ctx, live))
}

// novaComputeAffinity is the node affinity of the pool's pods: OR'ed terms,
// each a set of ANDed requirements.
//
//   - Unless the CR is being deleted, one term selects the pool: a matchExpressions
//     entry "key In [value]" per nodeSelector pair, plus a matchFields entry
//     "metadata.name NotIn [n]" per node in Conflict. A matchFields requirement
//     takes exactly one value, so each excluded node is its own entry.
//   - One term "metadata.name In [n]" per Draining node keeps its pod while its
//     instances leave, whatever its labels say now.
//
// Every list is sorted, so the rendered template is stable. It returns nil
// when there is no term, which the caller reads as "no pod anywhere".
func novaComputeAffinity(cr *novav1alpha1.NovaCompute, excluded, keepPods []string) *corev1.Affinity {
	var terms []corev1.NodeSelectorTerm

	if cr.DeletionTimestamp.IsZero() {
		var selector corev1.NodeSelectorTerm
		for _, key := range slices.Sorted(maps.Keys(cr.Spec.NodeSelector)) {
			selector.MatchExpressions = append(selector.MatchExpressions, corev1.NodeSelectorRequirement{
				Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{cr.Spec.NodeSelector[key]},
			})
		}
		for _, node := range slices.Sorted(slices.Values(excluded)) {
			selector.MatchFields = append(selector.MatchFields, corev1.NodeSelectorRequirement{
				Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node},
			})
		}
		terms = append(terms, selector)
	}

	for _, node := range slices.Sorted(slices.Values(keepPods)) {
		terms = append(terms, corev1.NodeSelectorTerm{
			MatchFields: []corev1.NodeSelectorRequirement{{
				Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node},
			}},
		})
	}

	if len(terms) == 0 {
		return nil
	}
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms},
	}}
}

// buildNovaComputeDaemonSet builds the pool's DaemonSet: the gate that waits for
// the node's chassis, and nova-compute.
//
// There is no probe. Nova's own view of the service (up or down) is what the
// Services step reports, and a liveness probe that restarted nova-compute
// would interrupt the operations it is running on instances.
func buildNovaComputeDaemonSet(cr *novav1alpha1.NovaCompute, image commonv1.ImageSpec,
	secretName, configMapName, configHash string, affinity *corev1.Affinity,
) *appsv1.DaemonSet {
	ref := image.Reference()
	var resources corev1.ResourceRequirements
	if cr.Spec.Resources != nil {
		resources = *cr.Spec.Resources.DeepCopy()
	}

	initContainers := []corev1.Container{{
		Name:            "wait-for-chassis",
		Image:           ref,
		Command:         []string{"python3", "-c", waitForChassisScript},
		Env:             []corev1.EnvVar{{Name: "OVSDB_SOCKET", Value: ovsDBSocket}},
		SecurityContext: deployment.RestrictedSecurityContext(),
		Resources:       resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: runOVSVolume, MountPath: runOVSDir},
		},
	}}

	containers := []corev1.Container{{
		Name:  novaComputeComponent,
		Image: ref,
		Command: []string{
			"nova-compute",
			"--config-file", path.Join(computeConfigMountPath, computeConfigFragmentKey),
			"--config-dir", poolConfigMountPath,
		},
		// Root, privileged: libvirt's socket on a stock host is root:libvirt
		// 0660 with a group ID that differs per host, the host directories come
		// up root-owned, and the privsep daemons nova-compute starts need the
		// full capability set.
		SecurityContext: novaComputeSecurityContext(),
		Resources:       resources,
		Env:             novaComputeEnv(secretName),
		VolumeMounts: []corev1.VolumeMount{
			{Name: computeContractVolume, MountPath: computeConfigMountPath, ReadOnly: true},
			{Name: poolConfigVolume, MountPath: poolConfigMountPath, ReadOnly: true},
			{Name: runLibvirtVolume, MountPath: "/run/libvirt"},
			// Bidirectional: os-brick mounts NFS volumes below state_path, and
			// the host's QEMU has to see those mounts.
			{
				Name:             varLibNovaVolume,
				MountPath:        novaStatePath,
				MountPropagation: ptr.To(corev1.MountPropagationBidirectional),
			},
			{Name: runOVSVolume, MountPath: runOVSDir},
			{Name: devVolume, MountPath: "/dev"},
			{Name: cgroupVolume, MountPath: "/sys/fs/cgroup", ReadOnly: true},
			// os-brick loads the iSCSI and NVMe-oF modules with modprobe.
			{Name: modulesVolume, MountPath: "/lib/modules", ReadOnly: true},
			{Name: etcISCSIVolume, MountPath: "/etc/iscsi"},
			{Name: etcNVMeVolume, MountPath: "/etc/nvme"},
			{Name: etcMultipathVolume, MountPath: "/etc/multipath"},
			{Name: multipathConfVolume, MountPath: "/etc/multipath.conf"},
			{Name: novaComputeTmpVolume, MountPath: "/tmp"},
		},
	}}

	volumes := []corev1.Volume{
		// The whole contract Secret, not one key: on a TLS bus the fragment's
		// ssl_ca_file names the ca.crt beside it.
		{Name: computeContractVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secretName},
		}},
		{Name: poolConfigVolume, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		}},
		hostPathVolume(runLibvirtVolume, "/run/libvirt", corev1.HostPathDirectoryOrCreate),
		hostPathVolume(varLibNovaVolume, novaStatePath, corev1.HostPathDirectoryOrCreate),
		hostPathVolume(runOVSVolume, runOVSDir, corev1.HostPathDirectoryOrCreate),
		hostPathVolume(devVolume, "/dev", ""),
		hostPathVolume(cgroupVolume, "/sys/fs/cgroup", ""),
		hostPathVolume(modulesVolume, "/lib/modules", ""),
		hostPathVolume(etcISCSIVolume, "/etc/iscsi", corev1.HostPathDirectoryOrCreate),
		hostPathVolume(etcNVMeVolume, "/etc/nvme", corev1.HostPathDirectoryOrCreate),
		hostPathVolume(etcMultipathVolume, "/etc/multipath", corev1.HostPathDirectoryOrCreate),
		hostPathVolume(multipathConfVolume, "/etc/multipath.conf", corev1.HostPathFileOrCreate),
		{Name: novaComputeTmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	return deployment.BuildDaemonSet(deployment.DaemonSetParams{
		Namespace:      cr.Namespace,
		Name:           novaComputeDaemonSetName(cr),
		Labels:         naming.ComponentLabels(novaComputeAppName, cr.Name, novaComputeComponent),
		SelectorLabels: novaComputeSelectorLabels(cr),
		PodAnnotations: map[string]string{novaComputeConfigHashAnnotation: configHash},
		UpdateStrategy: novaComputeUpdateStrategy(cr),
		Affinity:       affinity,
		Tolerations:    cr.Spec.Tolerations,
		// nova-compute reports the node's own address as my_ip and plugs
		// instance ports into the node's switch.
		HostNetwork: true,
		// The pod-level context carries the seccomp profile and nothing else:
		// the two containers pin their own users, and an fsGroup would be
		// applied to the host directories.
		PodSecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: initContainers,
		Containers:     containers,
		Volumes:        volumes,
	})
}

// hostPathVolume is a hostPath volume of the given type; "" performs no check.
func hostPathVolume(name, hostPath string, pathType corev1.HostPathType) corev1.Volume {
	source := &corev1.HostPathVolumeSource{Path: hostPath}
	if pathType != "" {
		source.Type = ptr.To(pathType)
	}
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: source}}
}

// novaComputeSecurityContext is the posture of the nova-compute container: the
// privileged profile, pinned to uid 0. RunAsUser and RunAsNonRoot are spelled
// out because PrivilegedSecurityContext leaves both unset, which would leave
// the container on the image's own user.
func novaComputeSecurityContext() *corev1.SecurityContext {
	sc := deployment.PrivilegedSecurityContext()
	sc.RunAsUser = ptr.To(int64(0))
	sc.RunAsNonRoot = ptr.To(false)
	return sc
}

// novaComputeEnv is the environment of the nova-compute container: the node's
// identity from the downward API, and the bus URL and the one service
// password from the contract, as oslo.config overrides so neither is written
// to a ConfigMap.
func novaComputeEnv(secretName string) []corev1.EnvVar {
	fieldRef := func(name, fieldPath string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: fieldPath},
		}}
	}
	env := []corev1.EnvVar{
		fieldRef("OS_DEFAULT__HOST", "spec.nodeName"),
		fieldRef("OS_DEFAULT__MY_IP", "status.hostIP"),
		fieldRef("OS_VNC__SERVER_PROXYCLIENT_ADDRESS", "status.hostIP"),
		{Name: "OS_DEFAULT__TRANSPORT_URL", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  transportURLKey,
			},
		}},
	}
	for _, section := range []string{"keystone_authtoken", "service_user", "placement", "neutron", "cinder"} {
		env = append(env, keystoneauth.ClientPasswordEnvVar(section, secretName, passwordKey))
	}
	return env
}

// novaComputeUpdateStrategy maps spec.updateStrategy onto the DaemonSet's own.
// An empty type counts as RollingUpdate, the CRD default.
func novaComputeUpdateStrategy(cr *novav1alpha1.NovaCompute) *appsv1.DaemonSetUpdateStrategy {
	if cr.Spec.UpdateStrategy.Type == string(appsv1.OnDeleteDaemonSetStrategyType) {
		return &appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}
	}
	maxUnavailable := ptr.To(intstr.FromInt32(1))
	if cr.Spec.UpdateStrategy.MaxUnavailable != nil {
		// Copied rather than aliased: the rendered object must not share a field
		// with the CR it was rendered from.
		maxUnavailable = ptr.To(*cr.Spec.UpdateStrategy.MaxUnavailable)
	}
	return &appsv1.DaemonSetUpdateStrategy{
		Type:          appsv1.RollingUpdateDaemonSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: maxUnavailable},
	}
}

// novaComputeSelectorLabels is the pod selector of the pool's DaemonSet.
func novaComputeSelectorLabels(cr *novav1alpha1.NovaCompute) map[string]string {
	labels := naming.SelectorLabels(novaComputeAppName, cr.Name)
	labels[naming.LabelKeyComponent] = novaComputeComponent
	return labels
}

// novaComputeDaemonSetName names the pool's DaemonSet.
func novaComputeDaemonSetName(cr *novav1alpha1.NovaCompute) string {
	return cr.Name + "-" + novaComputeComponent
}
