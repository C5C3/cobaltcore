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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeNorthdReady is the condition the northd step reports under.
const conditionTypeNorthdReady = "NorthdReady"

// The condition reasons shared by the two Deployment steps, northd and the
// Southbound relay. Both wait on an address the endpoint step publishes and
// both report the same three Deployment outcomes, so one vocabulary serves
// them; spelling it twice would let the two drift apart.
const (
	conditionReasonWaitingForEndpoints   = "WaitingForEndpoints"
	conditionReasonDeploymentReady       = "DeploymentReady"
	conditionReasonDeploymentProgressing = "DeploymentProgressing"
	conditionReasonDeploymentError       = "DeploymentError"
)

// componentNorthd is the component-label value and the name suffix of the
// northd children.
const componentNorthd = "northd"

// The in-pod paths of the northd container that the database container does not
// already name.
const (
	// northdPidFile and northdCtlSocket sit on the run volume. northd is told
	// both explicitly because the readiness probe opens the control socket by
	// path, and the default location depends on how the image was built.
	northdPidFile   = ovnRunDir + "/ovn-northd.pid"
	northdCtlSocket = ovnRunDir + "/ovn-northd.ctl"
)

// reconcileNorthd projects ovn-northd, the daemon that compiles the Northbound
// logical model into the Southbound flow table.
//
// Every replica connects to both databases and they coordinate through a lock
// in the Southbound: one of them is active and the others sit idle until it
// loses the lock, so the replica count buys failover rather than throughput.
func (r *OVNCentralReconciler) reconcileNorthd(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	// northd is configured with the two addresses rather than with a discovery
	// mechanism of its own, so there is nothing to apply before the endpoint
	// step has published them. Applying a Deployment with an empty --ovnnb-db
	// would crash-loop the pods until the next pass rewrote the template.
	if cr.Status.Northbound.InternalDbAddress == "" || cr.Status.Southbound.InternalDbAddress == "" {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNorthdReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonWaitingForEndpoints,
			Message:            "Waiting for both database addresses to be published",
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	deploy := buildNorthdDeployment(cr)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, cr, deploy)
	if err != nil {
		err = fmt.Errorf("ensuring northd Deployment: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeNorthdReady, conditionReasonDeploymentError, err)
		return ctrl.Result{}, err
	}

	if !ready {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNorthdReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonDeploymentProgressing,
			Message:            "Waiting for the northd Deployment to become available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	// The installed image is recorded once northd runs on it, which is what
	// makes the field a rollout signal: while a new spec.image is still rolling
	// out the two differ, and they agree again when the pods have taken it.
	// northd is the daemon the whole image is judged by, since it is the one
	// process of the three that fails on a Southbound schema it cannot compile
	// against.
	cr.Status.InstalledImage = effectiveImage(cr.Spec.Image).Reference()

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNorthdReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            "The northd Deployment is available",
	})
	return ctrl.Result{}, nil
}

// effectiveNorthd resolves the northd block the Deployment is rendered from: the
// CR's own, on a copy, with the shared deployment defaults applied. The copy is
// what keeps the resolution out of the CR that is written back at the end of the
// pass.
//
// commonv1.DefaultReplicas applies unchanged. Three northd pods are not three
// times the compile capacity, they are one active instance and two standbys, and
// a standby takes over within an election of the Southbound lock.
func effectiveNorthd(cr *ovnv1alpha1.OVNCentral) ovnv1alpha1.OVNNorthdSpec {
	northd := *cr.Spec.Northd.DeepCopy()
	northd.Deployment.Default()
	return northd
}

// northdName names the northd Deployment.
func northdName(cr *ovnv1alpha1.OVNCentral) string {
	return cr.Name + "-" + componentNorthd
}

// componentSelectorLabels is the pod selector of one component's workload: the
// shared selector labels narrowed by the component, so northd and the relay
// select their own pods and not each other's or a database member's.
func componentSelectorLabels(cr *ovnv1alpha1.OVNCentral, component string) map[string]string {
	labels := naming.SelectorLabels(centralAppName, cr.Name)
	labels[naming.LabelKeyComponent] = component
	return labels
}

// buildNorthdDeployment builds the northd Deployment.
//
// There is no liveness probe. A northd that has lost its Southbound connection
// still holds the lock it may be the active holder of, and restarting it costs
// the cluster a full recompile of the logical flow table for a fault the
// readiness probe already takes it out of rotation for.
func buildNorthdDeployment(cr *ovnv1alpha1.OVNCentral) *appsv1.Deployment {
	northd := effectiveNorthd(cr)

	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      cr.Namespace,
		Name:           northdName(cr),
		Labels:         naming.ComponentLabels(centralAppName, cr.Name, componentNorthd),
		SelectorLabels: componentSelectorLabels(cr, componentNorthd),
		Deployment:     &northd.Deployment,
		Container: deployment.ContainerParams{
			Name:  componentNorthd,
			Image: effectiveImage(cr.Spec.Image).Reference(),
			Command: []string{
				"ovn-northd",
				"--ovnnb-db=" + cr.Status.Northbound.InternalDbAddress,
				"--ovnsb-db=" + cr.Status.Southbound.InternalDbAddress,
				"-p", path.Join(ovnTLSDir, "tls.key"),
				"-c", path.Join(ovnTLSDir, "tls.crt"),
				"-C", path.Join(ovnTLSDir, "ca.crt"),
				fmt.Sprintf("--n-threads=%d", northd.Threads),
				"--pidfile=" + northdPidFile,
				"--unixctl=" + northdCtlSocket,
			},
			// The control socket answers only once northd has connected to both
			// databases and settled, which is exactly when it may be handed
			// traffic-shaping work.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"ovn-appctl", "-t", northdCtlSocket, "status"},
					},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				TimeoutSeconds:      5,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: runVolumeName, MountPath: ovnRunDir},
				{Name: logVolumeName, MountPath: ovnLogDir},
				{Name: tmpVolumeName, MountPath: "/tmp"},
				{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true},
			},
		},
		// northd dials both databases and listens for nothing, so it carries the
		// client keypair rather than a server one. Everything it writes outside
		// that mount goes to an emptyDir, because the root filesystem is
		// read-only.
		Volumes: append(ovnScratchVolumes(), corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: clientSecretName(cr)},
			},
		}),
	})
}

// ovnScratchVolumes builds the three emptyDirs every OVN control-plane pod
// writes to: the run directory holding its sockets and pid file, its log
// directory, and /tmp.
func ovnScratchVolumes() []corev1.Volume {
	return []corev1.Volume{
		{Name: runVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: logVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}
