// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The condition type and reasons of the endpoint step.
const (
	conditionTypeEndpointsReady       = "EndpointsReady"
	conditionReasonEndpointsPublished = "EndpointsPublished"
	conditionReasonEndpointsPending   = "EndpointsPending"
)

// databaseEndpoints holds the two connection strings of one database, each
// listing its members in ordinal order. pending marks a database whose addresses
// cannot be assembled yet, in which case both strings are empty.
type databaseEndpoints struct {
	internal string
	external string
	pending  bool
}

// reconcileEndpoints publishes the addresses clients connect to the two
// databases on: the cluster IP of each member's own Service for clients inside
// the cluster, and — for a database spec.<db>.externallyReachable published on
// node ports — the node the member runs on plus its node port for clients
// outside it.
//
// Both are IP literals rather than DNS names, because ovsdb-server resolves a
// remote once at startup and never again: a name whose address changes leaves
// every client wedged against the old one.
//
// Nothing is published while either database is incomplete. A client is handed
// the member list as one string and dials the members in it, so a list that is
// missing a member (the one that happens to be the leader, say) reads to the
// client as a cluster that cannot serve writes rather than as a cluster whose
// address is still being assembled.
func (r *OVNCentralReconciler) reconcileEndpoints(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	// The Services and pods are read through the target cluster's own uncached
	// reader: an address that a cache has not caught up with yet would be
	// published as absent, and the CR would report the wait until the next pass.
	reader := commonmulticluster.LiveReader(children)
	nbDB, sbDB := northboundDB(cr), southboundDB(cr)

	nb, err := collectDatabaseEndpoints(ctx, reader, cr, nbDB)
	if err != nil {
		markEndpointsFailed(cr, err)
		return ctrl.Result{}, err
	}
	sb, err := collectDatabaseEndpoints(ctx, reader, cr, sbDB)
	if err != nil {
		markEndpointsFailed(cr, err)
		return ctrl.Result{}, err
	}

	if nb.pending || sb.pending {
		clearPublishedAddresses(cr)
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeEndpointsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonEndpointsPending,
			Message: "Waiting for every member Service to be assigned a cluster IP " +
				"and for at least one member of each database to run on a node",
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	nbStatus := databaseStatus(cr, nbDB)
	nbStatus.InternalDbAddress = nb.internal
	nbStatus.DbAddress = nb.external
	sbStatus := databaseStatus(cr, sbDB)
	sbStatus.InternalDbAddress = sb.internal
	sbStatus.DbAddress = sb.external

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeEndpointsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonEndpointsPublished,
		Message:            "Both databases are reachable at the published addresses",
	})
	return ctrl.Result{}, nil
}

// collectDatabaseEndpoints assembles the connection strings of one database from
// its per-member Services and pods. A database that is not published on node
// ports yields the internal string alone; there is no address outside the
// cluster to assemble.
//
// A member whose pod is gone is skipped in the node-facing list rather than
// treated as a wait: a rescheduling member has no node to name, while the
// members beside it are still reachable and their addresses are what a client
// needs. A member whose Service is gone or carries no cluster IP does stop the
// whole database, because that address is the one clients inside the cluster
// have no substitute for.
func collectDatabaseEndpoints(ctx context.Context, reader client.Reader, cr *ovnv1alpha1.OVNCentral, db raftDB) (databaseEndpoints, error) {
	var internal, external []string

	for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
		name := raftMemberName(cr, db, ordinal)
		key := client.ObjectKey{Namespace: cr.Namespace, Name: name}

		var svc corev1.Service
		switch err := reader.Get(ctx, key, &svc); {
		case apierrors.IsNotFound(err):
			return databaseEndpoints{pending: true}, nil
		case err != nil:
			return databaseEndpoints{}, fmt.Errorf("reading endpoint Service %s: %w", name, err)
		}
		if svc.Spec.ClusterIP == "" {
			return databaseEndpoints{pending: true}, nil
		}
		internal = append(internal, fmt.Sprintf("ssl:%s:%d", svc.Spec.ClusterIP, db.clientPort))

		if !db.spec.ExternallyReachable {
			continue
		}

		var pod corev1.Pod
		switch err := reader.Get(ctx, key, &pod); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return databaseEndpoints{}, fmt.Errorf("reading endpoint pod %s: %w", name, err)
		}
		if pod.Status.HostIP != "" {
			external = append(external, fmt.Sprintf("ssl:%s:%d", pod.Status.HostIP, db.base+ordinal))
		}
	}

	// Not one member has a node address although the database is published: it is
	// unreachable from outside the cluster, and an OVNChassis on a node without
	// cluster networking has nothing to connect to.
	if db.spec.ExternallyReachable && len(external) == 0 {
		return databaseEndpoints{pending: true}, nil
	}
	return databaseEndpoints{
		internal: strings.Join(internal, ","),
		external: strings.Join(external, ","),
	}, nil
}

// markEndpointsFailed reports a read that failed under the same condition the
// waiting states use. What a client sees is the same in both cases (no address),
// and a reason of its own would only split the state consumers match on.
func markEndpointsFailed(cr *ovnv1alpha1.OVNCentral, err error) {
	clearPublishedAddresses(cr)
	centralSkeleton.MarkFailed(cr, conditionTypeEndpointsReady, conditionReasonEndpointsPending, err)
}

// clearPublishedAddresses drops every published address. A CR that cannot
// assemble the full picture publishes none of it: a stale address outlives the
// member it named, and a client that dials it waits out its own timeout instead
// of failing over to a member that is up.
func clearPublishedAddresses(cr *ovnv1alpha1.OVNCentral) {
	cr.Status.Northbound.InternalDbAddress = ""
	cr.Status.Northbound.DbAddress = ""
	cr.Status.Southbound.InternalDbAddress = ""
	cr.Status.Southbound.DbAddress = ""
}
