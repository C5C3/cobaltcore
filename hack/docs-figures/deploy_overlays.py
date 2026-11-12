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
fig = Fig("deploy-overlay-inheritance", "Deployment composition: public stages, compatibility paths, kind and lab", 1808, 2360)
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


def section(y, label, detail):
    text(48, y, label, fs=28, bold=True)
    text(48, y + 40, detail)


text(48, 26, "Deployment composition", fs=32, bold=True)
text(48, 72, "Arrow: a resource directory points to the root that imports it. Composition does not imply readiness or apply order.")

section(126, "Public stages and compatibility aliases", "Deployment repositories reconcile the public stages with their own ordering, patches and retention policies.")
for x, label in zip(X, ("Retained implementation root", "Public catalog", "Older GitOps import")):
    text(x, 208, label, fs=24, bold=True)

stages = [
    ("deploy/flux-system", "deploy/bases/operators", "deploy/gitops/bases/operators"),
    ("deploy/flux-system/infrastructure/tls", "deploy/bases/tls", "deploy/gitops/bases/tls"),
    ("deploy/flux-system/releases/openbao", "deploy/bases/shared-openbao", "deploy/gitops/bases/openbao"),
    ("deploy/eso", "deploy/bases/eso-store", "deploy/gitops/bases/secrets"),
    ("deploy/flux-system/releases/c5c3-operator", "deploy/bases/controlplane-operator", "deploy/gitops/bases/controlplane-operator"),
]
for row, paths in enumerate(stages):
    y = 252 + 78 * row
    for x, path in zip(X, paths):
        fig.box(x, y, W, 58, path, fs=20, fill=PAPER, stroke=GREY)
    link(y + 29)
    link(y + 29, 1)
text(48, 654, "Operators keeps a bounded adapter: excludes FluxInstance, shared OpenBao and c5c3; protects Namespaces from pruning.")
text(48, 682, "The raw manifest files stay in place. Shared OpenBao's public stage removes the storage-class pin; the environment supplies storage.")

section(744, "Catalog contents, examples and bootstrap inputs", "Leaf catalogs are positive resource lists; they do not create a second owner of the aggregate's objects.")
card(X[0], 828, "deploy/flux-system", [
    "sources, releases",
    "releases/openbao, releases/c5c3-operator",
    "plus raw namespaces.yaml and fluxinstance.yaml",
], height=154)
card(X[1], 828, "deploy/flux-system/infrastructure", [
    "tls, deploy/eso",
    "plus raw MariaDB, Memcached and Garage",
    "Apply once their controllers and CRDs exist.",
], height=154)
card(X[2], 828, "deploy/openbao", [
    "bootstrap, policies",
    "Package scripts and HCL as ConfigMaps.",
    "Environment-owned Jobs perform bootstrap.",
], height=154)
card(X[0], 1012, "deploy/examples/sizing-overlay", [
    "root imports deploy/flux-system",
    "infrastructure imports its infrastructure root",
    "Template to copy; retains aggregate ownership.",
], height=154, dashed=True)
card(X[1], 1012, "deploy/examples/base-customization", [
    "operators imports deploy/bases/operators",
    "shared-openbao imports its public stage",
    "Example controller sizing and storage patches.",
], height=154, dashed=True)
card(X[2], 1012, "Composition and readiness are separate", [
    "Flux controllers must already exist.",
    "Server TLS precedes OpenBao installation.",
    "Bootstrap precedes a usable ESO store.",
], height=154)

section(1218, "Existing environment flows", "make deploy-infra applies kind roots, or the external overlay interface under EXTERNAL_CLUSTER=true.")
for x, label in zip(X, ("Legacy foundation", "deploy/kind", "deploy/lab/metal-stack")):
    text(x, 1300, label, fs=24, bold=True)
card(X[0], 1344, "deploy/flux-system", ["Namespaces, FluxInstance, sources and releases", "Direct legacy consumers: kubectl apply -k"], height=126)
card(X[1], 1344, "base", ["Gateway and UI resources; one-node patches", "make deploy-infra, Step 3"], height=126)
card(X[2], 1344, "base", ["Namespace metadata, storage and Gateway patches", "EXTERNAL_CLUSTER=true, Step 3"], height=126)
link(1407)
link(1407, 1)
card(X[0], 1498, "deploy/flux-system/infrastructure", ["TLS, ESO and static reference instances", "Direct consumers: after controllers are ready"], height=126)
card(X[1], 1498, "infrastructure", ["Gateway certificates, secrets, proving OpenBao", "make deploy-infra, Step 5"], height=126)
card(X[2], 1498, "infrastructure", ["Lab CA and LoadBalancer; storage patches", "EXTERNAL_CLUSTER=true, Step 5"], height=126)
link(1561)
link(1561, 1)

# Prometheus has its own row: standalone -> managed -> lab. The other addons
# below have a direct kind -> lab edge, so the distinction cannot be missed.
card(X[0], 1652, "deploy/kind/prometheus", ["Standalone compatibility root includes Namespace", "Chart, dashboard ConfigMap; stage JSON first"], height=126, fs=21)
card(X[1], 1652, "prometheus-managed", ["Omits Namespace/monitoring; base owns it", "Step 3 with WITH_PROMETHEUS=true"], height=126)
card(X[2], 1652, "prometheus", ["Managed base; persistent metrics, lab scrapes", "Step 3 with WITH_PROMETHEUS=true"], height=126)
link(1715)
link(1715, 1)

card(X[0], 1810, "Optional addons", ["Each flag selects one environment's root.", "Shared output must have one reconciler.", "See the table for exact paths and appliers."], height=146)
card(X[1], 1810, "chaos-mesh, dizzy, nfs", ["One root per addon; no directory base", "Step 3 with its WITH_* flag"], height=126)
card(X[2], 1810, "chaos-mesh, dizzy, nfs", ["Each imports the same-named kind root", "Step 3 with its WITH_* flag"], height=126)
link(1873, 1)
card(X[1], 1964, "hypervisor-operator-fixtures", ["Applied by hand"], height=94)
card(X[2], 1964, "hypervisor-fixtures", ["Applied by hand, after the ControlPlane"], height=94)
link(2011, 1)

card(X[0], 2090, "Additional lab composition", [
    "controlplane -> controlplane-nfs",
    "migration-ports -> hypervisor",
    "Both overlays are applied by hand.",
    "ceph/cluster is a separate root, not a ceph base.",
], height=206)
card(X[1], 2090, "Other kind roots (no directory base)", [
    "metrics-server, vpa: opt-in Step 3",
    "messaging: WITH_MESSAGING after Step 5",
    "controlplane: by hand / WITH_CONTROLPLANE_CR",
    "fake-compute: applied by hand",
], height=206, fs=21)
card(X[2], 2090, "Other lab roots (no directory base)", [
    "controlplane, migration-ports, probe: by hand",
    "dizzy-soak: make dizzy-soak-start",
    "ceph: WITH_CEPH, Step 3",
    "ceph/cluster: WITH_CEPH, Step 5",
], height=206, fs=21)
text(48, 2314, "Raw-file callers, bootstrap policy keys and chart paths remain supported. Publishing a new import path does not transfer resource ownership.")

OUT.mkdir(parents=True, exist_ok=True)
fig.write(str(OUT / "deploy-overlay-inheritance"))
