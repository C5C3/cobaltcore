// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/cache"
	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// defaultConfigMapRetainCount is the number of historical immutable ConfigMaps
// to retain after pruning. Combined with the current active artefact, this
// allows rollback to 3 previous configurations.
const defaultConfigMapRetainCount = 3

// neutronConfigMountPath is the in-pod directory the rendered config ConfigMap
// is mounted at, read-only and as a whole volume, shadowing the image's own
// /etc/neutron. The API pods and the RPC workers pass its files to
// neutron-server as --config-file arguments, so every file the ConfigMap carries
// is named explicitly rather than picked up by a directory scan.
const neutronConfigMountPath = "/etc/neutron"

// loggingConfFilePath is the oslo.log fileConfig path [DEFAULT]
// log_config_append points at when spec.logging.format == "json".
const loggingConfFilePath = neutronConfigMountPath + "/logging.conf"

// neutronStatePath is the writable directory neutron-server keeps its runtime
// state under. The deployment step mounts an emptyDir there, so the path in the
// rendered config and the pod's volume layout stay in lockstep.
const neutronStatePath = "/var/lib/neutron"

// apiPasteConfigPath is the WSGI pipeline definition neutron-server loads. It is
// the file the image ships, not one the operator renders: the shipped pipeline
// already puts keystonemiddleware in front of the API, and pointing the option
// at anything else is what makes the key Rejected in the ownership registry.
const apiPasteConfigPath = "/var/lib/openstack/etc/neutron/api-paste.ini"

// The OVN client identity as the pods see it: the directory the mirrored
// <neutron.Name>-ovn-client Secret is projected at, and the three files inside
// it the ML2/OVN mechanism driver presents to, and verifies, both databases
// with.
const (
	ovnTLSMountPath   = "/etc/ovn/tls"
	ovnClientKeyPath  = ovnTLSMountPath + "/tls.key"
	ovnClientCertPath = ovnTLSMountPath + "/tls.crt"
	ovnClientCAPath   = ovnTLSMountPath + "/ca.crt"
)

// The CA bundle a TLS-secured broker connection is verified against: the
// directory spec.messaging.tls.caBundleSecretRef is projected at, and the file
// [oslo_messaging_rabbit] ssl_ca_file points at inside it.
const (
	rabbitmqCAMountPath = "/etc/rabbitmq-ca"
	rabbitmqCAFilePath  = rabbitmqCAMountPath + "/ca.crt"
)

// dbConnectionPlaceholder is the placeholder URL written to the [database]
// connection key in neutron.conf. The real URL is injected at runtime via the
// OS_DATABASE__CONNECTION env var sourced from the derived
// <neutron.Name>-db-connection Secret (oslo.config OS_<GROUP>__<OPTION>
// override). The placeholder MUST be a syntactically valid pymysql URL so
// oslo.config parses the file cleanly before the env override is applied.
const dbConnectionPlaceholder = "mysql+pymysql://placeholder"

// uwsgiINI is the uWSGI configuration the API pods start from. start-time is
// uWSGI's own magic variable "%t", the epoch second the master started, which
// uWSGI expands while it reads the file. The alternative was a placeholder a
// shell expands at container start, and that needs an entry point rewriting a
// file the kubelet projects read-only.
const uwsgiINI = "[uwsgi]\nstart-time = %t\n"

// The data keys of the rendered config ConfigMap. neutron-server reads the first
// two as --config-file arguments, in this order, so an ml2 option set in both
// files resolves to the ml2_conf.ini value.
const (
	neutronConfDataKey = "neutron.conf"
	ml2ConfDataKey     = "ml2_conf.ini"
	uwsgiConfDataKey   = "uwsgi.ini"
	loggingConfDataKey = "logging.conf"
)

// ml2Sections are the INI section names that belong in ml2_conf.ini rather than
// in neutron.conf. The ML2 plugin, its type drivers and the OVN mechanism driver
// read their options from the plugin file; everything else is neutron-server's
// own. The set is the routing table for both the operator defaults and
// spec.extraConfig, so a user override lands in the file its consumer reads.
//
// A section listed here that carries no key renders nowhere: the split routes
// names, the renderer emits sections.
var ml2Sections = map[string]struct{}{
	"ml2":             {},
	"ml2_type_flat":   {},
	"ml2_type_geneve": {},
	"ml2_type_gre":    {},
	"ml2_type_vlan":   {},
	"ml2_type_vxlan":  {},
	"ovn":             {},
	"ovn_nb_global":   {},
	"ovs":             {},
	"ovs_driver":      {},
	"securitygroup":   {},
	"sriov_driver":    {},
}

// conditionReasonConfigError is the SecretsReady=False reason set when
// reconcileConfig fails. Config artefacts (the rendered neutron.conf /
// ml2_conf.ini ConfigMap) gate the same downstream graph as the upstream
// credential Secrets, so failures reuse SecretsReady rather than a dedicated
// condition — matching reconcileDBConnectionSecret's Config→SecretsReady
// mapping.
const conditionReasonConfigError = "ConfigError"

// markConfigFailed flips SecretsReady to False so a reconcileConfig failure
// cannot leave the aggregate Ready condition stale-True at the new
// ObservedGeneration. It mirrors the sibling operators' markConfigFailed helper.
func markConfigFailed(neutron *neutronv1alpha1.Neutron, err error) {
	neutronSkeleton.MarkFailed(neutron, "SecretsReady", conditionReasonConfigError, err)
}

// reconcileConfig renders neutron.conf and ml2_conf.ini into one immutable
// ConfigMap, together with the uWSGI configuration and, for json logging, the
// oslo.log fileConfig, and returns the ConfigMap name for the database and
// deployment steps. Config failures flip SecretsReady=False (the
// Config→SecretsReady mapping the sibling operators use) so the aggregate Ready
// cannot stay stale-True at the new generation.
//
// It keeps no last-good artefact the way barbican does: the two OVN steps run
// ahead of it and short-circuit the pipeline while the endpoints or the client
// identity are unresolved, so there is no state in which this step could render
// a file against an incomplete projection.
func (r *NeutronReconciler) reconcileConfig(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron, ovn resolvedOVNEndpoints,
) (ctrl.Result, string, error) {
	// The extraConfig ownership guard is a pure function of the spec. The
	// ExtraConfigHealthy condition is informational and deliberately stays out of
	// subConditionTypes and subReconcilerConditionTypes.
	config.RecordExtraConfigHealth(r.Recorder, neutron, &neutron.Status.Conditions, neutron.Generation,
		config.FindOwnedOverrides(neutron.Spec.ExtraConfig, neutronv1alpha1.OwnedConfigKeys))

	merged := operatorDefaults(neutron, ovn)

	// logging is re-derived here (operatorDefaults keeps its own copy) for the
	// logging.conf data-key decision below.
	logging := effectiveLogging(neutron.Spec.Logging)

	// extraConfig overrides everything (the true escape hatch).
	if neutron.Spec.ExtraConfig != nil {
		merged = config.MergeDefaults(neutron.Spec.ExtraConfig, merged)
	}

	neutronConf, ml2Conf := splitML2Sections(merged)
	data := map[string]string{
		neutronConfDataKey: config.RenderINI(neutronConf),
		ml2ConfDataKey:     config.RenderINI(ml2Conf),
		uwsgiConfDataKey:   uwsgiINI,
	}
	if logging.Format == "json" {
		data[loggingConfDataKey] = config.RenderLoggingConf(logging.Level)
	}

	configMapName, err := config.CreateImmutableConfigMap(ctx, children, r.Scheme, neutron,
		neutron.Name+"-config", neutron.Namespace, data)
	if err != nil {
		markConfigFailed(neutron, err)
		return ctrl.Result{}, "", fmt.Errorf("creating config ConfigMap: %w", err)
	}
	if err := config.PruneImmutableConfigMaps(ctx, children, r.Scheme, neutron, config.PruneOptions{
		BaseName:    neutron.Name + "-config",
		Namespace:   neutron.Namespace,
		CurrentName: configMapName,
		Retain:      defaultConfigMapRetainCount,
	}); err != nil {
		markConfigFailed(neutron, err)
		return ctrl.Result{}, "", fmt.Errorf("pruning config ConfigMaps: %w", err)
	}

	return ctrl.Result{}, configMapName, nil
}

// splitML2Sections routes the merged sections to the file their consumer reads
// them from: the ml2Sections members to ml2_conf.ini, everything else to
// neutron.conf. Neither result shares a section map with the input, so the
// caller's merge result is left untouched.
func splitML2Sections(merged map[string]map[string]string) (neutronConf, ml2Conf map[string]map[string]string) {
	neutronConf = make(map[string]map[string]string, len(merged))
	ml2Conf = make(map[string]map[string]string, len(ml2Sections))
	for section, kvs := range merged {
		if _, isML2 := ml2Sections[section]; isML2 {
			ml2Conf[section] = kvs
			continue
		}
		neutronConf[section] = kvs
	}
	return neutronConf, ml2Conf
}

// operatorDefaults builds the operator-owned sections of both rendered files
// from the CRD spec and the resolved OVN endpoints. It is a pure function of the
// two (no cluster access), so it can be called directly to assert the rendered
// defaults stay in lockstep with neutronv1alpha1.OwnedConfigKeys.
//
// Nothing here depends on spec.openStackRelease: the option names neutron reads
// have not moved across the supported releases, so two CRs differing only in
// their release render byte-identical files.
func operatorDefaults(neutron *neutronv1alpha1.Neutron, ovn resolvedOVNEndpoints) map[string]map[string]string {
	logging := effectiveLogging(neutron.Spec.Logging)
	defaults := map[string]map[string]string{
		"DEFAULT": {
			"core_plugin": "ml2",
			// The OVN L3 plugin serves the routers; the ML2/OVN mechanism driver
			// implements the rest of the API in the Northbound model.
			"service_plugins":  "ovn-router",
			"auth_strategy":    "keystone",
			"api_paste_config": apiPasteConfigPath,
			"state_path":       neutronStatePath,
			// Both counts are zero because nothing in this deployment consumes
			// RPC: OVN answers DHCP and metadata out of the logical model and no
			// agent talks RPC, and neutron-rpc-server is not projected.
			"rpc_workers":              "0",
			"rpc_state_report_workers": "0",
			// OVN serves DHCP out of the logical model and no DHCP agent is
			// deployed, so the notifications would queue RPC casts nothing consumes.
			"dhcp_agent_notification": "false",
			// The two Nova notifications stay off until a Nova is deployed to
			// receive them; a port that waits for a vif-plugged event nothing sends
			// is worse than one that never announces itself.
			"notify_nova_on_port_status_changes": "false",
			"notify_nova_on_port_data_changes":   "false",
			// Route oslo.log records to stderr so kubectl logs surfaces them.
			"use_stderr": "true",
			// oslo.log gates several extra-verbose code paths on the debug flag
			// specifically, independent of the root logger level.
			"debug": fmt.Sprintf("%t", *logging.Debug),
		},
		// Neutron keeps its database options in the plain [database] section. The
		// real URL is materialized by reconcileDBConnectionSecret into a derived
		// Secret and injected at runtime via OS_DATABASE__CONNECTION.
		"database": {
			"connection": dbConnectionPlaceholder,
		},
		// The password is delivered through the OS_KEYSTONE_AUTHTOKEN__PASSWORD env
		// override the deployment step wires (keystoneauth.PasswordEnvVar), so it
		// never lands in the rendered file.
		"keystone_authtoken": keystoneauth.Section(keystoneauth.SectionParams{
			AuthURL:            neutron.Spec.KeystoneEndpoint,
			WWWAuthenticateURI: neutron.Spec.EffectiveKeystonePublicEndpoint(),
			Username:           neutron.Spec.ServiceUser.Username,
			ProjectName:        neutron.Spec.ServiceUser.ProjectName,
			UserDomainName:     neutron.Spec.ServiceUser.UserDomainName,
			ProjectDomainName:  neutron.Spec.ServiceUser.ProjectDomainName,
			RegionName:         neutron.Spec.Region,
			MemcachedServers:   cache.ResolveServers(&neutron.Spec.Cache),
		}),
		// Versioned notifications have no consumer in this control plane, so they
		// are dropped at the source rather than accumulating in a queue.
		"oslo_messaging_notifications": {
			"driver": "noop",
		},
		// The transport URL itself is never rendered: it arrives through the
		// OS_DEFAULT__TRANSPORT_URL env override sourced from the derived
		// transport-URL Secret, so the broker password stays out of the ConfigMap.
		"oslo_messaging_rabbit": messaging.RabbitSection(neutron.Spec.Messaging.TLS, rabbitmqCAFilePath),
		"oslo_concurrency": {
			"lock_path": neutronStatePath + "/lock",
		},
		// The section is empty and still rendered: neutron reads [nova] for the
		// notification credentials, and an absent section makes the first Nova
		// option a user adds through spec.extraConfig look like a new file rather
		// than a filled-in one.
		"nova": {},
		"ml2": {
			"mechanism_drivers":    "ovn",
			"type_drivers":         "geneve,flat",
			"tenant_network_types": "geneve",
			"extension_drivers":    "port_security",
		},
		"ml2_type_geneve": {
			"vni_ranges": "1:65536",
			// The Geneve header the chassis encapsulation reserves room for. A
			// larger header than the tunnel accounts for exceeds the path MTU.
			"max_header_size": "38",
		},
		"ml2_type_flat": {
			"flat_networks": "*",
		},
		"securitygroup": {
			"enable_security_group": "true",
		},
		"ovn": {
			"ovn_nb_connection":  ovn.nbAddress,
			"ovn_sb_connection":  ovn.sbAddress,
			"ovn_nb_private_key": ovnClientKeyPath,
			"ovn_nb_certificate": ovnClientCertPath,
			"ovn_nb_ca_cert":     ovnClientCAPath,
			"ovn_sb_private_key": ovnClientKeyPath,
			"ovn_sb_certificate": ovnClientCertPath,
			"ovn_sb_ca_cert":     ovnClientCAPath,
			// The gateway router scheduler: leastloaded spreads the routers over
			// the chassis that carry the gateway role.
			"ovn_l3_scheduler": "leastloaded",
			// OVN answers 169.254.169.254 on the chassis, which is what the
			// NeutronMetadataAgent proxies to Nova.
			"ovn_metadata_enabled": "true",
		},
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

// effectiveLogging returns the LoggingSpec to use for config rendering,
// materializing the production defaults (text/INFO/debug=false) through the
// shared LoggingSpec.Default when spec.logging is nil or partially filled — the
// case of a CR that bypassed the defaulting webhook. The returned value is a
// copy, so defaulting never writes back into the CR, and its Debug pointer is
// always non-nil.
func effectiveLogging(spec *neutronv1alpha1.LoggingSpec) neutronv1alpha1.LoggingSpec {
	var out neutronv1alpha1.LoggingSpec
	if spec != nil {
		out = *spec
	}
	out.Default()
	return out
}
