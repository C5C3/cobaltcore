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
	"github.com/c5c3/cobaltcore/internal/common/secrets"
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

// The data keys of one backend's projection Secret. The files are mounted by
// that backend's cinder-volume Deployment: the backend section it drives, the
// [DEFAULT] overlay that names the single backend this process serves, and the
// driver's own files: the NFS export list the driver reads its share from, or
// the Ceph client configuration and keyring of an RBD backend.
const (
	backendConfDataKey   = "backend.conf"
	sharesDataKey        = "shares"
	volumeOverlayDataKey = "volume.conf"
	cephConfDataKey      = "ceph.conf"
	keyringDataKey       = "keyring"
)

// cephConfigDir is the directory librados and the Ceph tools read
// <cluster>.conf from, and where the projected keyring lives beside it.
const cephConfigDir = "/etc/ceph"

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
// sorted by name. name, backendType and secretName are set for every type;
// server, path and mountOptions only for an NFS backend, rbd only for an RBD
// one.
type backendProjection struct {
	// name is the CinderBackend's name, which is also its cinder.conf section
	// and its volume_backend_name.
	name string
	// backendType is the driver the backend was rendered for.
	backendType cinderv1alpha1.CinderBackendType
	// server and path are the NFS export the backend's cinder-volume mounts.
	server string
	path   string
	// mountOptions is the option string the export is mounted with.
	mountOptions string
	// rbd carries what the volume Deployment and the NetworkPolicy need of an
	// RBD backend.
	rbd *rbdProjection
	// secretName is the content-hashed Secret carrying this backend's projected
	// files.
	secretName string
}

// rbdProjection is the RBD half of a backendProjection: the cluster name and the
// user the projected files are named by, and the networks the Ceph egress rule
// opens.
type rbdProjection struct {
	clusterName string
	user        string
	networks    []string
}

// renderedBackend is one backend rendered for projection: the data of its
// projection Secret, the projection without its Secret name, and the egress
// host its volume service mounts from ("" when it mounts nothing).
type renderedBackend struct {
	data       map[string][]byte
	projection backendProjection
	host       string
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
// waiting states (pending backends, control-char values, an RBD key Secret that
// vanished after its gate or carries a key the gate refuses) — the
// CinderBackend watch wakes the parent when a backend's status flips. Only
// genuine infrastructure failures (List, Get, create and prune errors) surface
// as errors.
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
	// backend without the block its type needs, whose rendered files carry a
	// control character, or whose RBD key Secret vanished or lost its cephx key
	// after its gate is added to the pending set and warned about rather than
	// failing the step and taking its healthy siblings down with it.
	var projections []backendProjection
	var hosts []string
	var pending []string
	for _, backend := range c.Attached {
		if !credentialsReady(backend.Status.Conditions) {
			pending = append(pending, backend.Name)
			continue
		}
		var rendered *renderedBackend
		var skip string
		switch backend.Spec.Type {
		case cinderv1alpha1.CinderBackendTypeNFS:
			rendered, skip = renderNFSBackend(cinder, backend)
		case cinderv1alpha1.CinderBackendTypeRBD:
			var err error
			rendered, skip, err = renderRBDBackend(ctx, children, cinder, backend)
			if err != nil {
				return ctrl.Result{}, nil, nil, err
			}
		default:
			skip = fmt.Sprintf("backend %s has type %s, which this operator does not render", backend.Name, backend.Spec.Type)
		}
		if skip != "" {
			r.skipBackend(ctx, cinder, backend.Name, skip)
			pending = append(pending, backend.Name)
			continue
		}

		baseName := backendSecretBaseName(cinder, backend.Name)
		secretName, err := config.CreateImmutableSecret(ctx, children, r.Scheme, cinder,
			baseName, cinder.Namespace, rendered.data)
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

		projection := rendered.projection
		projection.secretName = secretName
		projections = append(projections, projection)
		if rendered.host != "" {
			hosts = append(hosts, rendered.host)
		}
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

// renderNFSBackend renders the projection of an NFS backend: its section, the
// shares file the driver reads its export from, and the [DEFAULT] overlay. A
// non-empty skip reason leaves the backend out of the projection.
func renderNFSBackend(cinder *cinderv1alpha1.Cinder, backend *cinderv1alpha1.CinderBackend) (*renderedBackend, string) {
	nfs := backend.Spec.NFS
	if nfs == nil {
		// The schema union rule guarantees spec.nfs for a type-NFS backend; a
		// bypassed admission leaves nothing to render.
		return nil, fmt.Sprintf("backend %s has type %s but no nfs block", backend.Name, backend.Spec.Type)
	}

	section := renderBackendSection(cinder, backend)
	if fault := controlCharFault(backend.Name, section); fault != "" {
		return nil, fault
	}

	// The export is the one rendered value the section does not carry: the
	// shares file is assembled from server and path directly, so it needs a
	// pass of its own. A newline in either would add a second export line to
	// the file the driver reads, naming a share the operator never mounted or
	// derived an egress rule for.
	shares := nfs.Server + ":" + nfs.Path
	if fault := controlCharFault(backend.Name, map[string]string{sharesDataKey: shares}); fault != "" {
		return nil, fault
	}

	return &renderedBackend{
		data: map[string][]byte{
			backendConfDataKey:   []byte(config.RenderINI(map[string]map[string]string{backend.Name: section})),
			sharesDataKey:        []byte(shares + "\n"),
			volumeOverlayDataKey: volumeOverlay(backend.Name),
		},
		projection: backendProjection{
			name:         backend.Name,
			backendType:  cinderv1alpha1.CinderBackendTypeNFS,
			server:       nfs.Server,
			path:         nfs.Path,
			mountOptions: nfs.MountOptions,
		},
		host: fmt.Sprintf("tcp://%s:%d", nfs.Server, nfsEgressPort),
	}, ""
}

// renderRBDBackend renders the projection of an RBD backend: its section, the
// Ceph client configuration and keyring the driver connects with, and the
// [DEFAULT] overlay. The key is read from the backend's key Secret on children,
// where the volume pods mount the keyring, so a replaced key changes the
// content-hashed Secret name and rolls the volume service.
//
// A non-empty skip reason leaves the backend out of the projection. A Secret or
// data key that vanished between the gate and this pass is such a skip, and so
// is a key the gate would refuse (cephxKeyFault): a rotation reaches this pass
// through the Secret watch without re-running the gate. The next gate pass
// reports either on the backend. Any other read failure is returned.
func renderRBDBackend(ctx context.Context, children client.Client, cinder *cinderv1alpha1.Cinder,
	backend *cinderv1alpha1.CinderBackend,
) (*renderedBackend, string, error) {
	rbd := backend.Spec.RBD
	if rbd == nil {
		// The schema union rule guarantees spec.rbd for a type-RBD backend; a
		// bypassed admission leaves nothing to render.
		return nil, fmt.Sprintf("backend %s has type %s but no rbd block", backend.Name, backend.Spec.Type), nil
	}

	keySecret := rbd.KeySecretRef.Name
	key, err := secrets.GetSecretValue(ctx, children,
		client.ObjectKey{Namespace: cinder.Namespace, Name: keySecret}, cinderv1alpha1.RBDKeySecretDataKey)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			return nil, fmt.Sprintf("RBD key Secret %q or its %s data key is missing",
				keySecret, cinderv1alpha1.RBDKeySecretDataKey), nil
		}
		return nil, "", fmt.Errorf("reading the RBD key of backend %q: %w", backend.Name, err)
	}
	key = strings.TrimSpace(key)

	section := renderRBDBackendSection(cinder, backend)
	if fault := controlCharFault(backend.Name, section); fault != "" {
		return nil, fault, nil
	}
	// The two Ceph files are assembled from these values directly, so they need
	// a pass of their own; the fault names the section, never a value, so the
	// key does not reach the event.
	if fault := controlCharFault(backend.Name+"/ceph", map[string]string{
		"mon_host": strings.Join(rbd.Monitors, ","),
		"cluster":  rbd.ClusterName,
		"user":     rbd.User,
		"key":      key,
	}); fault != "" {
		return nil, fault, nil
	}
	if fault := cephxKeyFault(keySecret, key); fault != "" {
		return nil, fault, nil
	}

	return &renderedBackend{
		data: map[string][]byte{
			backendConfDataKey:   []byte(config.RenderINI(map[string]map[string]string{backend.Name: section})),
			cephConfDataKey:      []byte(renderCephConf(rbd)),
			keyringDataKey:       []byte(renderKeyring(rbd.User, key)),
			volumeOverlayDataKey: volumeOverlay(backend.Name),
		},
		projection: backendProjection{
			name:        backend.Name,
			backendType: cinderv1alpha1.CinderBackendTypeRBD,
			rbd: &rbdProjection{
				clusterName: rbd.ClusterName,
				user:        rbd.User,
				networks:    rbd.Networks,
			},
		},
	}, "", nil
}

// controlCharFault returns the skip reason of a backend whose options under
// section carry a newline or a carriage return, or "" when they are clean. The
// reason names the section, never a value.
func controlCharFault(section string, options map[string]string) string {
	if err := config.CheckNoControlChars(section, options); err != nil {
		return err.Error()
	}
	return ""
}

// volumeOverlay renders the [DEFAULT] overlay that names the single backend a
// cinder-volume serves.
func volumeOverlay(name string) []byte {
	return []byte("[DEFAULT]\nenabled_backends = " + name + "\n")
}

// renderBackendSection renders one NFS backend's [<name>] section: the NFS
// driver wiring, the optional image-volume cache bounds, and the backend's
// extraOptions (applyCacheAndExtraOptions).
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
		"nfs_shares_config":           cinderBackendsConfigDir + "/" + backend.Name + ".shares",
		"nfs_mount_point_base":        nfsMountPointBase,
		"nfs_mount_options":           nfs.MountOptions,
		"nas_secure_file_operations":  "true",
		"nas_secure_file_permissions": "true",
		"nfs_snapshot_support":        "false",
		"nfs_sparsed_volumes":         "true",
		"nfs_qcow2_volumes":           "false",
	}
	applyCacheAndExtraOptions(section, backend)
	return section
}

// renderRBDBackendSection renders one RBD backend's [<name>] section: the RBD
// driver wiring, rbd_secret_uuid only when spec.rbd.secretUUID is set (the
// driver otherwise uses the cluster FSID), the optional image-volume cache
// bounds, and the backend's extraOptions (applyCacheAndExtraOptions).
//
// The caller has checked that spec.rbd is set.
func renderRBDBackendSection(cinder *cinderv1alpha1.Cinder, backend *cinderv1alpha1.CinderBackend) map[string]string {
	rbd := backend.Spec.RBD
	section := map[string]string{
		"volume_driver":       "cinder.volume.drivers.rbd.RBDDriver",
		"volume_backend_name": backend.Name,
		// The host identity, as in the NFS section.
		"backend_host":     cinder.Name,
		"rbd_ceph_conf":    cephConfigDir + "/" + cephConfFile(rbd.ClusterName),
		"rbd_cluster_name": rbd.ClusterName,
		"rbd_pool":         rbd.Pool,
		"rbd_user":         rbd.User,
	}
	if rbd.SecretUUID != "" {
		section["rbd_secret_uuid"] = rbd.SecretUUID
	}
	applyCacheAndExtraOptions(section, backend)
	return section
}

// applyCacheAndExtraOptions adds the optional image-volume cache bounds to a
// rendered backend section, then merges the backend's extraOptions WITHOUT
// overriding an operator key and without any key the webhook denies for the
// backend's type. The webhook denylist normally keeps extraOptions disjoint from
// the rendered keys; this is the fail-closed backstop for a bypassed webhook,
// and the denylist check also drops a denied key the section does not render,
// such as rbd_keyring_conf.
func applyCacheAndExtraOptions(section map[string]string, backend *cinderv1alpha1.CinderBackend) {
	if cache := backend.Spec.ImageVolumeCache; cache != nil && cache.Enabled {
		section["image_volume_cache_enabled"] = "true"
		if cache.MaxSizeGB != nil {
			section["image_volume_cache_max_size_gb"] = fmt.Sprintf("%d", *cache.MaxSizeGB)
		}
		if cache.MaxCount != nil {
			section["image_volume_cache_max_count"] = fmt.Sprintf("%d", *cache.MaxCount)
		}
	}

	denied := cinderv1alpha1.ExtraOptionsDenylist(backend.Spec.Type)
	for k, v := range backend.Spec.ExtraOptions {
		if _, exists := section[k]; exists {
			continue
		}
		if _, deny := denied[k]; deny {
			continue
		}
		section[k] = v
	}
}

// renderCephConf renders the Ceph client configuration of an RBD backend, the
// file rbd_ceph_conf names. librados finds the key through its keyring line.
// log_file and admin_socket replace two defaults the service user cannot honour:
// it can write neither /var/log/ceph nor /var/run/ceph, and librados would log a
// warning for each on every connection the driver opens, which is one per
// operation. The dollar signs are Ceph metavariables written verbatim, which
// librados expands per process.
func renderCephConf(rbd *cinderv1alpha1.RBDBackendSpec) string {
	return "[global]\n" +
		"mon_host = " + strings.Join(rbd.Monitors, ",") + "\n" +
		"keyring = " + cephConfigDir + "/" + cephKeyringFile(rbd.ClusterName, rbd.User) + "\n" +
		"log_file = /dev/null\n" +
		"admin_socket = /tmp/$cluster-$name.$pid.$cctid.asok\n"
}

// renderKeyring renders the keyring file of the cephx user client.<user>.
func renderKeyring(user, key string) string {
	return "[client." + user + "]\n\tkey = " + key + "\n"
}

// cephConfFile returns the file name the Ceph client configuration of the
// cluster clusterName is projected under in cephConfigDir, the name librados
// reads by default.
func cephConfFile(clusterName string) string {
	return clusterName + ".conf"
}

// cephKeyringFile returns the file name the keyring of client.<user> is
// projected under in cephConfigDir, the name the Ceph tools look for by
// default.
func cephKeyringFile(clusterName, user string) string {
	return clusterName + ".client." + user + ".keyring"
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
