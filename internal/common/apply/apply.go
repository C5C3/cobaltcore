// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/multicluster"
)

// FieldManager is the Server-Side Apply field manager used by EnsureObject. It
// is a stable, descriptive name so the operator's field ownership is tracked
// consistently and conflicts (between two managers of the same field) surface
// reliably.
const FieldManager = "forge-operator"

// EnsureObject creates or updates obj using Server-Side Apply under fieldManager
// and sets owner as the controller reference so obj is garbage-collected when
// owner is deleted.
//
// Under SSA the field manager owns only the fields the builder actually sets, so
// server-defaulted fields the builder omits (e.g. a +kubebuilder:default value
// on an omitempty field) are never claimed and therefore never overwritten —
// repeated applies of an unchanged desired object are no-ops and converge,
// unlike a whole-object Update that rewrites the spec on every pass.
//
// The apply is wrapped in retry.RetryOnConflict so a benign FieldManagerConflict
// is absorbed as an internal retry rather than surfacing as transient condition
// noise. On success obj is overwritten with the server response, so callers may
// read fresh status (e.g. readiness) from obj without an extra Get.
//
// A client marked by multicluster.Remote writes to a target cluster, where a
// reference to owner names a UID that cluster cannot resolve. Such a child is
// applied with no owner reference at all and carries the ownership labels
// instead, and a live object those labels do not already name is refused rather
// than adopted. Every other client takes the local path, unchanged.
func EnsureObject[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, obj T, fieldManager string) error {
	if multicluster.IsRemote(c) {
		return ensureRemoteObject(ctx, c, scheme, owner, obj, fieldManager)
	}
	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return fmt.Errorf("setting owner reference on %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return EnsureUnownedObject(ctx, c, scheme, obj, fieldManager)
}

// ensureRemoteObject is EnsureObject against a target cluster: obj is claimed by
// the ownership labels rather than by a controller owner reference and applied
// unowned, because on that cluster a reference to owner names an absent UID and
// leaves the child one kind registration away from being collected as an orphan.
//
// A same-named object somebody else provisioned is refused instead of adopted.
// Adopting it would be doubly destructive: the apply overwrites its spec, and
// the labels stamped on it get it deleted at teardown. The refusal is
// multicluster.Claim's own, taken on the LIVE object, because that is where the
// UID and the labels answering "is this ours?" sit. The desired object, which no
// API server has ever seen, cannot answer for them. Claiming the live object is
// the check and nothing more: it is thrown away afterwards, and obj is what gets
// applied.
//
// The pre-check reads through multicluster.LiveReader, that is, through the
// target cluster's own uncached API reader. Reading through c instead would
// decide adoption from a cache that may still trail the cluster it describes,
// and would pull every kind this function touches into an informer on the
// target — including the kinds nothing else there reads.
//
// One limit the code cannot show: Server-Side Apply sheds the fields the shared
// field manager owned and no longer asserts. A remote child written before
// ownership moved to the labels therefore loses its dangling controller owner
// reference on the first pass after that move, without anyone rewriting it.
func ensureRemoteObject[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, obj T, fieldManager string) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return fmt.Errorf("resolving GVK for %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	// The live state is read into a fresh instance and not into a copy of obj: a
	// typed Get unmarshals the response into whatever it is handed, so every
	// field the live object omits (its labels, the very thing the ownership check
	// reads) would survive from the desired object and be taken for live state.
	fresh, err := scheme.New(gvk)
	if err != nil {
		return fmt.Errorf("building an empty %s to read the live %s/%s into: %w",
			gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
	}
	live := fresh.(client.Object)

	switch err := multicluster.LiveReader(c).Get(ctx, client.ObjectKeyFromObject(obj), live); {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		// Nothing to adopt: the object is absent, or its kind is not installed on
		// the target cluster at all. The apply reports the second case itself.
	case err != nil:
		return fmt.Errorf("checking for a pre-existing %s %s/%s before adopting it: %w",
			gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
	default:
		if err := multicluster.Claim(c, scheme, owner, live); err != nil {
			return err
		}
	}

	// Both Claim errors are returned as they are. Each already names the object
	// it failed on, and prefixing a refusal that spells out what it refuses and
	// why would only say the same thing twice.
	if err := multicluster.Claim(c, scheme, owner, obj); err != nil {
		return err
	}
	return EnsureUnownedObject(ctx, c, scheme, obj, fieldManager)
}

// EnsureUnownedObject creates or updates obj using Server-Side Apply under
// fieldManager, setting NO owner reference. It is EnsureObject's apply core, and
// the two share it so their SSA behavior (GVK stamping, metadata stripping,
// conflict retry, response decode) cannot drift.
//
// Use it for a child in a DIFFERENT namespace than its owner: Kubernetes rejects
// a cross-namespace controller owner reference — garbage collection only cascades
// within one namespace — so such a child cannot be owned, and EnsureObject would
// fail before the apply. Ownership and cleanup of an unowned child are the
// caller's responsibility: stamp it with ownership labels the controller can
// resolve, and delete it explicitly (a finalizer-driven teardown), because
// nothing garbage-collects it when the owner goes.
func EnsureUnownedObject[T client.Object](ctx context.Context, c client.Client, scheme *runtime.Scheme, obj T, fieldManager string) error {
	// Server-Side Apply requires apiVersion/kind in the request body, but
	// objects built in-code carry an empty TypeMeta, so resolve and stamp the
	// GVK before converting to the unstructured apply configuration.
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return fmt.Errorf("resolving GVK for %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("converting %s %s/%s to unstructured: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(gvk)
	// Strip server-managed metadata and the status subresource from the apply
	// body: they are not part of the desired state the operator owns, and a
	// resourceVersion would impose optimistic concurrency that ForceOwnership is
	// meant to avoid.
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(u.Object, "status")

	ac := client.ApplyConfigurationFromUnstructured(u)
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		return c.Apply(ctx, ac, client.FieldOwner(fieldManager), client.ForceOwnership)
	}); err != nil {
		return fmt.Errorf("applying %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	// The apply response is written back into u; decode it into obj so callers
	// observe server-fresh state (status, defaulted fields) without a re-Get.
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj); err != nil {
		return fmt.Errorf("decoding applied %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}
