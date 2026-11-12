#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Draw the deployment catalog and environment composition, using figlib's house style.

Run from any directory. An optional output-directory argument supports previewing
without touching docs/diagrams. Arrows always mean a resources-directory import,
never a readiness dependency; runtime ordering belongs in the deployment docs.
"""
import pathlib
import sys

from figlib import Fig, INK, MUTED, PAPER, FRAME_FILL, GREY

OUT = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parents[2] / "docs" / "diagrams"
fig = Fig("deploy-overlay-inheritance", "Deployment composition: canonical bases, kind and metal-stack", 1808, 2360)
X = (48, 640, 1232)
W = 528


def text(x, y, label, fs=20, bold=False):
    fig.text(x, y, label, fs=fs, bold=bold, color=INK if bold else MUTED)


def card(x, y, title, lines=(), height=None, fs=22, dashed=False):
    height = height or 62 + 28 * len(lines)
    fig.frame(x, y, W, height, fill=FRAME_FILL, dashed="3 2" if dashed else None)
    text(x + 18, y + 12, title, fs=fs, bold=True)
    for i, line in enumerate(lines):
        text(x + 18, y + 48 + 28 * i, line)


def link(y, left=0):
    fig.arrow([(X[left] + W, y), (X[left + 1], y)])


def fork(y, top):
    link(y)
    fig.arrow([(X[0] + W, y), (X[1] - 30, y),
               (X[1] - 30, top - 14), (X[2] + W / 2, top - 14),
               (X[2] + W / 2, top)])


def section(y, label, detail):
    text(48, y, label, fs=28, bold=True)
    text(48, y + 40, detail)


text(48, 26, "Deployment composition", fs=32, bold=True)
text(48, 72, "Arrow: a resource directory points to the root that imports it. Composition does not imply readiness or apply order.")

section(126, "Canonical bases", "Every reusable manifest and bootstrap asset lives directly in its owning base. There are no compatibility aliases.")
card(X[0], 236, "bases/operators", ["namespaces.yaml", "sources/ and releases/", "Protects staged namespaces from pruning"], height=154)
card(X[1], 236, "bases/flux", ["fluxinstance.yaml", "Runtime installed before operator resources", "No automatic Git synchronization"], height=154)
card(X[2], 236, "bases/controlplane-operator", ["helmrelease.yaml", "Reconcile after operator/bootstrap gates", "ControlPlane instances belong to overlays"], height=154)
card(X[0], 448, "bases/tls", ["Certificate and ClusterIssuer manifests", "Requires ready cert-manager", "Precedes shared OpenBao installation"], height=154)
card(X[1], 448, "bases/shared-openbao", ["helmrelease.yaml", "bootstrap/ scripts and policies/ HCL", "Separate inputs retain their lifecycle gates"], height=154)
card(X[2], 448, "bases/eso-store", ["clustersecretstore.yaml", "Requires initialized shared OpenBao", "Retains namespace grant boundaries"], height=154)
card(X[0], 660, "bases/infrastructure", ["Reference MariaDB, Memcached and Garage", "Composes TLS and the shared ESO store", "Requires controller CRDs"], height=154)
card(X[1], 660, "bases/target-cluster-access", ["Helm chart, templates and intentional CRD copies", "make helm-package-target-cluster", "make verify-crd-sync"], height=154)
card(X[2], 660, "deploy/examples", ["base-customization: individual stage patches", "sizing-overlay: a staged composition", "Copy into the deployment repository"], height=154)
section(890, "Ownership and readiness", "A directory import composes objects; it does not wait for dependencies or assign a second Flux owner.")
card(X[0], 1000, "Deployment repositories", ["Select bases and one environment overlay", "Pin the revision and supply storage choices", "Keep one reconciler per resource identity"], height=154)
card(X[1], 1000, "Development composition", ["Flux + operators + OpenBao + c5c3", "Environment gateways and storage are separate", "Bootstrap gates remain in make deploy-infra"], height=154)
card(X[2], 1000, "Path migration", ["Old flux-system/openbao/eso folders removed", "Old GitOps aliases removed", "Update external imports to canonical bases"], height=154)

section(1218, "Sibling environment overlays", "Each environment imports the same shared bases. Gateway, storage and host settings belong to its overlay.")
for x, label in zip(X, ("Shared bases", "deploy/overlays/kind", "deploy/overlays/metal-stack")):
    text(x, 1300, label, fs=24, bold=True)
card(X[0], 1344, "deploy/bases/devstack/base", ["Controllers, UI and development sizing", "Composes Flux, operators, OpenBao and c5c3"], height=126)
card(X[1], 1344, "base", ["Local gateway and standard storage class", "make deploy-infra, Step 3"], height=126)
card(X[2], 1344, "base", ["Namespace metadata, storage and Gateway patches", "EXTERNAL_CLUSTER=true, Step 3"], height=126)
fork(1407, 1344)
card(X[0], 1498, "deploy/bases/devstack/infrastructure", ["TLS, ESO, development instances and secret shims", "Cluster-default storage; no environment gateway"], height=126, fs=21)
card(X[1], 1498, "infrastructure", ["Loopback certificates, NodePort and local storage", "make deploy-infra, Step 5"], height=126)
card(X[2], 1498, "infrastructure", ["Lab CA and public proxy; cluster-default storage", "EXTERNAL_CLUSTER=true, Step 5"], height=126)
fork(1561, 1498)

# Every environment row branches from the shared base into sibling overlays.
card(X[0], 1652, "deploy/bases/prometheus", ["Shared chart and dashboard; no Namespace", "Stage the real dashboard JSON here"], height=126, fs=21)
card(X[1], 1652, "prometheus-managed", ["prometheus-managed: base owns the Namespace", "prometheus: standalone root adds the Namespace"], height=126)
card(X[2], 1652, "prometheus", ["Managed base; persistent metrics, lab scrapes", "Step 3 with WITH_PROMETHEUS=true"], height=126)
fork(1715, 1652)

card(X[0], 1810, "deploy/bases: shared addons", ["chaos-mesh, dizzy, nfs", "messaging: shared broker in either mode", "Sources/releases have one canonical location."], height=146)
card(X[1], 1810, "chaos-mesh, dizzy, nfs", ["Local storage and NodePort exposure", "Step 3 with its WITH_* flag"], height=126)
card(X[2], 1810, "chaos-mesh, dizzy, nfs", ["Persistence, namespace labels and node modules", "Step 3 with its WITH_* flag"], height=126)
fork(1873, 1810)
card(X[0], 1964, "deploy/bases/hypervisor-fixtures", ["Common K-ORC fixtures and image"], height=94, fs=21)
card(X[1], 1964, "hypervisor-operator-fixtures", ["Applied by hand"], height=94)
card(X[2], 1964, "hypervisor-fixtures", ["Applied by hand, after the ControlPlane"], height=94)
fork(2011, 1964)

card(X[0], 2090, "Additional lab composition", [
    "controlplane -> controlplane-nfs",
    "migration-ports -> hypervisor",
    "Both overlays are applied by hand.",
    "ceph/cluster is a separate root, not a ceph base.",
], height=206)
card(X[1], 2090, "Other kind roots (no directory base)", [
    "metrics-server, vpa: opt-in Step 3",
    "controlplane: by hand / WITH_CONTROLPLANE_CR",
    "fake-compute: applied by hand",
], height=206, fs=21)
card(X[2], 2090, "Other lab roots (no directory base)", [
    "controlplane, migration-ports, probe: by hand",
    "dizzy-soak: make dizzy-soak-start",
    "ceph: WITH_CEPH, Step 3",
    "ceph/cluster: WITH_CEPH, Step 5",
], height=206, fs=21)
text(48, 2314, "Repository callers use canonical paths. Bootstrap policy keys and resource identities are unchanged; keep one owner per resource.")

OUT.mkdir(parents=True, exist_ok=True)
fig.write(str(OUT / "deploy-overlay-inheritance"))
