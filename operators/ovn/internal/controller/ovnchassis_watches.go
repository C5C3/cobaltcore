// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Watch event mappers for the OVNChassis reconciler's client-Secret legs. Kept
// beside the controller so its file stays focused on the reconcile chain while
// the Secret-to-chassis plumbing lives here, mirroring the Neutron operator's
// neutron_watches.go.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// clientSecretToChassisMapper maps an event on a Secret to a reconcile request
// for every OVNChassis attached to an OVNCentral that publishes the Secret as its
// client identity (status.clientSecretName). It is what makes a copy on another
// cluster follow a cert-manager renewal at watch latency rather than at the next
// periodic pass.
//
// The centrals are listed in the Secret's own namespace, since a central
// publishes its client identity beside itself, and the chassis of each one are
// resolved through the OVNChassisCentralRefIndexKey index in that namespace,
// since spec.centralRef is namespace-local.
//
// A List failure is logged and returns what was collected, per the
// handler.MapFunc contract; the step's own polling is the fallback.
func clientSecretToChassisMapper(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			// Both legs watch Secrets alone, so this cannot happen; mapping an
			// object of another kind by its name would wake the chassis of the
			// central whose Secret happens to share it.
			return nil
		}

		var centrals ovnv1alpha1.OVNCentralList
		if err := c.List(ctx, &centrals, client.InNamespace(secret.Namespace)); err != nil {
			log.FromContext(ctx).Error(err, "listing OVNCentrals for the client Secret watch",
				"secret", client.ObjectKeyFromObject(secret))
			return nil
		}

		var requests []reconcile.Request
		toChassis := centralToChassisMapper(c)
		for i := range centrals.Items {
			central := &centrals.Items[i]
			if central.Status.ClientSecretName != secret.Name {
				continue
			}
			requests = append(requests, toChassis(ctx, central)...)
		}
		return requests
	}
}

// chassisTargetClusters reports the clusters a Secret event may wake the
// OVNChassis at key from: the cluster the chassis projects onto, where its copy
// lives, and the one its OVNCentral projects onto, where the source Secret
// cert-manager renews lives. A chassis on another cluster than its central needs
// both, which is what the single-target gate of AddInputWatch cannot express.
//
// A management cluster on either side contributes no name, since no engaged
// provider cluster is called that, so a chassis and a central both on the
// management cluster answer with an empty list, which keeps no remote event.
//
// The chassis read's error is returned as it comes back, NotFound included:
// RemoteRequestsAmong treats NotFound as the ordinary answer and logs anything
// else. A central that does not exist yet contributes nothing, since there is no
// source Secret to renew; any other error reading it is wrapped with the chassis
// it was read for. Both reads go through the local cache the For leg and the
// central leg already hold.
func chassisTargetClusters(c client.Reader) commonmulticluster.TargetClustersFunc {
	return func(ctx context.Context, key types.NamespacedName) ([]string, error) {
		chassis := &ovnv1alpha1.OVNChassis{}
		if err := c.Get(ctx, key, chassis); err != nil {
			return nil, err
		}

		var names []string
		if chassis.Spec.TargetClusterRef != nil {
			names = append(names, chassis.Spec.TargetClusterRef.Name)
		}

		central := &ovnv1alpha1.OVNCentral{}
		switch err := c.Get(ctx, client.ObjectKey{Namespace: chassis.Namespace, Name: chassis.Spec.CentralRef.Name},
			central); {
		case apierrors.IsNotFound(err):
			return names, nil
		case err != nil:
			return nil, fmt.Errorf("reading the OVNCentral of OVNChassis %s/%s: %w", chassis.Namespace, chassis.Name, err)
		}
		if central.Spec.TargetClusterRef != nil {
			names = append(names, central.Spec.TargetClusterRef.Name)
		}
		return names, nil
	}
}
