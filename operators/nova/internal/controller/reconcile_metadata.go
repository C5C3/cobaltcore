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
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition reason constants for MetadataReady.
const (
	conditionReasonMetadataReady      = "MetadataReady"
	conditionReasonWaitingForMetadata = "WaitingForMetadata"
)

// componentMetadata is the app.kubernetes.io/component value of the metadata
// pods, and the name suffix of their Deployment and Service.
const componentMetadata = "metadata"

// metadataName returns the name of the metadata Deployment and of the Service
// in front of it. The Neutron metadata agent of every compute node dials that
// Service, so the name is part of the deployment's contract with Neutron.
func metadataName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentMetadata
}

// reconcileMetadata projects the nova-metadata-api Deployment and the Service
// in front of it, and sets the MetadataReady condition.
//
// It runs one global metadata front end rather than one per cell: the API
// database holds the instance mappings, so the front end resolves an instance
// without being deployed beside the cell that holds it.
//
// Outside an upgrade it returns a zero result whatever the readiness is: the API
// step runs after it and must not be skipped over a metadata API that is still
// starting, and the controller's Owns(Deployment) watch re-enqueues the CR when
// the rollout progresses. During the RollingUpdate phase it requeues until the
// rollout has converged, so the contract phase cannot run against a front end
// still on the old image.
func (r *NovaReconciler) reconcileMetadata(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, art configArtifacts, digests workloadDigests,
) (ctrl.Result, error) {
	deploy := buildMetadataDeployment(nova, art, digests)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, nova, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring metadata Deployment: %w", err)
	}

	svc := buildMetadataService(nova)
	if err := deployment.EnsureService(ctx, children, r.Scheme, nova, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring metadata Service: %w", err)
	}

	if nova.Status.UpgradePhase == commonv1.UpgradePhaseRollingUpdate &&
		!novaDeploymentRolledOut(deploy) {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "MetadataReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForMetadata,
			Message:            "Waiting for the upgraded image to finish rolling out on the metadata API",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	if !ready {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               "MetadataReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonWaitingForMetadata,
			Message:            "Nova metadata deployment is not yet available",
		})
		return ctrl.Result{}, nil
	}

	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               "MetadataReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonMetadataReady,
		Message:            "Nova metadata deployment is available",
	})
	return ctrl.Result{}, nil
}

// buildMetadataDeployment constructs the desired nova-metadata-api Deployment:
// the second WSGI application under uWSGI, reading the shared config plus the
// overlay that makes it trust a proxied instance identity.
//
// It takes no autoscaling and no PodDisruptionBudget. The request rate is set by
// the instance population rather than by users, and a budget would only protect
// a front end whose clients (the metadata agent of each compute node) retry.
func buildMetadataDeployment(nova *novav1alpha1.Nova, art configArtifacts,
	digests workloadDigests,
) *appsv1.Deployment {
	volumes, mounts := novaWorkloadVolumes(nova, art, roleMetadata)
	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      nova.Namespace,
		Name:           metadataName(nova),
		Labels:         componentLabels(nova, componentMetadata),
		SelectorLabels: componentSelectorLabels(nova, componentMetadata),
		PodAnnotations: novaMetadataPodAnnotations(nova, digests),
		Deployment:     &nova.Spec.Metadata.Deployment,
		Autoscaling:    nil,
		Container: deployment.ContainerParams{
			Name:    "nova-metadata",
			Image:   nova.Spec.Image.Reference(),
			Command: novaUWSGICommand(nova.Spec.Metadata.UWSGI, novaMetadataPort, novaMetadataWSGIModule),
			Env:     novaWorkloadEnv(nova, roleMetadata),
			Ports: []corev1.ContainerPort{{
				Name:          "nova-metadata",
				ContainerPort: novaMetadataPort,
			}},
			StartupProbe: novaUWSGIStartupProbe(novaMetadataPort),
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        novaUWSGIProbeHandler(novaMetadataPort),
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        novaUWSGIProbeHandler(novaMetadataPort),
				InitialDelaySeconds: 10,
				PeriodSeconds:       15,
				TimeoutSeconds:      10,
				FailureThreshold:    3,
			},
			VolumeMounts: mounts,
		},
		Volumes: volumes,
	})
}

// buildMetadataService builds the Service the Neutron metadata agents dial,
// selecting the metadata component so only the metadata pods can become its
// endpoints.
func buildMetadataService(nova *novav1alpha1.Nova) *corev1.Service {
	return deployment.BuildService(nova.Namespace, metadataName(nova), commonLabels(nova),
		componentSelectorLabels(nova, componentMetadata), novaMetadataPort, novaMetadataPort)
}
