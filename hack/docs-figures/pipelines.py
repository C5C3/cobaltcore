#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The eleven sub-reconciler pipeline figures, drawn from the controllers' step slices."""
import pathlib
import sys
from pipeline_fig import pipeline_figure, seq, group

OUT = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parents[2] / "docs" / "diagrams"

SUB = "one entry after another; the first that returns a requeue or an error ends the pass"
EXIT = ["aggregates Ready from the sub-conditions", "writes the status only when it changed"]
FOOT_LABEL = "A step label is the sub_reconciler value of the operator's metrics."
FOOT_PATTERN = "The Keystone operator draws the same pattern with two parallel groups in service-reconciler-pipeline."


def api_group(api_cond, extra=()):
    members = [("HTTPRoute", "sets HTTPRouteReady"),
               ("HealthCheck", f"sets {api_cond}"),
               ("HPA", "sets HPAReady"),
               ("VPA", "sets VPAReady"),
               ("NetworkPolicy", "sets NetworkPolicyReady")]
    members.extend(extra)
    return group("Parallel group", members)


FIGURES = {
    "service-glance-pipeline": dict(
        name="Sub-reconciler pipeline of the Glance operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on authTokenDigest"),
            seq("DBConnectionSecret", "reports through SecretsReady", "hands on dsnDigest"),
            seq("Backends", "sets BackendsReady", "hands on the backend projection", "never ends the pass"),
            seq("Config", "reports through SecretsReady", "reads the projection", "hands on configMapName"),
            seq("Database", "sets DatabaseReady", "reads configMapName"),
            seq("Deployment", "sets DeploymentReady", "reads the two digests", "sets status.endpoint"),
            api_group("GlanceAPIReady", [("DBPurge", "sets DBPurgeReady")]),
        ],
    ),
    "service-barbican-pipeline": dict(
        name="Sub-reconciler pipeline of the Barbican operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on authtokenDigest"),
            seq("DBConnectionSecret", "reports through SecretsReady", "hands on dsnDigest"),
            seq("SecretStores", "sets SecretStoresReady", "hands on the store projection", "never ends the pass"),
            seq("Config", "reports through SecretsReady", "reads the projection", "hands on configSecretName"),
            seq("DBClean", "sets DBCleanReady", "reads configSecretName"),
            seq("Database", "sets DatabaseReady", "reads configSecretName"),
            seq("Deployment", "sets DeploymentReady", "reads the projection and the two digests", "sets status.endpoint"),
            api_group("BarbicanAPIReady"),
        ],
    ),
    "service-placement-pipeline": dict(
        name="Sub-reconciler pipeline of the Placement operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on authtokenDigest"),
            seq("DBConnectionSecret", "reports through SecretsReady", "hands on dsnDigest"),
            seq("Config", "reports through SecretsReady", "hands on configMapName"),
            seq("Database", "sets DatabaseReady", "reads configMapName"),
            seq("Deployment", "sets DeploymentReady", "reads configMapName and the two digests", "sets status.endpoint"),
            api_group("PlacementAPIReady"),
        ],
    ),
    "service-horizon-pipeline": dict(
        name="Sub-reconciler pipeline of the Horizon operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on secretKeyHash"),
            seq("Config", "sets ConfigReady", "hands on configMapName", "never requeues"),
            seq("Deployment", "sets DeploymentReady", "reads configMapName and secretKeyHash", "sets status.endpoint"),
            seq("(prune)", "no name and no condition of its own", "drops old ConfigMaps; a failure flips ConfigReady"),
            api_group("HorizonAPIReady"),
        ],
        footer=[FOOT_LABEL + " (prune) has none.", FOOT_PATTERN],
    ),
    "service-cinder-pipeline": dict(
        name="Sub-reconciler pipeline of the Cinder operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on authTokenDigest"),
            seq("DBConnectionSecret", "reports through SecretsReady", "hands on dsnDigest"),
            seq("TransportURLSecret", "reports through SecretsReady", "hands on transportDigest and egressPort"),
            seq("Backends", "sets BackendsReady", "hands on the backends and shareHosts", "never ends the pass"),
            seq("BackupBackend", "sets BackupBackendReady", "hands on the backup backend", "never ends the pass"),
            seq("Config", "reports through SecretsReady", "hands on configMapName"),
            seq("Database", "sets DatabaseReady", "reads configMapName and the backend count"),
            seq("Scheduler", "sets SchedulerReady", "reads the digests and egressPort"),
            seq("VolumeServices", "sets VolumeServicesReady", "one Deployment per projected backend"),
            seq("BackupService", "sets BackupServiceReady", "reads the backup backend and the backends"),
            seq("Deployment", "sets DeploymentReady", "reads the digests", "sets status.endpoint"),
            seq("DBPurge", "sets DBPurgeReady", "reads configMapName"),
            api_group("CinderAPIReady"),
        ],
    ),
    "compute-nova-pipeline": dict(
        name="Sub-reconciler pipeline of the Nova operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on the values and two digests"),
            seq("DBConnectionSecrets", "reports through SecretsReady", "hands on apiDSNDigest and cellDSNDigest"),
            seq("TransportURLSecret", "reports through SecretsReady", "hands on transportURL, its digest and egressPort"),
            seq("Config", "reports through SecretsReady", "hands on configMapName"),
            seq("ComputeConfig", "sets ComputeConfigReady", "reads transportURL and the values"),
            seq("Database", "sets DatabaseReady", "reads configMapName"),
            seq("Conductor", "sets ConductorReady", "reads the digests and egressPort"),
            seq("Scheduler", "sets SchedulerReady", "reads the digests and egressPort"),
            seq("Metadata", "sets MetadataReady", "reads the digests"),
            seq("ConsoleProxy", "sets ConsoleProxyReady", "reads the digests"),
            seq("Deployment", "sets DeploymentReady", "reads the digests", "sets status.endpoint"),
            seq("DBArchive", "sets DBArchiveReady", "reads configMapName"),
            group("Parallel group", [
                ("HTTPRoute", "sets HTTPRouteReady"),
                ("MetadataHTTPRoute", "sets MetadataHTTPRouteReady"),
                ("ConsoleHTTPRoute", "sets ConsoleHTTPRouteReady"),
                ("HealthCheck", "sets NovaAPIReady"),
                ("HPA", "sets HPAReady"),
                ("VPA", "sets VPAReady"),
                ("NetworkPolicy", "sets NetworkPolicyReady"),
            ]),
        ],
    ),
    "compute-novacompute-pipeline": dict(
        name="Pipeline of the NovaCompute controller",
        entries=[
            seq("NovaRef", "sets NovaReady"),
            seq("Nodes", "sets NodesReady"),
            seq("PoolConfig", "sets ConfigReady"),
            seq("DaemonSet", "sets DaemonSetReady"),
            seq("ComputeVPA", "sets VPAReady"),
            seq("Aggregates", "sets AggregatesReady"),
            seq("Services", "sets ServicesReady", "hands its poll of Nova to Traits", "never ends the pass"),
            seq("Traits", "sets TraitsReady", "returns the shorter of its requeue and the poll"),
        ],
        footer=[FOOT_LABEL, "There is no parallel group: every step runs in order."],
    ),
    "service-neutron-pipeline": dict(
        name="Sub-reconciler pipeline of the Neutron operator",
        entries=[
            seq("Secrets", "sets SecretsReady", "hands on authtokenDigest and novaNotifierDigest"),
            seq("DBConnectionSecret", "reports through SecretsReady", "hands on dsnDigest"),
            seq("TransportURLSecret", "reports through SecretsReady", "hands on transportDigest and egressPort"),
            seq("OVNEndpoints", "sets OVNEndpointsReady", "hands on the resolved OVN endpoints"),
            seq("OVNClientSecret", "reports through OVNEndpointsReady", "reads the OVN endpoints", "hands on ovnClientDigest"),
            seq("Config", "reports through SecretsReady", "reads the OVN endpoints", "hands on configMapName"),
            seq("OVNDBSync", "sets OVNDBSyncReady", "reads configMapName"),
            seq("Database", "sets DatabaseReady", "reads configMapName"),
            seq("Deployment", "sets DeploymentReady", "reads configMapName and the five digests", "sets status.endpoint"),
            seq("Workers", "sets WorkersReady", "reads configMapName and the five digests"),
            api_group("NeutronAPIReady"),
        ],
    ),
    "compute-neutronmetadataagent-pipeline": dict(
        name="Pipeline of the NeutronMetadataAgent controller",
        entries=[
            seq("Chassis", "sets ChassisReady", "hands on the resolved chassis"),
            seq("Secrets", "sets SecretsReady", "hands on transportDigest and sharedSecretDigest"),
            seq("Config", "reports through SecretsReady", "reads the chassis", "hands on configMapName"),
            seq("DaemonSet", "sets DaemonSetReady", "reads the chassis, configMapName and the digests", "sets status.installedImage"),
            seq("AgentVPA", "sets VPAReady"),
        ],
        footer=[FOOT_LABEL, "There is no parallel group: every step runs in order."],
    ),
    "compute-ovncentral-pipeline": dict(
        name="Sub-reconciler pipeline of the OVNCentral controller",
        entries=[
            seq("TLS", "sets TLSReady", "publishes status.clientSecretName"),
            seq("Northbound", "sets NorthboundReady"),
            seq("Southbound", "sets SouthboundReady"),
            seq("Endpoints", "sets EndpointsReady", "stamps internalDbAddress and dbAddress"),
            group("Parallel group", [
                ("Northd", "sets NorthdReady", "copies status.installedImage back"),
                ("Relay", "sets RelayReady", "copies the relay addresses back"),
                ("Backup", "sets BackupReady"),
                ("VPA", "sets VPAReady"),
            ]),
        ],
    ),
    "compute-ovnchassis-pipeline": dict(
        name="Pipeline of the OVNChassis controller",
        entries=[
            seq("Central", "sets CentralReady", "hands on the resolved central"),
            seq("ClientSecret", "reports through CentralReady", "hands on clientSecretName"),
            seq("Nodes", "sets NodesReady", "hands on the rendered nodes"),
            seq("OVS", "sets OVSReady"),
            seq("Controller", "sets ControllerReady", "reads the central", "sets status.installedImage"),
            seq("ChassisVPA", "sets VPAReady"),
            seq("Maintenance", "sets MaintenanceReady", "reads the central and the nodes"),
        ],
        footer=[FOOT_LABEL, "There is no parallel group: every step runs in order."],
    ),
}

for fid, f in FIGURES.items():
    spec = dict(fid=fid, name=f["name"], title="Pipeline", subtitle=SUB, entries=f["entries"],
                exit_notes=EXIT, footer=f.get("footer", [FOOT_LABEL, FOOT_PATTERN]))
    pipeline_figure(spec).write(str(OUT / fid))
    print("wrote", fid)
