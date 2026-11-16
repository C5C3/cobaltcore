---
title: BGP Node Substrate
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# BGP Node Substrate

This page describes the speaker that carries the node side of the
[BGP Fabric Contract](bgp-fabric-contract.md) on a CobaltCore OpenStack node:
frr-k8s, a DaemonSet that runs FRR on every node that opts in and renders its
configuration from `FRRConfiguration` objects. It records the decisions the BGP
meta [#899](https://github.com/C5C3/cobaltcore/issues/899) left to this phase,
the install, what a node needs before frr-k8s can peer, and the per-node
configuration that the OVN phase
[#1363](https://github.com/C5C3/cobaltcore/issues/1363) will generate and the
Neutron phase [#1364](https://github.com/C5C3/cobaltcore/issues/1364) builds on.
Until #1363 delivers its reconciler, a person writes that configuration by
hand.

## Decisions

**D1, speaker.** The speaker is frr-k8s. Its typed API carries the sessions,
the VRF, BFD and the filters, and `raw.rawConfig` carries what the typed API
cannot say. frr-k8s v0.0.27 lets a neighbor announce only the prefixes listed
in `routers[].prefixes` of its router or of an imported one, and it has no
`redistribute`. OVN's dynamic host routes therefore need `redistribute kernel`
and a `ge 32` entry in the generated outbound list, which the raw snippet adds
(see [What the typed API cannot say](#what-the-typed-api-cannot-say)). A plain
FRR DaemonSet was rejected: CobaltCore OpenStack would own the per-node
rendering, the reload, the status and the validation. A reconciler that mirrors
OVN's routes into `prefixes` was rejected too, because every floating IP would
then pass an operator, the API server and an FRR reload.

**D2, ownership.** This phase documents the per-node `FRRConfiguration` and
ships the example its suite applies; a person writes it by hand. The reconciler
that generates it arrives in #1363 on `OVNChassis`, beside the device
preparation that creates the VRF, the loopback and the peer bridges it needs.

**D3, nodes that already run FRR.** On such a node the host FRR carries the
fabric VRF, and frr-k8s never runs there. frr-k8s runs with `hostNetwork: true`
(the chart's `templates/controller.yaml`), and two zebra instances in one
network namespace are not a supported FRR deployment. A metal-stack machine,
the lab included, takes the stanza of
[Nodes with a host FRR](#nodes-with-a-host-frr) in its host FRR by hand.

**D4, placement.** frr-k8s is an opt-in overlay under
`deploy/overlays/kind/frr-k8s/`, switched on with `WITH_FRR_K8S=true`. It is
not part of `deploy/bases/operators/`, because everything listed there reaches
every control plane through `deploy/bases/operators/kustomization.yaml`. A
compute cluster applies the overlay's files with its own Flux, as it does for
the hypervisor operator
([Configure the hypervisor operator](../../guides/nova/connect-a-compute-cluster.md#_7-configure-the-hypervisor-operator)).

**D5, hardware.** The kind suite of the `e2e-bgp-fabric` CI job is the gate of
this phase. The [hardware acceptance](#hardware-acceptance) table has an empty
record until a run on real ports fills it in, as the contract page's
[leaf-pair acceptance](bgp-fabric-contract.md#leaf-pair-acceptance) does for
the leaves.

## Install

On kind, deploy the stack with the overlay:

```bash
WITH_FRR_K8S=true make deploy-infra
```

`hack/deploy-infra.sh` applies `deploy/overlays/kind/frr-k8s` in Step 3 and
waits for the HelmRelease `frr-k8s` in Phase 3. `EXTERNAL_CLUSTER=true` refuses
the flag, because a metal-stack machine runs its own FRR (D3).

A compute cluster applies three files of that directory through its own Flux:
`namespace.yaml` (the Namespace `frr-k8s-system` at the privileged PodSecurity
level), `source.yaml` (the HelmRepository) and `release.yaml` (the
HelmRelease). Nothing in the control plane needs to change.

The release is inert until a node carries the label that opts it in:

```bash
kubectl label node <node> openstack.c5c3.io/frr-k8s=true
```

With no labelled node, the DaemonSet `frr-k8s` has no pod, and the Deployment
`frr-k8s-statuscleaner` serves the validating webhook alone. On a labelled
node, the DaemonSet's pod runs in the host network namespace, and its FRR
container holds the capabilities `NET_ADMIN`, `NET_RAW`, `SYS_ADMIN` and
`NET_BIND_SERVICE` (the chart's `values.yaml`). bgpd runs with `-p 0` and
accepts no incoming connection, so the node always opens its sessions itself.

The chart installs its four CRDs as templates. Removing the release deletes
them, and with them every `FRRConfiguration` on the cluster.

## Node prerequisites

frr-k8s configures FRR; it creates no device. Until #1363 delivers the device
preparation, a person sets these up on each node by hand. The suite's
`node-prep.sh` and `peer.sh` set them up on a kind node.

| Prerequisite | Why |
| --- | --- |
| The `vrf` kernel module | The fabric VRF is a Linux VRF device |
| A VRF device named `fabric` | C10: the uplinks, the loopback, the sessions and the leaf defaults live in it, away from the main table |
| The fabric loopback, one /32 and one /128, on the device `fabric` | C2: the addresses the node announces, apart from the `InternalIP` |
| Both uplinks enslaved to `fabric` | C3 and C10: the unnumbered sessions run over them inside the VRF |
| `accept_ra=0` on both uplinks, set before they come up | The leaf sends router advertisements for the unnumbered session. A kind node takes one as a kernel default route in the VRF (`proto ra`, distance 0), which keeps the BGP IPv6 default out of the table |
| No other FRR in the host network namespace | D3: two zebra instances in one namespace are not a supported FRR deployment |

## The per-node FRRConfiguration

The configuration of one node, as the suite applies it. `NODE_NAME` is a
placeholder; the suite replaces it with `sed`, and a person replaces it by
hand. It lives in the namespace frr-k8s runs in, as upstream requires.

<<< ../../../tests/e2e-bgp-fabric/frr-k8s-fabric-peering/03-frrconfiguration.yaml

| Field | Contract clause |
| --- | --- |
| `routers[].id` and `routers[].prefixes` | C2: the router ID is the IPv4 fabric loopback, and the router announces the /32 and the /128 |
| `routers[].asn`, `neighbors[].interface` and `neighbors[].dynamicASN: external` | C3: one private 4-byte ASN per node, one unnumbered eBGP session per uplink |
| `neighbors[].toAdvertise` and `neighbors[].toReceive` | C4: `mode: all` announces the router's listed prefixes, not its RIB; the node accepts `0.0.0.0/0` and `::/0` alone |
| `bfdProfiles` and `neighbors[].bfdProfile` | C5: BFD 300 ms × 3 on every session. `maximum-paths` has no field; the suite checks that both defaults are installed over both uplinks |
| `routers[].vrf` | C10: the router runs in the fabric VRF |

## What the typed API cannot say

OVN announces a floating IP or a VM address as a kernel route in its
`ovnvrf<id>` VRF. The typed API can import that VRF into the fabric router, but
it cannot redistribute kernel routes, and the outbound list it generates allows
only the listed prefixes. The raw snippet adds both, here for a stand-in VRF
`ovnvrf42`:

<<< ../../../tests/e2e-bgp-fabric/frr-k8s-fabric-peering/04-frrconfiguration-kernel-routes.yaml

frr-k8s names the outbound list of a session `<neighbor ID>-allowed-<family>`,
and the ID of an unnumbered neighbor in a VRF is `<interface>-<vrf>`
(`internal/frr/config.go`, `ID()` and `ToAdvertisePrefixListV4()`). The four
lists of this node are `fab1-fabric-allowed-ipv4`, `fab2-fabric-allowed-ipv4`,
`fab1-fabric-allowed-ipv6` and `fab2-fabric-allowed-ipv6`. The generated entries
start at `seq 1`, so the snippet appends at `seq 100`.

::: warning The raw snippet is unsupported upstream
frr-k8s v0.0.27 says of it, in `api/v1beta1/frrconfiguration_types.go`: "The
RawConfig feature is UNSUPPORTED and intended ONLY FOR EXPERIMENTATION". A
chart bump can rename the generated lists, renumber their entries or change
the order in which raw snippets are merged, and the snippet then widens a list
that no longer exists. Step S9 of the suite fails on such a change, and
Renovate never merges an frr-k8s bump without review.
:::

## Nodes with a host FRR

A metal-stack machine runs FRR in its host network namespace, and the lab does
too. Such a node never carries the label `openstack.c5c3.io/frr-k8s=true`
(D3). Its host FRR carries the fabric VRF instead, with the stanza the lab's
node runs:

<<< ../../../tests/lab/bgp-fabric/node1/frr.conf

On a metal-stack machine, metal-networker owns the host's `frr.conf` and may
rewrite it on a reinstall. After a reinstall, check that the stanza is still
there.

## Observing

The sessions of every labelled node, with their BGP and BFD state:

```bash
kubectl get bgpsessionstates -n frr-k8s-system
```

The result of the last configuration change on one node, with the running FRR
configuration:

```bash
kubectl get frrnodestate <node> -o yaml
```

`status.lastConversionResult` and `status.lastReloadResult` are `success` once
FRR runs what the `FRRConfiguration` objects say. For FRR's own view, run
`vtysh` in the `frr` container of the node's pod:

```bash
kubectl exec -n frr-k8s-system <frr-k8s pod on the node> -c frr -- \
  vtysh -c 'show bgp vrf fabric summary'
```

## What the suite proves

`make e2e-bgp-fabric` runs `tests/e2e-bgp-fabric/frr-k8s-fabric-peering`
against a kind cluster deployed with `WITH_FRR_K8S=true`, and refuses a cluster
with a node that kind did not create. One kind node stands
in for the compute node, and two FRR Pods with the lab's FRR image stand in for
its leaf pair. The `e2e-bgp-fabric` job runs it on every pull request from this
repository that touches the suite or the overlay or carries the `ci:full`
label. A pull request from a fork does not run it: the suite's Pods are
privileged and share the host network and PID namespace of the self-hosted
runner.

| Step | What it proves |
| --- | --- |
| S1 | With no labelled node, the DaemonSet has no pod and no `BGPSessionState` exists; an `FRRConfiguration` is admitted and creates no session. Step 1 first refuses a node that kind did not create (`providerID` not `kind://`) and a node that carries the label without the suite's marker `e2e.c5c3.io/bgp-fabric=true`, removes what a killed run left under that marker, then labels the node |
| S2 | `node-prep.sh up` creates the VRF `fabric` with both loopbacks; without the `vrf` module the Pod never becomes Ready and the step fails |
| S3 | Both peers start and enslave their uplinks `fab1` and `fab2` to `fabric` |
| S4 | The DaemonSet runs one ready pod on the labelled node |
| S5 | From the typed API alone, both `BGPSessionState` objects in VRF `fabric` report `Established` and BFD `Up`, and the node's `FRRNodeState` reports a successful conversion and reload |
| S6 | Both peers hold `192.0.2.1/32` and `2001:db8::1/128` on one path with the AS path `4200000001` |
| S7 | The IPv4 and IPv6 defaults in VRF `fabric` are `proto bgp` over `fab1` and `fab2`; both peers advertise their baits `198.18.0.0/15` and `2001:db8:ffff::/48` to the node, and both stay out of the VRF |
| S8 | The main table keeps both defaults it had before the run, holds no `proto bgp` route and no route over `fab1`, `fab2` or `fabric` |
| S9 | With the raw snippet, both peers hold `198.51.100.10/32` and `2001:db8::10/128`; the baits `203.0.113.10/32` and `2001:db8:0:1::10/128` reach the fabric router, appear in no advertised route and reach neither peer; the reload drops no session (both peers' `connectionsEstablished` and `connectionsDropped` are the same before and after the apply) |

## Hardware acceptance

The run on real ports. The node side checks H1 and H2 with frr-k8s, and H3 with
the host FRR of a metal-stack machine; H4 holds on both nodes.

| Id | Set up | Read and expect |
| --- | --- | --- |
| H1 | A node without a host FRR, labelled, with the prerequisites and its per-node `FRRConfiguration`, cabled to a real leaf pair | frr-k8s holds both sessions with BFD `Up` |
| H2 | As for H1 | The leaf holds the node's /32 and /128 |
| H3 | A metal-stack machine with dedicated fabric ports and the stanza of [Nodes with a host FRR](#nodes-with-a-host-frr) in its host FRR | The host FRR holds both sessions in the fabric VRF, and the `lan0` and `lan1` sessions stay established |
| H4 | Both nodes of H1 and H3 | The main table holds no fabric route, and its default is the one it had before the sessions came up |

### Hardware record

No hardware run has been recorded yet. The first run on real ports records it
here.

## Pins

| Pin | Value | File | Moved by |
| --- | --- | --- | --- |
| frr-k8s chart | `0.0.27` | `deploy/overlays/kind/frr-k8s/release.yaml` | Renovate, group `frr-k8s`, minor and patch, never merged without review |
| frr-k8s controller image | `quay.io/metallb/frr-k8s:v0.0.27@sha256:de862b74a3a314eb76c8fb4cabc9d2c55cca51002cd022704c32df717f9366b7` | `deploy/overlays/kind/frr-k8s/release.yaml` | Renovate, group `frr-k8s`, minor, patch and digest |
| FRR image of frr-k8s | `quay.io/frrouting/frr:10.6.2@sha256:89e5e1ec8b17c5dffaf57dbac58a29ebeb3731a84e5bc09dad1662adff2ba7f5` | `deploy/overlays/kind/frr-k8s/release.yaml` | Renovate, group `frr-k8s`, patch and digest; its minor follows the chart's default by hand, in the pull request that moves the chart |
| FRR image of the suite's Pods | `quay.io/frrouting/frr:10.7.1@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2` | `tests/e2e-bgp-fabric/frr-k8s-fabric-peering/01-fabric-node.yaml` and `02-fabric-peers.yaml` | Renovate, group `bgp-fabric lab`, together with the lab's pin in `tests/lab/bgp-fabric/bgp-fabric.clab.yml` |

## See also

- [BGP Fabric Contract](bgp-fabric-contract.md): the clauses C2 to C5 and C10
  this page maps to frr-k8s, and the containerlab lab
- [E2E Infrastructure Deployment](../infrastructure/e2e-deployment.md): the
  overlay table and the `WITH_FRR_K8S` flag
- [CI Workflow](../ci-cd/ci-workflow.md#e2e-bgp-fabric): the `e2e-bgp-fabric`
  job
- [Connect a Compute Cluster](../../guides/nova/connect-a-compute-cluster.md):
  what a compute cluster installs with its own Flux
- [#899](https://github.com/C5C3/cobaltcore/issues/899), the BGP meta, and the
  phases [#1363](https://github.com/C5C3/cobaltcore/issues/1363),
  [#1364](https://github.com/C5C3/cobaltcore/issues/1364) and
  [#1365](https://github.com/C5C3/cobaltcore/issues/1365)
- frr-k8s v0.0.27:
  [README](https://github.com/metallb/frr-k8s/tree/v0.0.27) and
  [`api/v1beta1/frrconfiguration_types.go`](https://github.com/metallb/frr-k8s/blob/v0.0.27/api/v1beta1/frrconfiguration_types.go)
