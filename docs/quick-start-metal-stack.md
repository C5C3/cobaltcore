---
title: Quick Start (metal-stack)
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start (metal-stack): ControlPlane and KVM hypervisors on a metal-stack cluster

This guide builds the metal-stack devstack of the
[guide conventions](./contributing/guide-conventions.md#one-devstack-per-guide)
on a Gardener shoot with two or more workers. Part 1 deploys the infrastructure
stack and the full ControlPlane, reached through a port-forward. Part 2 turns
every worker into a KVM hypervisor of that ControlPlane and boots a server on
each. One server gets a Cinder volume and keeps it through a live migration and
the eviction of its node, and the volume is backed up after the detach. With
the optional lab Ceph, Part 3 adds an RBD volume backend and moves the backups
to Ceph. The teardown leaves the cluster as bare as it was. The ControlPlane CR is the one
of the [Quick Start (ControlPlane)](./quick-start-controlplane.md) with its
block-storage block and three lab settings; read Steps 3 and 4 there for its
anatomy.

On the map of the four quick starts this page is the box Quick Start
(metal-stack): the ControlPlane of the Quick Start (ControlPlane) on a cluster
you bring, with servers on KVM.

![A map of the four quick starts. Two of them run one standalone Keystone on a kind cluster on the workstation. The Quick Start deploys the infrastructure stack, the keystone-operator and one Keystone resource, and ends with an authenticated token. The Quick Start (Extended) is the same devstack in more depth, with the UIs, the opt-ins, local builds, the E2E suite and Tempest, and going there from the Quick Start needs make teardown-infra first, because its Step 2 creates the same cobaltcore cluster on port 443. The other two run a whole control plane from one ControlPlane resource. The Quick Start (ControlPlane) runs on a fresh kind cluster and ends with a token, an image, a secret, a network and the Horizon dashboard. The Quick Start (metal-stack) runs the same ControlPlane resource on a Gardener shoot on metal-stack, turns every worker into a KVM hypervisor, and ends with a server on every worker, a volume, a live migration, an eviction and a backup. Going from a standalone Keystone to a ControlPlane is a mode change that needs make teardown-infra and a fresh cluster. Each quick start begins at git clone and is complete in itself.](./diagrams/quickstart-map.svg)

The figure shows the lab after both parts: what runs once per cluster, what
runs on every worker, the port-forward as the only way in, and the traffic
between the workers.

![The metal-stack lab after both parts of the quick start. One Gardener shoot holds everything. Cluster-wide, built by Part 1: the Envoy proxy of the Gateway openstack-gw, the ControlPlane controlplane with its eight OpenStack services, the OVNCentral controlplane-ovn, the backing services, an NFS server for Cinder, and the hypervisor operator. Built by Part 2: the resources OVNChassis lab-chassis, NeutronMetadataAgent lab-metadata-agent and NovaCompute lab, which put one pod of each of their DaemonSets on every labelled worker. Every worker is a Kubernetes node, a KVM hypervisor and an OVN chassis at once: it runs Open vSwitch, ovn-controller, the metadata agent, nova-compute, libvirt with QEMU, kvm-node-agent and the reservation of the migration ports, and it hosts servers. The figure draws worker 1 and worker N and a box for more. Between any two workers run Geneve tunnels on UDP 6081, libvirt with TLS on TCP 16514, and QEMU migrations with TLS on TCP 49152 to 49215. Each worker reaches the bus, the Southbound database, the metadata API and the NFS server inside the cluster. The only way in from the workstation is a port-forward of local port 8443 to the Envoy proxy.](./diagrams/compute-metal-stack-lab.svg)

## Prerequisites

The lab assumes a cluster of this shape:

| The lab assumes | Where it shows |
| --- | --- |
| A Gardener shoot on metal-stack with two or more Ready workers that carry the same `topology.kubernetes.io/zone` label | `kubectl get nodes -L topology.kubernetes.io/zone` |
| `/dev/kvm` on every worker and the same CPU model; `deploy/lab/metal-stack/hypervisor/compute.yaml` names `Skylake-Server-IBRS`, the host-model of a `c1-medium-x86` (Xeon D-2141I), and other hardware changes `cpuModels` there | probe, `== kvm device` and `== cpu` |
| The module files `vhost_net`, `openvswitch` and `geneve` for the running kernel | probe, `== module files for <kernel>` |
| For the NFS stack that holds Cinder's volumes and backups: the module files `nfs` and `nfsv4` for the running kernel | probe, `== module files for <kernel>` |
| Only with the optional `WITH_CHAOS_MESH=true`: the module files `ip_set`, `ip_set_hash_ip`, `ip_set_hash_net`, `xt_set`, `sch_netem` and `sch_tbf` for the running kernel | probe, `== module files for <kernel>` |
| Only with the optional `WITH_CEPH=true`: no `/var/lib/rook` on any node | probe, `== rook` |
| cgroup v2 | probe, `== cgroup` |
| `/var/lib` on a volume with room for the instance disks (192 GiB on the surveyed node) | probe, `== disks` |
| No `libvirtd`, `qemu-system-x86_64` or `ovs-vswitchd` on the host | probe, `== host os / binaries` |
| A pod-network MTU of 1460 or more, the value of `global_physnet_mtu` | probe, `== nics`, the `cali*` lines |
| A default StorageClass, which every volume of the stack binds to, and no DaemonSet `kube-system/node-local-dns` | Step 1 of `EXTERNAL_CLUSTER=true make deploy-infra` exits 1 otherwise |
| Room in that class for a 100Gi volume, the claim `nfs-server-exports` of the NFS server | the `nfs-server` rollout wait of the deploy in Step 3, which exits 1 otherwise |
| Only with the optional `WITH_CEPH=true`: room in that class for three 100Gi raw block volumes and one 10Gi volume, from a provisioner that serves `volumeMode: Block` | the Ceph wait of the deploy, which exits 1 otherwise |
| Egress on TCP 443 to `ghcr.io`, the Helm repositories and the cirros download | the shoot's firewall |
| TCP 16514 and 49152 to 49215 open between the workers | `hack/lab-node-ports.sh` |

The shoot's kubeconfig is saved as `kubeconfig` in the root of the clone;
`.gitignore` keeps that file out of git. The workstation needs `kubectl`, `jq`,
[`yq`](https://github.com/mikefarah/yq) v4.40.1 or newer, `make`, and the
OpenStack CLI
([`python-openstackclient`](https://docs.openstack.org/python-openstackclient/latest/))
with the [`osc-placement`](https://docs.openstack.org/osc-placement/latest/)
plugin. Neither Docker nor kind is needed: the deploy creates no cluster and
loads no image.

## Part 1: The control plane {#control-plane}

### Step 1: Clone and select the cluster {#cp-clone}

```bash
git clone https://github.com/c5c3/cobaltcore.git
cd cobaltcore
```

Save the shoot's kubeconfig as `kubeconfig` in this directory, then point
`kubectl` at it:

```bash
export KUBECONFIG="$PWD/kubeconfig"
kubectl get nodes
```

Every worker reports `Ready`. Every command on this page runs from this
directory with `KUBECONFIG` set this way.

### Step 2: Probe the nodes {#cp-probe}

The [node probe](./reference/infrastructure/infrastructure-manifests.md#node-probe)
is a read-only Job that prints the node facts the lab depends on under
thirteen fixed headers. The loop pins it to each node in turn, under a `=== <node>`
line, and deletes it before and after every run: a Job's pod template is
immutable, so a Job left over from an interrupted run would make the apply
fail and print another node's facts:

```bash
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  echo "=== ${node}"
  kubectl delete job -n default node-probe --ignore-not-found
  yq ".spec.template.spec.nodeName = \"${node}\"" deploy/lab/metal-stack/probe/node-probe.yaml | kubectl apply -f - || continue
  kubectl wait --for=condition=complete job/node-probe -n default --timeout=5m
  kubectl logs -n default job/node-probe
  kubectl delete job -n default node-probe
done
```

Compare each node's output with the [Prerequisites](#prerequisites) table and
with the expected output in the header of
`deploy/lab/metal-stack/probe/node-probe.yaml`, which a lab-ready node printed
on 2026-10-04. The probe completes whatever it finds, so a missing piece shows
up as `absent`, `none` or `NOT FOUND` in the output and not as a failed Job.

### Step 3: Deploy the stack {#cp-deploy}

```bash
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

The script deploys onto the cluster the current kubeconfig context points at
and never switches the context. It refuses the opt-ins this cluster does not
take, checks the cluster before it applies anything, and installs the shared
infrastructure and the ControlPlane operator stack. It returns once the ten
operator releases are Ready and the cluster admits the manifests of Step 4.
When it completes, it prints the port-forward command on its `Access:` line.
[`make deploy-infra`](./reference/infrastructure/e2e-deployment.md#make-deploy-infra)
describes every step and variable.

`WITH_NFS=true` deploys the
[Lab NFS stack](./reference/infrastructure/infrastructure-manifests.md#lab-nfs-stack):
an NFS server with the two shares that hold Cinder's volumes and backups, the
`csi-driver-nfs` mounter, and pods that load the NFS kernel modules on the
nodes. Without it the script exits 1 before it applies anything, because the
ControlPlane of Step 4 puts Cinder's backends on that server.

`WITH_CHAOS_MESH=true` is optional, and this page does not need it. Added to
the deploy command, it deploys Chaos Mesh for fault injection, limited to the
namespaces the lab declares, with pods that load its kernel modules on the
nodes; see
[Lab Chaos Mesh](./reference/infrastructure/infrastructure-manifests.md#lab-chaos-mesh).

`WITH_DIZZY=true` is optional too, and this page does not need it either.
Added to the deploy command, it deploys the metrics stack of a dizzy load or
chaos soak: VictoriaMetrics, which keeps the metrics on a volume, and Grafana
with dizzy's dashboards. A soak reaches them through the port-forward of
Step 6 and a second one to VictoriaMetrics; see
[Lab dizzy stack](./reference/infrastructure/infrastructure-manifests.md#lab-dizzy-stack).
Once Part 2 has run, `make dizzy-soak-start` starts a six-hour soak inside the
cluster that needs no port-forward and ends with a report and a verdict; see
[In-cluster soak](./reference/testing/dizzy-chaos-testing.md#in-cluster-soak).

`WITH_PROMETHEUS=true` is optional as well. Added to the deploy command, it
deploys Prometheus and Grafana. Prometheus keeps its metrics on a volume and
scrapes the service operators, and the hypervisor operator once Part 2 has
applied it. Grafana carries a dashboard for the Keystone operator and one for
the hypervisor operator. Prometheus is reached with
`kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090`
and Grafana with
`kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`,
where it signs in `admin` with the password `prom-operator`; see
[Lab Prometheus stack](./reference/infrastructure/infrastructure-manifests.md#lab-prometheus-stack).

`WITH_CEPH=true` is optional. Parts 1 and 2 keep Cinder on the NFS shares;
[Part 3](#ceph) puts Cinder on this Ceph. Added to the deploy command, it deploys the
Rook operator and a Ceph with one OSD per worker on raw block volumes of the
default class, the RBD pools `volumes` and `backups`, and the users `cinder`
and `cinder-backup`, whose keys reach the Secrets `ceph-client-cinder` and
`ceph-client-cinder-backup` in `openstack` through OpenBao. The deploy waits
until the Ceph reports `HEALTH_OK`, which can take several minutes, and its
completion block names the status command,
`kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph status`; see
[Lab Ceph](./reference/infrastructure/infrastructure-manifests.md#lab-ceph).

The shoot brings a VerticalPodAutoscaler and a metrics-server of its own, so
the script refuses `WITH_VPA=true` and `WITH_METRICS_SERVER=true` and installs
neither. A workload opts into the platform's VPA with its
`verticalAutoscaling` block, and an API into an HPA with its `autoscaling`
block, both under `spec.sizing` of the ControlPlane; see
[Lab autoscaling](./reference/infrastructure/infrastructure-manifests.md#lab-autoscaling).

### Step 4: Apply the OVN central and the ControlPlane {#cp-apply}

```bash
kubectl apply -k deploy/lab/metal-stack/controlplane
kubectl wait ovncentral/controlplane-ovn -n openstack --for=condition=Ready --timeout=10m
```

The directory holds the `OVNCentral` `controlplane-ovn` and the ControlPlane
`controlplane` of the Quick Start (ControlPlane). The ControlPlane carries that
page's block-storage block, which runs Cinder with the volume backend `nfs1`
and the backup backend `nfsbk` on the two shares of the NFS server of Step 3.
It adds three settings for the lab:

- `sizing.profile: Minimal` gives every component one replica and the database
  a 512Mi volume, which a worker carries beside its servers.
- `global_physnet_mtu: "1460"` matches the pod network's MTU, so Neutron gives
  every tenant network an MTU of 1402 after the IPv4 and Geneve headers.
- `hypervisorOperator: {}` provisions the Keystone account of the hypervisor
  operator and the Secret `controlplane-nova-hypervisor-operator-auth` it logs
  in with in Part 2.

[Lab ControlPlane](./reference/infrastructure/infrastructure-manifests.md#lab-controlplane)
gives the reason behind each setting.

::: details The two manifests
<<< @/../deploy/lab/metal-stack/controlplane/ovncentral.yaml

<<< @/../deploy/lab/metal-stack/controlplane/controlplane-lab.yaml
:::

### Step 5: Onboard the database-engine tenant {#cp-tenant}

The c5c3-operator creates the MariaDB `openstack-db` from the ControlPlane.
Once it is `Ready`, onboard the OpenBao database-engine tenant that issues the
services' database credentials:

```bash
kubectl wait --for=create mariadb/openstack-db -n openstack --timeout=10m
kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready --timeout=10m
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

The first wait gives the c5c3-operator time to create the MariaDB: without it
the Ready wait fails at once with `Error from server (NotFound)` while the
MariaDB is missing.
Step 5 of the [Quick Start (ControlPlane)](./quick-start-controlplane.md)
explains what the script sets up.

### Step 6: Wait for the chain and open the port-forward {#cp-access}

```bash
kubectl wait controlplane/controlplane -n openstack --for=condition=Ready --timeout=30m
```

The wait covers the twenty-one conditions of the figure.
[Step 6 of the Quick Start (ControlPlane)](./quick-start-controlplane.md#step-6-—-watch-the-chain-reconcile)
lists what each one waits for.

![The conditions of a ControlPlane as a gate graph. A blocking prefix runs one step after another and ends the pass at the first step that is not done: SizingReady, NamespacesReady, InfrastructureReady, ESOTenantStoreReady, DBCredentialsReady, AdminPasswordReady, KeystoneReady. DBCredentialsReady waits for a step done by hand, the tenant onboarding with setup-database-tenant.sh. Once the prefix has passed, the fifteen members of the tail group all run on every pass and each gates itself. KORCReady gates AdminCredentialReady, which gates CatalogReady and the KeystoneService registrations. KeystoneReady gates HorizonReady and the six service legs GlanceReady, PlacementReady, BarbicanReady, NeutronReady, CinderReady and NovaReady, and each leg also waits for the AccountReady of its own registration. NeutronReady also waits for OVNReady, which mirrors an OVNCentral the ControlPlane references and does not own, and NovaReady for PlacementReady. ServiceAccountsReady folds the registrations and gates the KORCCatalogRefresh step, which sets no condition. RegistrationTenantStoresReady and NamespaceAssignmentsReady have no gate.](./diagrams/controlplane-gate-graph.svg)

No image is preloaded on the lab, so the wait allows 30 minutes, twice the
15 of the Quick Start (ControlPlane). Then open the port-forward to the Gateway
in a second terminal, from the root of the clone, and keep it running for the
rest of the page:

```bash
export KUBECONFIG="$PWD/kubeconfig"
kubectl -n envoy-gateway-system port-forward \
  "$(kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=openstack-gw -o name)" 8443:443
```

It is the command of the `Access:` line of Step 3. When it ends, for example
after the Envoy pod restarted, start it again.

### Step 7: Verify {#cp-verify}

The Gateway serves one self-signed certificate per hostname, each in a Secret
`<service>-nip-io-tls` in `openstack` whose `ca.crt` is the certificate
itself. Each names its hostname as its subject, which lets one CA file hold
all of them. Collect them into that file for the OpenStack CLI:

```bash
kubectl get secret -n openstack --field-selector type=kubernetes.io/tls -o json |
  jq -r '.items[] | select(.metadata.name | endswith("-nip-io-tls")) | .data["ca.crt"] | @base64d' > gateway-ca.pem
grep -c 'BEGIN CERTIFICATE' gateway-ca.pem
```

The count is the number of Gateway hostnames, twelve on the lab. `.gitignore`
keeps `*.pem` out of git. Then log in as the admin through the port-forward:

```bash
export OS_CACERT="$PWD/gateway-ca.pem"
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret controlplane-keystone-admin-credentials -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack token issue
```

The command prints a token table. `OS_CACERT` makes the CLI verify every
endpoint against the Gateway's certificates, so no command on this page takes
`--insecure`, and the admin password goes only to a listener that holds one of
their keys. A `certificate verify failed` error means the file predates a
reissued certificate; write it again. On a host that cannot resolve
`*.nip.io`, the tip in Step 7 of the
[Quick Start (ControlPlane)](./quick-start-controlplane.md) gives the
`/etc/hosts` entries. Then list the catalog, the compute services and the
hypervisors:

```bash
openstack catalog list
openstack volume service list
openstack compute service list
openstack hypervisor list
```

The catalog holds a row per service of the ControlPlane, among them
`block-storage` for Cinder. The volume service list shows `cinder-scheduler`,
`cinder-volume` on the host `controlplane-cinder@nfs1` and `cinder-backup`,
each `up`. The compute service list has no `nova-compute` row, and the
hypervisor list is empty: no node runs a hypervisor yet. The image, placement,
secret and volume checks of the Quick Start (ControlPlane) run with these
variables, with or without the `--insecure` they carry there:
[Upload a first image](./quick-start-controlplane.md#upload-a-first-image),
[List placement resource classes](./quick-start-controlplane.md#list-placement-resource-classes),
[Store and retrieve a first secret](./quick-start-controlplane.md#store-and-retrieve-a-first-secret)
with the `python-barbicanclient` plugin, and
[Create a first volume](./quick-start-controlplane.md#create-a-first-volume).

## Part 2: The hypervisors {#hypervisors}

Part 2 turns every worker into a KVM hypervisor and boots a server on each. It
attaches a Cinder volume to one of them, live-migrates that server and evicts
its node with the volume attached, then detaches the volume and backs it up.
It needs three things, whatever brought them up: a `Ready` ControlPlane
`controlplane` in `openstack` with `spec.services.nova.hypervisorOperator` and
`spec.services.cinder`, a running port-forward to the Gateway
on local port 8443, and the `OS_*` variables of an admin login exported,
`OS_CACERT` with the Gateway's certificates among them. Run
it from the root of the clone with `KUBECONFIG` pointing at the cluster. The
commands read the names of all nodes into the array `nodes`, and their
availability zone. A command that needs one node takes it by slice,
`${nodes[@]:0:1}` for the first and `${nodes[@]:1:1}` for the second. bash and
zsh count a slice from 0, while zsh counts an array index from 1:

```bash
nodes=($(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'))
zone=$(kubectl get node "${nodes[@]:0:1}" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
```

### Step 1: Check the node network and label the nodes {#hv-nodes}

Live migration needs libvirt's TLS port 16514 and QEMU's migration ports 49152
to 49215 open between the workers. The
[node port check](./reference/infrastructure/infrastructure-manifests.md#node-port-check)
prints one line per ordered pair of nodes and exits 0 when all 65 ports are
open in both directions:

```bash
kubectl apply -k deploy/lab/metal-stack/migration-ports
kubectl rollout status daemonset/migration-port-reservation -n hypervisor-system --timeout=5m
hack/lab-node-ports.sh
```

The migration ports lie in Linux's ephemeral port range, where an outgoing
connection on a node can get one of them as its local port. The first two
commands reserve them on every node, so the kernel hands out none of them from
then on. A connection that took one before keeps it: the listener tries such a
port again every second for 10 seconds, and a port still taken then is
reported as `listener bind failed` and fails the check, because nothing could
listen on it. The reservation's log names the process that holds the port, and
[Migration port reservation](./reference/infrastructure/infrastructure-manifests.md#migration-port-reservation)
says how to free it.

Then label and annotate the nodes:

```bash
kubectl label node --all openstack.c5c3.io/chassis=true \
  openstack.c5c3.io/nova-compute-pool=lab \
  nova.openstack.cloud.sap/virt-driver=kvm \
  cobaltcore.cloud.sap/node-hypervisor-lifecycle=skip-tests
kubectl annotate node --all nova.openstack.cloud.sap/aggregates=
```

The labels select the nodes for the OVN chassis, the `NovaCompute` pool, the
libvirt DaemonSet and the hypervisor operator. The empty
`nova.openstack.cloud.sap/aggregates` annotation keeps each node in its zone's
aggregate after onboarding, which the servers of Step 5 need.
The label and annotation tables of
[Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors)
give the reason behind each.

### Step 2: Apply the fixtures {#hv-fixtures}

The fixtures are the OpenStack objects the hypervisor operator looks up, among
them the flavor `1` and the image `cirros-kvm` the servers boot. K-ORC creates
them:

```bash
kubectl apply -k deploy/lab/metal-stack/hypervisor-fixtures
kubectl kustomize deploy/lab/metal-stack/hypervisor-fixtures |
  kubectl wait -f - --for=condition=Available --timeout=15m
```

### Step 3: Apply the hypervisors {#hv-apply}

The kustomization adds libvirt in a DaemonSet, the OVN chassis, the metadata
agent, the `NovaCompute` pool `lab`, and the upstream
openstack-hypervisor-operator and kvm-node-agent:

```bash
kubectl apply -k deploy/lab/metal-stack/hypervisor
kubectl wait helmrelease/openstack-hypervisor-operator -n openstack --for=condition=Ready --timeout=10m
kubectl wait crd/hypervisors.kvm.cloud.sap --for=condition=Established --timeout=2m
for node in "${nodes[@]}"; do
  kubectl wait --for=create "hypervisor/${node}" --timeout=10m
done
kubectl wait certificate --all -n hypervisor-system --for=condition=Ready --timeout=10m
kubectl wait pod -l app.kubernetes.io/name=libvirt -n openstack --for=condition=Ready --timeout=15m
```

The hypervisor operator creates a `Hypervisor` per node with
`spec.highAvailability: false`, because the release starts it with
`--default-high-availability=false`: onboarding would otherwise wait for an HA
service the lab does not run. The two waits in front of the loop give the
operator's chart time to install: without them the wait in the loop fails at
once with `the server doesn't have a resource type` while the `Hypervisor` CRD
is missing. A libvirt pod turns `Ready` once kvm-node-agent has written its
node's TLS files.

With `WITH_CEPH=true` the libvirt pods also carry the container `ceph-secret`.
It defines the libvirt secret `090e4a3c-6c20-4e74-82dc-1a70382babe8` from the
key in `openstack/ceph-client-cinder` and sets its value again after every
rotation ([Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors)).
Each node logs one `set the value` line:

```bash
kubectl logs -n openstack -l app.kubernetes.io/name=libvirt -c ceph-secret
```

Without the flag the container waits for the Secret and the pod is `Ready`
regardless. A server with an attached RBD volume keeps the key its QEMU started
with until it is migrated or restarted.

### Step 4: Wait for onboarding {#hv-onboarding}

```bash
kubectl wait hypervisor --all --timeout=20m \
  --for=jsonpath='{.status.conditions[?(@.type=="Onboarding")].reason}'=Succeeded
kubectl wait ovnchassis/lab-chassis neutronmetadataagent/lab-metadata-agent novacompute/lab \
  -n openstack --for=condition=Ready --timeout=20m
kubectl get hypervisor -o custom-columns='NAME:.metadata.name,LIBVIRTD:.status.conditions[?(@.type=="libvirtd.service")].status,LIBVIRT:.status.conditions[?(@.type=="LibVirtConnection")].status,TLS:.status.conditions[?(@.type=="TLSCertificateInstalled")].status'
openstack hypervisor list
openstack aggregate list
openstack aggregate show "${zone}" -c hosts -f value
```

`novacompute/lab` turns `Ready` only once Nova has mapped every host into the
cell, so the servers of Step 5 can be scheduled at once.

Each `Hypervisor` shows `True` in the `LIBVIRTD`, `LIBVIRT` and `TLS` columns.
The hypervisor list prints one row per node, and the aggregate list holds the
aggregate named after the nodes' zone, the availability zone the servers of
the next step name. That aggregate lists every node as a host.

### Step 5: Boot a server on each node {#hv-boot}

Create a network without a router and boot one server on each node. The loop
counts the nodes from 0, as a slice does, and boots `lab-<n>` on the node
`${nodes[@]:<n>:1}` names: `lab-0` on the first node, `lab-1` on the second.
Steps 6 to 10 use these two:

```bash
openstack network create lab-net
openstack subnet create lab-subnet --network lab-net --subnet-range 192.168.77.0/24
openstack network show lab-net -c mtu -f value
i=0
for node in "${nodes[@]}"; do
  openstack server create "lab-${i}" --image cirros-kvm --flavor 1 \
    --network lab-net --availability-zone "${zone}:${node}" --wait
  i=$((i + 1))
done
for pod in $(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name); do
  kubectl exec -n openstack "${pod}" -c libvirtd -- virsh list
done
```

The network's MTU prints `1402`. Every server reaches `ACTIVE`, each on the
node its availability zone names, and each libvirt pod lists one running
domain.

### Step 6: Check metadata and the tunnel from the console {#hv-console}

A server on a network without a router is reached through its console. Print
`lab-1`'s address, then open the console of `lab-0` in the libvirt pod of its
node:

```bash
openstack server show lab-1 -c addresses -f value
host=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:host -f value)
domain=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:instance_name -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

Press Enter for the login prompt and log in as `cirros` with the password
`gocubsgo`. In a browser, the noVNC console of
[Expose the Console Proxy](./guides/nova/expose-the-console-proxy.md) reaches
the same server. On the console, with `<lab-1>` replaced by the address the
first command printed:

```sh
curl http://169.254.169.254/latest/meta-data/instance-id
ping -c 3 <lab-1>
ping -c 3 -s 1374 -M do <lab-1>
ping -c 1 -s 1375 -M do <lab-1>
```

The metadata call prints an instance ID, which proves the metadata agent on
`lab-0`'s node answers. The plain ping and the 1374-byte ping pass through the
Geneve tunnel between the first two nodes: 1374 bytes of payload and 28 bytes
of ICMP and IPv4 headers fill the network's MTU of 1402. The 1375-byte ping
fails, because the packet does not fit and `-M do` forbids fragmenting it.
Leave the console with `Ctrl+]`.

The figure follows the metadata call of this step. The lab's agent runs on the
cluster of its Nova, so the call takes the solid path, to the Service
`controlplane-nova-metadata` over plain HTTP.
[The path of a request](./reference/neutron/neutron-metadata-agent-crd.md#metadata-path)
lists the hops.

![The path of a metadata request in six numbered hops. Hop 1: an instance calls http://169.254.169.254, an address OVN answers on the chassis of its node. Hop 2: Open vSwitch hands the request to a haproxy in the network namespace of the instance's network; the metadata agent creates one such namespace per network under /run/netns and starts one haproxy in each. Hop 3: haproxy passes the request to the metadata agent over the socket metadata_proxy. Hop 4: the agent finds the port that is asking in the Southbound database. Hop 5: the agent forwards the request to the Nova metadata API {nova}-metadata on port 8775 and signs it with the shared secret, as the header X-Instance-ID-Signature. Hop 6: the metadata API checks the signature with the same secret and resolves the instance from the mappings in the API database. One Secret, {cp}-nova-metadata-secret, carries the shared secret to both ends, as an environment variable on each. For an agent on a compute cluster two things change: it reads a copy of the secret that the c5c3-operator writes there, and it reaches the metadata API over https through the metadata Gateway.](./diagrams/compute-metadata-path.svg)

### Step 7: Attach a volume {#hv-volume}

Create a 1 GiB volume, which the scheduler places on the backend `nfs1`, and
attach it to `lab-0`. Both volume commands return before the volume reaches
the status the next command needs, so a bounded wait for that status follows
each; a wait that runs out exits 124. `lab-0` is still on the node of Step 6,
so the step uses `libvirt_pod` and `domain` from there:

```bash
openstack volume create --size 1 lab-vol
timeout 120 bash -c 'until [ "$(openstack volume show lab-vol -c status -f value)" = available ]; do sleep 2; done'
openstack server add volume lab-0 lab-vol
timeout 120 bash -c 'until [ "$(openstack volume show lab-vol -c status -f value)" = in-use ]; do sleep 2; done'
volume=$(openstack volume show lab-vol -c id -f value)
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- \
  sh -c "grep ' /var/lib/nova/mnt/' /proc/mounts; stat -c '%u:%g %a %n' /var/lib/nova/mnt/*/volume-${volume}"
```

The last command runs in the libvirt pod of `lab-0`'s node and prints two
lines: the `nfs4` mount of `nfs-server.openstack.svc.cluster.local:/volumes`
below `/var/lib/nova/mnt/<md5>`, and
`42424:42424 660 /var/lib/nova/mnt/<md5>/volume-<id>`. `nova-compute` mounted
the share in its own pod, and the `Bidirectional` mount propagation of both
pods carried the mount through the host into the libvirt pod. The file keeps
Cinder's owner and mode, so libvirt changed no owner on the attach (see
`dynamic_ownership` in
[Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors)).

The figure shows the mounts behind this step. The volume service
`controlplane-cinder-volume-nfs1` and `nova-compute` on the node of `lab-0`
mount the same export, each below its own state directory.

![The Cinder processes with their NFS mounts. One Cinder resource runs four processes: the API {cinder} on port 8776, the scheduler {cinder}-scheduler, one cinder-volume Deployment {cinder}-volume-{backend} per CinderBackend, and the backup Deployment {cinder}-backup, which exists only while a CinderBackupBackend is attached. All four hold a connection to RabbitMQ and to MariaDB: the API hands a volume request to the scheduler over the bus, the scheduler hands it to a cinder-volume, and backup jobs travel the same way. Each cinder-volume mounts the NFS export of its own backend at /var/lib/cinder/mnt/{md5}, where {md5} is the MD5 of server:path. The backup pod mounts every volume export at that same path and its backup target at /var/lib/cinder/backup_mount/{md5}. Every export in a Cinder pod is an inline CSI volume of the driver nfs.csi.k8s.io. On a hypervisor node nova-compute mounts the export itself when a volume attaches, at /var/lib/nova/mnt/{md5}, and mount propagation carries that mount to QEMU on the host. Locks are files inside each pod, and Memcached holds the token cache only.](./diagrams/service-cinder-nfs-mounts.svg)

Open the console of `lab-0` as in Step 6:

```bash
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

On the console, find the volume, write a marker to it and read the marker
back:

```sh
grep vdb /proc/partitions
echo lab-volume-marker | sudo dd of=/dev/vdb
sync
echo 3 | sudo tee /proc/sys/vm/drop_caches
sudo head -c 17 /dev/vdb
```

`/proc/partitions` lists `vdb` with 1048576 blocks: flavor `1` has one 1 GiB
root disk and neither an ephemeral nor a swap disk, so the volume is the second
virtio disk. The last command prints `lab-volume-marker`: `head -c 17` reads
the 17 bytes of the marker and stops before the newline `echo` appended. The
page cache was dropped before it, so the guest read the marker back from the
volume. Leave the console with `Ctrl+]`.

### Step 8: Live-migrate a server {#hv-migrate}

```bash
openstack server migrate --live-migration --wait lab-0
openstack server show lab-0 -c OS-EXT-SRV-ATTR:host -f value
openstack server migration list --server lab-0
```

`lab-0` now runs on another node, which the second command prints, and the
migration list shows the migration `completed`. libvirt carried it over TLS
between the nodes, with the CPU model that `cpuModels` names in
`deploy/lab/metal-stack/hypervisor/compute.yaml`, which every node provides.

The volume moved with the server. Read the mount and the volume file on the
destination node, with `volume` from Step 7 and `domain` from Step 6, and open
the console there:

```bash
host=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:host -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- \
  sh -c "grep ' /var/lib/nova/mnt/' /proc/mounts; stat -c '%u:%g %a %n' /var/lib/nova/mnt/*/volume-${volume}"
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

The libvirt pod of the destination node prints the mount and
`42424:42424 660` for the volume file: `nova-compute` on the destination
mounted the share for the migration, and libvirt changed no owner there
either. On the console:

```sh
echo 3 | sudo tee /proc/sys/vm/drop_caches
sudo head -c 17 /dev/vdb
```

The guest prints `lab-volume-marker` again, read through the destination's
mount. Leave the console with `Ctrl+]`.

### Step 9: Evict a node {#hv-evict}

The step evicts the node that holds `lab-0`, which `host` names since Step 8.
Manual maintenance makes the hypervisor operator create an `Eviction` that
live-migrates every server off the node, `lab-0` with its volume.

The figure shows a whole drain. This step runs its steps 1 to 3 and then clears
`maintenance` again, so the pool label stays on and the node stays `Active`.

![The drain of a compute node under the hypervisor operator, in seven numbered steps across four lanes: a person, the hypervisor operator, the NovaCompute pool and the Nova API. 1: the person sets spec.maintenance of the Hypervisor resource to manual. 2: the hypervisor operator disables the compute service of the node in Nova. 3: it creates an Eviction, which migrates every server away, and sets status.evicted. Up to here clearing spec.maintenance reverts the drain. 4: the person removes the pool label from the Node, and the pool turns the node Draining. 5: the pool counts the servers on the host and finds none; the service is disabled already. 6: the pool turns the node Releasing, releases its pod and waits until it is gone. 7: the pool deletes the compute service, and Nova drops the host mapping, the resource providers and the aggregate membership. That delete is the point of no return. Without the hypervisor operator the order starts at step 4: the pool disables the service itself, and a person moves the servers.](./diagrams/compute-node-drain.svg)

```bash
kubectl patch hypervisor "${host}" --type merge \
  -p '{"spec":{"maintenance":"manual","maintenanceReason":"lab eviction check"}}'
kubectl wait --for=create "eviction/${host}" --timeout=5m
kubectl wait "eviction/${host}" --timeout=20m \
  --for=jsonpath='{.status.conditions[?(@.type=="Evicting")].reason}'=Succeeded
openstack server list --all-projects --host "${host}"
kubectl patch hypervisor "${host}" --type merge \
  -p '{"spec":{"maintenance":"","maintenanceReason":""}}'
openstack compute service list --service nova-compute
openstack volume show lab-vol -c status -f value
```

The operator disables the node's compute service in Nova before it creates
the `Eviction`, so the first wait gives the object time to appear; the second
wait alone exits 1 with `Error from server (NotFound)` while it is missing.
The server list of the evicted node is empty. The second patch clears
`maintenance`, which deletes the `Eviction`; sent before the wait succeeded,
it would leave the servers not yet moved on the node. After it, the node's
compute service is `enabled` and `up` again. The volume moved with `lab-0` and
is still `in-use`.

### Step 10: Detach the volume and back it up {#hv-backup}

Detach the volume from `lab-0`, check that no node keeps the share mounted and
that Cinder still reads the volume file, with `volume` from Step 7, then back
the volume up:

```bash
openstack server remove volume lab-0 lab-vol
timeout 120 bash -c 'until [ "$(openstack volume show lab-vol -c status -f value)" = available ]; do sleep 2; done'
for pod in $(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name); do
  kubectl exec -n openstack "${pod}" -c libvirtd -- sh -c "grep -c ' /var/lib/nova/mnt/' /proc/mounts || true"
done
kubectl exec -n openstack deployment/controlplane-cinder-volume-nfs1 -- \
  sh -c "stat -c '%u:%g %a' /var/lib/cinder/mnt/*/volume-${volume}; head -c 17 /var/lib/cinder/mnt/*/volume-${volume}"
openstack volume backup create --name lab-bk lab-vol
timeout 300 bash -c 'until [ "$(openstack volume backup show lab-bk -c status -f value)" = available ]; do sleep 2; done'
openstack volume backup show lab-bk -c status -f value
```

The loop prints `0` for each node: Nova unmounted the share with the node's
last detach. The `cinder-volume` pod prints `42424:42424 660` and
`lab-volume-marker`: the guest's write reached the volume file, and Cinder
still reads the file after the attach, the migration and the eviction. The
last line prints `available`.

The backup follows the detach. A backup of an attached volume needs `--force`
and runs through a temporary clone and a snapshot that Nova assists, a path
nothing has run on backends the operator renders with
`nfs_snapshot_support = false`.

## Part 3: Cinder on Ceph {#ceph}

Part 3 puts the lab's Cinder on the lab Ceph beside the NFS shares. It adds the
RBD volume backend `ceph1` beside `nfs1` and replaces the backup target `nfsbk`
with the RBD target `cephbk`. Then it attaches an RBD volume, live-migrates its
server, backs up an RBD and an NFS volume to Ceph, restores one, and rotates the
key of the Ceph user `cinder` under the running servers. Part 3 needs a lab
deployed with `WITH_CEPH=true` added to the command of Part 1, Step 3,
`EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true WITH_NFS=true WITH_CEPH=true make deploy-infra`,
and Part 2 through Step 10, which leaves `lab-vol` on `nfs1` `available`, its
backup `lab-bk` on `nfsbk`, and `lab-0` and `lab-1` `ACTIVE`. The port-forward
of Part 1, Step 6 keeps running, and the `OS_*` variables of Part 1, Step 7 and
`KUBECONFIG` stay exported. Part 2, Steps 8 and 9 moved `lab-0`, so Part 3 sets
`host`, `domain` and `libvirt_pod` itself.

### Step 1: Put Cinder on the lab Ceph {#ceph-apply}

Check the key Secrets, the health of Ceph and the shoot's networks:

```bash
kubectl get secret -n openstack ceph-client-cinder ceph-client-cinder-backup
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph health
kubectl -n kube-system get configmap shoot-info -o jsonpath='{.data.podNetwork} {.data.serviceNetwork}'; echo
```

The Secret read lists both Secrets. On a lab deployed without
`WITH_CEPH=true` it fails with `NotFound`, and Part 3 cannot run there.
`ceph health` prints `HEALTH_OK`. The `shoot-info` read prints
`10.240.0.0/13 10.248.0.0/18` on newforge, the two networks the variant names;
on another shoot, write its own two into the variant first. Go on only when
all three hold, because the next block replaces the backup target.

Delete the backup `lab-bk`, then apply the variant once Cinder lists no
backup:

```bash
openstack volume backup delete lab-bk
timeout 300 bash -c 'until out=$(openstack volume backup list --all-projects -f value -c ID) && [ -z "${out}" ]; do sleep 2; done' &&
  kubectl apply -k deploy/lab/metal-stack/controlplane-ceph
```

`lab-bk` goes first, because `nfsbk` wrote it, and once `cephbk` replaces
`nfsbk` Cinder can neither restore nor delete it. The same holds for every
backup `nfsbk` wrote, so the apply runs only once Cinder lists no backup in
any project; a list that fails counts as not empty. The apply prints the
webhook's warning
`Warning: spec.services.cinder.backupBackend switches from "nfsbk" (NFS) to "cephbk" (RBD): every backup the NFS driver wrote can be neither restored nor deleted until "nfsbk" is declared again.`
A block that prints no warning ran out of time after five minutes and applied
nothing. `openstack volume backup list --all-projects` then names the backups
left: delete them, reset one stuck in `error_deleting` with
`openstack volume backup set --state error <backup>` before its delete, and
repeat the block.

Then wait for the RBD entries and the volume service of `ceph1`:

```bash
kubectl wait --for=create cinderbackend/ceph1 -n openstack --timeout=5m
kubectl wait --for=create cinderbackupbackend/cephbk -n openstack --timeout=5m
kubectl wait cinderbackend/ceph1 cinderbackupbackend/cephbk -n openstack --for=condition=Ready --timeout=10m
kubectl get cinderbackupbackend -n openstack
kubectl wait controlplane/controlplane -n openstack --for=condition=Ready --timeout=15m
timeout 300 bash -c 'until openstack volume backend pool list -f value | grep -q "@ceph1#"; do sleep 5; done'
openstack volume service list
```

The c5c3-operator prunes `nfsbk` before it projects `cephbk`, so the list
shows `cephbk` alone, and the backup Deployment rolls onto the Ceph driver.
The pool wait catches a volume service that started but could not reach Ceph:
its row in the service list reads `up` while its pool never reports. The
service list shows `cinder-volume` on `controlplane-cinder@nfs1` and on
`controlplane-cinder@ceph1`, and `cinder-backup`, each `up`.

When a wait runs out, `kubectl describe cinderbackend ceph1 -n openstack` and
`kubectl describe cinderbackupbackend cephbk -n openstack` name the failing
condition, and the
[Troubleshooting](./guides/cinder/attach-an-rbd-backend.md#troubleshooting)
table of Attach an RBD Backend to Cinder lists the causes.
[Lab ControlPlane](./reference/infrastructure/infrastructure-manifests.md#lab-controlplane)
names the source of every value of the variant.

::: details The Ceph variant
<<< @/../deploy/lab/metal-stack/controlplane-ceph/kustomization.yaml
:::

### Step 2: Attach an RBD volume {#ceph-volume}

Create the volume type `ceph` for the backend, a 1 GiB volume of that type,
and attach it to `lab-0`. As in Part 2, a bounded wait for the status the next
command needs follows each volume command:

```bash
openstack volume type create --property volume_backend_name=ceph1 ceph
openstack volume create --type ceph --size 1 lab-rbd
timeout 120 bash -c 'until [ "$(openstack volume show lab-rbd -c status -f value)" = available ]; do sleep 2; done'
rbd_volume=$(openstack volume show lab-rbd -c id -f value)
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd info "volumes/volume-${rbd_volume}"
openstack server add volume lab-0 lab-rbd
timeout 120 bash -c 'until [ "$(openstack volume show lab-rbd -c status -f value)" = in-use ]; do sleep 2; done'
host=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:host -f value)
domain=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:instance_name -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- virsh dumpxml "${domain}" |
  grep -E "protocol='rbd'|<host name=|secret type='ceph'"
```

`rbd info` shows the image `volume-<id>` of 1 GiB in the pool `volumes`. The
dumpxml prints three lines of the volume's disk: the source
`protocol='rbd' name='volumes/volume-<id>'`, the monitor
`<host name='<mon address>' port='3300'/>`, and
`<secret type='ceph' uuid='090e4a3c-6c20-4e74-82dc-1a70382babe8'/>`, the
libvirt secret of [Part 2, Step 3](#hv-apply). Cinder reads the monitor's
address from `ceph mon dump`, and the port is the msgr2 port the cluster
requires. No mount is involved: QEMU on the host opens the image through
librbd.

Open the console of `lab-0` as in [Part 2, Step 6](#hv-console):

```bash
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

On the console, find the volume, write a marker to it and read the marker
back:

```sh
grep vdb /proc/partitions
echo rbd-volume-marker | sudo dd of=/dev/vdb
sync
echo 3 | sudo tee /proc/sys/vm/drop_caches
sudo head -c 17 /dev/vdb
```

`/proc/partitions` lists `vdb` with 1048576 blocks: Part 2, Step 10 detached
`lab-vol`, so the RBD volume is the second virtio disk. The last command
prints `rbd-volume-marker`, which has the 17 bytes of Part 2's marker. Leave
the console with `Ctrl+]`.

### Step 3: Live-migrate the server with the RBD volume {#ceph-migrate}

```bash
openstack server migrate --live-migration --wait lab-0
host=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:host -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -n openstack "${libvirt_pod}" -c libvirtd -- virsh dumpxml "${domain}" |
  grep -E "protocol='rbd'|secret type='ceph'"
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

The libvirt pod of the destination node prints the same source and secret:
QEMU there opened the image with the libvirt secret of its own node. On the
console:

```sh
echo 3 | sudo tee /proc/sys/vm/drop_caches
sudo head -c 17 /dev/vdb
```

The guest prints `rbd-volume-marker` again, read through the destination's
connection to Ceph. Leave the console with `Ctrl+]`.

### Step 4: Back up and restore {#ceph-backup}

Detach the RBD volume, back up both volumes to Ceph, and restore the RBD
backup into a new volume:

```bash
openstack server remove volume lab-0 lab-rbd
timeout 120 bash -c 'until [ "$(openstack volume show lab-rbd -c status -f value)" = available ]; do sleep 2; done'
openstack volume backup create --name lab-ceph-bk lab-rbd
timeout 600 bash -c 'until [ "$(openstack volume backup show lab-ceph-bk -c status -f value)" = available ]; do sleep 2; done'
openstack volume backup create --name lab-nfs-bk lab-vol
timeout 600 bash -c 'until [ "$(openstack volume backup show lab-nfs-bk -c status -f value)" = available ]; do sleep 2; done'
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd ls backups
openstack volume create --type ceph --size 1 lab-restored
timeout 120 bash -c 'until [ "$(openstack volume show lab-restored -c status -f value)" = available ]; do sleep 2; done'
openstack volume backup restore --force lab-ceph-bk lab-restored
timeout 600 bash -c 'until [ "$(openstack volume show lab-restored -c status -f value)" = available ]; do sleep 2; done'
restored=$(openstack volume show lab-restored -c id -f value)
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- sh -c "rbd export volumes/volume-${restored} - 2>/dev/null | head -c 17"; echo
openstack server add volume lab-0 lab-restored
timeout 120 bash -c 'until [ "$(openstack volume show lab-restored -c status -f value)" = in-use ]; do sleep 2; done'
```

Both backups reach `available`. `lab-nfs-bk` is the NFS volume of Part 2,
which the Ceph driver reads through the share the backup pod mounts and writes
to the pool `backups` as a full copy. `rbd ls backups` lists two images, each
named `volume-<volume id>.backup.<backup id>`.

`lab-restored` is a new volume of type `ceph`, so the marker read back from it
can only come from the backup. The restore needs `--force` because
`lab-restored` exists, and the OpenStack CLI refuses an existing volume
without it. The API sets `restoring-backup` before it returns, so the wait for
`available` sees the restore end. The toolbox prints `rbd-volume-marker` from
the restored image. The last two commands attach `lab-restored` to `lab-0`,
whose QEMU opens it with the current key of `cinder`. Step 5 rotates that key
under the running guest.

### Step 5: Rotate the cinder key {#ceph-rotate}

Raise the `keyGeneration` of the Ceph user `cinder`, then wait until the
volume and backup Deployments and every node's libvirt secret carry the new
key:

```bash
volume_generation=$(kubectl -n openstack get deploy controlplane-cinder-volume-ceph1 -o jsonpath='{.metadata.generation}')
backup_generation=$(kubectl -n openstack get deploy controlplane-cinder-backup -o jsonpath='{.metadata.generation}')
export volume_generation backup_generation
key_generation=$(kubectl -n rook-ceph get cephclient cinder -o jsonpath='{.spec.security.cephx.keyGeneration}')
old_key=$(kubectl -n openstack get secret ceph-client-cinder -o jsonpath='{.data.userKey}')
if [ -n "${volume_generation}" ] && [ -n "${backup_generation}" ] && [ -n "${key_generation}" ] && [ -n "${old_key}" ] &&
  kubectl -n rook-ceph patch cephclient cinder --type merge \
    -p "{\"spec\":{\"security\":{\"cephx\":{\"keyGeneration\":$((key_generation + 1))}}}}" &&
  timeout 600 bash -c 'until g=$(kubectl -n openstack get deploy controlplane-cinder-volume-ceph1 -o jsonpath="{.metadata.generation}") && [ -n "${g}" ] && [ "${g}" != "${volume_generation}" ]; do sleep 5; done' &&
  timeout 300 bash -c 'until g=$(kubectl -n openstack get deploy controlplane-cinder-backup -o jsonpath="{.metadata.generation}") && [ -n "${g}" ] && [ "${g}" != "${backup_generation}" ]; do sleep 5; done' &&
  kubectl -n openstack rollout status deploy/controlplane-cinder-volume-ceph1 --timeout=300s &&
  kubectl -n openstack rollout status deploy/controlplane-cinder-backup --timeout=300s &&
  key=$(kubectl -n openstack get secret ceph-client-cinder -o jsonpath='{.data.userKey}') &&
  [ -n "${key}" ] && [ "${key}" != "${old_key}" ]; then
  printf '%s' "${key}" | base64 -d | sha256sum
  kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph auth get-key client.cinder | sha256sum
  digest=$(printf '%s' "${key}" | base64 -d | tr -d '[:space:]' | sha256sum | cut -c1-12)
  listed=
  for pod in $(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name); do
    listed=yes
    timeout 300 bash -c "until kubectl logs -n openstack ${pod} -c ceph-secret | grep -qF '(sha256 ${digest})'; do sleep 10; done" ||
      echo "${pod} did not log the new digest within 300 s: repeat the loop before the migration"
  done
  [ -n "${listed}" ] || echo 'no libvirt pod was listed: repeat the loop before the migration'
  kubectl logs -n openstack -l app.kubernetes.io/name=libvirt -c ceph-secret --tail=1
else
  echo 'a read, the patch, a wait or the key check failed: repeat the block'
fi
```

The patch raises the generation it reads by one, so it rotates the key also
on a lab where a guide raised it before. Rook writes the new key, and Ceph
accepts only the new key from then on. The PushSecret and the ExternalSecret
carry it to `openstack` within their one-minute intervals. The cinder-operator
renders it into the keyrings of `ceph1` and of the backup pod, which carries
the keyring of every RBD volume backend, and both Deployments roll. The two
full digests, of the Secret in `openstack` and of the key Ceph holds, are
equal.

The loop waits until the `ceph-secret` container of every node has logged
`set the value … (sha256 <digest>)` with the new digest, which the last
command prints for each pod. The container's 10-second pass and the kubelet's
sync of about a minute bound that wait. A pod that has not logged it within
300 s prints its name and
`did not log the new digest within 300 s: repeat the loop before the migration`,
and an empty pod list prints
`no libvirt pod was listed: repeat the loop before the migration`. The loop is
the lines from `listed=` to the last command, which reuse `digest` and so
repeat without a second rotation. The two generations are exported
because each wait runs in its own `bash -c`, under bash or zsh. A wait ends
only on a generation it read that differs from the exported one, because an
empty read differs from every generation.

The digests and the loop run only when the four reads returned a value, the
patch, both waits and both rollouts passed, and the Secret in `openstack`
holds a key other than the one read before the patch. Otherwise the block
prints `a read, the patch, a wait or the key check failed: repeat the block`.
The loop alone cannot tell a missed rotation from a completed one: every node
logged the digest of the old key at its start, and the two full digests are
equal for the old key too.

A guest that holds an RBD volume through a rotation keeps the session its QEMU
opened with the old key until the server is live-migrated or restarted. Move
`lab-0` onto the new key with a migration and open its console on the
destination:

```bash
openstack server migrate --live-migration --wait lab-0
host=$(openstack server show lab-0 -c OS-EXT-SRV-ATTR:host -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

On the console:

```sh
echo 3 | sudo tee /proc/sys/vm/drop_caches
sudo head -c 17 /dev/vdb
```

The guest prints `rbd-volume-marker` from `lab-restored`, read by the
destination's QEMU with the new key. Leave the console with `Ctrl+]`. Then
attach the RBD volume and the NFS volume to `lab-1`, which held neither, and
open its console:

```bash
openstack server add volume lab-1 lab-rbd
timeout 120 bash -c 'until [ "$(openstack volume show lab-rbd -c status -f value)" = in-use ]; do sleep 2; done'
openstack server add volume lab-1 lab-vol
timeout 120 bash -c 'until [ "$(openstack volume show lab-vol -c status -f value)" = in-use ]; do sleep 2; done'
openstack volume service list --service cinder-volume
host=$(openstack server show lab-1 -c OS-EXT-SRV-ATTR:host -f value)
domain=$(openstack server show lab-1 -c OS-EXT-SRV-ATTR:instance_name -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

On the console:

```sh
grep vd /proc/partitions
for disk in vdb vdc; do sudo head -c 17 /dev/${disk}; echo; done
```

`/proc/partitions` lists `vdb` and `vdc` with 1048576 blocks each, and the
loop prints `rbd-volume-marker` and `lab-volume-marker`. The attach of
`lab-rbd` asked the volume service of `ceph1` for the connection, so that
service runs on the new key. QEMU on `lab-1`'s node opened a new connection to
Ceph, so the libvirt secret of that node carries the new key, and `nfs1` still
attaches beside `ceph1`. The service list shows both `cinder-volume` rows `up`.
Leave the console with `Ctrl+]`.

## Teardown

Delete the backups, the servers, the volumes and the network, then the stack:

```bash
for backup in lab-bk lab-ceph-bk lab-nfs-bk; do
  if openstack volume backup show "${backup}" >/dev/null 2>&1; then openstack volume backup delete "${backup}"; fi
done
openstack server delete --wait $(openstack server list --name '^lab-[0-9]+$' -f value -c ID)
for volume in lab-vol lab-rbd lab-restored; do
  if openstack volume show "${volume}" >/dev/null 2>&1; then
    timeout 120 bash -c "until [ \"\$(openstack volume show ${volume} -c status -f value)\" = available ]; do sleep 2; done"
    openstack volume delete "${volume}"
  fi
done
openstack subnet delete lab-subnet
openstack network delete lab-net
EXTERNAL_CLUSTER=true make teardown-infra
```

The guards skip what the run did not create: a run without
[Part 3](#ceph) has no Ceph backup or volume, and a run with it has no
`lab-bk`. The volumes go after the servers. A run that stopped before
[Part 2, Step 10](#hv-backup) left `lab-vol` attached to `lab-0`, and after
Part 3 all three volumes are attached when the servers go. Deleting a server
detaches its volumes, and each volume delete waits for `available` first,
because Cinder deletes no attached volume. The RBD images go with Cinder's
deletes, and the pools with the Ceph teardown that follows the NFS stack.

The teardown removes the hypervisors first, node labels included, while the
ControlPlane and the operators still run, and then the rest of the stack (see
[`make teardown-infra`](./reference/infrastructure/e2e-deployment.md#make-teardown-infra)).
A `NovaCompute` pool keeps its finalizer while Nova counts a server on its
nodes, so the teardown exits 1 with
`Delete the servers on the lab hypervisors first (openstack server list --all-projects).`
while a server is left. The NFS stack goes at the end of the teardown's step 2,
and the claim `nfs-server-exports` with it. Where the default class has the
reclaim policy `Delete`, the claim's volume goes too, with every volume file
and backup on it. The node state under `/var/lib/nova`, `/var/lib/libvirt` and
`/etc/pki` stays on the nodes, and the NFS kernel modules stay loaded until a
node reboots. A Ceph deployed with `WITH_CEPH=true` goes at the end of the
same step, after the NFS stack: its CephCluster with its claims, whose volumes
go too where the class reclaims with `Delete`, and its state under
`/var/lib/rook` on every node, which Rook's cleanup Jobs empty before the
teardown goes on. A Chaos Mesh deployed with `WITH_CHAOS_MESH=true` goes before
the hypervisors: its experiments are released while its controller still runs,
and its kernel modules stay loaded until a node reboots as well. A dizzy soak
started with `make dizzy-soak-start` ends before the hypervisors go, and its
project, Keystone user and report claim go with it, so fetch its reports with
`make dizzy-soak-report` first. A dizzy stack
deployed with `WITH_DIZZY=true` goes in the teardown's step 3, and its claim
with it. Where the default class has the reclaim policy `Delete`, the metrics
on its volume go too. A Prometheus deployed with `WITH_PROMETHEUS=true` goes in
the same step, and its claim with it, then the Service and the Endpoints
`kube-prometheus-stack-kubelet` that its operator wrote in `kube-system`. Stop
the port-forward of Part 1, and those to VictoriaMetrics, Prometheus and
Grafana if you opened them, once the teardown has finished. Delete
`gateway-ca.pem`, and `_output/dizzy/clouds.yaml` if a soak wrote it: it holds
the admin password of the removed stack.

## Caveats

Nothing on the lab is reachable from outside the cluster. MetalLB has no
address pool, so no Service gets an external address, and the APIs answer only
through the port-forward of the workstation that opened it. Tenant networks
have no router, so a server is reached through its console.

A server lives on its worker's local disk, under `/var/lib/nova`. A worker that
Gardener replaces, after a machine update or a failed health check, takes its
servers with it unless [Part 2, Step 9](#hv-evict) moved them to another
worker first. With one worker left, no node can receive them.

Until Part 3, every Cinder volume and backup of the lab lives on one NFS server pod with one
`ReadWriteOnce` volume of 100Gi, which bounds all of them together. A backup
sits on the same volume as its source: it restores a deleted or overwritten
volume, and the loss of the export takes both. Nova mounts the share `hard`
with NFS 4.2, so while the server pod is away, a guest's requests to an
attached volume wait. The server is NFS-Ganesha, which keeps its NFSv4 client
records on the export claim, so once the pod is back the node can reclaim the
lock QEMU holds on the volume file; the `cinder-nfs-outage` chaos suite checks
that a lock outlives a restart of the server. The
[lab fault runs](./reference/infrastructure/infrastructure-manifests.md#lab-fault-runs)
have not measured that on the lab yet. They ran on 2026-10-04 on the kernel's
NFS server, which kept no records: a reschedule of the pod stalled the guest's
disk for 107 seconds, and a scale-down to 0 for 300 seconds, which stood for a
node replacement, for 414 seconds. Each time the node lost QEMU's lock, and
from then on every request to the volume failed with an I/O error until the
Nova server it was attached to was hard-rebooted with
`openstack server reboot --hard`
([#1245](https://github.com/c5c3/cobaltcore/issues/1245)). A volume that fails
that way after a restart of the server pod still needs that reboot. The
guest's network kept working. Meanwhile Nova reported the server `ACTIVE`,
Cinder the volume `in-use` and `cinder-volume` `up`, and every CR stayed
`Ready`, so only the guest shows the failure.
[Lab NFS stack](./reference/infrastructure/infrastructure-manifests.md#lab-nfs-stack)
describes the posture of the export.

After [Part 3](#ceph), the volumes of `ceph1` and every backup live on the
[Lab Ceph](./reference/infrastructure/infrastructure-manifests.md#lab-ceph):
one monitor, one manager and three OSDs whose disks are Lightbits volumes of the
default class, reached over NVMe/TCP. A write reaches two OSDs and, below
them, the replicas the platform keeps of each Lightbits volume; on newforge the
default class is `premium`, which keeps three, as the survey of
[#1338](https://github.com/c5c3/cobaltcore/issues/1338) found on 2026-10-09.
No run has measured the latency of that stack. The one monitor is a single
point of failure the lab accepts: a Ceph client opens its connection through a
monitor, so while the monitor's pod is away no volume is created, attached,
migrated or backed up on Ceph. A guest keeps the key its QEMU started with
until the server is live-migrated or restarted. Backups that `nfsbk` wrote are
unreachable once `cephbk` replaces it.

A killed libvirt, `nova-compute` or `ovn-controller` pod leaves the servers on
its node running. In the same runs each was killed under a running server and
replaced within 9 seconds. QEMU kept running, the guest lost no ping and no
disk write, the console log kept growing, and after the libvirt kill the
console answered through the new pod. Only the `OVNChassis` was not `Ready`
for less than 30 seconds after the `ovn-controller` kill.
[Lab fault runs](./reference/infrastructure/infrastructure-manifests.md#lab-fault-runs)
lists the timings.

Every pod that runs an image by tag pulls the tag when it starts. The operators
set `imagePullPolicy: Always` on every container that runs a tag, and the hvo
and kna releases set it on their managers, so a node does not keep starting a
build it cached under a tag that `main` has pushed again since. A running pod
keeps its build until it restarts. The libvirt DaemonSet stays on its digest
with `IfNotPresent`. While ghcr.io cannot be reached, a pod that starts waits
in `ImagePullBackOff` although the node holds the image.

The shoot's VPA updater runs with `--min-replicas=1`, so a component of the
Minimal profile that opts into `verticalAutoscaling` loses its only pod in
`Recreate` mode whenever the pod's request lies outside the recommendation,
with or without `minReplicas: 1`. In the
[lab autoscaling run](./reference/infrastructure/infrastructure-manifests.md#lab-autoscaling)
the evicted Placement API had no ready pod for 11 and 12 seconds. In
`InPlaceOrRecreate` mode with `minReplicas: 1` the updater resized the pod in
place, without a restart. Without `minReplicas` the run's pod already lay
inside the recommended range, so the run did not show what that mode does
there.

With `WITH_PROMETHEUS=true`, Prometheus loads the hypervisor operator's
alerts, and those that read `kube_customresource_*` series never have data:
only a kube-state-metrics with the operator chart's custom-resource config
exports them, and the lab runs none. The hvo rows of
[Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors)
name the alerts with and without data. No
lab run has judged the chart's scrape jobs `apiserver`, `coredns` and `kubelet`
yet, so a target of one of them may show `down`.

## Proven by

The page as of commit `e6a34f1b`, before Cinder was added, ran in page order
on 2026-10-03, every `bash` block but the `git clone`, on shoot `forge` with
two workers and Kubernetes v1.35.6, from a bare cluster to a bare cluster, and
each block exited 0 on its first attempt. Of the Cinder additions, the run of
2026-10-04 below ran all but the volume checks of Part 2, Steps 8 and 9, and
the run of 2026-10-05 below ran those as well. The node selection
of Part 2 changed after the run: a command that needs one node takes it from
`nodes` by slice, and Step 9 evicts the node `host` names. On 2026-10-04, on
`forge`, the opening block of Part 2 read the same zone in bash and in zsh.
Step 5 changed after that: it boots `lab-<n>` on every node, where the run
booted `lab-a` and `lab-b` on two, and the Teardown deletes every `lab-<n>`.
The run of 2026-10-05 below ran Step 9 with the changes, and the run of
2026-10-04 ran Step 5 and the server delete of the Teardown on three workers. The port-forward of
Part 1, Step 6 ran in a second terminal until the teardown had finished and
was then stopped. The console commands of Part 2, Step 6 were typed by a
script. The teardown waited 93 seconds for the stack's objects in `openstack`
to be finalized and then finished. Two things were done on the workers before
the run. `calico-node` on
`shoot--df33f0b4c1--forge-group-0-666b6-qmhrg` was restarted, because it held
port 49152 from before the reservation of Part 2, Step 1. The Open vSwitch
database an earlier lab stack had left on both workers was removed, because
the metadata agents otherwise read its old chassis name and the servers get no
metadata ([#1205](https://github.com/c5c3/cobaltcore/issues/1205)).
No chainsaw suite runs against the lab, because CI has no metal-stack cluster.
The findings of the lab runs so far, upstream and in this repository, are
listed under
[Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors).

On 2026-10-04 the page as of commit `647736c1` ran on shoot `newforge`, three
Xeon D-2141I workers on Kubernetes v1.35.6, from a bare cluster to a bare
cluster, in the session of the
[lab fault runs](./reference/infrastructure/infrastructure-manifests.md#lab-fault-runs).
Part 1 ran with `WITH_CHAOS_MESH=true` added to the command of Step 3. Part 2
ran Steps 1 to 7 and 10 and the Teardown, whose last line gave way to the
teardown of the Chaos Mesh
[Proving run](./reference/infrastructure/infrastructure-manifests.md#proving-run);
Steps 8 and 9 did not run. Every block exited 0 on its first attempt, and none
was repeated. Step 5 booted `lab-0`, `lab-1` and `lab-2`, one on each worker,
and the Teardown's server delete removed all three. The fault runs between
Steps 7 and 10 hard-rebooted `lab-0` twice, and Step 10 still read
`lab-volume-marker` and backed the volume up. A script typed the console
commands, and the port-forward ran in a second terminal without a restart.
Nothing was done by hand on the workers.

On 2026-10-05 the page as of commit `d34fde24` ran on shoot `newforge`, three
workers on Kubernetes v1.35.6, from a bare cluster to a bare cluster, in one
session with the
[lab measurement](./reference/testing/sizing-calibration.md#lab-measurement)
and the
[lab autoscaling run](./reference/infrastructure/infrastructure-manifests.md#lab-autoscaling).
Part 1 ran Steps 1 to 7, without the `git clone` and without the optional
checks Step 7 links, and Part 2 ran Steps 1 to 10. Step 7 attached the volume
to `lab-0`, Step 8 live-migrated `lab-0` with it and read `lab-volume-marker`
on the destination, Step 9 evicted that node with the volume still `in-use`,
and Step 10 backed it up. Every block exited 0 on its first attempt. The
measurement's blocks ran around Part 1, Step 4 and after Part 2, Step 10. The
first five lines of the Teardown followed, then the autoscaling block, then the
Teardown's last line, which exited 0 and left the platform's namespaces alone.
A script typed the console commands through `tmux send-keys`, and the
port-forward ran in a second terminal without a restart. Nothing was done by
hand on the cluster or the workers.

Part 2, Steps 1 and 4 changed after the runs above: Step 1 annotates the
nodes, and Step 4 shows the hosts of the zone's aggregate. The run that proves
them is recorded on [#1314](https://github.com/c5c3/cobaltcore/issues/1314).

Part 3 has not run on the lab yet. The Teardown changed after the runs above
as well: it deletes the backups and the volumes behind a guard that each
exists, and waits for `available` before each volume delete. That block has
not run on the lab yet.

No lab run has set `WITH_PROMETHEUS=true` yet; the
[Lab Prometheus run](./reference/infrastructure/infrastructure-manifests.md#lab-prometheus-run)
lists the steps that would prove it.

## Related references

- [Metal-stack lab](./reference/infrastructure/infrastructure-manifests.md#metal-stack-lab):
  the lab's manifests and the reason behind each setting, from the
  [node probe](./reference/infrastructure/infrastructure-manifests.md#node-probe)
  to the [node port check](./reference/infrastructure/infrastructure-manifests.md#node-port-check).
- [Lab Ceph](./reference/infrastructure/infrastructure-manifests.md#lab-ceph):
  the Ceph of `WITH_CEPH=true`, its pools, its users and the hand-off of their
  keys, on which Part 3 puts Cinder.
- [Attach an RBD Backend to Cinder](./guides/cinder/attach-an-rbd-backend.md)
  and [Configure RBD Volume Backups](./guides/cinder/configure-rbd-backups.md):
  an RBD backend and an RBD backup target as user-owned resources, run before
  Part 3.
- [E2E Deployment](./reference/infrastructure/e2e-deployment.md): `make deploy-infra`
  and `make teardown-infra`, with every variable.
- [NovaCompute CRD](./reference/nova/novacompute-crd.md): the compute pool, its
  aggregates, live migration and the node drain.
- [Quick Start (ControlPlane)](./quick-start-controlplane.md): the same
  ControlPlane on kind, with every block of the CR explained.
