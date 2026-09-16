// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller - reconcileComputeConfig publishes the compute contract:
// the <nova.Name>-compute-config Secret a nova-compute reads to join this
// control plane. The operator deploys no compute node, so this Secret is the
// whole handover. It carries a nova.conf fragment with the sections a compute
// needs and nothing else, the bus URL, the two credentials that never enter a
// config file, and the name of the cell the compute registers into.
//
// The Secret keeps one name for the lifetime of the CR and is updated in place.
// A consumer mounts it by name (the CI fake compute and the compute-cluster
// deployment both do), so a content-hashed name of the kind reconcileConfig
// derives would break the mount on every rotation.

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition type and reason constants for ComputeConfigReady.
const (
	conditionTypeComputeConfigReady       = "ComputeConfigReady"
	conditionReasonComputeConfigPublished = "ComputeConfigPublished"
	conditionReasonComputeConfigError     = "ComputeConfigError"
)

// componentComputeConfig is the app.kubernetes.io/component value of the
// compute-contract Secret and the suffix of its name.
const componentComputeConfig = "compute-config"

// computeConfigMountPath is the directory a compute node projects this Secret
// at. No pod this operator builds mounts it, yet the path is part of the
// rendered fragment: [oslo_messaging_rabbit] ssl_ca_file names a file inside it,
// so a compute that mounts the Secret elsewhere finds no CA bundle where the
// config says one is.
const computeConfigMountPath = "/etc/nova/compute-config"

// The data keys of the compute-contract Secret.
//
// nova-compute.conf is the config fragment; the four value keys beside it are
// read as env overrides or as files by whatever runs the compute, so none of
// them appears in the fragment itself. ca.crt is present only on a Nova whose
// bus is TLS, and its name is the file name ssl_ca_file resolves to under
// computeConfigMountPath.
const (
	computeConfigFragmentKey = "nova-compute.conf"
	transportURLKey          = "transport_url"
	passwordKey              = "password"
	metadataSharedSecretKey  = "metadata_proxy_shared_secret" // #nosec G101 -- Secret data key, not a credential.
	cellNameKey              = "cell_name"
	caBundleKey              = "ca.crt"
)

// computeConfigSecretName returns the name of the compute-contract Secret,
// "<nova.Name>-compute-config". It is stable for the lifetime of the CR: the
// consumers mount it by name and status.computeConfigSecretRef publishes it.
func computeConfigSecretName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentComputeConfig
}

// computeConfigDefaults builds the nova.conf fragment a nova-compute reads. It
// is a pure function of the spec, so the golden test renders it without a
// cluster.
//
// Every section comes from the helper the control plane's own nova.conf is
// rendered with, so the two documents name one service the same way or neither
// does, and switch the console on or off the same way: [vnc] enabled defaults to
// true on a compute, so a disabled proxy has to be rendered as the switch off
// rather than left out. What differs is what is left out, and each omission is a
// decision:
//
//   - No [database], [api_database] or [api]. A compute node reaches no schema
//     directly; the conductor reads the cell database on its behalf, which is
//     the whole point of the split.
//   - No [cache]. The memcached this deployment runs is a management-cluster
//     Service, and a compute cluster does not resolve it. The token middleware
//     therefore caches nothing, and [keystone_authtoken] drops
//     memcached_servers. www_authenticate_uri stays: it is the address a 401
//     points a client at, not one this process dials.
//   - No [scheduler], [conductor] or [oslo_concurrency], which configure
//     processes a compute node does not run, and no state_path or
//     log_config_append, which name paths inside a pod this operator builds.
//   - No metadata keys in [neutron]. service_metadata_proxy and the shared
//     secret belong to the metadata API that verifies a proxied request, not to
//     the compute.
//   - No spec.extraConfig. The overrides configure the control plane the CR
//     author runs, while this fragment is loaded by hypervisors in another trust
//     domain, where a section of the author's choosing would reach options no
//     typed field exposes (the privsep helpers, libvirt, the instance paths). An
//     override a compute needs as well belongs to the compute deployment.
func computeConfigDefaults(nova *novav1alpha1.Nova) map[string]map[string]string {
	spec := &nova.Spec
	logging := effectiveLogging(spec.Logging)
	params := clientSectionParams(spec)

	// The token middleware runs on the compute too (nova-compute serves no API,
	// but the same account is what its outgoing calls authenticate with), and it
	// runs without the token cache the control plane has.
	tokenParams := params
	tokenParams.MemcachedServers = ""

	sections := map[string]map[string]string{
		"DEFAULT": {
			"use_stderr": "true",
			"debug":      fmt.Sprintf("%t", logging.Debug != nil && *logging.Debug),
		},
		"keystone_authtoken":    keystoneauth.Section(tokenParams),
		"service_user":          keystoneauth.ServiceUserSection(params),
		"placement":             catalogClientSection(params, spec.Endpoints.Placement.Override),
		"neutron":               catalogClientSection(params, spec.Endpoints.Neutron.Override),
		"glance":                glanceSection(spec),
		"oslo_messaging_rabbit": messaging.RabbitSection(spec.Messaging.TLS, computeConfigMountPath+"/"+caBundleKey),
		// The compute drops instance notifications at the source, like the
		// control plane: nothing this deployment runs consumes them.
		"oslo_messaging_notifications": {"driver": "noop"},
		// A compute that upgrades before its peers must not send a newer message
		// version to the ones that have not, which is what live migration and
		// resize do between two computes.
		"upgrade_levels": {"compute": "auto"},
		"vnc":            vncSection(nova),
	}

	// The two optional siblings, gated exactly as they are in the shared
	// document: a section naming a service the deployment does not run would
	// only fail at first use.
	if spec.Endpoints.Cinder.Enabled {
		sections["cinder"] = cinderSection(spec)
	}
	if spec.Endpoints.Barbican.Enabled {
		sections["key_manager"] = map[string]string{"backend": "barbican"}
		sections["barbican"] = barbicanSection(spec)
	}

	return sections
}

// computeFragmentUsable reports whether every section of the fragment is free of
// newlines and carriage returns, and flips ComputeConfigReady False naming the
// first offending section, in name order, when one is not.
func computeFragmentUsable(nova *novav1alpha1.Nova, sections map[string]map[string]string) bool {
	for _, section := range slices.Sorted(maps.Keys(sections)) {
		if err := config.CheckNoControlChars(section, sections[section]); err != nil {
			conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
				Type:               conditionTypeComputeConfigReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: nova.Generation,
				Reason:             conditionReasonComputeConfigError,
				Message:            err.Error(),
			})
			return false
		}
	}
	return true
}

// reconcileComputeConfig writes the compute-contract Secret and sets the
// ComputeConfigReady condition. It runs after the Secret gates and the transport
// URL, because four of the six keys are the values those steps read.
//
// The Secret is applied server-side under the shared field manager, so an
// updated fragment or a rotated password replaces the previous value in place
// and the ca.crt key disappears again when a Nova moves off a TLS bus. On a
// target cluster the apply claims the Secret by ownership label and refuses a
// same-named Secret nobody labelled, which surfaces here as
// ComputeConfigReady=False rather than as a silently overwritten object.
func (r *NovaReconciler) reconcileComputeConfig(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, transportURL string, values secretValues,
) (ctrl.Result, error) {
	// The fragment is rendered from typed spec fields, and a newline in one of
	// them injects further INI lines. The config step refuses the same values for
	// nova.conf but lets the pipeline go on with the last-good ConfigMap, so this
	// step has to refuse them on its own: the injected section would otherwise
	// reach every hypervisor that loads the fragment. The Secret already
	// published is left as it is, the way the config step keeps its last-good
	// ConfigMap.
	sections := computeConfigDefaults(nova)
	if !computeFragmentUsable(nova, sections) {
		return ctrl.Result{}, nil
	}

	name := computeConfigSecretName(nova)
	data := map[string][]byte{
		computeConfigFragmentKey: []byte(config.RenderINI(sections)),
		transportURLKey:          []byte(transportURL),
		passwordKey:              []byte(values.serviceUserPassword),
		metadataSharedSecretKey:  []byte(values.metadataSharedSecret),
		cellNameKey:              []byte(computeCellName),
	}
	// The bundle ships only for a bus the CR asked to verify. On a plaintext bus
	// the fragment names no ssl_ca_file, so a key carrying an empty value would
	// be a file the compute mounts and nothing reads.
	if nova.Spec.Messaging.TLS != nil {
		data[caBundleKey] = []byte(values.messagingCA)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nova.Namespace,
			Labels:    componentLabels(nova, componentComputeConfig),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := apply.EnsureObject(ctx, children, r.Scheme, nova, secret, apply.FieldManager); err != nil {
		conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
			Type:               conditionTypeComputeConfigReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: nova.Generation,
			Reason:             conditionReasonComputeConfigError,
			Message:            err.Error(),
		})
		return ctrl.Result{}, fmt.Errorf("publishing the compute config Secret: %w", err)
	}

	nova.Status.ComputeConfigSecretRef = &corev1.LocalObjectReference{Name: name}
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               conditionTypeComputeConfigReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonComputeConfigPublished,
	})
	return ctrl.Result{}, nil
}
