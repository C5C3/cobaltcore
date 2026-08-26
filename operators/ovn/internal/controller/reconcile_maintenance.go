// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/job"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeMaintenanceReady is the condition the maintenance step reports
// under: the Jobs that re-apply a node's changed values, move the gateway
// duties off a node that lost the role, and deregister a chassis that left. The
// other four chassis condition types are declared in the file of the step that
// sets them.
const conditionTypeMaintenanceReady = "MaintenanceReady"

// The condition reasons of the maintenance step.
const (
	conditionReasonMaintenanceIdle      = "MaintenanceIdle"
	conditionReasonMaintenanceRunning   = "MaintenanceRunning"
	conditionReasonMaintenanceDeferred  = "MaintenanceDeferred"
	conditionReasonMaintenanceJobFailed = "MaintenanceJobFailed"
	conditionReasonMaintenanceError     = "MaintenanceError"
)

// eventReasonMaintenanceJobFailed is the Warning raised for a failed
// maintenance Job. The condition repeats the same finding on every pass, so the
// event is what dates the failure.
const eventReasonMaintenanceJobFailed = "ChassisMaintenanceJobFailed"

// eventReasonMaintenanceDeferred is the Warning the shared terminal-state helper
// raises when it cannot stamp its dedupe annotation and postpones the callback
// to the next pass. The helper words its own message in terms of metric
// emission; what the callback carries here is the failure event.
const eventReasonMaintenanceDeferred = "ChassisMaintenanceMetricEmissionDeferred"

// componentMaintenance is the component-label value of the maintenance Jobs and
// the name of the single container each of them runs.
const componentMaintenance = "maintenance"

// The three maintenance Job kinds. Each is a segment of the Job name, so what a
// pod is doing to a node is readable from a Job listing alone.
const (
	maintenanceKindApply      = "apply"
	maintenanceKindEvacuate   = "evacuate"
	maintenanceKindChassisDel = "chassis-del"
)

// maintenanceNameHashLength is how many hex characters of the digest of a node
// name a Job name keeps. Four bytes tell any two node names of one cluster
// apart, and a short suffix is what leaves the CR name room in the 63 characters
// Kubernetes allows an object name.
const maintenanceNameHashLength = 8

// maintenanceActiveDeadlineSeconds caps how long one maintenance Job may stay
// active. Every one of the three is a handful of ovs-vsctl or ovn-nbctl calls
// against a database that either answers or times out on its own, so a run that
// is still active after five minutes is wedged rather than slow, and a wedged
// run reaches no terminal condition for the step to report on.
const maintenanceActiveDeadlineSeconds int64 = 300

// maintenanceTTLSecondsAfterFinished is how long a finished maintenance Job and
// its pod are kept. A day is long enough for an operator to read the logs of a
// node that misbehaved the night before, and the step needs none of it: what a
// Job achieved is recorded in status.nodes, so a reaped Job is not rerun.
const maintenanceTTLSecondsAfterFinished int32 = 86400

// maintenanceOutcome is what one Job run leaves the pass in. The zero value is
// the pending one, which is what an error path should count as: a Job whose
// state could not be established has not finished.
type maintenanceOutcome int

const (
	maintenanceJobPending maintenanceOutcome = iota
	maintenanceJobDone
	maintenanceJobFailed
)

// maintenanceJobName names the Job of one kind for one node. The node name is
// hashed rather than embedded: it is a DNS subdomain of up to 253 characters and
// a Job name has 63.
//
// The longest kind renders "{name}-chassis-del-{8 hex}", 21 characters on top of
// metadata.name. That is the budget MaxOVNChassisNameLength bounds the CR name
// by, and the webhook enforces.
func maintenanceJobName(cr *ovnv1alpha1.OVNChassis, kind, node string) string {
	sum := sha256.Sum256([]byte(node))
	return cr.Name + "-" + kind + "-" + hex.EncodeToString(sum[:])[:maintenanceNameHashLength]
}

// maintenanceJob builds one maintenance Job. The three kinds differ in the
// script they run, the values they are handed and where they may land; the
// hardening, the retry posture and the shared scripts mount are the same for
// all of them and live here.
//
// A pinned nodeName bypasses the scheduler, so the apply Job runs on the node
// whose local database it writes rather than wherever there is room. The
// tolerations come from the CR either way: a NoExecute taint evicts a pinned pod
// that does not tolerate it, and the two unpinned Jobs need a node that admits
// them at all on a cluster whose networking nodes are tainted.
func maintenanceJob(cr *ovnv1alpha1.OVNChassis, name, script string, env []corev1.EnvVar,
	extraVolumes []corev1.Volume, extraMounts []corev1.VolumeMount, nodeName string, hostNetwork bool,
) *batchv1.Job {
	labels := naming.ComponentLabels(chassisAppName, cr.Name, componentMaintenance)

	mounts := append([]corev1.VolumeMount{
		{Name: scriptsVolumeName, MountPath: chassisScriptDir},
		{Name: tmpVolumeName, MountPath: "/tmp"},
	}, extraMounts...)
	volumes := append([]corev1.Volume{
		chassisScriptsVolume(cr),
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}, extraVolumes...)

	podSpec := corev1.PodSpec{
		// Never rather than OnFailure: backoffLimit 0 makes the first failure
		// terminal, and a restarting pod would keep the Job active past the point
		// the failure is worth reporting.
		RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		NodeName:    nodeName,
		Tolerations: cr.Spec.Tolerations,
		Containers: []corev1.Container{{
			Name:  componentMaintenance,
			Image: effectiveImage(cr.Spec.Image).Reference(),
			// The unprivileged posture is enough for all three: the apply Job
			// reaches the local database through /run/openvswitch/db.sock, which
			// host-prepare.sh hands to uid 42424, and the other two speak to a
			// central database over TLS.
			SecurityContext: deployment.RestrictedSecurityContext(),
			Command:         []string{"/bin/bash", path.Join(chassisScriptDir, script)},
			Env:             env,
			VolumeMounts:    mounts,
		}},
		Volumes: volumes,
	}
	if hostNetwork {
		podSpec.HostNetwork = true
		podSpec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			// A retry would run against the same unreachable database and produce
			// the same failure, while the step kept reporting a run in flight. The
			// operator reruns the Job itself once its inputs change, which is what
			// the rerun key gates.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(maintenanceActiveDeadlineSeconds),
			TTLSecondsAfterFinished: ptr.To(maintenanceTTLSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
}

// applyJob builds the Job that re-applies a node's rendered values. It is the
// same script the ovn-controller pod's init container runs, in the same posture:
// pinned to the node and in the node's own network namespace, so what it writes
// describes the machine it writes on.
func applyJob(cr *ovnv1alpha1.OVNChassis, central resolvedCentral, node string) *batchv1.Job {
	return maintenanceJob(cr, maintenanceJobName(cr, maintenanceKindApply, node), applyNodeScriptKey,
		// The downward-API fieldRefs resolve on the pinned node, so the Job is
		// handed the same environment the DaemonSet pod on that node has.
		chassisEnv(cr, central),
		[]corev1.Volume{
			{Name: runOVSVolumeName, VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: ovsRunDir,
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			}},
			{Name: nodesVolumeName, VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: chassisNodesName(cr)},
				},
			}},
		},
		[]corev1.VolumeMount{
			{Name: runOVSVolumeName, MountPath: ovsRunDir},
			{Name: nodesVolumeName, MountPath: chassisNodesDir},
		},
		node, true)
}

// evacuateJob builds the Job that moves the gateway duties off a node. It edits
// the logical model, so it talks to the Northbound database.
func evacuateJob(cr *ovnv1alpha1.OVNChassis, central resolvedCentral, node string, entry nodeEntry) *batchv1.Job {
	return centralClientJob(cr, central, maintenanceKindEvacuate, evacuateScriptKey, node,
		corev1.EnvVar{Name: "NB_ADDR", Value: central.nbAddress}, entry.systemID)
}

// chassisDelJob builds the Job that deletes the Southbound Chassis row of a node
// that left. It addresses the database rather than the relay, for the reason
// chassisDelScript names.
func chassisDelJob(cr *ovnv1alpha1.OVNChassis, central resolvedCentral, node string, entry nodeEntry) *batchv1.Job {
	return centralClientJob(cr, central, maintenanceKindChassisDel, chassisDelScriptKey, node,
		corev1.EnvVar{Name: "SB_ADDR", Value: central.sbAddress}, entry.systemID)
}

// centralClientJob is the shape the two Jobs that talk to a central database
// share: no node pin and no host network, because neither touches the node, and
// the client keypair the central publishes mounted read-only. The chassis is
// named by its system-id, which is the only handle a database has on it once the
// node itself is gone.
func centralClientJob(cr *ovnv1alpha1.OVNChassis, central resolvedCentral, kind, script, node string,
	address corev1.EnvVar, systemID string,
) *batchv1.Job {
	return maintenanceJob(cr, maintenanceJobName(cr, kind, node), script,
		[]corev1.EnvVar{address, {Name: "CHASSIS", Value: systemID}},
		[]corev1.Volume{{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: central.clientSecretName},
		}}},
		[]corev1.VolumeMount{{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true}},
		"", false)
}

// reconcileMaintenance moves every node through whatever step of its lifecycle
// is due: re-applying values that changed, giving up the gateway role, and
// leaving.
//
// statusBefore is the status as the pass read it, before the node step rewrote
// status.nodes. It is what tells a change from a steady state: the rendered
// entry says what a node should be running, and the recorded entry says what it
// was last known to run.
//
// Nodes are walked in name order, and a node whose Job is still running does not
// hold the others up: one slow apply must not keep another node's chassis from
// being deregistered. A failure does stop the walk, because it needs an operator
// rather than another Job.
func (r *OVNChassisReconciler) reconcileMaintenance(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, statusBefore *ovnv1alpha1.OVNChassisStatus,
	central resolvedCentral, nodes renderedNodes,
) (ctrl.Result, error) {
	previous := make(map[string]ovnv1alpha1.OVNChassisNodeStatus, len(statusBefore.Nodes))
	for _, node := range statusBefore.Nodes {
		previous[node.Name] = node
	}
	current := make(map[string]*ovnv1alpha1.OVNChassisNodeStatus, len(cr.Status.Nodes))
	for i := range cr.Status.Nodes {
		current[cr.Status.Nodes[i].Name] = &cr.Status.Nodes[i]
	}

	var pending, deferred, deregistered []string

	for _, name := range slices.Sorted(maps.Keys(nodes)) {
		entry := nodes[name]
		prev := previous[name]
		// The node step rebuilt status.nodes from this very map, so an entry is
		// always there. The lookup is guarded anyway: a missing one would be a
		// nil dereference that takes the operator down rather than a node it
		// skips this pass.
		status := current[name]
		if status == nil {
			continue
		}

		if entry.leaving {
			if central.sbAddress == "" {
				deferred = append(deferred, name)
				continue
			}
			outcome, err := r.runMaintenanceJob(ctx, children, cr, maintenanceKindChassisDel, name,
				chassisDelJob(cr, central, name, entry), entry.systemID+":leave")
			switch {
			case err != nil, outcome == maintenanceJobFailed:
				return ctrl.Result{}, err
			case outcome == maintenanceJobPending:
				pending = append(pending, name)
			default:
				deregistered = append(deregistered, name)
			}
			continue
		}

		// A node that took the gateway role back is no longer drained, whatever an
		// earlier evacuation found.
		if entry.gateway {
			status.GatewayEvacuated = false
		}

		// The role a node announces is what it last applied, not what is rendered
		// for it, and it stops announcing the gateway role only once the
		// evacuation has landed. The node step stamps status.gateway from the
		// rendered entry, so the flip is held back here until then: released
		// earlier, the trigger below would be gone by the next pass and the
		// gateway bindings would stay on a node that no longer carries the role.
		if !entry.gateway && prev.Gateway && !prev.GatewayEvacuated {
			status.Gateway = true
		}

		hash := entry.hash()
		applyPending := false
		// An empty recorded hash is a node seen for the first time. Its values are
		// applied by the init container of its own ovn-controller pod, so there is
		// nothing for a Job to do.
		if prev.ConfigHash != "" && prev.ConfigHash != hash {
			outcome, err := r.runMaintenanceJob(ctx, children, cr, maintenanceKindApply, name,
				applyJob(cr, central, name), hash)
			switch {
			case err != nil, outcome == maintenanceJobFailed:
				return ctrl.Result{}, err
			case outcome == maintenanceJobPending:
				// The recorded hash stays at the old value, so the drift survives
				// this pass and the Job is still due on the next one.
				applyPending = true
				pending = append(pending, name)
			default:
				status.ConfigHash = hash
			}
		}

		// The apply runs first: the chassis has to stop announcing itself as a
		// gateway before the bindings are taken off it, or the model would hand
		// them back while the node still claims the role.
		if applyPending || entry.gateway || !prev.Gateway || prev.GatewayEvacuated {
			continue
		}
		if central.nbAddress == "" {
			deferred = append(deferred, name)
			continue
		}
		outcome, err := r.runMaintenanceJob(ctx, children, cr, maintenanceKindEvacuate, name,
			evacuateJob(cr, central, name, entry), entry.systemID+":gateway-off:"+hash)
		switch {
		case err != nil, outcome == maintenanceJobFailed:
			return ctrl.Result{}, err
		case outcome == maintenanceJobPending:
			pending = append(pending, name)
		default:
			status.GatewayEvacuated = true
			status.Gateway = false
		}
	}

	if len(deregistered) > 0 {
		if err := r.dropDeregisteredNodes(ctx, children, cr, nodes, deregistered); err != nil {
			chassisSkeleton.MarkFailed(cr, conditionTypeMaintenanceReady, conditionReasonMaintenanceError, err)
			return ctrl.Result{}, err
		}
	}

	return maintenanceCondition(cr, pending, deferred), nil
}

// maintenanceCondition reports what the pass left outstanding.
//
// A deferred node outranks a running one: a Job that runs finishes on its own,
// while a node waiting for an address the OVNCentral has not published needs
// that other CR to move. Both poll at the same interval, so the ranking decides
// what the message says rather than when the next pass happens.
func maintenanceCondition(cr *ovnv1alpha1.OVNChassis, pending, deferred []string) ctrl.Result {
	switch {
	case len(deferred) > 0:
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeMaintenanceReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonMaintenanceDeferred,
			Message: fmt.Sprintf("Waiting for OVNCentral %s to publish the address the maintenance of "+
				"%s needs", cr.Spec.CentralRef.Name, strings.Join(deferred, ", ")),
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}
	case len(pending) > 0:
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeMaintenanceReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonMaintenanceRunning,
			Message: fmt.Sprintf("Maintenance Jobs are running for %d nodes: %s",
				len(pending), strings.Join(pending, ", ")),
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}
	default:
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeMaintenanceReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonMaintenanceIdle,
			Message:            "No node has maintenance outstanding",
		})
		return ctrl.Result{}
	}
}

// runMaintenanceJob runs one maintenance Job and classifies what came of it.
//
// A permanently failed Job is reported rather than returned: rerunning it under
// an unchanged key produces the same failure, so a returned error would only
// put the CR on the workqueue's backoff and hot-loop a pass that cannot help.
// The Warning that dates the failure rides the shared terminal-state helper,
// which dedupes on the Job UID through an annotation on the CR, so it fires once
// per Job rather than once per pass. That annotation lands on the CR, which is
// why the patch goes through the embedded client rather than through children.
func (r *OVNChassisReconciler) runMaintenanceJob(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, kind, node string, jobObj *batchv1.Job, rerunKey string,
) (maintenanceOutcome, error) {
	done, observed, err := job.RunJobWithRerunKey(ctx, children, r.Scheme, cr, jobObj, rerunKey)

	failure := fmt.Sprintf("The %s Job %s for node %s failed; inspect its pod logs",
		kind, jobObj.Name, node)
	job.RecordJobTerminalState(ctx, r.Client, r.Recorder, cr, componentMaintenance, observed,
		eventReasonMaintenanceDeferred,
		func(result string, _ time.Duration) {
			if result == "failed" {
				r.Recorder.Event(cr, corev1.EventTypeWarning, eventReasonMaintenanceJobFailed, failure)
			}
		})

	switch {
	case errors.Is(err, job.ErrJobFailed):
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeMaintenanceReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonMaintenanceJobFailed,
			Message:            failure,
		})
		return maintenanceJobFailed, nil
	case err != nil:
		err = fmt.Errorf("running %s Job %s: %w", kind, jobObj.Name, err)
		chassisSkeleton.MarkFailed(cr, conditionTypeMaintenanceReady, conditionReasonMaintenanceError, err)
		return maintenanceJobPending, err
	case done:
		return maintenanceJobDone, nil
	default:
		return maintenanceJobPending, nil
	}
}

// dropDeregisteredNodes forgets the nodes whose chassis-deletion Job succeeded:
// their key in the nodes ConfigMap and their entry in status.nodes go together.
//
// The ConfigMap is re-applied without those keys rather than patched, so the
// shared field manager keeps owning exactly the keys it asserts and Server-Side
// Apply removes the rest. A key left behind would have the node step render the
// node as leaving again on the next pass and schedule a second deletion for a
// chassis that no longer exists.
func (r *OVNChassisReconciler) dropDeregisteredNodes(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, nodes renderedNodes, deregistered []string,
) error {
	remaining := make(renderedNodes, len(nodes))
	maps.Copy(remaining, nodes)
	for _, name := range deregistered {
		delete(remaining, name)
	}

	cm := nodesConfigMap(cr, remaining)
	if err := apply.EnsureObject(ctx, children, r.Scheme, cr, cm, apply.FieldManager); err != nil {
		return fmt.Errorf("dropping %s from the %s ConfigMap: %w",
			strings.Join(deregistered, ", "), cm.Name, err)
	}

	cr.Status.Nodes = slices.DeleteFunc(cr.Status.Nodes, func(node ovnv1alpha1.OVNChassisNodeStatus) bool {
		return slices.Contains(deregistered, node.Name)
	})
	return nil
}
