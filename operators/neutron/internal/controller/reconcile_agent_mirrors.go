// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// eventReasonMetadataSharedSecretMirrorReaped is the Normal event recorded for
// each metadata shared-secret copy an agent's teardown deletes.
const eventReasonMetadataSharedSecretMirrorReaped = "MetadataSharedSecretMirrorReaped"

// reapMetadataSharedSecretMirrors deletes the metadata shared-secret copies the
// ControlPlane wrote into this agent's namespace on its target cluster, once no
// other agent there names them. The ControlPlane writes a copy for every
// cluster an agent of its plane names it on, and never deletes one: the agent
// that asked for the copy is the one that knows when nobody needs it anymore.
//
// A copy is recognized by neutronv1alpha1.MetadataSharedSecretMirrorLabel; a
// Secret without it is somebody else's and is left alone. A copy is in use
// while an agent in the same namespace, on the same cluster, and not being
// deleted names it in spec.novaMetadata.sharedSecretRef. Every other labelled
// Secret goes, which also reaps a copy an agent stopped naming when it was
// re-pointed. Each deletion records an event.
//
// The Secrets are listed through the target's uncached reader, so a copy the
// ControlPlane wrote moments ago is not missed and no informer on Secrets is
// started on the target for a one-off read.
func (r *NeutronMetadataAgentReconciler) reapMetadataSharedSecretMirrors(ctx context.Context,
	children client.Client, cr *neutronv1alpha1.NeutronMetadataAgent,
) error {
	ns := cr.Namespace

	var mirrors corev1.SecretList
	if err := commonmulticluster.LiveReader(children).List(ctx, &mirrors, client.InNamespace(ns),
		client.MatchingLabels{neutronv1alpha1.MetadataSharedSecretMirrorLabel: "true"}); err != nil {
		return fmt.Errorf("listing metadata shared-secret mirrors in namespace %q: %w", ns, err)
	}
	if len(mirrors.Items) == 0 {
		return nil
	}

	var agents neutronv1alpha1.NeutronMetadataAgentList
	if err := r.List(ctx, &agents, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("listing NeutronMetadataAgents in namespace %q: %w", ns, err)
	}
	inUse := map[string]bool{}
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.Name == cr.Name || !agent.DeletionTimestamp.IsZero() ||
			!sameTargetCluster(agent.Spec.TargetClusterRef, cr.Spec.TargetClusterRef) {
			continue
		}
		if ref := agentSharedSecretRef(agent); ref != nil && ref.Name != "" {
			inUse[ref.Name] = true
		}
	}

	for i := range mirrors.Items {
		mirror := &mirrors.Items[i]
		if inUse[mirror.Name] {
			continue
		}
		if err := children.Delete(ctx, mirror); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("deleting metadata shared-secret mirror %s/%s: %w", ns, mirror.Name, err)
		}
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonMetadataSharedSecretMirrorReaped,
			"Deleted the metadata shared-secret mirror %s: no other NeutronMetadataAgent on this cluster names it",
			mirror.Name)
	}
	return nil
}
