// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/satellite"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// CinderBackendCinderRefIndexKey is the field-indexer key under which
// CinderBackend CRs are indexed by spec.cinderRef.name. Used by the cinder-side
// sub-reconciler (list the backends attached to one Cinder) and by the watch
// mapper that fans a Cinder event out to its backends.
const CinderBackendCinderRefIndexKey = "spec.cinderRef.name"

// conditionTypeCredentialsReady is the per-backend condition the dedicated
// CinderBackend and CinderBackupBackend controllers own. The cinder-side
// projection reads it as its gate and never writes it.
const conditionTypeCredentialsReady = "CredentialsReady"

// Aggregated BackendsReady vocabulary. The cinder-side sub-reconciler owns this
// condition on the Cinder CR; the per-backend conditions stay owned by the
// dedicated CinderBackend controller.
const (
	// conditionTypeBackendsReady is the aggregated Cinder condition this
	// sub-reconciler drives.
	conditionTypeBackendsReady = "BackendsReady"
	// conditionReasonAllBackendsProjected is set when every attached backend is
	// credential-ready and projected.
	conditionReasonAllBackendsProjected = "AllBackendsProjected"
	// conditionReasonWaitingForBackends is set while at least one attached
	// backend is pending (not yet credential-ready, or skipped for a per-backend
	// fault). The ready subset is still projected.
	conditionReasonWaitingForBackends = "WaitingForBackends"
	// conditionReasonNoBackends is set when no CinderBackend is attached at all.
	// It is True rather than False: a Cinder without volume backends serves its
	// API and its scheduler, and attaching storage is a separate act.
	conditionReasonNoBackends = "NoBackends"
)

// defaultConfigMapRetainCount is the number of historical immutable
// ConfigMaps/Secrets to retain after pruning. Combined with the current active
// artefact, this allows rollback to 3 previous configurations.
const defaultConfigMapRetainCount = 3

// The data keys of one backend's projection Secret. The three files are mounted
// by that backend's cinder-volume Deployment: the backend section it drives, the
// NFS export list the driver reads its share from, and the [DEFAULT] overlay
// that names the single backend this process serves.
const (
	backendConfDataKey   = "backend.conf"
	sharesDataKey        = "shares"
	volumeOverlayDataKey = "volume.conf"
)

// nfsMountPointBase is the in-pod directory a cinder-volume mounts its export
// under. The volumes it serves are files inside it, so it has to be the same
// path the workload step mounts the backend's scratch volume at.
const nfsMountPointBase = "/var/lib/cinder/mnt"

// nfsEgressPort is the TCP port an NFSv4 mount connects to. The projection
// reports one egress host per backend so the networkpolicy step opens the
// exports the volume services mount, and nothing else.
const nfsEgressPort = 2049

// backendProjection is what the backend sub-reconciler hands downstream (the
// deployment and networkpolicy steps): one entry per projected CinderBackend,
// sorted by name.
type backendProjection struct {
	// name is the CinderBackend's name, which is also its cinder.conf section
	// and its volume_backend_name.
	name string
	// server and path are the NFS export the backend's cinder-volume mounts.
	server string
	path   string
	// mountOptions is the option string the export is mounted with.
	mountOptions string
	// secretName is the content-hashed Secret carrying this backend's three
	// projected files.
	secretName string
}

// backendSecretBaseName returns the base name of one backend's projection
// Secret. Each backend gets its own base name — rather than one Secret for all
// of them — because each is mounted by its own cinder-volume Deployment, so a
// change to one backend must not roll the pods of the others.
func backendSecretBaseName(cinder *cinderv1alpha1.Cinder, name string) string {
	return cinder.Name + "-backend-" + name
}

// reconcileBackends projects every attached, credential-ready CinderBackend into
// its own content-hashed Secret and sets the aggregated BackendsReady condition
// on the Cinder CR. It returns the projections the deployment step turns into
// cinder-volume Deployments and the egress hosts the networkpolicy step opens.
//
// It gates each backend on its CredentialsReady==True condition rather than the
// aggregate Ready: Ready also requires the backend's own projection, which only
// turns True AFTER this step projects it, so gating on Ready would deadlock.
//
// There is no default backend to elect: a volume type selects its backend by
// name, so the backends a Cinder serves are peers and an empty set is a valid
// state rather than a violated invariant.
//
// CONTRACT: this step never returns a requeue and never returns an error for
// waiting states (pending backends, control-char values) — the CinderBackend
// watch wakes the parent when a backend's status flips. Only genuine
// infrastructure failures (List/create/prune errors) surface as errors.
func (r *CinderReconciler) reconcileBackends(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, []backendProjection, []string, error) {
	logger := log.FromContext(ctx)

	// The attached backends are sibling configuration CRs on the management
	// cluster, so this list goes through the embedded client and its field
	// index; only the Secrets they render into are written on children.
	var backends cinderv1alpha1.CinderBackendList
	if err := r.List(
		ctx, &backends,
		client.InNamespace(cinder.Namespace),
		client.MatchingFields{CinderBackendCinderRefIndexKey: cinder.Name},
	); err != nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf("listing CinderBackends for %s: %w", cinder.Name, err)
	}

	items := make([]*cinderv1alpha1.CinderBackend, 0, len(backends.Items))
	for i := range backends.Items {
		items = append(items, &backends.Items[i])
	}
	// Drop the deleting backends and sort by name, so the rendered Secrets — and
	// therefore their content-hashed names — are deterministic across passes.
	c := satellite.Collect(satellite.CollectParams[cinderv1alpha1.CinderBackend, *cinderv1alpha1.CinderBackend]{
		Items: items,
		Gate:  func(b *cinderv1alpha1.CinderBackend) bool { return credentialsReady(b.Status.Conditions) },
	})

	// Render every credential-ready backend, isolating per-backend faults: a
	// backend whose rendered section carries a control character is added to the
	// pending set and warned about rather than failing the step and taking its
	// healthy siblings down with it.
	var projections []backendProjection
	var hosts []string
	var pending []string
	for _, backend := range c.Attached {
		if !credentialsReady(backend.Status.Conditions) {
			pending = append(pending, backend.Name)
			continue
		}
		nfs := backend.Spec.NFS
		if nfs == nil {
			// The schema union rule guarantees spec.nfs for a type-NFS backend; a
			// bypassed admission leaves nothing to render.
			r.skipBackend(ctx, cinder, backend.Name,
				fmt.Sprintf("backend %s has type %s but no nfs block", backend.Name, backend.Spec.Type))
			pending = append(pending, backend.Name)
			continue
		}

		section := renderBackendSection(cinder, backend)
		if err := config.CheckNoControlChars(backend.Name, section); err != nil {
			r.skipBackend(ctx, cinder, backend.Name, err.Error())
			pending = append(pending, backend.Name)
			continue
		}

		// The export is the one rendered value the section does not carry: the
		// shares file is assembled from server and path directly, so it needs a
		// pass of its own. A newline in either would add a second export line to
		// the file the driver reads, naming a share the operator never mounted or
		// derived an egress rule for.
		shares := nfs.Server + ":" + nfs.Path
		if err := config.CheckNoControlChars(backend.Name, map[string]string{sharesDataKey: shares}); err != nil {
			r.skipBackend(ctx, cinder, backend.Name, err.Error())
			pending = append(pending, backend.Name)
			continue
		}

		baseName := backendSecretBaseName(cinder, backend.Name)
		secretName, err := config.CreateImmutableSecret(ctx, children, r.Scheme, cinder,
			baseName, cinder.Namespace, map[string][]byte{
				backendConfDataKey:   []byte(config.RenderINI(map[string]map[string]string{backend.Name: section})),
				sharesDataKey:        []byte(shares + "\n"),
				volumeOverlayDataKey: []byte("[DEFAULT]\nenabled_backends = " + backend.Name + "\n"),
			})
		if err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("creating backend secret for %q: %w", backend.Name, err)
		}
		if err := config.PruneImmutableSecrets(ctx, children, r.Scheme, cinder, config.PruneOptions{
			BaseName:    baseName,
			Namespace:   cinder.Namespace,
			CurrentName: secretName,
			Retain:      defaultConfigMapRetainCount,
		}); err != nil {
			return ctrl.Result{}, nil, nil, fmt.Errorf("pruning backend Secrets for %q: %w", backend.Name, err)
		}

		projections = append(projections, backendProjection{
			name:         backend.Name,
			server:       nfs.Server,
			path:         nfs.Path,
			mountOptions: effectiveMountOptions(nfs.MountOptions),
			secretName:   secretName,
		})
		hosts = append(hosts, fmt.Sprintf("tcp://%s:%d", nfs.Server, nfsEgressPort))
		logger.V(1).Info("projected CinderBackend", "backend", backend.Name, "secret", secretName)
	}

	// A backend that was detached, renamed or skipped keeps no Deployment, so its
	// base name is swept whole rather than retained: nothing mounts the Secrets
	// under it anymore, and each of them carries an export this Cinder no longer
	// serves.
	projected := make(map[string]struct{}, len(projections))
	for _, projection := range projections {
		projected[projection.name] = struct{}{}
	}
	if err := r.pruneStaleSatelliteSecrets(ctx, children, cinder,
		backendSecretBaseName(cinder, ""), projected); err != nil {
		return ctrl.Result{}, nil, nil, err
	}

	switch {
	case len(c.Attached) == 0:
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackendsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonNoBackends,
			Message:            "No CinderBackend is attached; volume services are not rendered",
		})
	case len(pending) > 0:
		// A pending backend keeps the condition False even while its siblings are
		// projected: the ready subset runs, and the message names what does not.
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackendsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonWaitingForBackends,
			Message:            "Waiting for backends: " + strings.Join(pending, ", "),
		})
	default:
		names := make([]string, 0, len(projections))
		for _, projection := range projections {
			names = append(names, projection.name)
		}
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackendsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonAllBackendsProjected,
			Message: fmt.Sprintf("All %d attached backends are projected: %s",
				len(names), strings.Join(names, ", ")),
		})
	}
	return ctrl.Result{}, projections, hosts, nil
}

// skipBackend logs and warns about a per-backend fault. The event goes to the
// Cinder rather than to the backend, mirroring glance: the fault is what keeps
// the parent's BackendsReady False, so it belongs where an operator reads that
// condition.
func (r *CinderReconciler) skipBackend(ctx context.Context, cinder *cinderv1alpha1.Cinder, name, reason string) {
	msg := fmt.Sprintf("Skipping backend %s: %s", name, reason)
	log.FromContext(ctx).Info(msg)
	r.Recorder.Event(cinder, corev1.EventTypeWarning, "CinderBackendSkipped", msg)
}

// credentialsReady reports whether a satellite's CredentialsReady condition is
// present and True — the gate both projections use. It deliberately does NOT
// consult the aggregate Ready (which also requires the projection this step
// performs), so gating here never deadlocks. It takes the condition slice rather
// than the object so the CinderBackend and the CinderBackupBackend share it.
func credentialsReady(conds []metav1.Condition) bool {
	cond := conditions.GetCondition(conds, conditionTypeCredentialsReady)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// effectiveMountOptions returns the mount option string an export is mounted
// with, falling back to DefaultNFSMountOptions when the CR leaves it empty (a CR
// that bypassed the CRD default). An empty value would render an empty
// nfs_mount_options, which mounts the export with the kernel defaults: a hard
// mount that blocks the service on an unreachable server.
func effectiveMountOptions(mountOptions string) string {
	if mountOptions != "" {
		return mountOptions
	}
	return cinderv1alpha1.DefaultNFSMountOptions
}

// renderBackendSection renders one backend's [<name>] section: the NFS driver
// wiring, the optional image-volume cache bounds, and the backend's extraOptions
// merged WITHOUT overriding an operator key (operator keys win on collision —
// the webhook denylist normally guarantees disjointness, this is the fail-closed
// backstop for a bypassed webhook).
//
// The caller has checked that spec.nfs is set.
func renderBackendSection(cinder *cinderv1alpha1.Cinder, backend *cinderv1alpha1.CinderBackend) map[string]string {
	nfs := backend.Spec.NFS
	section := map[string]string{
		"volume_driver":       "cinder.volume.drivers.nfs.NfsDriver",
		"volume_backend_name": backend.Name,
		// The host identity every volume this backend owns is keyed by. It is the
		// Cinder's name rather than the backend's: cinder appends "@<backend>"
		// itself, so the rendered value is the half the operator owns.
		"backend_host":                cinder.Name,
		"nfs_shares_config":           "/etc/cinder/backends.conf.d/" + backend.Name + ".shares",
		"nfs_mount_point_base":        nfsMountPointBase,
		"nfs_mount_options":           effectiveMountOptions(nfs.MountOptions),
		"nas_secure_file_operations":  "true",
		"nas_secure_file_permissions": "true",
		"nfs_snapshot_support":        "false",
		"nfs_sparsed_volumes":         "true",
		"nfs_qcow2_volumes":           "false",
	}

	if cache := backend.Spec.ImageVolumeCache; cache != nil && cache.Enabled {
		section["image_volume_cache_enabled"] = "true"
		if cache.MaxSizeGB != nil {
			section["image_volume_cache_max_size_gb"] = fmt.Sprintf("%d", *cache.MaxSizeGB)
		}
		if cache.MaxCount != nil {
			section["image_volume_cache_max_count"] = fmt.Sprintf("%d", *cache.MaxCount)
		}
	}

	for k, v := range backend.Spec.ExtraOptions {
		if _, exists := section[k]; exists {
			continue
		}
		section[k] = v
	}
	return section
}

// pruneStaleSatelliteSecrets removes every historical Secret of a satellite that
// is no longer projected: the Secrets whose config-base label starts with prefix
// and whose satellite name is absent from projected. The current base names are
// pruned by the projection itself (it keeps a rollback history); a base name
// nothing projects anymore keeps nothing, because the export details it carries
// belong to a backend the Cinder no longer serves.
func (r *CinderReconciler) pruneStaleSatelliteSecrets(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder, prefix string, projected map[string]struct{},
) error {
	var list corev1.SecretList
	if err := children.List(ctx, &list, client.InNamespace(cinder.Namespace),
		client.HasLabels{config.ConfigBaseLabelKey}); err != nil {
		return fmt.Errorf("listing projection Secrets in namespace %s: %w", cinder.Namespace, err)
	}

	stale := map[string]struct{}{}
	for i := range list.Items {
		baseName := list.Items[i].Labels[config.ConfigBaseLabelKey]
		if !strings.HasPrefix(baseName, prefix) {
			continue
		}
		if _, ok := projected[strings.TrimPrefix(baseName, prefix)]; ok {
			continue
		}
		stale[baseName] = struct{}{}
	}

	// Sorted so a pass deletes in the same order it did last time, which keeps
	// the emitted events and logs comparable across passes.
	baseNames := make([]string, 0, len(stale))
	for baseName := range stale {
		baseNames = append(baseNames, baseName)
	}
	sort.Strings(baseNames)

	for _, baseName := range baseNames {
		if err := config.PruneImmutableSecrets(ctx, children, r.Scheme, cinder, config.PruneOptions{
			BaseName:    baseName,
			Namespace:   cinder.Namespace,
			CurrentName: "",
			Retain:      0,
		}); err != nil {
			return fmt.Errorf("pruning stale Secrets for %q: %w", baseName, err)
		}
	}
	return nil
}
