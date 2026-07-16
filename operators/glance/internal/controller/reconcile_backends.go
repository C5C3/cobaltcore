// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	"github.com/c5c3/forge/internal/common/secrets"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// Aggregated BackendsReady vocabulary. The glance-side sub-reconciler owns this
// condition on the Glance CR; the per-backend CredentialsReady / ConfigProjected
// conditions stay owned by the dedicated GlanceBackend controller.
const (
	// conditionTypeBackendsReady is the aggregated Glance condition this
	// sub-reconciler drives.
	conditionTypeBackendsReady = "BackendsReady"
	// conditionReasonNoDefaultBackend is set when the exactly-one-default
	// invariant is violated (zero or more than one attached, credential-ready
	// backend marked isDefault). The projection is invalid and last-good is
	// retained.
	conditionReasonNoDefaultBackend = "NoDefaultBackend"
	// conditionReasonWaitingForBackends is set while at least one attached
	// backend is pending (not yet credential-ready, or skipped for a per-backend
	// fault) but a valid default exists — the ready subset is still projected.
	conditionReasonWaitingForBackends = "WaitingForBackends"
	// conditionReasonAllBackendsProjected is set when every attached backend is
	// credential-ready and projected, with a valid default.
	conditionReasonAllBackendsProjected = "AllBackendsProjected"
)

// backendsProjection is the result the glance-side backend sub-reconciler hands
// downstream (the config and networkpolicy steps). A zero value (valid=false)
// means the exactly-one-default invariant is unmet, in which case the config
// step keeps the last-good artefacts the live Deployment mounts.
type backendsProjection struct {
	// valid reports whether exactly one attached, credential-ready backend is
	// marked isDefault. Only a valid projection is rendered/reprojected;
	// otherwise last-good is retained downstream.
	valid bool
	// enabledBackends is the [DEFAULT] enabled_backends value —
	// "<name>:s3,<name2>:s3" over the credential-ready, rendered backends in
	// deterministic (sorted-name) order.
	enabledBackends string
	// defaultBackend is the single credential-ready default's name, rendered as
	// [glance_store] default_backend.
	defaultBackend string
	// secretName is the content-hashed backends Secret currently valid (the
	// projection Secret holding backends.conf).
	secretName string
	// hosts is the s3 host URL of ALL attached (non-deleting) backends — not
	// only the credential-ready ones — for the networkpolicy egress step.
	hosts []string
}

// reconcileBackends aggregates the attached, credential-ready GlanceBackends
// into the content-hashed backends Secret (one [<name>] store section per
// backend) and sets the aggregated BackendsReady condition on the Glance CR. It
// returns the projection (zero-valued when the exactly-one-default rule is
// unmet) for the downstream config/networkpolicy steps.
//
// It gates each backend on its CredentialsReady==True condition rather than the
// aggregate Ready: Ready also requires ConfigProjected, which only turns True
// AFTER this step projects the backend, so gating on Ready would deadlock —
// exactly the identity-backend D-gate rationale.
//
// CONTRACT: this step never returns a requeue and never returns an error for
// waiting states (a missing default, pending backends, missing credential
// Secrets, control-char values) — the GlanceBackend watch wakes the parent when
// a backend's status flips. Only genuine infrastructure failures (List/create/
// prune errors) surface as errors.
func (r *GlanceReconciler) reconcileBackends(ctx context.Context, glance *glancev1alpha1.Glance) (ctrl.Result, backendsProjection, error) {
	logger := log.FromContext(ctx)

	var backends glancev1alpha1.GlanceBackendList
	if err := r.List(
		ctx, &backends,
		client.InNamespace(glance.Namespace),
		client.MatchingFields{GlanceBackendGlanceRefIndexKey: glance.Name},
	); err != nil {
		return ctrl.Result{}, backendsProjection{}, fmt.Errorf("listing GlanceBackends for %s: %w", glance.Name, err)
	}

	// Sort by name so the rendered Secret content — and therefore its
	// content-hashed name and enabled_backends order — is deterministic across
	// passes.
	sort.Slice(backends.Items, func(i, j int) bool {
		return backends.Items[i].Name < backends.Items[j].Name
	})

	// First pass: collect the egress hosts of every attached backend and the
	// credential-ready default candidates. Both must be known before deciding
	// projection validity.
	var hosts []string
	var defaultCandidates []string
	for i := range backends.Items {
		backend := &backends.Items[i]
		// A deleting backend is de-projected immediately: its store section is
		// dropped and its host removed from the egress set on this pass.
		if backend.DeletionTimestamp != nil {
			continue
		}
		if backend.Spec.S3 != nil && backend.Spec.S3.Host != "" {
			hosts = append(hosts, backend.Spec.S3.Host)
		}
		if backend.Spec.IsDefault && credentialsReady(backend) {
			defaultCandidates = append(defaultCandidates, backend.Name)
		}
	}

	// Exactly-one-default rule. Zero or more than one credential-ready default is
	// an invalid projection: BackendsReady=False / NoDefaultBackend, nothing is
	// re-rendered, and last-good is retained by the downstream config step (which
	// keeps whatever the live Deployment mounts). hosts is still surfaced so the
	// networkpolicy step tracks every attached store.
	if len(defaultCandidates) != 1 {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackendsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonNoDefaultBackend,
			Message:            noDefaultBackendMessage(defaultCandidates),
		})
		return ctrl.Result{}, backendsProjection{valid: false, hosts: hosts}, nil
	}
	defaultBackend := defaultCandidates[0]

	// Second pass: render the store section of every credential-ready backend,
	// isolating per-backend faults. A backend that is not yet credential-ready,
	// whose credentials Secret is missing/unreadable, or whose credential values
	// carry a control character is added to the pending set (and warned, for a
	// fault) rather than failing the step.
	sections := map[string]map[string]string{}
	var renderedNames []string
	var pending []string
	for i := range backends.Items {
		backend := &backends.Items[i]
		if backend.DeletionTimestamp != nil {
			continue
		}
		if !credentialsReady(backend) {
			pending = append(pending, fmt.Sprintf("%s (credentials not ready)", backend.Name))
			continue
		}

		section, err := r.renderStoreSection(ctx, glance.Namespace, backend)
		if err != nil {
			// A missing/unreadable credentials Secret or a value carrying a
			// control character is a per-backend fault: skip the backend, warn
			// loudly, and keep projecting the healthy siblings. A transient
			// (non-NotFound) client failure is infrastructure, not a per-backend
			// fault, so it surfaces as an error and the workqueue backs off.
			if secrets.IsMissingSecretOrKey(err) || errors.Is(err, errControlCharInValue) {
				msg := fmt.Sprintf("Skipping backend %s: %v", backend.Name, err)
				logger.Info(msg)
				r.Recorder.Event(glance, corev1.EventTypeWarning, "GlanceBackendSkipped", msg)
				pending = append(pending, fmt.Sprintf("%s (%v)", backend.Name, err))
				continue
			}
			return ctrl.Result{}, backendsProjection{}, fmt.Errorf("rendering store section for backend %s: %w", backend.Name, err)
		}
		sections[backend.Name] = section
		renderedNames = append(renderedNames, backend.Name)
	}

	// enabled_backends lists the rendered stores as "<name>:s3" in sorted order
	// (renderedNames follows the sorted backend list).
	enabled := make([]string, 0, len(renderedNames))
	for _, name := range renderedNames {
		enabled = append(enabled, name+":s3")
	}

	secretName, err := config.CreateImmutableSecret(ctx, r.Client, r.Scheme, glance,
		glance.Name+"-backends", glance.Namespace,
		map[string][]byte{backendsConfDataKey: []byte(config.RenderINI(sections))})
	if err != nil {
		return ctrl.Result{}, backendsProjection{}, fmt.Errorf("creating backends Secret: %w", err)
	}
	if err := config.PruneImmutableSecrets(ctx, r.Client, glance, config.PruneOptions{
		BaseName:    glance.Name + "-backends",
		Namespace:   glance.Namespace,
		CurrentName: secretName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		return ctrl.Result{}, backendsProjection{}, fmt.Errorf("pruning backends Secrets: %w", err)
	}

	projection := backendsProjection{
		valid:           true,
		enabledBackends: strings.Join(enabled, ","),
		defaultBackend:  defaultBackend,
		secretName:      secretName,
		hosts:           hosts,
	}

	if len(pending) > 0 {
		conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackendsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: glance.Generation,
			Reason:             conditionReasonWaitingForBackends,
			Message:            "Waiting for backends: " + strings.Join(pending, "; "),
		})
		return ctrl.Result{}, projection, nil
	}

	conditions.SetCondition(&glance.Status.Conditions, metav1.Condition{
		Type:               conditionTypeBackendsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: glance.Generation,
		Reason:             conditionReasonAllBackendsProjected,
		Message:            "All attached backends are projected",
	})
	return ctrl.Result{}, projection, nil
}

// credentialsReady reports whether a backend's CredentialsReady condition is
// present and True — the D-gate the projection uses. It deliberately does NOT
// consult the aggregate Ready (which also requires ConfigProjected, only True
// after this step projects the backend), so gating here never deadlocks.
func credentialsReady(backend *glancev1alpha1.GlanceBackend) bool {
	cond := conditions.GetCondition(backend.Status.Conditions, conditionTypeCredentialsReady)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// errControlCharInValue is returned by renderStoreSection when an assembled
// [<name>] option name or value carries a newline or carriage-return.
// config.RenderINI writes both verbatim as `key = value`, so such a character
// injects arbitrary INI lines. The webhook rejects CR-set option keys and
// values up front, but the renderer revalidates as the last line of defense: it
// is the only gate that sees the Secret-sourced access/secret keys (which the
// webhook never reads) and the only gate that still runs when a CR bypassed
// admission. A poisoned option is a per-backend fault, so the caller skips and
// warns rather than failing the whole step.
var errControlCharInValue = errors.New("[glance_store] option name or value contains a newline or carriage-return character")

// renderStoreSection renders one credential-ready backend's [<name>] store
// section. It reads the S3 access/secret keys from the credentials Secret (the
// contract data keys), sets the operator-owned s3_store_* options, then merges
// the backend's extraOptions WITHOUT overriding an operator key (operator keys
// win on collision — the webhook denylist normally guarantees disjointness, this
// is the fail-closed backstop for a bypassed webhook). Only user-set optional
// fields are rendered so upstream Glance defaults apply otherwise.
func (r *GlanceReconciler) renderStoreSection(ctx context.Context, namespace string, backend *glancev1alpha1.GlanceBackend) (map[string]string, error) {
	s3 := backend.Spec.S3
	if s3 == nil {
		// The schema union rule guarantees spec.s3 for a type-S3 backend; a
		// bypassed admission leaves nothing to render.
		return nil, fmt.Errorf("backend %s has type %s but no s3 block", backend.Name, backend.Spec.Type)
	}

	credKey := client.ObjectKey{Namespace: namespace, Name: s3.CredentialsSecretRef.Name}
	accessKey, err := secrets.GetSecretValue(ctx, r.Client, credKey, glancev1alpha1.S3AccessKeyIDKey)
	if err != nil {
		return nil, err
	}
	secretKey, err := secrets.GetSecretValue(ctx, r.Client, credKey, glancev1alpha1.S3SecretAccessKeyKey)
	if err != nil {
		return nil, err
	}

	section := map[string]string{
		"s3_store_host":              s3.Host,
		"s3_store_bucket":            s3.Bucket,
		"s3_store_access_key":        accessKey,
		"s3_store_secret_key":        secretKey,
		"s3_store_bucket_url_format": effectiveBucketURLFormat(s3),
	}
	if s3.Region != "" {
		section["s3_store_region_name"] = s3.Region
	}
	// Rendered only when true so an unset flag falls back to the upstream Glance
	// default rather than an explicit "false" override.
	if s3.CreateBucketOnPut {
		section["s3_store_create_bucket_on_put"] = "true"
	}
	if s3.LargeObjectSize != nil {
		section["s3_store_large_object_size"] = fmt.Sprintf("%d", *s3.LargeObjectSize)
	}
	if s3.LargeObjectChunkSize != nil {
		section["s3_store_large_object_chunk_size"] = fmt.Sprintf("%d", *s3.LargeObjectChunkSize)
	}

	// extraOptions merged without clobbering an operator key: operator keys win
	// on collision.
	for k, v := range backend.Spec.ExtraOptions {
		if _, exists := section[k]; exists {
			continue
		}
		section[k] = v
	}

	// Last line of defense against INI injection: config.RenderINI writes every
	// option verbatim, so a newline in any key OR value injects arbitrary
	// [<name>] options. The webhook rejects CR-set keys/values, but the S3
	// access/secret keys come from a Secret it never reads, and a CRD-bypass CR
	// reaches here unvalidated. Fail the render (the caller skips and warns).
	for k, v := range section {
		if strings.ContainsAny(k, "\n\r") || strings.ContainsAny(v, "\n\r") {
			return nil, errControlCharInValue
		}
	}
	return section, nil
}

// effectiveBucketURLFormat returns the s3_store_bucket_url_format value,
// defaulting to "path" when spec.s3.bucketURLFormat is empty (a CR that bypassed
// the defaulting webhook; the CRD also defaults it to "path").
func effectiveBucketURLFormat(s3 *glancev1alpha1.S3BackendSpec) string {
	if s3.BucketURLFormat != "" {
		return s3.BucketURLFormat
	}
	return "path"
}

// noDefaultBackendMessage builds the BackendsReady=False message for the
// exactly-one-default violation, naming the colliding defaults when there is
// more than one.
func noDefaultBackendMessage(defaults []string) string {
	if len(defaults) == 0 {
		return "No attached, credential-ready backend is marked isDefault; exactly one is required"
	}
	return fmt.Sprintf("More than one attached, credential-ready backend is marked isDefault (%s); exactly one is required",
		strings.Join(defaults, ", "))
}
