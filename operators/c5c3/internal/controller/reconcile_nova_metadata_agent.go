// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// A NeutronMetadataAgent on a compute cluster signs the instance requests it
// proxies with the metadata shared secret, read from a Secret in its own
// namespace on that cluster. The ControlPlane delivers that Secret: it copies
// the shared secret, and nothing else of the compute contract, into the
// namespace of every agent of its plane that names
// "<cp>-nova-metadata-agent-secret", on each cluster such an agent runs on. The
// plane never deletes a copy; the teardown of the last agent there that names
// it does (neutronv1alpha1.MetadataSharedSecretMirrorLabel).

// reasonNovaMetadataAgentSecretError reports a Kubernetes-level failure listing
// the metadata agents, reading the compute contract, or writing a copy.
const reasonNovaMetadataAgentSecretError = "NovaMetadataAgentSecretError" //nolint:gosec // G101 false positive: condition reason name, not a credential.

// novaMetadataAgentSecretName returns the name of the copy a metadata agent on a
// target cluster names in spec.novaMetadata.sharedSecretRef,
// "<cp>-nova-metadata-agent-secret". It differs from every Secret the plane
// writes into the Nova namespace, so the copy may land on the Nova's own cluster
// too.
func novaMetadataAgentSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + "-metadata-agent-secret"
}

// novaMetadataAgentTargets returns the places the metadata shared secret has to
// be delivered to: one per cluster a qualifying NeutronMetadataAgent runs on, in
// the OVN central's namespace, sorted by cluster name.
//
// Every agent answering for this plane's ports is a CR in that namespace: its
// chassisRef is namespace-local, a chassis's centralRef is too, and one
// OVNCentral serves one ControlPlane. An agent qualifies while it is not being
// deleted, names a target cluster, and names novaMetadataAgentSecretName in
// sharedSecretRef. A local agent gets no copy: it carries no finalizer that
// would reap one, and in the Nova namespace it names the generated Secret
// directly. A cluster that does not serve the kind has no agents, so a no-match
// yields no target rather than an error.
func (r *ControlPlaneReconciler) novaMetadataAgentTargets(ctx context.Context,
	cp *c5c3v1alpha1.ControlPlane,
) ([]computeConfigMirrorTarget, error) {
	if cp.Spec.Services.Neutron == nil {
		return nil, nil
	}
	ns := cp.NeutronOVNCentralNamespace()
	var agents neutronv1alpha1.NeutronMetadataAgentList
	if err := r.List(ctx, &agents, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing the NeutronMetadataAgents in namespace %q: %w", ns, err)
	}

	name := novaMetadataAgentSecretName(cp)
	clusters := map[string]*commonv1.TargetClusterRefSpec{}
	for i := range agents.Items {
		agent := &agents.Items[i]
		if !agent.DeletionTimestamp.IsZero() || agent.Spec.TargetClusterRef == nil ||
			agent.Spec.NovaMetadata == nil || agent.Spec.NovaMetadata.SharedSecretRef == nil ||
			agent.Spec.NovaMetadata.SharedSecretRef.Name != name {
			continue
		}
		clusters[agent.Spec.TargetClusterRef.Name] = agent.Spec.TargetClusterRef.DeepCopy()
	}

	targets := make([]computeConfigMirrorTarget, 0, len(clusters))
	for _, cluster := range slices.Sorted(maps.Keys(clusters)) {
		targets = append(targets, computeConfigMirrorTarget{ClusterRef: clusters[cluster], Namespace: ns})
	}
	return targets, nil
}

// reconcileNovaMetadataAgentSecrets delivers the metadata shared secret to every
// target novaMetadataAgentTargets returns. While halt is true the caller returns
// res and err verbatim; every condition is on NovaReady.
//
// The value comes from the in-cluster compute contract the nova operator
// publishes once the Nova child has converged, so every copy has one source
// whichever Secret services.nova.metadataSharedSecretRef names. reconcileNova
// runs it after the Nova child is Ready. A rotated value is rewritten on the
// next Nova pass.
//
// Each copy carries the single key the agent's webhook defaults
// sharedSecretRef.key to, so the bus URL and the service password of the
// contract stay out of the agent's privileged namespace. It carries this
// ControlPlane's ownership labels and MetadataSharedSecretMirrorLabel, and
// ensureUnownedOrOwned refuses a same-named Secret the plane did not write.
func (r *ControlPlaneReconciler) reconcileNovaMetadataAgentSecrets(ctx context.Context,
	cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, bool, error) {
	fail := conditionFailer(cp, conditionTypeNovaReady)

	targets, err := r.novaMetadataAgentTargets(ctx, cp)
	if err != nil {
		fail(reasonNovaMetadataAgentSecretError, err.Error())
		return ctrl.Result{}, true, err
	}
	if len(targets) == 0 {
		return ctrl.Result{}, false, nil
	}

	novaNS := cp.NovaNamespace()
	source, err := r.childrenClientFor(ctx, cp, novaNS)
	if err != nil {
		fail(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	contractName := novaComputeConfigSecretName(cp)
	contract := &corev1.Secret{}
	switch gerr := source.Get(ctx, client.ObjectKey{Namespace: novaNS, Name: contractName}, contract); {
	case apierrors.IsNotFound(gerr):
		fail(reasonWaitingForComputeConfig, fmt.Sprintf(
			"the compute config Secret %s/%s has not been published yet; the metadata agents in namespace %q "+
				"sign with the shared secret it carries", novaNS, contractName, targets[0].Namespace))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	case gerr != nil:
		fail(reasonNovaMetadataAgentSecretError, gerr.Error())
		return ctrl.Result{}, true, fmt.Errorf("reading the compute config Secret %s/%s: %w", novaNS, contractName, gerr)
	}
	value := contract.Data[novav1alpha1.ComputeConfigMetadataSharedSecretKey]
	if len(value) == 0 {
		fail(reasonWaitingForComputeConfig, fmt.Sprintf(
			"the compute config Secret %s/%s carries no %s yet",
			novaNS, contractName, novav1alpha1.ComputeConfigMetadataSharedSecretKey))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	// A target that fails does not hold back the ones sorted after it: the
	// agents on each cluster sign with their own copy. Every copy has the same
	// namespace and name, so each failure names its cluster, and NovaReady lists
	// every one. A failed write fails the pass; clusters that only did not
	// resolve are waited for.
	var failures []string
	var writeErrs []error
	for _, target := range targets {
		where := fmt.Sprintf("delivering the metadata shared secret into namespace %q on cluster %q",
			target.Namespace, clusterNameOf(target.ClusterRef))
		delivery, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, target.ClusterRef)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", where, err))
			continue
		}
		copied := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: novaMetadataAgentSecretName(cp), Namespace: target.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{novaMetadataSecretKey: value},
		}
		stampControlPlaneChildLabels(copied, cp)
		copied.Labels[neutronv1alpha1.MetadataSharedSecretMirrorLabel] = "true"
		if err := r.ensureUnownedOrOwned(ctx, delivery, cp, copied); err != nil {
			writeErr := fmt.Errorf("%s: %w", where, err)
			writeErrs = append(writeErrs, writeErr)
			failures = append(failures, writeErr.Error())
		}
	}
	switch {
	case len(writeErrs) > 0:
		fail(reasonNovaMetadataAgentSecretError, strings.Join(failures, "; "))
		return ctrl.Result{}, true, errors.Join(writeErrs...)
	case len(failures) > 0:
		fail(commonmulticluster.TargetClusterUnavailable, strings.Join(failures, "; "))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}
	return ctrl.Result{}, false, nil
}

// neutronMetadataAgentToControlPlaneMapper maps a NeutronMetadataAgent event
// onto the ControlPlanes whose OVN central lives in the agent's namespace, so an
// agent that starts naming the copy, or the last one on a cluster that leaves,
// changes the delivery targets without waiting for a periodic resync.
//
// Agents are user-authored and carry no ControlPlane owner reference, so a
// plain Owns() would never fire. Only planes that run both the compute and the
// network service deliver a copy, so only they are woken.
func (r *ControlPlaneReconciler) neutronMetadataAgentToControlPlaneMapper(ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	agent, ok := obj.(*neutronv1alpha1.NeutronMetadataAgent)
	if !ok {
		return nil
	}

	var list c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing ControlPlanes for NeutronMetadataAgent event",
			"neutronMetadataAgent", client.ObjectKeyFromObject(agent))
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		cp := &list.Items[i]
		if cp.Spec.Services.Nova != nil && cp.Spec.Services.Neutron != nil &&
			cp.NeutronOVNCentralNamespace() == agent.Namespace {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
		}
	}
	return requests
}

// neutronMetadataAgentDeliveryPredicate admits the NeutronMetadataAgent events
// that change the delivery targets: an agent created, deleted, starting to be
// deleted, or re-pointing spec.novaMetadata.sharedSecretRef.name. targetClusterRef
// is immutable, so no other update moves an agent between clusters, and an
// agent's status writes must not reconcile the whole plane.
func neutronMetadataAgentDeliveryPredicate() predicate.Predicate {
	sharedSecretName := func(obj client.Object) string {
		agent, ok := obj.(*neutronv1alpha1.NeutronMetadataAgent)
		if !ok || agent.Spec.NovaMetadata == nil || agent.Spec.NovaMetadata.SharedSecretRef == nil {
			return ""
		}
		return agent.Spec.NovaMetadata.SharedSecretRef.Name
	}
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero() ||
				sharedSecretName(e.ObjectOld) != sharedSecretName(e.ObjectNew)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
