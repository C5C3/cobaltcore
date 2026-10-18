#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The remaining figures: chains, lanes and the hand-laid ones."""
import pathlib
import sys
from figlib import Fig, INK, INK_DARK, MUTED, GREY, LINE, PAPER, GL_FILL, FRAME_FILL, text_width
from flow_fig import chain_figure, lanes_figure

OUT = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parents[2] / "docs" / "diagrams"
figs = {}

# --- D7: FluxCD data flow of the keystone-operator HelmRelease ---------------
figs["deploy-keystone-operator-helmrelease"] = chain_figure(
    "deploy-keystone-operator-helmrelease",
    "How the Flux helm-controller installs the keystone-operator HelmRelease",
    [
        ("Reconcile the HelmRelease", ["checks dependsOn: cert-manager, mariadb-operator,", "memcached-operator and external-secrets are ready"]),
        ("Fetch the chart", ["from the c5c3-charts OCI HelmRepository"]),
        ("Install the CRDs", ["from crds/: keystone.openstack.c5c3.io_keystones.yaml"]),
        ("Render the templates", ["chart defaults merged with the HelmRelease values:",
                                  "ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment, Service,",
                                  "PodDisruptionBudget, Issuer, Certificate, the two webhook configurations,",
                                  "and NetworkPolicy and ServiceMonitor when opted in"]),
        ("Apply the resources", ["into the keystone-system namespace"]),
        ("cert-manager injects the CA", ["the inject-ca-from annotation on the webhook configurations",
                                         "fills caBundle from the Certificate"]),
        ("The operator pods start", ["leader election picks the active replica,", "which begins reconciling Keystone resources"]),
    ],
    footer=["Steps 1 to 5 are the helm-controller's; step 6 is cert-manager's; step 7 is the operator's."],
)

# --- D8: sub-condition progression the basic-deployment suite asserts --------
figs["test-keystone-condition-progression"] = chain_figure(
    "test-keystone-condition-progression",
    "The order the Keystone reconciler sets its conditions in, as the basic-deployment suite asserts them",
    [
        ("SecretsReady", ["True, reason SecretsAvailable"]),
        ("reconcileConfig", ["sets no condition; returns configMapName"]),
        ("FernetKeysReady", ["True, reason FernetKeysAvailable"]),
        ("DatabaseReady", ["True, reason DatabaseSynced"]),
        ("DeploymentReady", ["True, reason DeploymentReady"]),
        ("BootstrapReady", ["True, reason BootstrapComplete"]),
        ("Ready", ["True, reason AllReady: the aggregate of every sub-condition"]),
    ],
    footer=["The suite asserts the five sub-conditions and Ready in one step; the order is the execution order",
            "within Reconcile, not the order of subConditionTypes."],
)

# --- D9: configuration pipeline of reconcileConfig --------------------------
figs["service-keystone-config-pipeline"] = lanes_figure(
    "service-keystone-config-pipeline",
    "The eight stages reconcileConfig renders keystone.conf through",
    [("Configuration pipeline", "each stage reads what the stage before it produced",
      ["Spec fields", "Operator defaults", "Secret injection", "Plugin config merge",
       "ExtraConfig merge", "Policy override", "INI rendering", "Immutable ConfigMap"])],
    width=1560,
)

# --- D10: in-place key rotation ---------------------------------------------
figs["secrets-rotation-key-projection"] = lanes_figure(
    "secrets-rotation-key-projection",
    "How a rotated key reaches the running pods without a rollout",
    [("In-place key rotation", "no rollout: the pod template carries no hash of the key Secrets",
      ["The CronJob stages the keys", "The operator commits them", "The kubelet projects the new keys",
       "Running pods see the new key files"])],
    width=1560,
)

# --- E2: OpenBao bootstrap script order --------------------------------------
figs["secrets-openbao-bootstrap-order"] = chain_figure(
    "secrets-openbao-bootstrap-order",
    "The order the five OpenBao bootstrap scripts run in",
    [
        ("init-unseal.sh", ["initializes the Shamir keys and unseals every replica"]),
        ("setup-secret-engines.sh", ["enables the KV v2 and PKI engines"]),
        ("setup-auth.sh", ["configures the Kubernetes auth method and AppRole"]),
        ("setup-policies.sh", ["applies the 12 least-privilege HCL policies"]),
        ("write-bootstrap-secrets.sh", ["generates and seeds the initial passwords"]),
    ],
    footer=["Each script needs what the one before it wrote."],
)

# --- E3: Tempest image and test chain ----------------------------------------
figs["test-tempest-image-chain"] = _chain = None  # built below by hand

# --- E4: one tempest job, command by command ---------------------------------
figs["test-tempest-job-steps"] = chain_figure(
    "test-tempest-job-steps",
    "The commands of one tempest job in CI, in the order they run",
    [
        ("hack/nix-lock.sh", ["locks releases/<release>/test-refs.yaml into nix/locks/<release>/tempest/", "(tempest and the four plugin pins, committed)"]),
        ("build-e2e-images job", ["per release: hack/ci-nix-build-e2e-images.sh runs",
                                  "nix build .#tempest-<rel_>-image in the nixos/nix container, then",
                                  "docker load < .nix-e2e/tempest-<release>.tar"]),
        ("hack/ci-run-tempest.sh", ["per matrix.service and matrix.release:",
                                    "1. kubectl get secret keystone-admin -n openstack (the admin password)",
                                    "2. kubectl port-forward svc/<service-k8s-name> -n openstack 5000:5000, plus one",
                                    "   forward per <SERVICE>_K8S_NAME the leg sets; each polled on localhost, 10 attempts",
                                    "3. sed renders tempest.conf (localhost:5000 and the password) into _output/tempest/config;",
                                    "   exclude-tests.txt and the runner scripts are copied there",
                                    "4. include-tests.txt is split into phases/phase-1-core.txt and phases/phase-2-plugin.txt"]),
        ("docker run", ["--rm --network host, one --add-host pair per forwarded Service",
                        "(<name>.openstack.svc.cluster.local and <name>.openstack.svc to 127.0.0.1),",
                        "-v _output/tempest/config:/etc/tempest:ro, -v _output/tempest:/output,",
                        "-e TEMPEST_CONCURRENCY, TEMPEST_GROUP_START, TEMPEST_GROUP_END, TEMPEST_ERROR_PREFIX",
                        "ghcr.io/c5c3/tempest:<release> bash /etc/tempest/run-tests.sh"]),
        ("run-tests.sh", ["tempest init . and cp tempest.conf etc/",
                          "stestr run --include-list phases/phase-1-core.txt --subunit > /output/phase-1-core.subunit",
                          "stestr run --include-list phases/phase-2-plugin.txt --subunit > /output/phase-2-plugin.subunit",
                          "cat both > tempest.subunit; subunit2junitxml < tempest.subunit > tempest-results.xml",
                          "extract-failed.py lists the failed tests; stestr run --include-list retry-list.txt --concurrency 1",
                          "merge-retry-junit.py rewrites the resolved failures in tempest-results.xml"]),
        ("actions/upload-artifact", ["tempest-<service>-<release>-results, 14-day retention"]),
    ],
    footer=["The runner scripts never inline the steps of run-tests.sh: CI and local runs share that one code path."],
)

# --- F: chaos test patterns --------------------------------------------------
figs["test-chaos-patterns"] = lanes_figure(
    "test-chaos-patterns",
    "The six step patterns of the chaos suites",
    [
        ("Degradation and Recovery", "SC-CHAOS-001, SC-CHAOS-003: a critical dependency is killed",
         ["Apply CR", "Assert Ready=True", "Inject PodChaos", "Assert SubCondition=False", "Delete PodChaos", "Assert SubCondition=True and Ready=True"]),
        ("No-Regression", "SC-CHAOS-002: a non-critical dependency is killed",
         ["Apply CR", "Assert Ready=True", "Inject PodChaos", "Poll readyReplicas < desired", "Poll readyReplicas = desired",
          "Assert all conditions True", "Delete PodChaos", "Assert Ready=True"]),
        ("Operator Self-Recovery", "SC-CHAOS-004: one operator pod is killed",
         ["Apply CR", "Assert Ready=True", "Snapshot operator pod UIDs", "Inject PodChaos", "Wait one pre-chaos UID gone",
          "Wait readyReplicas = desired", "Delete PodChaos", "Assert Ready=True"]),
        ("Workload Fault Tolerance", "SC-CHAOS-005: a Job of the operator fails",
         ["Apply CR", "Assert Ready=True", "Inject PodChaos (pod-failure)", "Trigger the Job", "Assert conditions maintained",
          "Delete PodChaos", "Assert Ready=True"]),
        ("PDB Availability Guarantee", "SC-CHAOS-008: one API pod of three is killed",
         ["Apply CR (replicas: 3)", "Assert Ready=True", "Assert PDB minAvailable=1", "Inject PodChaos", "Verify availableReplicas >= 1",
          "Assert DeploymentReady=True and Ready=True", "Delete PodChaos", "Assert Ready=True"]),
        ("Operator Pod Kill All with Failover Reconciliation", "SC-CHAOS-009: every operator pod is killed",
         ["Apply CR", "Assert Ready=True", "Snapshot operator pod UIDs", "Inject PodChaos (mode: all)", "Wait all pre-chaos UIDs gone",
          "Wait readyReplicas = desired", "Delete PodChaos", "Assert all 6 conditions True", "Patch replicas 1 to 2",
          "Assert Deployment replicas=2 and Ready=True"]),
    ],
    width=1760,
)

# --- D1: DeepCopy condition merge --------------------------------------------
def parallel_merge():
    fid = "service-keystone-parallel-merge"
    fig = Fig(fid, "How parallel group 1 of the Keystone operator merges its conditions", 1500, 760)
    fig.box(60, 172, 340, 72, "Keystone CR (original)")
    fig.text(60, 252, "SecretsReady already set", fs=20, color=MUTED)
    copies = [("ksCopyFernet", "goroutine 1", "sets FernetKeysReady on the copy"),
              ("ksCopyCred", "goroutine 2", "sets CredentialKeysReady on the copy"),
              ("ksCopyNetpol", "goroutine 3", "sets NetworkPolicyReady on the copy")]
    ys = [60, 180, 300]
    for (label, g, cond), y in zip(copies, ys):
        fig.arrow([(400, 208), (470, 208), (470, y + 28), (560, y + 28)])
        fig.box(560, y, 240, 56, label)
        fig.text(560, y + 62, g, fs=20, color=MUTED)
        fig.text(560, y + 88, cond, fs=20, color=MUTED)
        if y == 300:
            fig.arrow([(800, y + 28), (1080, y + 28), (1080, 440)])
        else:
            fig.arrow([(800, y + 28), (1080, y + 28)], head=False)
    fig.text(482, 240, "DeepCopy", fs=20, color=INK, h=24)
    fig.arrow([(1080, 88), (1080, 328)], head=False)
    fig.box(960, 440, 240, 56, "errgroup.Wait()")
    fig.arrow([(1080, 496), (1080, 550)])
    fig.box(880, 550, 400, 56, "mergeParallelConditions")
    fig.text(880, 616, "sequential, one condition type per copy:", fs=20, color=MUTED)
    fig.text(880, 642, "copy 1 FernetKeysReady, copy 2 CredentialKeysReady,", fs=20, color=MUTED)
    fig.text(880, 668, "copy 3 NetworkPolicyReady, each into the original", fs=20, color=MUTED)
    fig.arrow([(880, 578), (230, 578), (230, 244)])
    fig.text(250, 550, "conditions written back; no mutex, no shared state", fs=20, color=MUTED)
    return fig

figs["service-keystone-parallel-merge"] = parallel_merge()

# --- D2/D3: branch on the deletion timestamp --------------------------------
def deletion_branch():
    fid = "service-keystone-deletion-branch"
    fig = Fig(fid, "How the Keystone reconciler branches on the deletion timestamp", 1720, 520)
    BXX = 760
    fig.dot(50, 108)
    fig.arrow([(60, 108), (120, 108)])
    fig.box(120, 80, 300, 56, "Fetch the Keystone")
    fig.text(120, 144, "NotFound ends the pass", fs=20, color=MUTED)
    fig.arrow([(420, 108), (480, 108), (480, 68), (BXX, 68)])
    fig.text(500, 36, "deletionTimestamp set", fs=20, color=INK, h=24)
    fig.box(BXX, 40, 320, 56, "reconcileDelete")
    fig.text(BXX + 335, 54, "the MariaDB finalizer", fs=20, color=MUTED)
    fig.arrow([(BXX + 160, 96), (BXX + 160, 130)])
    fig.box(BXX, 130, 320, 56, "reconcileDeleteOpenBao")
    fig.text(BXX + 335, 144, "the OpenBao finalizer", fs=20, color=MUTED)
    fig.text(BXX, 200, "no sub-reconciler runs against a terminating resource", fs=20, color=MUTED)
    fig.arrow([(420, 108), (480, 108), (480, 338), (BXX, 338)])
    fig.text(500, 306, "deletionTimestamp unset", fs=20, color=INK, h=24)
    fig.box(BXX, 310, 320, 56, "AddFinalizer")
    fig.text(BXX + 335, 297, "keystone.openstack.c5c3.io/finalizer and", fs=20, color=MUTED)
    fig.text(BXX + 335, 323, "keystone.openstack.c5c3.io/openbao-finalizer, each if missing;", fs=20, color=MUTED)
    fig.text(BXX + 335, 349, "a pass that installs one ends with a requeue after 1s", fs=20, color=MUTED)
    fig.arrow([(BXX + 160, 366), (BXX + 160, 410)])
    fig.box(BXX, 410, 320, 56, "the sub-reconcilers")
    fig.text(BXX + 335, 424, "the pipeline, see Reconciliation Flow", fs=20, color=MUTED)
    return fig

figs["service-keystone-deletion-branch"] = deletion_branch()

# --- E3: Tempest image chain --------------------------------------------------
def tempest_chain():
    fid = "test-tempest-image-chain"
    fig = Fig(fid, "From the Tempest version pins to the JUnit report", 1800, 1000)
    col = 60
    steps = [("releases/<release>/test-refs.yaml", ["the version pins: tempest and the barbican, keystone,", "neutron and cinder Tempest plugins"]),
             ("nix/locks/<release>/tempest/", ["the uv lock of the five pins and the clients", "written by hack/nix-lock.sh (make nix-lock)"]),
             ("images/tempest/image.nix", ["the Nix build over nix/lib/service-image.nix"])]
    y = 60
    prev = None
    for label, notes in steps:
        fig.box(col, y, 420, 56, label)
        for i, n in enumerate(notes):
            fig.text(col + 440, y + 1 + i * 26, n, fs=20, color=MUTED)
        if prev:
            fig.arrow([prev, (col + 210, y)])
        prev = (col + 210, y + 56)
        y += 110
    # fork
    fig.arrow([prev, (col + 210, y + 10), (col + 210, y + 10)], head=False)
    fig.arrow([(col + 210, y + 10), (col + 210, y + 40)], head=False)
    fig.arrow([(col + 210, y + 40), (300, y + 40), (300, y + 70)])
    fig.arrow([(col + 210, y + 40), (980, y + 40), (980, y + 70)])
    fy = y + 70
    # left frame: build-images.yaml
    fig.frame(80, fy, 640, 250)
    fig.text(99, fy + 13, "build-images.yaml", fs=26, bold=True, color=INK)
    fig.text(99, fy + 50, "builds, scans, signs and pushes to GHCR", fs=20, color=MUTED)
    for i, (job, note) in enumerate([("generate-matrix", "discovers the releases from releases/*/"),
                                     ("build-tempest", "per release and platform: amd64, arm64"),
                                     ("merge-tempest-image", "multi-arch manifest, SBOM, Grype, cosign")]):
        yy = fy + 90 + i * 50
        fig.box(100, yy, 240, 40, job, fs=20)
        fig.text(355, yy + 6, note, fs=18, color=MUTED)
    # right frame: ci.yaml
    fig.frame(780, fy, 960, 250)
    fig.text(799, fy + 13, "ci.yaml", fs=26, bold=True, color=INK)
    fig.text(799, fy + 50, "builds the image and runs Tempest per service and release", fs=20, color=MUTED)
    for i, (job, note) in enumerate([("build-e2e-images", "per release: hack/ci-nix-build-e2e-images.sh builds the image"),
                                     ("tempest (matrix)", "per leg: load the image, deploy the services, run Tempest, upload the JUnit")]):
        yy = fy + 90 + i * 50
        fig.box(800, yy, 240, 40, job, fs=20)
        fig.text(1055, yy + 6, note, fs=18, color=MUTED)
    # config dir into the tempest job
    cy = fy + 300
    fig.box(60, cy, 420, 56, "tests/tempest/<config-dir>/")
    fig.text(60, cy + 66, "tempest.conf: rendered with the localhost URI and the admin password", fs=20, color=MUTED)
    fig.text(60, cy + 92, "include-tests.txt: split into phases/phase-1-core.txt and phases/phase-2-plugin.txt", fs=20, color=MUTED)
    fig.text(60, cy + 118, "exclude-tests.txt: copied", fs=20, color=MUTED)
    fig.arrow([(480, cy + 28), (980, cy + 28), (980, fy + 250)])
    fig.text(1000, cy - 2, "prepared in _output/tempest/config,", fs=20, color=INK, h=24)
    fig.text(1000, cy + 24, "mounted at /etc/tempest", fs=20, color=INK, h=24)
    # result
    ry = cy + 170
    fig.arrow([(1260, fy + 250), (1260, ry)])
    fig.box(1080, ry, 360, 56, "tempest-results.xml", fill=GL_FILL, stroke=GREY)
    fig.text(1080, ry + 66, "_output/tempest/, uploaded as the JUnit artifact", fs=20, color=MUTED)
    fig.h = ry + 120
    return fig

figs["test-tempest-image-chain"] = tempest_chain()

# --- E1: build-images job graph ------------------------------------------------
def build_images_jobs():
    fid = "ci-build-images-jobs"
    fig = Fig(fid, "The jobs of the build-images workflow and what each needs", 1960, 1500)
    GX, BX, MX, VX = 60, 560, 1000, 1500
    BW, MW = 360, 440

    def family(y, title, gates, rows, extra=None, extra_h=0):
        """rows: list of (build, merge, verify or None, note)."""
        h = 110 + 56 * len(rows) + 30 + extra_h
        fig.frame(40, y, 1880, h)
        fig.text(59, y + 13, title, fs=26, bold=True, color=INK)
        gy = y + 60
        for i, g in enumerate(gates):
            fig.box(GX + 20, gy + i * 46, 240, 36, g, fs=20, fill=GL_FILL, stroke=GREY)
        bus_x = GX + 300
        gtop, gbot = gy + 18, gy + (len(gates) - 1) * 46 + 18
        for i in range(len(gates)):
            fig.arrow([(GX + 260, gy + i * 46 + 18), (bus_x, gy + i * 46 + 18)], head=False, color=GREY, sw=2)
        ry = y + 60
        rows_mid = []
        for build, merge, verify, note in rows:
            fig.box(BX, ry, BW, 44, build, fs=20)
            rows_mid.append(ry + 22)
            fig.arrow([(bus_x, ry + 22), (BX, ry + 22)])
            fig.arrow([(BX + BW, ry + 22), (MX, ry + 22)])
            fig.box(MX, ry, MW, 44, merge, fs=20)
            if verify:
                fig.arrow([(MX + MW, ry + 22), (VX, ry + 22)])
                fig.box(VX, ry, 380, 44, verify, fs=20)
            if note:
                fig.text(BX, ry + 48, note, fs=18, color=MUTED, h=24)
            ry += 56 + (26 if note else 0)
        fig.arrow([(bus_x, min(gtop, rows_mid[0])), (bus_x, max(gbot, rows_mid[-1]))], head=False, color=GREY, sw=2)
        if extra:
            extra(y, ry)
        return y + h

    y = 40
    fig.text(60, y, "needs: every job to the left of an arrow", fs=20, color=MUTED)
    y = 90
    y = family(y, "Release-independent images", ["changes", "nix-flake-check", "prepare"],
               [("build-keystone-federation-proxy", "merge-keystone-federation-proxy-image", None, "matrix: amd64 and arm64; merge on push only"),
                ("build-backup-shifter", "merge-backup-shifter-image", None, "matrix: amd64 and arm64; merge on push only"),
                ("build-libvirt", "merge-libvirt-image", None, "matrix: amd64 and arm64; merge on push only")]) + 30
    y = family(y, "Nix-locked images", ["changes", "nix-flake-check", "nix-lock-check", "prepare"],
               [("build-ovn", "merge-ovn-image", "verify-ovn-image", "matrix: amd64 and arm64; merge and verify on push only"),
                ("build-hvo", "merge-hvo-image", "verify-hvo-image", "matrix: amd64 and arm64; merge and verify on push only"),
                ("build-kna", "merge-kna-image", "verify-kna-image", "matrix: amd64 and arm64; merge and verify on push only")]) + 30

    def services_extra(y0, ry):
        # test-service-images feeds verify-service-images
        ty = ry + 10
        fig.box(BX, ty, BW, 44, "test-service-images", fs=20)
        fig.text(BX, ty + 48, "nix build .#<service>-<rel_>-test-run; needs changes, generate-matrix and nix-lock-check", fs=18, color=MUTED, h=24)
        fig.arrow([(BX + BW, ty + 22), (VX + 190, ty + 22), (VX + 190, y0 + 60 + 56 + 26 + 44)])
    y = family(y, "Release-matrix images", ["changes", "generate-matrix", "nix-flake-check", "nix-lock-check", "prepare"],
               [("build-tempest", "merge-tempest-image", None, "matrix: release and platform; merge on push only"),
                ("build-service-images", "merge-service-images", "verify-service-images", "matrix: service, release and platform; verify also needs test-service-images"),
                ("build-nova-compute-image", "merge-nova-compute-image", "verify-nova-compute-image", "matrix: release and platform; merge and verify on push only")],
               extra=services_extra, extra_h=90)
    fig.text(60, y + 30, "generate-matrix itself needs only changes. The merge and verify jobs also need prepare or generate-matrix, which the frame's gates cover.", fs=20, color=MUTED)
    fig.h = y + 90
    return fig

figs["ci-build-images-jobs"] = build_images_jobs()

for fid, fig in figs.items():
    if fig is None:
        continue
    fig.write(str(OUT / fid))
    print(fid, fig.w, fig.h)
