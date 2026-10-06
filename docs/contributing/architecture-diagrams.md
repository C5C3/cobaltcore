---
title: Architecture Diagrams
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Architecture Diagrams

The diagrams of these docs live in `docs/diagrams/`. Each one is
a pair: a `.drawio` file that is the editable source, and an `.svg` file that
the page embeds. A change to a diagram updates both files in the same commit.

## Inventory

| Diagram | Shows | Embedded in |
| --- | --- | --- |
| `cobaltcore-overview` | What CobaltCore does, as a layer stack from bare metal to the OpenStack clouds, and why it matters | [Overview](../index.md) |
| `cobaltcore-management-cluster` | The implemented management cluster, from one `ControlPlane` CR to the running OpenStack services, with optional target clusters | [Implemented topology](../architecture/index.md#implemented-topology), [Quick Start (ControlPlane)](../quick-start-controlplane.md), [Core Components](../architecture/core-components.md), [ControlPlane Reconciler Architecture](../reference/c5c3/controlplane-reconciler.md), [ControlPlane CRD API Reference](../reference/c5c3/controlplane-crd.md), [Target Clusters](../reference/target-clusters.md), [Architecture Overview](../reference/infrastructure/e2e-deployment.md#architecture-overview) |
| `cobaltcore-stack` | The stack layer by layer, with the project behind each layer (IronCore, Garden Linux, Gardener, CobaltCore, OpenStack) | [The multi-cluster target picture](../architecture/index.md#the-multi-cluster-target-picture) |
| `cobaltcore-architecture` | The five clusters (Operation and Monitoring, OpenStack Control Plane, Ceph Storage, OpenStack Compute, OpenStack Network) on Garden Linux, managed by Gardener, on bare metal managed by IronCore | [The multi-cluster target picture](../architecture/index.md#the-multi-cluster-target-picture), [Hypervisor Cluster](../future/hypervisor-cluster.md), [Storage Cluster](../future/storage-cluster.md), [Management Cluster](../future/management-cluster.md) |
| `cobaltcore-control-planes` | One Operation and Monitoring cluster that creates and manages several OpenStack control planes | [Several control planes](../architecture/index.md#several-control-planes) |
| `cobaltcore-attached-clusters` | One control plane with several storage, compute, and network clusters attached | [Attached clusters](../architecture/index.md#attached-clusters), [Connect a Compute Cluster](../guides/nova/connect-a-compute-cluster.md) |
| `quickstart-map` | The four quick starts as a map: what each deploys and ends with, and the three ways they relate | [Start here](../index.md#start-here), [Quick Start](../quick-start.md), [Quick Start (Extended)](../quick-start-extended.md), [Quick Start (ControlPlane)](../quick-start-controlplane.md), [Quick Start (metal-stack)](../quick-start-metal-stack.md), [One devstack per guide](./guide-conventions.md#one-devstack-per-guide) |
| `quickstart-request-path` | The six hops of a request from the workstation to an OpenStack API on the kind devstack, and the two port-forwards that leave hops out | [Access Keystone from your local machine](../quick-start-extended.md#access-keystone-from-your-local-machine), [Quick Start](../quick-start.md), [Service exposure](../architecture/index.md#service-exposure), [Step 7 of the Quick Start (ControlPlane)](../quick-start-controlplane.md#step-7-—-verify) |
| `secrets-flow` | The read, write-back and dynamic paths between OpenBao, ESO, the operators and the services, with the cluster store and the tenant store | [Secret flow](../architecture/index.md#secret-flow), [Architecture Overview](../reference/infrastructure/openbao-bootstrap.md#architecture-overview), [External Secrets Operator](../reference/infrastructure/infrastructure-manifests.md#external-secrets-operator), [Per-ControlPlane secret stores and OpenBao identities](../guides/multi-tenant-deployment.md#per-controlplane-secret-stores-and-openbao-identities) |
| `secrets-db-credentials` | Static and dynamic database credentials side by side, with the refresh interval inside the two TTLs | [setup-database-tenant.sh](../reference/infrastructure/openbao-bootstrap.md#setup-database-tenant-sh), [Migrate Keystone DB to Dynamic Credentials](../guides/keystone/migrate-keystone-db-to-dynamic-credentials.md#what-changes), [Migrate Glance DB to Dynamic Credentials](../guides/glance/migrate-glance-db-to-dynamic-credentials.md#what-changes), [Migrate Placement DB to Dynamic Credentials](../guides/placement/migrate-placement-db-to-dynamic-credentials.md#what-changes), [Migrate Barbican DB to Dynamic Credentials](../guides/barbican/migrate-barbican-db-to-dynamic-credentials.md#what-changes), [Migrate Cinder DB to Dynamic Credentials](../guides/cinder/migrate-cinder-db-to-dynamic-credentials.md#what-changes), [Migrate Nova DB to Dynamic Credentials](../guides/nova/migrate-nova-db-to-dynamic-credentials.md#what-changes), [Quick Start (ControlPlane)](../quick-start-controlplane.md) |
| `secrets-admin-credential-loop` | The nine steps that bring the admin application credential from OpenBao to K-ORC and back | [K-ORC admin credential chain](../reference/c5c3/controlplane-reconciler.md#k-orc-admin-credential-chain), [Admin Credential Chain](../reference/infrastructure/infrastructure-manifests.md#admin-credential-chain), [AdminCredentialSpec](../reference/c5c3/controlplane-crd.md#admincredentialspec) |
| `secrets-rotation-keys` | Staged rotation of Fernet and credential keys: CronJob, staging Secret, operator, pods | [Key Rotation RBAC Split](../reference/keystone/keystone-reconciler.md#key-rotation-rbac-split), [Background: Who Writes What](../guides/keystone/keystone-key-rotation.md#background-who-writes-what), [Rotate Fernet keys manually](../guides/day-2-operations.md#rotate-fernet-keys-manually) |
| `secrets-rotation-admin-password` | Scheduled and manual rotation of the admin password through OpenBao to the bootstrap Job | [Schedule Keystone Admin Password Rotation](../guides/keystone/keystone-admin-password-scheduled-rotation.md), [Rotate the Keystone Admin Password](../guides/keystone/keystone-admin-password-rotation.md), [reconcilePasswordRotation](../reference/keystone/keystone-reconciler.md#reconcilepasswordrotation) |
| `secrets-issuer-chains` | The self-signed issuer, the four CAs and their leaves as trust domains | [Self-Signed ClusterIssuer](../reference/infrastructure/infrastructure-manifests.md#self-signed-clusterissuer), [TLS Configuration](../reference/infrastructure/openbao-bootstrap.md#tls-configuration), [Enable Keystone Database TLS/mTLS](../guides/keystone/enable-keystone-database-tls.md#prerequisites) |
| `controlplane-gate-graph` | The twenty conditions of a ControlPlane as a gate graph: the blocking prefix, the tail group, and an arrow wherever one condition gates another | [Reconciliation Flow](../reference/c5c3/controlplane-reconciler.md#reconciliation-flow), [Status Conditions](../reference/c5c3/controlplane-crd.md#status-conditions), [Quick Start (ControlPlane)](../quick-start-controlplane.md#step-6-—-watch-the-chain-reconcile), [Quick Start (metal-stack)](../quick-start-metal-stack.md#cp-access), [Conditions of a ControlPlane](../guides/observability.md#conditions-of-a-controlplane) |
| `controlplane-children-placement` | The ControlPlane namespace, a dedicated service namespace and a target cluster, with the ownership model of each | [ControlPlane placement](../reference/target-clusters.md#controlplane-placement), [Owner-ref / GC model](../reference/c5c3/controlplane-reconciler.md#owner-ref-gc-model), [Ownership and garbage collection](../reference/c5c3/controlplane-crd.md#ownership-and-garbage-collection), [Background: what a namespace assignment moves](../guides/dedicated-service-namespaces.md#background-what-a-namespace-assignment-moves) |
| `controlplane-target-cluster` | A workload CR on a target cluster: the registration Secret, the access chart, the labelled children, and the routes that cross the cluster boundary | [Registering a target cluster](../reference/target-clusters.md#registering-a-target-cluster), [Deploy to a Target Cluster](../guides/deploy-to-a-target-cluster.md) |
| `controlplane-keystoneservice-registration` | The seven steps of a KeystoneService registration across two namespaces, from the consent to the consumer Secret | [Child Naming and Placement](../reference/c5c3/keystoneservice-reconciler.md#child-naming-and-placement), [Namespace consent](../reference/c5c3/keystoneservice-crd.md#namespace-consent), [Background: what lands where](../guides/register-a-foreign-service.md#background-what-lands-where) |
| `compute-node-anatomy` | One hypervisor node: the four pods three resources put on it, their init containers, the host paths they share, and the three gates that order their start | [Node contract of a NovaCompute](../reference/nova/novacompute-crd.md#node-contract), [Node contract of a NeutronMetadataAgent](../reference/neutron/neutron-metadata-agent-crd.md#node-contract), [Node contract of an OVNChassis](../reference/ovn/ovn-chassis-crd.md#node-contract), [Label a Node as a Compute or Network Node](../guides/ovn/label-a-chassis-node.md) |
| `compute-cluster-wiring` | A control-plane cluster and a compute cluster: the Secrets that cross with their writers, and the address every component on the compute cluster dials | [What the ControlPlane delivers](../guides/nova/connect-a-compute-cluster.md#what-the-controlplane-delivers), [Namespaces on a compute cluster](../reference/target-clusters.md#namespaces-on-a-compute-cluster), [reconcileNova](../reference/c5c3/controlplane-reconciler.md#reconcilenova), [The remote contract](../reference/nova/nova-crd.md#the-remote-contract) |
| `compute-ovn-control-plane` | What one OVNCentral runs, and which client reads or writes which database | [OVN Operator](../reference/ovn/index.md), [Address computation](../reference/ovn/ovn-central-crd.md#address-computation), [The two kinds](../reference/neutron/index.md#the-two-kinds), [Drain a Chassis Node](../guides/ovn/drain-a-chassis-node.md), [Restore an OVN Database Snapshot](../guides/ovn/restore-an-ovn-database-snapshot.md), [Repair OVN Drift with db-sync](../guides/neutron/repair-ovn-drift-with-db-sync.md), [Create a Provider Network](../guides/neutron/create-a-provider-network.md) |
| `compute-nova-control-plane` | The five Nova Deployments with the schemas and the bus each holds a connection to | [Processes](../reference/nova/index.md#processes), [Two cells per CR](../reference/nova/nova-cells.md#two-cells-per-cr), [Migrate Nova DB to Dynamic Credentials](../guides/nova/migrate-nova-db-to-dynamic-credentials.md#what-changes) |
| `compute-metadata-path` | The six hops of a metadata request from an instance to the Nova metadata API, and the shared secret at both ends | [The path of a request](../reference/neutron/neutron-metadata-agent-crd.md#metadata-path), [NovaMetadataSpec](../reference/nova/nova-crd.md#novametadataspec), [Quick Start (metal-stack)](../quick-start-metal-stack.md#hv-console) |
| `compute-node-phases` | The five phases of a node in a NovaCompute pool, with the trigger and the actor of every change | [Node phases](../reference/nova/novacompute-crd.md#node-phases), [Drain a Compute Node](../guides/nova/drain-a-compute-node.md) |
| `compute-node-drain` | The seven steps of a drain under the hypervisor operator across four lanes, with the point of no return | [The drain](../reference/nova/novacompute-crd.md#the-drain), [On a compute cluster](../guides/nova/drain-a-compute-node.md#on-a-compute-cluster), [Quick Start (metal-stack)](../quick-start-metal-stack.md#hv-evict) |
| `compute-metal-stack-lab` | The metal-stack lab after both parts of its quick start: cluster-wide parts, the pods of every worker, the traffic between workers, and the port-forward | [Quick Start (metal-stack)](../quick-start-metal-stack.md), [Open questions](../future/hypervisor-cluster.md#open-questions), [Metal-stack lab](../reference/infrastructure/infrastructure-manifests.md#metal-stack-lab) |
| `conventions-legend` | The notation for frames, objects, arrows, sequences, state machines, markers, outside actors and nodes | [Notation legend](./architecture-diagrams.md#notation-legend) |

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

A new diagram follows the same pattern. Put the pair into `docs/diagrams/`
under a name from [File names](#file-names). On a page directly under `docs/`
the path is `./diagrams/<name>.svg`. A page further down puts one `../` per
directory level in front of `diagrams/<name>.svg`, so a page two levels down
spells it `../../diagrams/<name>.svg`. Give the image an alt text that states
what the diagram shows, and add a row to the [inventory](#inventory) that lists
every page that embeds it. Every embed of a figure carries the same alt text,
so a change to what a figure shows updates the alt text on every page its row
lists. When a file, a row and an embed disagree, or two embeds of a figure
carry different alt texts, `tests/unit/docs/diagrams_inventory_test.sh` fails,
and a missing file also fails `npm run docs:build`.

## File names

A file is named `<prefix><subject>`, lower case, words joined by hyphens.

| Prefix | Figures |
| --- | --- |
| `cobaltcore-` | The product architecture: the six figures of the start page and the Architecture page |
| `quickstart-` | Entry pages: the quick-start map and the request path |
| `secrets-` | Secret and credential paths between OpenBao, ESO, the operators and the services |
| `controlplane-` | ControlPlane orchestration, placement and target clusters |
| `service-` | Service operator patterns and paths specific to one service |
| `compute-` | Compute nodes, the compute cluster, Nova, OVN and the metadata path |
| `deploy-` | Installation: Flux dependencies, the deploy run, overlays |
| `ci-` | CI and image builds |
| `test-` | Test beds |
| `conventions-` | Figures about the figures: the legend |

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
| Secret material | `#B0387F` | `#8A2B63` | `#F7E8F0` |
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
- Green arrows: an operator creates or reconciles what the arrow points at.
- Line patterns: solid is declared in an object or enforced in code, dashed
  (`3 2`) is optional or a further instance, and dotted (`1 3`) holds at run
  time and is declared nowhere.

The icons in `cobaltcore-overview` are plain line icons and can be replaced by
another icon set.

### Notation legend

The legend draws one sample of every element in the table below.

![The notation of the docs figures in seven groups. Frames: a cluster frame with a K8s chip that contains a namespace frame with an ns chip. Objects: a custom resource as a filled pill, a workload as an outline pill, a Secret and a ConfigMap as sheets with a folded corner, a Job and a CronJob as boxes with a bar at each side. Arrows: green for creates, blue for the OpenStack API, magenta for secret material labelled ExternalSecret or PushSecret, dark for order (solid when declared, dotted at run time), a grey line with a filled diamond for an owner reference, a dotted grey line with an open diamond for ownership by label, and a dashed arrow for optional parts. Sequences: numbered step badges at the tail of each arrow. State machines: an initial marker, a state, a stuck state in grey, and a transition labelled with its trigger and who drives it. Markers and outside actors: the chips by hand, kind only and lab only, and the actors Workstation, CI runner and Identity provider. Nodes: a node frame with a node chip that contains a workload, a host path as a grey box with square corners, a thin grey line for a mount, and a line with an arrowhead at each end for traffic between nodes.](../diagrams/conventions-legend.svg)

Every draw.io style also carries `whiteSpace=wrap;html=1;` and the font
settings of the existing files. `<stroke>` and `<text>` are the palette colors
of the role that owns the object.

| Element | Meaning | Looks like | draw.io style |
| --- | --- | --- | --- |
| Cluster frame | One Kubernetes cluster | Light frame, dark `K8s` chip and bold name at the top left | `rounded=1;absoluteArcSize=1;arcSize=37;fillColor=#FDFCF9;strokeColor=#3A4452;strokeWidth=3;` |
| Namespace frame | One namespace inside a cluster frame | Recessed frame, grey `ns` chip and bold name at the top left | `rounded=1;absoluteArcSize=1;arcSize=24;fillColor=#F6F5F1;strokeColor=#7D8794;strokeWidth=2;` and for the chip `fillColor=#7D8794;strokeColor=none;` |
| Custom resource | A CR a person or an operator applies | Filled pill, bold light text | `rounded=1;absoluteArcSize=1;arcSize=40;fillColor=<text>;strokeColor=none;` |
| Workload | Deployment, StatefulSet, DaemonSet or pod, or the Service in front of one | Outline pill | `rounded=1;absoluteArcSize=1;arcSize=34;fillColor=#FDFCF9;strokeColor=<stroke>;strokeWidth=2;` |
| Secret | A Kubernetes Secret | Sheet with a folded corner in the Secret colors | `shape=note;size=16;fillColor=#F7E8F0;strokeColor=#B0387F;strokeWidth=2;` |
| ConfigMap | A ConfigMap | Sheet with a folded corner, grey | `shape=note;size=16;fillColor=#FDFCF9;strokeColor=#7D8794;strokeWidth=2;` |
| Job, CronJob | A workload that runs to completion. The label starts with the kind | Box with a bar at each side | `shape=process;size=0.08;fillColor=#FDFCF9;strokeColor=<stroke>;strokeWidth=2;` |
| Creates | An operator creates or reconciles what the arrow points at | Green arrow | `strokeColor=#2F8A57;strokeWidth=3;endArrow=block;endFill=1;` |
| OpenStack API | An OpenStack API or control-plane connection | Blue arrow | `strokeColor=#2457C5;strokeWidth=3;endArrow=block;endFill=1;` |
| Secret material | A secret value travels in the arrow's direction. The label names the resource that moves it: `ExternalSecret` out of OpenBao, `PushSecret` into it | Magenta arrow, label in font size 20 | `strokeColor=#B0387F;strokeWidth=3;endArrow=block;endFill=1;` |
| Order | What must be ready first points at what waits for it | Dark arrow, solid when declared or enforced, dotted when it holds only at run time | `strokeColor=#3A4452;strokeWidth=3;endArrow=block;endFill=1;` plus `dashed=1;dashPattern=1 3;` |
| Owner reference | The owner garbage-collects the object | Grey line, filled diamond at the owner | `strokeColor=#7D8794;strokeWidth=2;endArrow=none;startArrow=diamondThin;startFill=1;startSize=14;` |
| Ownership by label | Owned through labels, because no owner reference can cross the namespace or cluster | Dotted grey line, open diamond at the owner | `strokeColor=#7D8794;strokeWidth=2;endArrow=none;startArrow=diamondThin;startFill=0;startSize=14;dashed=1;dashPattern=1 3;` |
| Optional | An optional part or a further instance of the same kind | Dashed arrow in the color of its kind | the style of its kind plus `dashed=1;dashPattern=3 2;` |
| Step badge | Step n of a sequence. One page lists the steps under the same numbers, and the other pages that embed the figure link to that list | Dark circle with a light number at the tail of the arrow | `ellipse;aspect=fixed;fillColor=#17202B;strokeColor=none;fontColor=#FDFCF9;fontStyle=1;fontSize=20;` at 36 by 36 |
| State | A state of a state machine, a status condition in a gate graph, or a quick start in the quick-start map | Box with slightly rounded corners, bold name | `rounded=1;absoluteArcSize=1;arcSize=12;fillColor=#FDFCF9;strokeColor=#3A4452;strokeWidth=2;` |
| Stuck state | A state the machine leaves only after someone intervenes | The same box, grey | `rounded=1;absoluteArcSize=1;arcSize=12;fillColor=#E9ECEF;strokeColor=#7D8794;strokeWidth=2;` |
| Initial marker | Where the machine starts | Small dark dot with an Order arrow into the first state | `ellipse;aspect=fixed;fillColor=#17202B;strokeColor=none;` at 20 by 20 |
| Transition | A change of state. First label line: the trigger. Second line, font size 20 in `#55606E`: who drives it. In the quick-start map the first line says how two quick starts relate and the second what changes for the reader | Order arrow with a two-line label | the Order style |
| By hand | A person does this step, no controller | Outlined dark chip `by hand` at the top right of the part | `rounded=1;arcSize=50;fillColor=#FDFCF9;strokeColor=#17202B;strokeWidth=2;fontColor=#17202B;fontStyle=1;fontSize=18;` |
| kind only, lab only | The part exists on that devstack only. The part itself is drawn solid, since dashed means optional | Outlined grey chip `kind only` or `lab only` | `rounded=1;arcSize=50;fillColor=#FDFCF9;strokeColor=#7D8794;strokeWidth=2;fontColor=#55606E;fontStyle=1;fontSize=18;` |
| Outside actor | A workstation, a CI runner or an identity provider. It sits on the canvas, outside every cluster frame | Box with a bold name and a second line, font size 20 in `#55606E`, that says what acts | `rounded=1;absoluteArcSize=1;arcSize=16;fillColor=#FDFCF9;strokeColor=#3A4452;strokeWidth=2;` |
| Node frame | One Kubernetes node. A pod on it is a group box named after its DaemonSet; inside, a container is a Workload pill and an init container a Job box whose label starts with `init` | Light frame with a grey outline, dark `node` chip and bold name at the top left | `rounded=1;absoluteArcSize=1;arcSize=24;fillColor=#FDFCF9;strokeColor=#7D8794;strokeWidth=3;` and for the chip `fillColor=#3A4452;strokeColor=none;` |
| Host path | A directory or socket in the node's file system that pods mount | Grey box with square corners, the path as its label | `rounded=0;fillColor=#E9ECEF;strokeColor=#7D8794;strokeWidth=2;` |
| Mount | The pod at one end mounts the host path at the other | Thin grey line without an arrowhead | `strokeColor=#7D8794;strokeWidth=2;endArrow=none;` |
| Node-to-node traffic | Traffic of instances, or of their memory and disks, between two nodes. The label names protocol and port | Line with an arrowhead at each end, in the colour of the role that owns the traffic | `strokeColor=<stroke>;strokeWidth=3;endArrow=block;endFill=1;startArrow=block;startFill=1;` |
