// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// metadataAgentConfigFile is the data key of the rendered agent config and the
// file the container passes to neutron-ovn-metadata-agent as --config-file.
const metadataAgentConfigFile = "neutron_ovn_metadata_agent.ini"

// ovsdbSocketPath is the local Open vSwitch database the agent reads its node's
// port bindings from. The socket is the one the OVNChassis pods create on the
// host, which is why the two workloads have to share a node.
const ovsdbSocketPath = "unix:/run/openvswitch/db.sock"

// reconcileAgentConfig renders neutron_ovn_metadata_agent.ini into one immutable
// ConfigMap, together with the oslo.log fileConfig for json logging, and returns
// the ConfigMap name for the DaemonSet step. Config failures flip
// SecretsReady=False (the Config→SecretsReady mapping this operator uses on both
// kinds) so the aggregate Ready cannot stay stale-True at the new generation.
func (r *NeutronMetadataAgentReconciler) reconcileAgentConfig(ctx context.Context, children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent, chassis resolvedChassis,
) (ctrl.Result, string, error) {
	// The extraConfig ownership guard is a pure function of the spec. The
	// ExtraConfigHealthy condition is informational and deliberately stays out of
	// agentSubConditionTypes and subReconcilerConditionTypes.
	config.RecordExtraConfigHealth(r.Recorder, cr, &cr.Status.Conditions, cr.Generation,
		config.FindOwnedOverrides(cr.Spec.ExtraConfig, neutronv1alpha1.MetadataAgentOwnedConfigKeys))

	merged := agentOperatorDefaults(cr, chassis)

	// logging is re-derived here (agentOperatorDefaults keeps its own copy) for
	// the logging.conf data-key decision below.
	logging := effectiveLogging(cr.Spec.Logging)

	// extraConfig overrides everything (the true escape hatch).
	if cr.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(cr.Spec.ExtraConfig, merged)
	}

	data := map[string]string{metadataAgentConfigFile: config.RenderINI(merged)}
	if logging.Format == "json" {
		data[loggingConfDataKey] = config.RenderLoggingConf(logging.Level)
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, cr,
		cr.Name+"-config", cr.Namespace, data)
	if err != nil {
		err = fmt.Errorf("creating config ConfigMap: %w", err)
		agentSkeleton.MarkFailed(cr, "SecretsReady", conditionReasonConfigError, err)
		return ctrl.Result{}, "", err
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, r.Scheme, cr, config.PruneOptions{
		BaseName:    cr.Name + "-config",
		Namespace:   cr.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		err = fmt.Errorf("pruning config ConfigMaps: %w", err)
		agentSkeleton.MarkFailed(cr, "SecretsReady", conditionReasonConfigError, err)
		return ctrl.Result{}, "", err
	}

	return ctrl.Result{}, configMapName, nil
}

// agentOperatorDefaults builds the operator-owned sections of
// neutron_ovn_metadata_agent.ini from the CRD spec and the resolved chassis. It
// is a pure function of the two (no cluster access), so it can be called
// directly to assert the rendered defaults stay in lockstep with
// neutronv1alpha1.MetadataAgentOwnedConfigKeys.
//
// Three groups of keys are deliberately absent. metadata_proxy_shared_secret
// reaches the process as OS_DEFAULT__METADATA_PROXY_SHARED_SECRET, so the
// credential stays out of the ConfigMap every agent pod mounts. [DEFAULT]
// root_helper and the [privsep] helper_command keys stay at their oslo defaults
// of "sudo" and "sudo privsep-helper": the image ships /usr/bin/sudo and the
// container runs as root, so the defaults resolve.
func agentOperatorDefaults(cr *neutronv1alpha1.NeutronMetadataAgent, chassis resolvedChassis) map[string]map[string]string {
	logging := effectiveLogging(cr.Spec.Logging)
	defaults := map[string]map[string]string{
		"DEFAULT": {
			"state_path": neutronStatePath,
			// oslo.log gates several extra-verbose code paths on the debug flag
			// specifically, independent of the root logger level.
			"debug": fmt.Sprintf("%t", *logging.Debug),
		},
		// The local database the agent watches its node's port bindings in.
		"ovs": {
			"ovsdb_connection": ovsdbSocketPath,
		},
		// The Southbound database and the client identity the agent presents to
		// it, the same keypair the chassis pods on this node mount.
		"ovn": {
			"ovn_sb_connection":  chassis.sbAddress,
			"ovn_sb_private_key": ovnClientKeyPath,
			"ovn_sb_certificate": ovnClientCertPath,
			"ovn_sb_ca_cert":     ovnClientCAPath,
		},
		// Versioned notifications have no consumer in this control plane, so they
		// are dropped at the source rather than accumulating in a queue.
		"oslo_messaging_notifications": {
			"driver": "noop",
		},
		"oslo_concurrency": {
			"lock_path": neutronStatePath + "/lock",
		},
	}

	if cr.Spec.NovaMetadata != nil {
		// An empty host leaves the oslo default in place; the webhook fills the
		// port with 8775 when the block is set, so a zero here is a CR that
		// bypassed admission.
		if cr.Spec.NovaMetadata.Host != "" {
			defaults["DEFAULT"]["nova_metadata_host"] = cr.Spec.NovaMetadata.Host
		}
		if cr.Spec.NovaMetadata.Port != 0 {
			defaults["DEFAULT"]["nova_metadata_port"] = fmt.Sprintf("%d", cr.Spec.NovaMetadata.Port)
		}
	}

	// The broker section is rendered only for an agent that names a bus. The
	// transport URL itself never lands here: it arrives through the
	// OS_DEFAULT__TRANSPORT_URL env override, so the broker password stays out of
	// the ConfigMap.
	if cr.Spec.Messaging != nil {
		defaults["oslo_messaging_rabbit"] = messaging.RabbitSection(cr.Spec.Messaging.TLS, rabbitmqCAFilePath)
	}

	// PerLoggerLevels render into oslo.log's default_log_levels CSV; empty omits
	// the key so oslo.log keeps its compiled-in defaults.
	if v := config.RenderSortedPairs(logging.PerLoggerLevels, "="); v != "" {
		defaults["DEFAULT"]["default_log_levels"] = v
	}
	// format=json ships a logging.conf and points oslo.log at it via
	// log_config_append.
	if logging.Format == "json" {
		defaults["DEFAULT"]["log_config_append"] = loggingConfFilePath
	}

	return defaults
}
