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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeOVSReady is the condition the Open vSwitch step reports under. It
// covers the local switching layer of every selected node: the configuration
// database and the daemon that owns the node's datapath. ovn-controller
// configures both, so a node whose Open vSwitch is not ready has nothing for the
// next step to program.
const conditionTypeOVSReady = "OVSReady"

// The condition reasons shared by the two DaemonSet steps, Open vSwitch and
// ovn-controller. Both report the same three outcomes of one DaemonSet, so one
// vocabulary serves them; spelling it out twice would let the two drift apart.
const (
	conditionReasonDaemonSetReady       = "DaemonSetReady"
	conditionReasonDaemonSetProgressing = "DaemonSetProgressing"
	conditionReasonDaemonSetError       = "DaemonSetError"
)

// componentOVS is the component-label value and the name suffix of the Open
// vSwitch DaemonSet.
const componentOVS = "ovs"

// The host directories the chassis pods mount.
const (
	// ovsRunDir holds the local Open vSwitch database, the Unix sockets the
	// containers reach each other through, and the pid files. It is a host path
	// rather than an emptyDir because the kernel datapath ovs-vswitchd programs
	// outlives the pod: a restarted pod that found an empty directory would
	// build a second database and leave the flows of the first one behind.
	ovsRunDir = "/run/openvswitch"

	// chassisOVNRunDir holds ovn-controller's socket and pid file, on the host
	// for the same reason. It is not the central's ovnRunDir, which is a path
	// inside a database pod's emptyDir.
	chassisOVNRunDir = "/run/ovn"

	// modulesDir is the node's kernel module tree. The init container loads the
	// datapath modules from it.
	modulesDir = "/lib/modules"
)

// The in-pod paths the chassis containers share.
const (
	// ovsLogDir is where ovsdb-server and ovs-vswitchd write their log files. It
	// is an emptyDir: what a node keeps of a chassis pod is the stdout the
	// container runtime collects, and this directory only keeps the daemons from
	// failing on a read-only root filesystem.
	ovsLogDir = "/var/log/openvswitch"

	// chassisScriptDir is where the scripts ConfigMap is mounted. Every container
	// that runs a script mounts it at the same path, so one command line is valid
	// in all of them.
	chassisScriptDir = "/etc/ovn-chassis/bin"
)

// The Unix sockets of the Open vSwitch containers. Each daemon is told its own
// socket explicitly rather than left on the location the image was built with,
// because a readiness probe and a preStop hook open them by path.
const (
	ovsDBSocket       = ovsRunDir + "/db.sock"
	ovsdbCtlSocket    = ovsRunDir + "/ovsdb-server.ctl"
	vswitchdCtlSocket = ovsRunDir + "/ovs-vswitchd.ctl"
)

// The chassis volume names the central's constants do not already cover. The two
// run directories are separate volumes because they are separate host paths, and
// each DaemonSet has its own log volume.
const (
	runOVSVolumeName  = "run-ovs"
	runOVNVolumeName  = "run-ovn"
	modulesVolumeName = "modules"
	logOVSVolumeName  = "log-ovs"
)

// reconcileOVS projects the Open vSwitch DaemonSet onto the selected nodes.
//
// It is the step ovn-controller depends on rather than the other way round: the
// local database is where a node's chassis configuration is written, and the
// switching daemon is what carries the traffic once ovn-controller has
// programmed it.
func (r *OVNChassisReconciler) reconcileOVS(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNChassis) (ctrl.Result, error) {
	live, ready, err := deployment.EnsureDaemonSet(ctx, children, r.Scheme, cr, buildOVSDaemonSet(cr))
	if err != nil {
		err = fmt.Errorf("ensuring ovs DaemonSet: %w", err)
		chassisSkeleton.MarkFailed(cr, conditionTypeOVSReady, conditionReasonDaemonSetError, err)
		return ctrl.Result{}, err
	}

	if !ready {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeOVSReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonDaemonSetProgressing,
			Message: fmt.Sprintf("Waiting for the ovs DaemonSet: %d of %d nodes run a ready pod",
				live.Status.NumberReady, live.Status.DesiredNumberScheduled),
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// A DaemonSet that selects no node is ready on zero nodes. Its rollout has
	// nothing left to do, and reporting the CR unready until somebody labels a
	// node would make an empty selection look like a stuck one.
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeOVSReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonDaemonSetReady,
		Message: fmt.Sprintf("The ovs DaemonSet runs a ready pod on %d nodes",
			live.Status.DesiredNumberScheduled),
	})
	return ctrl.Result{}, nil
}

// buildOVSDaemonSet builds the Open vSwitch DaemonSet: ovsdb-server holding the
// node's configuration database, ovs-vswitchd driving the datapath, and a
// privileged init container preparing the host for both.
//
// Neither container has a liveness probe. ovs-vswitchd owns the kernel datapath
// every workload on the node forwards through, so restarting it for a fault the
// readiness probe already reports would turn a degraded node into a
// disconnected one.
func buildOVSDaemonSet(cr *ovnv1alpha1.OVNChassis) *appsv1.DaemonSet {
	image := effectiveImage(cr.Spec.Image).Reference()

	// The one privileged container of the pod. It loads the datapath kernel
	// modules and creates the host run directories the other containers write
	// into, and both of those need the host's own root rather than the
	// unprivileged user the image declares.
	prepare := deployment.PrivilegedSecurityContext()
	prepare.RunAsUser = ptr.To(int64(0))

	initContainers := []corev1.Container{{
		Name:            "host-prepare",
		Image:           image,
		Command:         []string{"/bin/bash", path.Join(chassisScriptDir, hostPrepareScriptKey)},
		SecurityContext: prepare,
		VolumeMounts: []corev1.VolumeMount{
			{Name: modulesVolumeName, MountPath: modulesDir, ReadOnly: true},
			{Name: runOVSVolumeName, MountPath: ovsRunDir},
			{Name: runOVNVolumeName, MountPath: chassisOVNRunDir},
			{Name: scriptsVolumeName, MountPath: chassisScriptDir},
		},
	}}

	containers := []corev1.Container{
		{
			Name:  "ovsdb-server",
			Image: image,
			// The daemon is behind a script so that its umask is set before it
			// creates the socket the two datapath containers connect to. The
			// script carries the flags, including the manager_options remote that
			// lets ovs-vsctl set-manager work on this node.
			Command: []string{"/bin/bash", path.Join(chassisScriptDir, runOVSDBScriptKey)},
			// The probe asks the database the same question every client asks
			// first, so a pod counts as ready once the socket answers rather than
			// once the process exists.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"ovs-vsctl", "--timeout=5", "--no-wait", "show"},
					},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				TimeoutSeconds:      5,
			},
			Lifecycle: &corev1.Lifecycle{
				PreStop: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"ovs-appctl", "-t", ovsdbCtlSocket, "exit"},
					},
				},
			},
			SecurityContext: deployment.RestrictedSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: runOVSVolumeName, MountPath: ovsRunDir},
				{Name: logOVSVolumeName, MountPath: ovsLogDir},
				{Name: tmpVolumeName, MountPath: "/tmp"},
				{Name: scriptsVolumeName, MountPath: chassisScriptDir},
			},
		},
		{
			Name:  "ovs-vswitchd",
			Image: image,
			// SYS_NICE beside NET_ADMIN: the daemon raises the priority of its own
			// polling threads, and without the capability every start logs the
			// failure and runs at ordinary priority.
			SecurityContext: rootCapabilitySecurityContext("NET_ADMIN", "SYS_NICE"),
			Resources:       chassisResources(cr.Spec.OVS),
			Command:         []string{"/bin/bash", path.Join(chassisScriptDir, runVswitchdScriptKey)},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"ovs-appctl", "-t", vswitchdCtlSocket, "version"},
					},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				TimeoutSeconds:      5,
			},
			// The exit is deliberately not "exit --cleanup": the cleanup flag
			// tears the kernel datapath down, and the node would stop forwarding
			// for the whole time it takes the replacement pod to start. Leaving
			// the datapath in place keeps the flows valid across the restart, and
			// the new ovs-vswitchd adopts it.
			Lifecycle: &corev1.Lifecycle{
				PreStop: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"ovs-appctl", "-t", vswitchdCtlSocket, "exit"},
					},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: runOVSVolumeName, MountPath: ovsRunDir},
				{Name: logOVSVolumeName, MountPath: ovsLogDir},
				{Name: tmpVolumeName, MountPath: "/tmp"},
				{Name: scriptsVolumeName, MountPath: chassisScriptDir},
			},
		},
	}

	volumes := append(chassisRunVolumes(),
		// The module tree is mounted as it is: a node that has none is a node
		// whose datapath modules cannot be loaded, and DirectoryOrCreate would
		// hide that behind an empty directory and a modprobe failure.
		corev1.Volume{Name: modulesVolumeName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: modulesDir},
		}},
		corev1.Volume{Name: logOVSVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
		corev1.Volume{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
		chassisScriptsVolume(cr),
	)

	return deployment.BuildDaemonSet(chassisDaemonSetParams(cr, chassisOVSName(cr), componentOVS,
		initContainers, containers, volumes))
}

// chassisDaemonSetParams is the shape both chassis DaemonSets share: the node's
// own network namespace, the node selection and rollout pace the CR names, and a
// pod-level posture that leaves every container to pin its own.
func chassisDaemonSetParams(cr *ovnv1alpha1.OVNChassis, name, component string,
	initContainers, containers []corev1.Container, volumes []corev1.Volume,
) deployment.DaemonSetParams {
	return deployment.DaemonSetParams{
		Namespace:      cr.Namespace,
		Name:           name,
		Labels:         naming.ComponentLabels(chassisAppName, cr.Name, component),
		SelectorLabels: chassisSelectorLabels(cr, component),
		UpdateStrategy: chassisUpdateStrategy(cr),
		NodeSelector:   cr.Spec.NodeSelector,
		Tolerations:    cr.Spec.Tolerations,
		// The pods run in the node's network namespace. The tunnels between
		// chassis terminate on the node's own address, and the bridges the
		// datapath attaches to are the node's interfaces.
		HostNetwork: true,
		// The pod-level context carries the seccomp profile and nothing else. The
		// containers below run under three different postures and pin their own
		// users, and an fsGroup would be applied to the host directories the pod
		// mounts, where the ownership is the node's business rather than the
		// pod's.
		PodSecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		// TerminationGracePeriodSeconds is left to the builder's default of 30
		// seconds, which is what the preStop hooks need: each of them is a single
		// ovs-appctl call that returns as soon as the daemon has exited.
		InitContainers: initContainers,
		Containers:     containers,
		Volumes:        volumes,
	}
}

// chassisUpdateStrategy maps spec.updateStrategy onto the DaemonSet's own.
//
// There is no maxSurge counterpart, because a second ovn-controller on a node
// would contend with the first one over the local database. An empty type
// counts as RollingUpdate, the value the CRD defaults: a CR that reaches the
// operator without one bypassed admission, and a DaemonSet rendered with an
// empty strategy type is rejected by the API server.
func chassisUpdateStrategy(cr *ovnv1alpha1.OVNChassis) *appsv1.DaemonSetUpdateStrategy {
	if cr.Spec.UpdateStrategy.Type == string(appsv1.OnDeleteDaemonSetStrategyType) {
		// OnDelete hands the pace to whoever deletes the pods, so there is no
		// rolling-update block to fill in.
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

// chassisSelectorLabels is the pod selector of one chassis DaemonSet: the shared
// selector labels narrowed by the component, so the two DaemonSets of one CR
// select their own pods rather than each other's.
func chassisSelectorLabels(cr *ovnv1alpha1.OVNChassis, component string) map[string]string {
	labels := naming.SelectorLabels(chassisAppName, cr.Name)
	labels[naming.LabelKeyComponent] = component
	return labels
}

// chassisOVSName names the Open vSwitch DaemonSet.
func chassisOVSName(cr *ovnv1alpha1.OVNChassis) string {
	return cr.Name + "-" + componentOVS
}

// chassisResources resolves the requests and limits of one chassis container. A
// block the CR leaves unset renders none, the same way the database container
// does: what a datapath needs depends on the traffic the node carries, and a
// default picked here would be wrong on most hardware.
func chassisResources(spec *ovnv1alpha1.OVNChassisContainerSpec) corev1.ResourceRequirements {
	if spec == nil || spec.Resources == nil {
		return corev1.ResourceRequirements{}
	}
	return *spec.Resources
}

// rootCapabilitySecurityContext is the posture of a container that programs the
// node's datapath: the Restricted profile with the named capabilities added, run
// as uid 0 in the group that owns the Open vSwitch run directories.
//
// The capabilities alone are not enough for either daemon. Both open the netlink
// families and the device nodes the kernel restricts to root, so RunAsNonRoot is
// spelled out as false rather than left unset, which keeps the container from
// being rejected by a cluster that defaults it the other way.
//
// The group is what gets them to the local database. Dropping ALL takes
// CAP_DAC_OVERRIDE with it, so uid 0 here is an ordinary user as far as file
// permissions go, and every socket and directory under ovsRunDir belongs to the
// unprivileged user ovsdb-server runs as. Sharing that group is what the two
// daemons need rather than the capability that would bypass the check.
func rootCapabilitySecurityContext(caps ...corev1.Capability) *corev1.SecurityContext {
	sc := deployment.CapabilitySecurityContext(caps...)
	sc.RunAsUser = ptr.To(int64(0))
	sc.RunAsGroup = ptr.To(deployment.OpenStackUID)
	sc.RunAsNonRoot = ptr.To(false)
	return sc
}

// chassisRunVolumes are the two host directories both DaemonSets mount. The
// sockets in them are how the containers of the two pods reach each other, which
// is why neither may be an emptyDir. DirectoryOrCreate covers the first boot of
// a node that has never run Open vSwitch.
func chassisRunVolumes() []corev1.Volume {
	return []corev1.Volume{
		{Name: runOVSVolumeName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: ovsRunDir,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		}},
		{Name: runOVNVolumeName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: chassisOVNRunDir,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		}},
	}
}

// chassisScriptsVolume mounts the scripts ConfigMap the node step applies. 0555
// rather than the default 0644: the containers execute these files, and a
// ConfigMap volume carries no executable bit unless it is asked for.
func chassisScriptsVolume(cr *ovnv1alpha1.OVNChassis) corev1.Volume {
	return corev1.Volume{Name: scriptsVolumeName, VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: chassisScriptsName(cr)},
			DefaultMode:          ptr.To(int32(0o555)),
		},
	}}
}
