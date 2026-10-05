---
title: Architecture Diagrams
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Architecture Diagrams

The diagrams of these docs live in `docs/architecture/diagrams/`. Each one is
a pair: a `.drawio` file that is the editable source, and an `.svg` file that
the page embeds. A change to a diagram updates both files in the same commit.

| Diagram | Shows | Embedded in |
| --- | --- | --- |
| `cobaltcore-overview` | What CobaltCore does, as a layer stack from bare metal to the OpenStack clouds, and why it matters | [Overview](../index.md) |
| `cobaltcore-management-cluster` | The implemented management cluster, from one `ControlPlane` CR to the running OpenStack services, with optional target clusters | [Implemented topology](../architecture/index.md#implemented-topology) |
| `cobaltcore-architecture` | The five clusters (Operation and Monitoring, OpenStack Control Plane, Ceph Storage, OpenStack Compute, OpenStack Network) on Garden Linux, managed by Gardener, on bare metal managed by IronCore | [The multi-cluster target picture](../architecture/index.md#the-multi-cluster-target-picture) |
| `cobaltcore-control-planes` | One Operation and Monitoring cluster that creates and manages several OpenStack control planes | [Several control planes](../architecture/index.md#several-control-planes) |
| `cobaltcore-attached-clusters` | One control plane with several storage, compute, and network clusters attached | [Attached clusters](../architecture/index.md#attached-clusters) |

## Change a diagram

1. Open the `.drawio` file in the draw.io desktop app, in
   [diagrams.net](https://app.diagrams.net), or in VS Code with the Draw.io
   Integration extension. The file is stored uncompressed, so a change shows
   up as a readable diff.
2. Edit the diagram. Most arrows are attached to their boxes and follow when a
   box moves.
3. Export with **File › Export as › SVG** and overwrite the `.svg` of the same
   name. Leave **Transparent Background** unchecked: on a transparent
   background the dark labels vanish in the dark theme of the docs. A
   **Border Width** of 40 keeps the margin the current files have.
4. Run `npm run docs:dev`, open the page that embeds the diagram, and check
   it in the light and the dark theme.
5. Commit the `.drawio` and the `.svg` together.

A new diagram follows the same pattern. Put the pair into
`docs/architecture/diagrams/`, reference the `.svg` by a relative path from
the page that shows it, and give the image an alt text that states what the
diagram shows. The build resolves the path, so a
missing file fails `npm run docs:build`.

## Visual conventions

The font is IBM Plex Sans from Google Fonts. A browser that does not have it
installed falls back to Arial.

| Element | Stroke | Text | Fill |
| --- | --- | --- | --- |
| IronCore / bare-metal node | `#C9620F` | `#A44E07` | `#FBEFE3` (node: `#FFF8F1`) |
| Gardener | `#5E43A8` | `#4B3592` | `#EEEAF7` |
| CobaltCore / Operation and Monitoring / c5c3-operator | `#2F8A57` | `#1E6B40` | `#E6F1EA` |
| OpenStack Control Plane / API / services | `#2457C5` | `#1D47A6` | `#E7EDF9` |
| Ceph Storage | `#0E7D89` | `#0B5F69` | `#E3F1F2` |
| OpenStack Compute | `#C6343C` | `#A3262E` | `#F8E8E9` |
| OpenStack Network / OVN | `#B38A00` | `#6F5600` | `#FAF3DA` |
| Garden Linux | `#7D8794` | `#2B3440` | `#E9ECEF` |
| Platform and infrastructure components | `#7D8794` | `#2B3440` | `#EEF0F3` |
| Background | none | none | `#F6F5F1` |

The shapes and arrows carry a fixed meaning:

- Orange node frames: bare-metal servers that IronCore provisions and
  manages.
- Solid orange arrows: IronCore manages the nodes out of band. Where a
  Gardener arrow crosses one, the orange line has a small gap.
- A violet **K8s** chip and violet arrows mark a Kubernetes cluster that
  Gardener creates and manages through its IronCore provider extension.
- A grey **K8s** chip marks a Kubernetes cluster whose management the diagram
  leaves open.
- Blue arrows: OpenStack API and control-plane connections.
- Dashed boxes: optional parts, or further instances of the same kind
  (**+ more**).

The icons in `cobaltcore-overview` are plain line icons and can be replaced by
another icon set.
