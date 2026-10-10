// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
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

// CinderBackupBackendCinderRefIndexKey is the field-indexer key under which
// CinderBackupBackend CRs are indexed by spec.cinderRef.name. Used by the
// cinder-side sub-reconciler (find the backup driver attached to one Cinder) and
// by the watch mapper that fans a Cinder event out to it.
const CinderBackupBackendCinderRefIndexKey = "spec.cinderRef.name"

// Aggregated BackupBackendReady vocabulary. The cinder-side sub-reconciler owns
// this condition on the Cinder CR; the per-backend conditions stay owned by the
// dedicated CinderBackupBackend controller.
const (
	// conditionTypeBackupBackendReady is the aggregated Cinder condition this
	// sub-reconciler drives.
	conditionTypeBackupBackendReady = "BackupBackendReady"
	// conditionReasonNoBackupBackend is set when no CinderBackupBackend is
	// attached. It is True rather than False: backups are an opt-in service, and
	// a Cinder without them still serves volumes.
	conditionReasonNoBackupBackend = "NoBackupBackend"
	// conditionReasonWaitingForBackupBackend is set while the attached backup
	// backend is not yet credential-ready or was skipped for a fault.
	conditionReasonWaitingForBackupBackend = "WaitingForBackupBackend"
	// conditionReasonMultipleBackupBackends is set when more than one attached
	// backup backend is credential-ready. The backup driver is a property of the
	// one cinder-backup Deployment, so a second candidate is ambiguous rather
	// than additive and nothing is rendered.
	conditionReasonMultipleBackupBackends = "MultipleBackupBackends"
	// conditionReasonBackupBackendProjected is set when the single attached,
	// credential-ready backup backend is projected.
	conditionReasonBackupBackendProjected = "BackupBackendProjected"
)

// backupConfDataKey is the data key inside the projection Secret that holds the
// rendered [DEFAULT] backup section the cinder-backup Deployment mounts. An RBD
// target's Secret carries the Ceph client configuration and the keyring beside
// it, under cephConfDataKey and keyringDataKey.
const backupConfDataKey = "backup.conf"

// backupMountPointBase is the in-pod directory the cinder-backup process mounts
// its target export under. It differs from a volume backend's mount point
// because the backup service mounts its own export alongside the volumes it
// reads.
const backupMountPointBase = "/var/lib/cinder/backup_mount"

// backupProjection is what the backup sub-reconciler hands downstream (the
// deployment and networkpolicy steps). A nil projection means no backup service
// is rendered. name, backupType and secretName are set for every type; server,
// path and mountOptions only for an NFS target, rbd only for an RBD one.
type backupProjection struct {
	// name is the CinderBackupBackend's name.
	name string
	// backupType is the driver the target was rendered for.
	backupType cinderv1alpha1.CinderBackupBackendType
	// server and path are the NFS export the cinder-backup pod mounts.
	server string
	path   string
	// mountOptions is the option string the export is mounted with.
	mountOptions string
	// rbd carries what the backup Deployment and the NetworkPolicy need of an
	// RBD target.
	rbd *rbdProjection
	// secretName is the content-hashed Secret carrying backup.conf, and for an
	// RBD target the Ceph client configuration and keyring.
	secretName string
}

// backupSecretBaseName returns the base name of the backup backend's projection
// Secret. It carries the backend's name so a replacement backup backend renders
// under its own base name and the old one's history is swept rather than
// rewritten.
func backupSecretBaseName(cinder *cinderv1alpha1.Cinder, name string) string {
	return cinder.Name + "-backup-" + name
}

// reconcileBackupBackend projects the single attached, credential-ready
// CinderBackupBackend into a content-hashed Secret and sets the aggregated
// BackupBackendReady condition on the Cinder CR. It returns the projection the
// deployment step turns into the cinder-backup Deployment, or nil when no backup
// service is rendered.
//
// Exactly one candidate is projected: the backup driver is a property of the one
// cinder-backup Deployment rather than one backend among several, so zero
// candidates render nothing and two or more are ambiguous.
//
// CONTRACT: this step never returns a requeue and never returns an error for
// waiting states — the CinderBackupBackend watch wakes the parent when the
// backend's status flips. Only genuine infrastructure failures (List/create/
// prune errors) surface as errors.
func (r *CinderReconciler) reconcileBackupBackend(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, *backupProjection, error) {
	// The attached backup backends are sibling configuration CRs on the
	// management cluster, so this list goes through the embedded client and its
	// field index; only the Secret it renders into is written on children.
	var backupBackends cinderv1alpha1.CinderBackupBackendList
	if err := r.List(
		ctx, &backupBackends,
		client.InNamespace(cinder.Namespace),
		client.MatchingFields{CinderBackupBackendCinderRefIndexKey: cinder.Name},
	); err != nil {
		return ctrl.Result{}, nil, fmt.Errorf("listing CinderBackupBackends for %s: %w", cinder.Name, err)
	}

	items := make([]*cinderv1alpha1.CinderBackupBackend, 0, len(backupBackends.Items))
	for i := range backupBackends.Items {
		items = append(items, &backupBackends.Items[i])
	}
	// Every gated backend is a candidate, so Collect's exactly-one-default rule
	// decides the single-attachment invariant this kind carries.
	c := satellite.Collect(satellite.CollectParams[cinderv1alpha1.CinderBackupBackend, *cinderv1alpha1.CinderBackupBackend]{
		Items:     items,
		Gate:      func(b *cinderv1alpha1.CinderBackupBackend) bool { return credentialsReady(b.Status.Conditions) },
		IsDefault: func(*cinderv1alpha1.CinderBackupBackend) bool { return true },
	})

	projection, err := r.projectBackupBackend(ctx, children, cinder, c)
	if err != nil {
		return ctrl.Result{}, nil, err
	}

	// A backup backend that detached, was replaced or was skipped keeps no
	// Deployment, so its base name is swept whole: nothing mounts the Secrets
	// under it anymore, and each of them names the target a restore would read
	// (an NFS export, or an RBD pool together with the cephx key that opens it).
	projected := map[string]struct{}{}
	if projection != nil {
		projected[projection.name] = struct{}{}
	}
	if err := r.pruneStaleSatelliteSecrets(ctx, children, cinder,
		backupSecretBaseName(cinder, ""), projected); err != nil {
		return ctrl.Result{}, nil, err
	}
	return ctrl.Result{}, projection, nil
}

// projectBackupBackend picks the single credential-ready candidate out of c,
// renders it into its content-hashed Secret and sets BackupBackendReady. It
// returns nil when nothing is rendered: no candidate, an ambiguous pair, or a
// per-backend fault. The caller sweeps the base names this outcome leaves
// behind.
func (r *CinderReconciler) projectBackupBackend(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
	c satellite.Collection[cinderv1alpha1.CinderBackupBackend, *cinderv1alpha1.CinderBackupBackend],
) (*backupProjection, error) {
	logger := log.FromContext(ctx)

	switch {
	case len(c.Attached) == 0:
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackupBackendReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonNoBackupBackend,
			Message:            "No CinderBackupBackend is attached; the backup service is not rendered",
		})
		return nil, nil
	case len(c.DefaultCandidates) == 0:
		r.markBackupBackendWaiting(cinder, "Waiting for backup backends: "+strings.Join(backupBackendNames(c.Attached), ", "))
		return nil, nil
	case !c.Valid:
		conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
			Type:               conditionTypeBackupBackendReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cinder.Generation,
			Reason:             conditionReasonMultipleBackupBackends,
			Message: fmt.Sprintf("More than one attached CinderBackupBackend is credential-ready (%s); exactly one is required",
				strings.Join(backupBackendNames(c.DefaultCandidates), ", ")),
		})
		return nil, nil
	}

	backupBackend := c.DefaultCandidates[0]
	var (
		data       map[string][]byte
		projection *backupProjection
		skip       string
	)
	switch backupBackend.Spec.Type {
	case cinderv1alpha1.CinderBackupBackendTypeNFS:
		data, projection, skip = renderNFSBackupBackend(cinder, backupBackend)
	case cinderv1alpha1.CinderBackupBackendTypeRBD:
		var err error
		data, projection, skip, err = renderRBDBackupBackend(ctx, children, cinder, backupBackend)
		if err != nil {
			return nil, err
		}
	default:
		// The schema enum admits no other type; a bypassed admission leaves
		// nothing this operator knows how to render.
		skip = fmt.Sprintf("backup backend %s has type %s, which this operator does not render",
			backupBackend.Name, backupBackend.Spec.Type)
	}
	if skip != "" {
		r.skipBackupBackend(ctx, cinder, backupBackend.Name, skip)
		return nil, nil
	}

	baseName := backupSecretBaseName(cinder, backupBackend.Name)
	secretName, err := config.CreateImmutableSecret(ctx, children, r.Scheme, cinder,
		baseName, cinder.Namespace, data)
	if err != nil {
		return nil, fmt.Errorf("creating backup secret for %q: %w", backupBackend.Name, err)
	}
	if err := config.PruneImmutableSecrets(ctx, children, r.Scheme, cinder, config.PruneOptions{
		BaseName:    baseName,
		Namespace:   cinder.Namespace,
		CurrentName: secretName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		return nil, fmt.Errorf("pruning backup Secrets for %q: %w", backupBackend.Name, err)
	}

	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               conditionTypeBackupBackendReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonBackupBackendProjected,
		Message:            "Backup backend " + backupBackend.Name + " is projected",
	})
	logger.V(1).Info("projected CinderBackupBackend", "backupBackend", backupBackend.Name, "secret", secretName)
	projection.secretName = secretName
	return projection, nil
}

// renderNFSBackupBackend renders the projection of an NFS backup target: the
// [DEFAULT] section of the NFS backup driver alone, because the export is
// mounted with the pod's own identity. A non-empty skip reason leaves the
// target unprojected.
func renderNFSBackupBackend(cinder *cinderv1alpha1.Cinder, backupBackend *cinderv1alpha1.CinderBackupBackend,
) (map[string][]byte, *backupProjection, string) {
	nfs := backupBackend.Spec.NFS
	if nfs == nil {
		// The schema union rule guarantees spec.nfs for a type-NFS backup
		// backend; a bypassed admission leaves nothing to render.
		return nil, nil, fmt.Sprintf("backup backend %s has type %s but no nfs block",
			backupBackend.Name, backupBackend.Spec.Type)
	}

	section := renderNFSBackupSection(cinder, backupBackend)
	if fault := controlCharFault("DEFAULT", section); fault != "" {
		return nil, nil, fault
	}
	data := map[string][]byte{
		backupConfDataKey: []byte(config.RenderINI(map[string]map[string]string{"DEFAULT": section})),
	}
	return data, &backupProjection{
		name:         backupBackend.Name,
		backupType:   cinderv1alpha1.CinderBackupBackendTypeNFS,
		server:       nfs.Server,
		path:         nfs.Path,
		mountOptions: nfs.MountOptions,
	}, ""
}

// renderRBDBackupBackend renders the projection of an RBD backup target: the
// [DEFAULT] section of the Ceph backup driver, and the Ceph client
// configuration and keyring the driver connects with. The key is read from the
// target's key Secret on children, where the backup pod mounts the keyring, so
// a replaced key changes the content-hashed Secret name and rolls the backup
// service.
//
// It mirrors renderRBDBackend. A non-empty skip reason leaves the target
// unprojected: a Secret or data key that vanished between the gate and this
// pass, or a key the gate would refuse (cephxKeyFault), because a rotation
// reaches this pass through the Secret watch without re-running the gate. The
// next gate pass reports either on the target. Any other read failure is
// returned.
func renderRBDBackupBackend(ctx context.Context, children client.Client, cinder *cinderv1alpha1.Cinder,
	backupBackend *cinderv1alpha1.CinderBackupBackend,
) (map[string][]byte, *backupProjection, string, error) {
	rbd := backupBackend.Spec.RBD
	if rbd == nil {
		// The schema union rule guarantees spec.rbd for a type-RBD backup
		// backend; a bypassed admission leaves nothing to render.
		return nil, nil, fmt.Sprintf("backup backend %s has type %s but no rbd block",
			backupBackend.Name, backupBackend.Spec.Type), nil
	}

	key, skip, err := readRBDKey(ctx, children, cinder.Namespace, rbd.KeySecretRef.Name,
		fmt.Sprintf("backup backend %q", backupBackend.Name))
	if err != nil || skip != "" {
		return nil, nil, skip, err
	}

	section := renderRBDBackupSection(cinder, backupBackend)
	if fault := controlCharFault("DEFAULT", section); fault != "" {
		return nil, nil, fault, nil
	}
	if fault := cephFilesFault("DEFAULT/ceph", rbd.Monitors, rbd.ClusterName, rbd.User,
		rbd.KeySecretRef.Name, key); fault != "" {
		return nil, nil, fault, nil
	}

	data := map[string][]byte{
		backupConfDataKey: []byte(config.RenderINI(map[string]map[string]string{"DEFAULT": section})),
		cephConfDataKey:   []byte(renderCephConf(rbd.Monitors, rbd.ClusterName, rbd.User)),
		keyringDataKey:    []byte(renderKeyring(rbd.User, key)),
	}
	return data, &backupProjection{
		name:       backupBackend.Name,
		backupType: cinderv1alpha1.CinderBackupBackendTypeRBD,
		rbd: &rbdProjection{
			clusterName: rbd.ClusterName,
			user:        rbd.User,
			networks:    rbd.Networks,
			keyDigest:   cephxKeyDigest(key),
		},
	}, "", nil
}

// skipBackupBackend warns about a fault that keeps the backup backend
// unprojected and flips the condition to the waiting state. As with the volume
// backends, the event goes to the Cinder, where the condition it explains lives.
func (r *CinderReconciler) skipBackupBackend(ctx context.Context, cinder *cinderv1alpha1.Cinder, name, reason string) {
	msg := fmt.Sprintf("Skipping backup backend %s: %s", name, reason)
	log.FromContext(ctx).Info(msg)
	r.Recorder.Event(cinder, corev1.EventTypeWarning, "CinderBackupBackendSkipped", msg)
	r.markBackupBackendWaiting(cinder, msg)
}

// markBackupBackendWaiting sets BackupBackendReady=False with the waiting
// reason and the given message.
func (r *CinderReconciler) markBackupBackendWaiting(cinder *cinderv1alpha1.Cinder, message string) {
	conditions.SetCondition(&cinder.Status.Conditions, metav1.Condition{
		Type:               conditionTypeBackupBackendReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cinder.Generation,
		Reason:             conditionReasonWaitingForBackupBackend,
		Message:            message,
	})
}

// backupBackendNames returns the names of the given backup backends, in the
// order Collect sorted them into.
func backupBackendNames(items []*cinderv1alpha1.CinderBackupBackend) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}

// renderNFSBackupSection renders the NFS backup driver's [DEFAULT] section: the
// host identity the backup service registers under, the NFS driver wiring, the
// chunk and compression knobs, and the backend's extraOptions
// (applyBackupExtraOptions).
//
// The caller has checked that spec.nfs is set.
func renderNFSBackupSection(cinder *cinderv1alpha1.Cinder, backupBackend *cinderv1alpha1.CinderBackupBackend) map[string]string {
	nfs := backupBackend.Spec.NFS
	section := map[string]string{
		// The identity the backups this deployment writes are recorded against.
		// It is derived from the Cinder rather than from the backup backend, so
		// replacing the backend keeps the existing backups restorable.
		"host":                         cinder.Name + "-backup",
		"backup_driver":                "cinder.backup.drivers.nfs.NFSBackupDriver",
		"backup_share":                 nfs.Server + ":" + nfs.Path,
		"backup_mount_point_base":      backupMountPointBase,
		"backup_mount_options":         nfs.MountOptions,
		"backup_file_size":             fmt.Sprintf("%d", effectiveBackupFileSize(backupBackend.Spec.FileSize)),
		"backup_compression_algorithm": backupBackend.Spec.Compression,
		// The backup service owns its target through its host identity, so a
		// backup is never handed to another host.
		"backup_use_same_host": "false",
	}
	applyBackupExtraOptions(section, backupBackend)
	return section
}

// renderRBDBackupSection renders the Ceph backup driver's [DEFAULT] section: the
// host identity, the Ceph driver wiring with the configuration file the backup
// pod finds in cephConfigDir, the pool and the user, and the backend's
// extraOptions (applyBackupExtraOptions). The chunked driver's fileSize and
// compression are not rendered: the Ceph driver reads neither.
//
// The caller has checked that spec.rbd is set.
func renderRBDBackupSection(cinder *cinderv1alpha1.Cinder, backupBackend *cinderv1alpha1.CinderBackupBackend) map[string]string {
	rbd := backupBackend.Spec.RBD
	section := map[string]string{
		// The host identity, as in the NFS section.
		"host":                 cinder.Name + "-backup",
		"backup_driver":        "cinder.backup.drivers.ceph.CephBackupDriver",
		"backup_ceph_conf":     cephConfigDir + "/" + cephConfFile(rbd.ClusterName),
		"backup_ceph_pool":     rbd.Pool,
		"backup_ceph_user":     rbd.User,
		"backup_use_same_host": "false",
	}
	applyBackupExtraOptions(section, backupBackend)
	return section
}

// applyBackupExtraOptions merges the backup backend's extraOptions into a
// rendered section WITHOUT overriding an operator key and without any key the
// webhook denies for the backend's type (cinderv1alpha1.BackupExtraOptionsDenylist).
// The webhook denylist normally keeps extraOptions disjoint from the rendered
// keys; this is the fail-closed backstop for a bypassed webhook, and it drops a
// denied key whether or not the section renders it.
func applyBackupExtraOptions(section map[string]string, backupBackend *cinderv1alpha1.CinderBackupBackend) {
	denied := cinderv1alpha1.BackupExtraOptionsDenylist(backupBackend.Spec.Type)
	for k, v := range backupBackend.Spec.ExtraOptions {
		if _, exists := section[k]; exists {
			continue
		}
		if _, deny := denied[k]; deny {
			continue
		}
		section[k] = v
	}
}

// effectiveBackupFileSize returns the backup chunk size in bytes, falling back
// to DefaultBackupFileSize when the CR leaves it unset (a CR that bypassed the
// CRD default).
func effectiveBackupFileSize(fileSize *int64) int64 {
	if fileSize != nil {
		return *fileSize
	}
	return cinderv1alpha1.DefaultBackupFileSize
}
