// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/config"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The reasons of the ConfigReady condition.
const (
	conditionReasonConfigRendered          = "ConfigRendered"
	conditionReasonWaitingForComputeConfig = "WaitingForComputeConfig"
	conditionReasonComputeConfigIncomplete = "ComputeConfigIncomplete"
)

// The pool config: the data key of the ConfigMap, which is also the file
// nova-compute reads from its --config-dir, and the directory it is mounted at.
const (
	poolConfigFile      = "compute-pool.conf"
	poolConfigMountPath = "/etc/nova/compute-pool.conf.d"
)

// ovsDBSocket is the local Open vSwitch database the OVNChassis pods on the node
// serve. os-vif plugs instance ports through it, and the wait-for-chassis gate
// reads the chassis registration from it.
const ovsDBSocket = "/run/openvswitch/db.sock"

// novaComputeConfigHashAnnotation carries a hash of the compute contract on the
// pod template, so a rotated password or bus URL rolls the pods.
const novaComputeConfigHashAnnotation = "nova.openstack.c5c3.io/compute-config-hash"

// reconcileNovaComputeConfig renders the pool's compute-pool.conf into an
// immutable ConfigMap on the children cluster, hashes the compute contract the
// pods mount beside it, and sets ConfigReady.
//
// The contract Secret is read in the CR's namespace on the children cluster:
// the Nova publishes it there when the pool runs beside it, and the ControlPlane
// mirrors it there for a pool on a compute cluster.
func (r *NovaComputeReconciler) reconcileNovaComputeConfig(ctx context.Context, children client.Client,
	cr *novav1alpha1.NovaCompute, pass *novaComputePass,
) (ctrl.Result, error) {
	// The ownership guard is a pure function of the spec. ExtraConfigHealthy is
	// informational and stays out of novaComputeSubConditionTypes.
	config.RecordExtraConfigHealth(r.Recorder, cr, &cr.Status.Conditions, cr.Generation,
		config.FindOwnedOverrides(cr.Spec.ExtraConfig, novav1alpha1.NovaComputeOwnedConfigKeys))

	key := types.NamespacedName{Namespace: cr.Namespace, Name: pass.secretName}
	secret := &corev1.Secret{}
	if err := children.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return waitOnComputeConfig(cr, conditionReasonWaitingForComputeConfig, fmt.Sprintf(
				"compute-contract Secret %s not found in namespace %s on %s; the ControlPlane mirrors it "+
					"for a ControlPlane-managed Nova; otherwise copy it (docs/guides/nova/connect-a-compute-cluster.md)",
				key.Name, key.Namespace, clusterDescription(cr)))
		}
		err = fmt.Errorf("getting compute-contract Secret %s: %w", key, err)
		novaComputeSkeleton.MarkFailed(cr, conditionTypeConfigReady, conditionReasonConfigError, err)
		return ctrl.Result{}, err
	}

	var missing []string
	for _, k := range []string{computeConfigFragmentKey, transportURLKey, passwordKey} {
		if len(secret.Data[k]) == 0 {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return waitOnComputeConfig(cr, conditionReasonComputeConfigIncomplete, fmt.Sprintf(
			"compute-contract Secret %s lacks %s", key, strings.Join(missing, ", ")))
	}

	merged := config.MergeDefaults(cr.Spec.ExtraConfig, novaComputePoolDefaults(cr))
	baseName := cr.Name + "-config"
	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, cr, baseName, cr.Namespace,
		map[string]string{poolConfigFile: config.RenderINI(merged)})
	if err != nil {
		err = fmt.Errorf("creating pool config ConfigMap: %w", err)
		novaComputeSkeleton.MarkFailed(cr, conditionTypeConfigReady, conditionReasonConfigError, err)
		return ctrl.Result{}, err
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, r.Scheme, cr, config.PruneOptions{
		BaseName:    baseName,
		Namespace:   cr.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		err = fmt.Errorf("pruning pool config ConfigMaps: %w", err)
		novaComputeSkeleton.MarkFailed(cr, conditionTypeConfigReady, conditionReasonConfigError, err)
		return ctrl.Result{}, err
	}

	pass.configMapName = configMapName
	pass.configHash = computeConfigHash(secret)

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeConfigReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonConfigRendered,
		Message:            fmt.Sprintf("Rendered %s into ConfigMap %s", poolConfigFile, configMapName),
	})
	return ctrl.Result{}, nil
}

// waitOnComputeConfig sets ConfigReady False for a wait on the contract and
// requeues.
func waitOnComputeConfig(cr *novav1alpha1.NovaCompute, reason, message string) (ctrl.Result, error) {
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeConfigReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            message,
	})
	return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
}

// clusterDescription names the cluster the CR's children land on.
func clusterDescription(cr *novav1alpha1.NovaCompute) string {
	if cr.Spec.TargetClusterRef == nil {
		return "the local cluster"
	}
	return "target cluster " + cr.Spec.TargetClusterRef.Name
}

// novaComputePoolDefaults builds the operator-owned compute-pool.conf, before
// spec.extraConfig is merged over it. It is a pure function of the spec.
//
// The contract fragment deliberately leaves out what depends on the node
// (host, state_path, the libvirt and Open vSwitch sockets, the locks, the
// console address); this file carries the paths the pod mounts, and the pod's
// environment carries the node's name and address. The three optional
// [libvirt] keys are rendered only when set, so Nova's own defaults apply
// otherwise.
func novaComputePoolDefaults(cr *novav1alpha1.NovaCompute) map[string]map[string]string {
	libvirt := map[string]string{
		"virt_type":      effectiveVirtType(cr),
		"connection_uri": "qemu:///system",
	}
	if mode := cr.Spec.Libvirt.CPUMode; mode != "" {
		libvirt["cpu_mode"] = mode
		if mode == "custom" {
			libvirt["cpu_models"] = strings.Join(cr.Spec.Libvirt.CPUModels, ",")
		}
	}
	if images := cr.Spec.Libvirt.ImagesType; images != "" {
		libvirt["images_type"] = images
	}

	return map[string]map[string]string{
		"DEFAULT": {
			"compute_driver": "libvirt.LibvirtDriver",
			"state_path":     novaStatePath,
		},
		"libvirt": libvirt,
		"os_vif_ovs": {
			"ovsdb_connection": "unix:" + ovsDBSocket,
		},
		"oslo_concurrency": {
			"lock_path": novaLockPath,
		},
		// The pod shares the node's network, so a wildcard would open every
		// instance's console, unauthenticated, on every interface of the node.
		// $my_ip is the node address the pod's environment sets, the one the
		// console proxy dials through server_proxyclient_address.
		"vnc": {
			"server_listen": "$my_ip",
		},
	}
}

// effectiveVirtType is spec.libvirt.virtType, or kvm, the CRD default, for a
// CR that reached the operator without it.
func effectiveVirtType(cr *novav1alpha1.NovaCompute) string {
	if cr.Spec.Libvirt.VirtType != "" {
		return cr.Spec.Libvirt.VirtType
	}
	return "kvm"
}

// computeConfigHash is a SHA-256 over the contract Secret's key/value pairs in
// key order.
func computeConfigHash(secret *corev1.Secret) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(secret.Data)) {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, secret.Data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
