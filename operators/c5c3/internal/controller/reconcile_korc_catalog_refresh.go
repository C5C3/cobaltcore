// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The K-ORC Deployment reconcileKORCCatalogRefresh restarts, and the pod-template
// annotation it records a ControlPlane's catalog epoch under. The name and
// namespace are the ones the k-orc Flux Kustomization and hack/ci-deploy-korc.sh
// install K-ORC with.
const (
	korcDeploymentNamespace          = "orc-system"
	korcDeploymentName               = "orc-controller-manager"
	korcCatalogEpochAnnotationPrefix = "c5c3.io/korc-catalog-epoch-"
)

// korcCatalogEpochAnnotationKey returns the pod-template annotation key one
// ControlPlane records its epoch under: the prefix plus the first 10 hex
// characters of sha256(cp.Namespace + "/" + cp.Name). One key per ControlPlane
// lets several ControlPlanes share one K-ORC without overwriting each other.
func korcCatalogEpochAnnotationKey(cp *c5c3v1alpha1.ControlPlane) string {
	sum := sha256.Sum256([]byte(cp.Namespace + "/" + cp.Name))
	return korcCatalogEpochAnnotationPrefix + hex.EncodeToString(sum[:])[:10]
}

// korcCatalogEpoch returns the epoch of the catalog registered through cp and
// whether every counted registration is settled. epoch is "" when no
// registration declares a catalog entry.
//
// A registration counts when it declares a catalog entry, is not Terminating and
// comes from a namespace cp admits. One from any other namespace projects
// nothing and stays CatalogReady=False for good, so counting it would hold every
// later restart of the plane. A counted registration is settled when its
// CatalogReady condition is True at its current generation. The generation
// guard keeps an edited endpoint URL from changing the epoch before Keystone has
// the new row. The epoch is the first 16 hex
// characters of a SHA-256 over cp's UID followed by, per counted registration in
// namespace/name order, a newline and the NUL-joined fields "<namespace>/<name>",
// the service type, the service name (the CR name when unset) and one
// "<interface>=<url>" per endpoint in interface order. The UID is part of it
// because a re-created ControlPlane of the same name logs into K-ORC with a new
// credential before its services are registered, so it needs a restart although
// its registrations are byte-identical to the old ones.
//
// The byte stream is a contract with later operator versions: changing it changes
// every epoch, and the first pass after the upgrade restarts K-ORC once per
// ControlPlane.
func korcCatalogEpoch(cp *c5c3v1alpha1.ControlPlane, registrations []c5c3v1alpha1.KeystoneService) (epoch string, settled bool) {
	var counted []*c5c3v1alpha1.KeystoneService
	for i := range registrations {
		ks := &registrations[i]
		if ks.Spec.Catalog == nil || !ks.DeletionTimestamp.IsZero() || !keystoneServiceNamespaceAllowed(cp, ks) {
			continue
		}
		ready := conditions.GetCondition(ks.Status.Conditions, conditionTypeKeystoneServiceCatalogReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != ks.Generation {
			return "", false
		}
		counted = append(counted, ks)
	}
	if len(counted) == 0 {
		return "", true
	}

	slices.SortFunc(counted, func(a, b *c5c3v1alpha1.KeystoneService) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	var stream strings.Builder
	stream.WriteString(string(cp.UID))
	for _, ks := range counted {
		catalog := ks.Spec.Catalog
		fields := []string{ks.Namespace + "/" + ks.Name, catalog.ServiceType, cmp.Or(catalog.ServiceName, ks.Name)}
		endpoints := slices.Clone(catalog.Endpoints)
		slices.SortFunc(endpoints, func(a, b c5c3v1alpha1.KeystoneServiceEndpointSpec) int {
			return cmp.Compare(a.Interface, b.Interface)
		})
		for _, endpoint := range endpoints {
			fields = append(fields, string(endpoint.Interface)+"="+endpoint.URL)
		}
		stream.WriteString("\n")
		stream.WriteString(strings.Join(fields, "\x00"))
	}
	sum := sha256.Sum256([]byte(stream.String()))
	return hex.EncodeToString(sum[:])[:16], true
}

// reconcileKORCCatalogRefresh restarts K-ORC once the catalog registered through
// cp has settled on a value K-ORC has not seen. K-ORC keeps one provider client
// per clouds.yaml cloud for half the token lifetime, and that client carries the
// service catalog of the token it logged in with, so a service registered after
// the ControlPlane's first K-ORC login stays missing from it until the cache
// expires or the process restarts (k-orc/openstack-resource-controller#941).
//
// The member records the catalog epoch in a pod-template annotation of the K-ORC
// Deployment. A changed pod template is what `kubectl rollout restart` writes, so
// the Deployment controller replaces the pod and the new process logs in against
// the current catalog. The annotation is the only state: an unchanged epoch
// writes nothing. The patch goes out under the shared field manager, which the
// k-orc Flux Kustomization does not revert, so no restart loop forms.
//
// It is gated on ServiceAccountsReady, which the member before it wrote in the
// same pass. That is what makes a bring-up cost one restart: the built-in
// registrations are projected one after another, and without the gate each one
// turning Ready would settle the list again. A missing or forbidden Deployment,
// and a patch admission rejects (400 from a webhook, 422 from a
// ValidatingAdmissionPolicy), is a Warning event and not an error: the outcome
// is a catalog that expires on its own, and an error would park the whole
// ControlPlane in a permanent backoff, since every retry meets the same refusal.
//
// The mechanism is a workaround that #1202 removes once upstream evicts the stale
// client.
func (r *ControlPlaneReconciler) reconcileKORCCatalogRefresh(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (ctrl.Result, error) {
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeServiceAccountsReady) {
		return ctrl.Result{}, nil
	}

	var registrations c5c3v1alpha1.KeystoneServiceList
	if err := r.List(ctx, &registrations, client.MatchingFields{
		KeystoneServiceControlPlaneRefIndexKey: keystoneServiceControlPlaneRefIndexValue(cp.Namespace, cp.Name),
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing KeystoneServices for the K-ORC catalog refresh: %w", err)
	}
	// No requeue while unsettled. A projected child turning Ready wakes the plane
	// through the Owns leg and its labelled twin. A standalone registration's
	// status change does not (its leg drops status-only writes), so its epoch is
	// read on the plane's next pass.
	epoch, settled := korcCatalogEpoch(cp, registrations.Items)
	if !settled || epoch == "" {
		return ctrl.Result{}, nil
	}

	// Read uncached: the cached client would start a cluster-wide Deployment
	// informer for this one object.
	dep := &appsv1.Deployment{}
	if err := r.apiReader().Get(ctx, client.ObjectKey{
		Namespace: korcDeploymentNamespace, Name: korcDeploymentName,
	}, dep); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			r.recordKORCRestartSkipped(cp, err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading Deployment %s/%s for the K-ORC catalog refresh: %w",
			korcDeploymentNamespace, korcDeploymentName, err)
	}

	key := korcCatalogEpochAnnotationKey(cp)
	value := cp.Namespace + "/" + cp.Name + "=" + epoch
	if dep.Spec.Template.Annotations[key] == value {
		return ctrl.Result{}, nil
	}

	orig := dep.DeepCopy()
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[key] = value
	if err := r.Patch(ctx, dep, client.MergeFrom(orig), client.FieldOwner(apply.FieldManager)); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) ||
			apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
			r.recordKORCRestartSkipped(cp, err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("restarting Deployment %s/%s for the K-ORC catalog refresh: %w",
			korcDeploymentNamespace, korcDeploymentName, err)
	}

	message := fmt.Sprintf("restarted Deployment %s/%s: the service catalog registered through ControlPlane %s/%s changed",
		korcDeploymentNamespace, korcDeploymentName, cp.Namespace, cp.Name)
	log.FromContext(ctx).Info(message)
	if r.Recorder != nil {
		r.Recorder.Event(cp, "Normal", "KORCRestarted", message)
	}
	return ctrl.Result{}, nil
}

// recordKORCRestartSkipped emits the Warning for a K-ORC Deployment the
// reconciler cannot read or patch, naming the cause and what it leaves behind.
func (r *ControlPlaneReconciler) recordKORCRestartSkipped(cp *c5c3v1alpha1.ControlPlane, cause error) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(cp, "Warning", "KORCRestartSkipped", fmt.Sprintf(
		"Deployment %s/%s cannot be restarted (%v); K-ORC keeps its cached service catalog for up to half a token lifetime",
		korcDeploymentNamespace, korcDeploymentName, cause))
}
