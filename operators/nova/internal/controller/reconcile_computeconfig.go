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
//
// While spec.remoteCompute is set, the step publishes a second contract beside
// it, <nova.Name>-remote-compute-config, for a nova-compute on another cluster.
// It carries the same keys with addresses that leave the cluster: the public
// Keystone URL, the public catalog rows, and the broker's external listener.
// The in-cluster contract keeps its bytes, so a compute beside this Nova keeps
// reading it.

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
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// Condition type and reason constants for ComputeConfigReady.
const (
	conditionTypeComputeConfigReady       = "ComputeConfigReady"
	conditionReasonComputeConfigPublished = "ComputeConfigPublished"
	conditionReasonComputeConfigError     = "ComputeConfigError"

	// conditionReasonWaitingForRemoteTransportURL reports a remote contract
	// whose transport URL Secret, or the key inside it, is not there yet.
	conditionReasonWaitingForRemoteTransportURL = "WaitingForRemoteTransportURL"
)

// componentComputeConfig is the app.kubernetes.io/component value of the
// compute-contract Secret and the suffix of its name.
const componentComputeConfig = "compute-config"

// componentRemoteComputeConfig is the same for the remote compute contract.
const componentRemoteComputeConfig = "remote-compute-config"

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
	metadataSharedSecretKey  = novav1alpha1.ComputeConfigMetadataSharedSecretKey
	cellNameKey              = "cell_name"
	caBundleKey              = "ca.crt"
)

// computeConfigSecretName returns the name of the compute-contract Secret,
// "<nova.Name>-compute-config". It is stable for the lifetime of the CR: the
// consumers mount it by name and status.computeConfigSecretRef publishes it.
func computeConfigSecretName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentComputeConfig
}

// remoteComputeConfigSecretName returns the name of the remote compute-contract
// Secret, "<nova.Name>-remote-compute-config". The c5c3 operator reads it under
// this name to mirror it onto compute clusters.
func remoteComputeConfigSecretName(nova *novav1alpha1.Nova) string {
	return nova.Name + "-" + componentRemoteComputeConfig
}

// computeFragment builds the nova.conf fragment a nova-compute reads. It is a
// pure function of the spec, so the golden tests render it without a cluster.
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
//
// remote re-addresses the client sections for a compute on another cluster,
// and changes nothing else. Every auth_url, and [barbican] auth_endpoint, is
// spec.remoteCompute.keystoneEndpoint. [placement], [neutron] and [glance]
// resolve the public catalog row, [cinder] the publicURL, and [barbican] the
// public endpoint type. Every endpoint override is dropped: spec.endpoints names
// an address the control-plane pods dial, which says nothing about what a
// compute cluster reaches.
func computeFragment(nova *novav1alpha1.Nova, remote bool) map[string]map[string]string {
	spec := &nova.Spec
	logging := effectiveLogging(spec.Logging)
	params := clientSectionParams(spec)
	if remote {
		params.AuthURL = spec.RemoteCompute.KeystoneEndpoint
	}

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

	if remote {
		for _, name := range []string{"placement", "neutron", "glance"} {
			sections[name]["valid_interfaces"] = "public"
			delete(sections[name], "endpoint_override")
		}
		if cinder, ok := sections["cinder"]; ok {
			cinder["auth_url"] = params.AuthURL
			cinder["catalog_info"] = "block-storage:cinder:publicURL"
			delete(cinder, "endpoint_template")
		}
		if barbican, ok := sections["barbican"]; ok {
			barbican["auth_endpoint"] = params.AuthURL
			barbican["barbican_endpoint_type"] = "public"
			delete(barbican, "barbican_endpoint")
		}
	}

	return sections
}

// computeConfigDefaults builds the fragment of the in-cluster contract,
// addressed at spec.keystoneEndpoint and the internal catalog rows.
func computeConfigDefaults(nova *novav1alpha1.Nova) map[string]map[string]string {
	return computeFragment(nova, false)
}

// remoteComputeConfigDefaults builds the fragment of the remote contract,
// addressed at spec.remoteCompute.keystoneEndpoint and the public catalog rows.
// The caller checks spec.remoteCompute first.
func remoteComputeConfigDefaults(nova *novav1alpha1.Nova) map[string]map[string]string {
	return computeFragment(nova, true)
}

// computeFragmentUsable reports whether every section of the fragment is free of
// newlines and carriage returns, and flips ComputeConfigReady False naming the
// first offending section, in name order, when one is not.
func computeFragmentUsable(nova *novav1alpha1.Nova, sections map[string]map[string]string) bool {
	for _, section := range slices.Sorted(maps.Keys(sections)) {
		if err := config.CheckNoControlChars(section, sections[section]); err != nil {
			markComputeConfigFalse(nova, conditionReasonComputeConfigError, err.Error())
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
//
// The remote contract follows the in-cluster one (reconcileRemoteComputeConfig).
// The condition turns True only once both are in the state the spec asks for.
// A remote contract that waits for its transport URL does not stop the
// pipeline: it is an output only a compute reads, so the control-plane steps
// behind this one keep running.
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
	if err := r.applyComputeContract(ctx, children, nova, name, componentComputeConfig,
		sections, transportURL, values); err != nil {
		markComputeConfigFalse(nova, conditionReasonComputeConfigError, err.Error())
		return ctrl.Result{}, fmt.Errorf("publishing the compute config Secret: %w", err)
	}
	nova.Status.ComputeConfigSecretRef = &corev1.LocalObjectReference{Name: name}

	settled, err := r.reconcileRemoteComputeConfig(ctx, children, nova, values)
	if err != nil || !settled {
		return ctrl.Result{}, err
	}

	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               conditionTypeComputeConfigReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: nova.Generation,
		Reason:             conditionReasonComputeConfigPublished,
	})
	return ctrl.Result{}, nil
}

// reconcileRemoteComputeConfig brings the remote contract into the state the
// spec asks for and reports whether it got there. Every path that returns
// settled=false has already set ComputeConfigReady False.
//
// Without spec.remoteCompute the remote Secret is deleted, unless a same-named
// Secret is not this Nova's. With it, the transport URL is read from the
// Secret the block names, in the Nova's namespace on the cluster its children
// run on. A missing Secret or key is a wait: the Secret published earlier, if
// any, stays as it was, and the Secret watch brings the Nova back once the
// value appears. The rendered fragment gets the same newline check as the
// in-cluster one.
func (r *NovaReconciler) reconcileRemoteComputeConfig(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, values secretValues,
) (settled bool, err error) {
	name := remoteComputeConfigSecretName(nova)
	rc := nova.Spec.RemoteCompute
	if rc == nil {
		stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nova.Namespace}}
		if derr := commonreconcile.DeleteOrphanedChildFunc(ctx, children, stale, func(live client.Object) bool {
			owned, cerr := commonmulticluster.Controls(r.Scheme, nova, live)
			return cerr == nil && owned
		}); derr != nil {
			markComputeConfigFalse(nova, conditionReasonComputeConfigError, derr.Error())
			return false, fmt.Errorf("deleting the remote compute config Secret: %w", derr)
		}
		nova.Status.RemoteComputeConfigSecretRef = nil
		return true, nil
	}

	transportURL, _, waitMessage, err := messaging.ResolveTransportURL(ctx, messaging.TransportURLSecretFlowParams{
		Client:    children,
		Namespace: nova.Namespace,
		Messaging: &commonv1.MessagingSpec{SecretRef: &rc.TransportURLSecretRef},
	})
	if err != nil {
		// The resolver never quotes the URL, which carries the broker password.
		markComputeConfigFalse(nova, conditionReasonComputeConfigError, err.Error())
		return false, fmt.Errorf("resolving the remote transport URL: %w", err)
	}
	if waitMessage != "" {
		markComputeConfigFalse(nova, conditionReasonWaitingForRemoteTransportURL,
			"spec.remoteCompute.transportURLSecretRef: "+waitMessage)
		return false, nil
	}

	sections := remoteComputeConfigDefaults(nova)
	if !computeFragmentUsable(nova, sections) {
		return false, nil
	}

	if err := r.applyComputeContract(ctx, children, nova, name, componentRemoteComputeConfig,
		sections, transportURL, values); err != nil {
		markComputeConfigFalse(nova, conditionReasonComputeConfigError, err.Error())
		return false, fmt.Errorf("publishing the remote compute config Secret: %w", err)
	}
	nova.Status.RemoteComputeConfigSecretRef = &corev1.LocalObjectReference{Name: name}
	return true, nil
}

// applyComputeContract writes one compute-contract Secret. Both contracts are
// written through it, so their key sets cannot drift apart: the rendered
// fragment, the transport URL, the two credentials that never enter a config
// file, the cell name, and the CA bundle on a verified bus.
func (r *NovaReconciler) applyComputeContract(ctx context.Context, children client.Client,
	nova *novav1alpha1.Nova, name, component string, sections map[string]map[string]string,
	transportURL string, values secretValues,
) error {
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
			Labels:    componentLabels(nova, component),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	return apply.EnsureObject(ctx, children, r.Scheme, nova, secret, apply.FieldManager)
}

// markComputeConfigFalse sets ComputeConfigReady False with reason and message.
func markComputeConfigFalse(nova *novav1alpha1.Nova, reason, message string) {
	conditions.SetCondition(&nova.Status.Conditions, metav1.Condition{
		Type:               conditionTypeComputeConfigReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: nova.Generation,
		Reason:             reason,
		Message:            message,
	})
}
