// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"path"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeControllerReady is the condition the ovn-controller step reports
// under. It is the second of the two DaemonSet conditions and the one a node's
// chassis registration follows: ovn-controller is what connects to the
// Southbound database, registers the chassis and programs the local datapath.
const conditionTypeControllerReady = "ControllerReady"

// componentOVNController is the component-label value and the name suffix of the
// ovn-controller DaemonSet.
const componentOVNController = "ovn-controller"

// The paths of the ovn-controller pod that the Open vSwitch one does not name.
const (
	// chassisNodesDir is where the nodes ConfigMap is mounted. The init container
	// reads the file named after its own node from it; applyNodeScript names the
	// same path.
	chassisNodesDir = "/etc/ovn-chassis/nodes"

	// ovnControllerCtlSocket is the control socket the readiness probe and the
	// preStop hook open.
	ovnControllerCtlSocket = chassisOVNRunDir + "/ovn-controller.ctl"
)

// The volume names of the ovn-controller pod that the Open vSwitch one does not
// carry.
const (
	logOVNVolumeName = "log-ovn"
	nodesVolumeName  = "nodes"
)

// reconcileController projects the ovn-controller DaemonSet onto the selected
// nodes and mirrors the node counters of that DaemonSet into status.
//
// The counters are read from this DaemonSet rather than from the Open vSwitch
// one because ovn-controller is what makes a node a chassis: a node running
// Open vSwitch alone carries no logical flows.
func (r *OVNChassisReconciler) reconcileController(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, central resolvedCentral,
) (ctrl.Result, error) {
	live, ready, err := deployment.EnsureDaemonSet(ctx, children, r.Scheme, cr,
		buildControllerDaemonSet(cr, central))
	if err != nil {
		err = fmt.Errorf("ensuring ovn-controller DaemonSet: %w", err)
		chassisSkeleton.MarkFailed(cr, conditionTypeControllerReady, conditionReasonDaemonSetError, err)
		return ctrl.Result{}, err
	}

	cr.Status.DesiredNumberScheduled = live.Status.DesiredNumberScheduled
	cr.Status.NumberReady = live.Status.NumberReady

	if !ready {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeControllerReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonDaemonSetProgressing,
			Message: fmt.Sprintf("Waiting for the ovn-controller DaemonSet: %d of %d nodes run a ready pod",
				live.Status.NumberReady, live.Status.DesiredNumberScheduled),
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// The installed image records what runs rather than what was applied, which
	// is why it is stamped on this arm only. A rollout that has reached no node
	// yet leaves the previous value in place, and that is what tells the two
	// apart.
	cr.Status.InstalledImage = effectiveImage(cr.Spec.Image).Reference()

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeControllerReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonDaemonSetReady,
		Message: fmt.Sprintf("The ovn-controller DaemonSet runs a ready pod on %d nodes",
			live.Status.DesiredNumberScheduled),
	})
	return ctrl.Result{}, nil
}

// chassisEnv is the environment both containers of the ovn-controller pod run
// with. The init container writes these values into the local Open vSwitch
// database and ovn-controller reads them back from there, so the two have to
// agree on them: a container handed a different remote than the one that was
// written would connect somewhere else than the node advertises.
//
// The node's name and address come from the downward API rather than from the
// nodes ConfigMap. Both are per-pod facts, and a ConfigMap that carried them
// would have to be rewritten every time a node's address changed.
func chassisEnv(cr *ovnv1alpha1.OVNChassis, central resolvedCentral) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
		{Name: "NODE_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
		}},
		{Name: "OVN_REMOTE", Value: central.ovnRemote},
		{Name: "OVN_REMOTE_PROBE_INTERVAL_MS", Value: strconv.Itoa(int(cr.Spec.RemoteProbeIntervalMs))},
	}
}

// buildControllerDaemonSet builds the ovn-controller DaemonSet: the init
// container that writes this node's values into the local Open vSwitch database
// and the daemon that turns the Southbound logical flows into datapath flows.
//
// There is no liveness probe. A chassis that lost its Southbound connection
// keeps forwarding on the flows it has, and restarting it would drop them for
// the fault the readiness probe already reports.
func buildControllerDaemonSet(cr *ovnv1alpha1.OVNChassis, central resolvedCentral) *appsv1.DaemonSet {
	image := effectiveImage(cr.Spec.Image).Reference()
	env := chassisEnv(cr, central)

	initContainers := []corev1.Container{{
		Name:            "apply-node",
		Image:           image,
		Command:         []string{"/bin/bash", path.Join(chassisScriptDir, applyNodeScriptKey)},
		Env:             env,
		SecurityContext: deployment.RestrictedSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: runOVSVolumeName, MountPath: ovsRunDir},
			{Name: scriptsVolumeName, MountPath: chassisScriptDir},
			{Name: nodesVolumeName, MountPath: chassisNodesDir},
		},
	}}

	containers := []corev1.Container{{
		Name:  componentOVNController,
		Image: image,
		// NET_ADMIN and uid 0: the daemon programs the datapath through netlink
		// and opens the local database socket the privileged init container
		// created.
		SecurityContext: rootCapabilitySecurityContext("NET_ADMIN"),
		Resources:       chassisResources(cr.Spec.Controller),
		Env:             env,
		// The Southbound address is not passed here. ovn-controller reads it from
		// the local database, where the init container wrote it, so a change to
		// the remote reaches a running chassis without a pod restart.
		Command: []string{
			"ovn-controller",
			"unix:" + ovsDBSocket,
			"--pidfile=" + path.Join(chassisOVNRunDir, "ovn-controller.pid"),
			"--unixctl=" + ovnControllerCtlSocket,
			"-p", path.Join(ovnTLSDir, "tls.key"),
			"-c", path.Join(ovnTLSDir, "tls.crt"),
			"-C", path.Join(ovnTLSDir, "ca.crt"),
		},
		// Readiness is the Southbound connection rather than the process: a
		// chassis that cannot reach the database serves stale flows and must not
		// count as a node the rollout may move on from.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"sh", "-c", fmt.Sprintf(
					"ovn-appctl -t %s connection-status | grep -q connected", ovnControllerCtlSocket)}},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
		},
		// "exit --restart" rather than a plain exit: it leaves the datapath flows
		// in place and keeps the Southbound Chassis row, so the successor pod
		// takes both over instead of re-registering the node and waiting for the
		// flows to be recomputed.
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"ovn-appctl", "-t", ovnControllerCtlSocket, "exit", "--restart"},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: runOVSVolumeName, MountPath: ovsRunDir},
			{Name: runOVNVolumeName, MountPath: chassisOVNRunDir},
			{Name: logOVNVolumeName, MountPath: ovnLogDir},
			{Name: tmpVolumeName, MountPath: "/tmp"},
			{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true},
		},
	}}

	volumes := append(chassisRunVolumes(),
		corev1.Volume{Name: logOVNVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
		corev1.Volume{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}},
		chassisScriptsVolume(cr),
		corev1.Volume{Name: nodesVolumeName, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: chassisNodesName(cr)},
			},
		}},
		// The client keypair the central publishes. Every chassis presents it to
		// the Southbound database, which authenticates it against the issuing CA
		// and authorizes it no further: the connection row carries no role=
		// column, so this one keypair is full read and write on both databases
		// and it is mounted on every selected node.
		corev1.Volume{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: central.clientSecretName},
		}},
	)

	return deployment.BuildDaemonSet(chassisDaemonSetParams(cr, chassisControllerName(cr),
		componentOVNController, initContainers, containers, volumes))
}

// chassisControllerName names the ovn-controller DaemonSet.
func chassisControllerName(cr *ovnv1alpha1.OVNChassis) string {
	return cr.Name + "-" + componentOVNController
}
