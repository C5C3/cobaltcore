#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Gate-tree figures: the four order reconcilers, the Keystone schema check and the upgrade dispatch."""
import pathlib
import sys
from flow_fig import gate_tree_figure, node, gate, goto, done

OUT = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parents[2] / "docs" / "diagrams"

FOOT = ["A chip is the reason both sub-conditions carry when the pass ends there; the note is the requeue.",
        "A bold name after an arrow is the function the pass continues in."]

ADMISSION = node("orderAdmission", None, [
    gate("cluster name longer than 63", "ClusterNameTooLong", "no requeue"),
    gate("ControlPlane read fails", "ControlPlaneNotFound", "10s; on a target cluster NamespaceNotAssigned, 1m"),
    gate("no namespace assignment", "NamespaceNotAssigned", "1m, the freeze"),
])

ORDERS = {
    "controlplane-keystoneservice-flow": dict(
        name="Reconciliation flow of the KeystoneService controller",
        roots=[("Reconcile", None, [
            goto("deleting?", "reconcileDelete", "see Deletion and Teardown"),
            node("EnsureFinalizer", "c5c3.io/keystoneservice-teardown, before any projection"),
            node("reconcileNormal", None, [
                node("setUndeclaredBlockConditions", "stamps NotDeclared on absent blocks"),
                gate("resolveControlPlane fails", "ControlPlaneNotFound"),
                gate("namespace not consented", "NamespaceNotAllowed"),
                gate("AdminCredentialReady not met", "WaitingForAdminCredential"),
                gate("role not on the allowlist (account only)", "RoleNotAllowed", "after the sweep"),
                node("ensureCatalog", "instrumented: KeystoneServiceCatalog"),
                node("ensureAccount", "instrumented: KeystoneServiceAccount; skipped when a role is refused"),
                node("sweepChildren", "prunes what the spec stopped declaring"),
            ]),
        ])],
    ),
    "controlplane-keystoneuser-flow": dict(
        name="Reconciliation flow of the KeystoneUser controller",
        roots=[("Reconcile", None, [
            goto("the order's cluster does not resolve", "log and end the pass", "no requeue"),
            goto("deleting?", "reconcileDelete", "see Deletion and Teardown"),
            node("reconcileNormal", None, [
                ADMISSION,
                node("EnsureFinalizer", "c5c3.io/keystoneuser-teardown"),
                gate("AdminCredentialReady not met", "WaitingForAdminCredential", "10s"),
                node("provisionUser", "instrumented: KeystoneUserProvision", [
                    done("schedule due: next password generation", "held 1m while a KeystoneApplicationCredential naming the user is being deleted"),
                    done("converged: timer at status.nextPasswordRotation"),
                ]),
                node("deliverCredentials", "instrumented: KeystoneUserDelivery"),
            ]),
        ])],
    ),
    "controlplane-mariadbdatabase-flow": dict(
        name="Reconciliation flow of the MariaDBDatabase controller",
        roots=[("Reconcile", None, [
            goto("the order's cluster does not resolve", "log and end the pass", "no requeue"),
            goto("deleting?", "reconcileDelete", "see Deletion and Teardown"),
            node("reconcileNormal", None, [
                ADMISSION,
                node("EnsureFinalizer", "c5c3.io/mariadbdatabase-teardown"),
                node("mariaDBDatabaseDynamicGate", None, [
                    gate("credentials mode is not Dynamic", "DynamicCredentialsUnavailable", "10m"),
                    gate("DBCredentialsReady not met", "WaitingForDBCredentials", "10s"),
                ]),
                node("provisionDatabase", "instrumented: MariaDBDatabaseProvision"),
                node("deliverCredentials", "instrumented: MariaDBDatabaseDelivery"),
            ]),
        ])],
    ),
    "controlplane-rabbitmqvhost-flow": dict(
        name="Reconciliation flow of the RabbitMQVhost controller",
        roots=[("Reconcile", None, [
            goto("the order's cluster does not resolve", "log and end the pass", "no requeue"),
            goto("deleting?", "reconcileDelete", "see Deletion and Teardown"),
            node("reconcileNormal", None, [
                node("orderAdmission", None, [
                    gate("cluster name longer than 63", "ClusterNameTooLong"),
                    gate("ControlPlane read fails", "ControlPlaneNotFound", "10s; on a target cluster NamespaceNotAssigned, 1m"),
                    gate("no namespace assignment", "NamespaceNotAssigned", "1m, the freeze"),
                ]),
                node("EnsureFinalizer", "c5c3.io/rabbitmqvhost-teardown"),
                node("rabbitMQVhostMessagingGate", None, [
                    gate("no messaging block", "MessagingNotDeclared"),
                    gate("brownfield secretRef", "MessagingNotManaged"),
                    gate("default-user Secret missing", "WaitingForMessagingCredentials", "10s"),
                    gate("not AllReplicasReady", "WaitingForMessaging", "10s"),
                ]),
                node("provisionVhost", "instrumented: RabbitMQVhostProvision"),
                node("deliverCredentials", "instrumented: RabbitMQVhostDelivery"),
            ]),
        ])],
    ),
    "service-keystone-schema-check-flow": dict(
        name="The database step of the Keystone operator with the schema check",
        roots=[("reconcileDatabase", None, [
            node("MariaDB CRs ready?", "managed mode only"),
            goto("upgrade path?", "reconcileUpgrade"),
            node("db_sync Job", "existing", [
                gate("failed", "DatabaseReady=False/DBSyncFailed", "returns the error"),
                gate("running", "DatabaseReady=False/DBSyncInProgress", "requeue 30s"),
                done("done: proceed to the schema check"),
            ]),
            node("schema-check Job", None, [
                gate("failed", "DatabaseReady=False/SchemaDriftDetected", "returns the error"),
                gate("running", "DatabaseReady=False/SchemaCheckInProgress", "requeue 30s"),
                done("done: proceed to DatabaseReady=True"),
            ]),
            done("status.installedRelease = spec.image.tag"),
            done("DatabaseReady=True/DatabaseSynced", "the message includes \"revision verified\""),
        ])],
        footer=["A chip is the condition and reason the step sets when it ends the pass there; the note is what it returns."],
    ),
    "service-keystone-upgrade-dispatch": dict(
        name="How the two sub-reconcilers of the Keystone operator share a release upgrade",
        roots=[
            ("reconcileDatabase", "called first in the reconciler chain", [
                node("active upgrade? (upgradePhase set)", None, [
                    goto("tag reverted to installedRelease", "abort", "deletes the phase Jobs, clears the state, requeues"),
                    gate("tag changed to another value", "UpgradeTargetChanged", "blocks"),
                    goto("otherwise", "reconcileUpgrade", "dispatches by phase"),
                ]),
                node("isUpgrade()?", "installedRelease set, tag differs, not patch-only", [
                    done("no: simple db_sync", "on completion sets installedRelease to the tag"),
                    goto("yes", "initiateUpgrade", "validates the path, sets targetRelease and Expanding, requeues"),
                ]),
                node("zero-value result"),
            ]),
            ("reconcileDeployment", None, [
                node("EnsureDeployment(spec.image.tag)"),
                done("upgradePhase RollingUpdate and the Deployment fully rolled out", "sets upgradePhase Contracting and requeues, back to reconcileDatabase"),
                done("otherwise: sets DeploymentReady and Endpoint"),
            ]),
        ],
        links=[("zero-value result", 1, "only then the pass continues to reconcileDeployment")],
        footer=["A chip is the condition reason the step sets when it ends the pass there."],
    ),
}

for fid, f in ORDERS.items():
    fig = gate_tree_figure(fid, f["name"], f["roots"], footer=f.get("footer", FOOT), links=f.get("links", ()))
    fig.write(str(OUT / fid))
    print(fid, fig.w, fig.h)
