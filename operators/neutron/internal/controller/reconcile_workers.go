// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// conditionTypeWorkersReady is the condition the worker step reports under. It
// is separate from DeploymentReady because the API pods and the workers fail
// independently: an API that serves reads while the maintenance worker is down
// is a different state from one that serves nothing.
const conditionTypeWorkersReady = "WorkersReady"

// Condition reasons for WorkersReady.
const (
	conditionReasonWorkersReady      = "WorkersReady"
	conditionReasonWaitingForWorkers = "WaitingForWorkers"
)

// The component label values of the two worker Deployments, which are also their
// container names and the suffixes of their object names.
const (
	componentPeriodicWorkers      = "periodic-workers"
	componentOVNMaintenanceWorker = "ovn-maintenance-worker"
)

// neutronWorkloadEnv returns the environment every Neutron process is started
// with: the two oslo.config overrides that deliver the database URL and the
// transport URL from their derived Secrets, so neither credential is written to
// the rendered config. The API container adds its own variables on top.
func neutronWorkloadEnv(neutron *neutronv1alpha1.Neutron) []corev1.EnvVar {
	return []corev1.EnvVar{
		database.ConnectionEnvVar(neutron.Name),
		messaging.TransportURLEnvVar(neutron.Name),
	}
}

// workerSelectorLabels returns the pod selector of one worker Deployment: the
// shared selector labels narrowed by the component. Each of the three
// Deployments a Neutron owns selects on its own component value, so none of them
// counts or adopts another's pods.
func workerSelectorLabels(neutron *neutronv1alpha1.Neutron, component string) map[string]string {
	labels := naming.SelectorLabels(neutronAppName, neutron.Name)
	labels[naming.LabelKeyComponent] = component
	return labels
}

// reconcileWorkers ensures the two worker Deployments that run the neutron
// processes serving no HTTP: the periodic workers, which run the recurring
// maintenance tasks of the ML2 plugin, and the OVN maintenance worker, which
// reconciles the Northbound model against the Neutron database.
//
// There is deliberately no third Deployment for neutron-rpc-server. Nothing in
// this deployment consumes RPC: OVN answers DHCP and metadata out of the logical
// model, no agent registers with the API, and the rendered config sets both RPC
// worker counts to zero to match.
//
// Neither Deployment gets a Service, an HPA or a PodDisruptionBudget: no client
// dials them, they scale on the maintenance load rather than on request rate,
// and an eviction costs a delayed maintenance pass rather than a failed request.
//
// The four digests are the API Deployment's, stamped for the same reason: both
// worker processes read the database and the broker through env-injected
// credentials and mount the OVN client identity, so a rotation has to roll them
// too.
func (r *NeutronReconciler) reconcileWorkers(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, configMapName, dsnDigest, authtokenDigest, transportDigest, ovnClientDigest string,
) (ctrl.Result, error) {
	workers := []struct {
		component string
		command   []string
	}{
		{componentPeriodicWorkers, neutronCommand("neutron-periodic-workers")},
		{componentOVNMaintenanceWorker, neutronCommand("neutron-ovn-maintenance-worker")},
	}

	allReady := true
	for _, worker := range workers {
		deploy := buildWorkerDeployment(neutron, worker.component, worker.command,
			configMapName, dsnDigest, authtokenDigest, transportDigest, ovnClientDigest)
		ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, neutron, deploy)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring %s Deployment: %w", deploy.Name, err)
		}
		allReady = allReady && ready
	}

	if !allReady {
		conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
			Type:               conditionTypeWorkersReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: neutron.Generation,
			Reason:             conditionReasonWaitingForWorkers,
			Message:            "Waiting for the periodic-workers and ovn-maintenance-worker Deployments to become available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	conditions.SetCondition(&neutron.Status.Conditions, metav1.Condition{
		Type:               conditionTypeWorkersReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: neutron.Generation,
		Reason:             conditionReasonWorkersReady,
		Message:            "The periodic-workers and ovn-maintenance-worker Deployments are available",
	})
	return ctrl.Result{}, nil
}

// buildWorkerDeployment constructs one worker Deployment. It carries the same
// config, OVN identity, state directory and TLS projections as the API pods,
// because both processes read the same two files. It differs from them in what
// it leaves out: no ports, no probes, no autoscaling. Both workers are singleton
// consumers of a work queue rather than request servers, so a readiness gate
// would report nothing a client acts on, and an HPA has no request rate to scale
// against.
func buildWorkerDeployment(neutron *neutronv1alpha1.Neutron, component string, command []string,
	configMapName, dsnDigest, authtokenDigest, transportDigest, ovnClientDigest string,
) *appsv1.Deployment {
	volumes, mounts := neutronWorkloadVolumes(neutron, configMapName)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      neutron.Namespace,
		Name:           neutron.Name + "-" + component,
		Labels:         componentLabels(neutron, component),
		SelectorLabels: workerSelectorLabels(neutron, component),
		PodAnnotations: neutronPodAnnotations(dsnDigest, authtokenDigest, transportDigest, ovnClientDigest),
		Deployment:     &neutron.Spec.Workers.Deployment,
		// The worker replica count is spec.workers.deployment.replicas alone: no
		// HorizontalPodAutoscaler targets these Deployments, so nothing else owns
		// the field.
		Autoscaling: nil,
		Container: deployment.ContainerParams{
			Name:         component,
			Image:        neutron.Spec.Image.Reference(),
			Command:      command,
			Env:          neutronWorkloadEnv(neutron),
			VolumeMounts: mounts,
		},
		Volumes: volumes,
	})
}
