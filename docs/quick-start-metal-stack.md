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
on a Gardener shoot with two workers. Part 1 deploys the infrastructure stack and
the full ControlPlane, reached through a port-forward. Part 2 turns both
workers into KVM hypervisors of that ControlPlane, boots a server on each,
live-migrates one and evicts a node. The teardown leaves the cluster as bare
as it was. The ControlPlane CR is the one of the
[Quick Start (ControlPlane)](./quick-start-controlplane.md) with three lab
settings; read Steps 3 and 4 there for its anatomy.

## Prerequisites

The lab assumes a cluster of this shape:

| The lab assumes | Where it shows |
| --- | --- |
| A Gardener shoot on metal-stack with two Ready workers that carry the same `topology.kubernetes.io/zone` label | `kubectl get nodes -L topology.kubernetes.io/zone` |
| `/dev/kvm` on both workers and the same CPU model; `deploy/lab/metal-stack/hypervisor/compute.yaml` names `Skylake-Server-IBRS`, the host-model of a `c1-medium-x86` (Xeon D-2141I), and other hardware changes `cpuModels` there | probe, `== kvm device` and `== cpu` |
| The module files `vhost_net`, `openvswitch` and `geneve` for the running kernel | probe, `== module files for <kernel>` |
| cgroup v2 | probe, `== cgroup` |
| `/var/lib` on a volume with room for the instance disks (192 GiB on the surveyed node) | probe, `== disks` |
| No `libvirtd`, `qemu-system-x86_64` or `ovs-vswitchd` on the host | probe, `== host os / binaries` |
| A pod-network MTU of 1460 or more, the value of `global_physnet_mtu` | probe, `== nics`, the `cali*` lines |
| A default StorageClass and no DaemonSet `kube-system/node-local-dns` | Step 1 of `EXTERNAL_CLUSTER=true make deploy-infra` exits 1 otherwise |
| A StorageClass named `premium`, which `deploy/lab/metal-stack/base/kustomization.yaml` and `deploy/lab/metal-stack/infrastructure/kustomization.yaml` pin for OpenBao, MariaDB and Garage; nothing checks it | `kubectl get storageclass premium` |
| The address `10.248.0.200` free in the service network (`10.248.0.0/18` on the surveyed shoot) | `kubectl apply -k deploy/lab/metal-stack/hypervisor` answers `provided IP is already allocated` otherwise |
| Egress on TCP 443 to `ghcr.io`, the Helm repositories and the cirros download | the shoot's firewall |
| TCP 16514 and 49152 to 49215 open between the workers | `hack/lab-node-ports.sh` |

The shoot's kubeconfig is saved as `kubeconfig` in the root of the clone;
`.gitignore` keeps that file out of git. The workstation needs `kubectl`, `jq`,
`yq` v4, `make`, and the OpenStack CLI
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

Both workers report `Ready`. Every command on this page runs from this
directory with `KUBECONFIG` set this way.

### Step 2: Probe the nodes {#cp-probe}

The [node probe](./reference/infrastructure/infrastructure-manifests.md#node-probe)
is a read-only Job that prints the node facts the lab depends on under ten
fixed headers. The loop pins it to each node in turn, under a `=== <node>`
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
on 2026-09-29. The probe completes whatever it finds, so a missing piece shows
up as `absent`, `none` or `NOT FOUND` in the output and not as a failed Job.

### Step 3: Deploy the stack {#cp-deploy}

```bash
EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true make deploy-infra
```

The script deploys onto the cluster the current kubeconfig context points at
and never switches the context. It refuses the kind-only opt-ins
(`WITH_NFS`, `WITH_VPA` and the other flags that need a kind node), checks the
cluster before it applies anything, and installs the shared infrastructure and
the ControlPlane operator stack. It returns once the ten operator releases are
Ready and the cluster admits the manifests of Step 4. When it completes, it
prints the port-forward command on its `Access:` line. [`make deploy-infra`](./reference/infrastructure/e2e-deployment.md#make-deploy-infra)
describes every step and variable.

### Step 4: Apply the OVN central and the ControlPlane {#cp-apply}

```bash
kubectl apply -k deploy/lab/metal-stack/controlplane
kubectl wait ovncentral/controlplane-ovn -n openstack --for=condition=Ready --timeout=10m
```

The directory holds the `OVNCentral` `controlplane-ovn` and the ControlPlane
`controlplane` of the Quick Start (ControlPlane), with three settings for the
lab:

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
kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready --timeout=10m
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

The wait exits 1 with `Error from server (NotFound)` until the c5c3-operator
has created the MariaDB; run it again. Step 5 of the
[Quick Start (ControlPlane)](./quick-start-controlplane.md) explains what the
script sets up.

### Step 6: Wait for the chain and open the port-forward {#cp-access}

```bash
kubectl wait controlplane/controlplane -n openstack --for=condition=Ready --timeout=30m
```

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
itself. Collect them into one CA file for the OpenStack CLI:

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
openstack compute service list
openstack hypervisor list
```

The catalog holds a row per service of the ControlPlane. The compute service
list has no `nova-compute` row, and the hypervisor list is empty: no node runs
a hypervisor yet. The image, placement and secret checks of the Quick Start
(ControlPlane) run with these variables, with or without the `--insecure`
they carry there:
[Upload a first image](./quick-start-controlplane.md#upload-a-first-image),
[List placement resource classes](./quick-start-controlplane.md#list-placement-resource-classes)
and [Store and retrieve a first secret](./quick-start-controlplane.md#store-and-retrieve-a-first-secret),
the last with the `python-barbicanclient` plugin.

## Part 2: The hypervisors {#hypervisors}

Part 2 turns both workers into KVM hypervisors, boots a server on each,
live-migrates one and evicts a node. It needs three things, whatever brought
them up: a `Ready` ControlPlane `controlplane` in `openstack` with
`spec.services.nova.hypervisorOperator`, a running port-forward to the Gateway
on local port 8443, and the `OS_*` variables of an admin login exported,
`OS_CACERT` with the Gateway's certificates among them. Run
it from the root of the clone with `KUBECONFIG` pointing at the cluster. The
commands read the node names and their availability zone from the cluster:

```bash
nodes=($(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'))
zone=$(kubectl get node "${nodes[0]}" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
```

### Step 1: Check the node network and label the nodes {#hv-nodes}

Live migration needs libvirt's TLS port 16514 and QEMU's migration ports 49152
to 49215 open between the workers. The
[node port check](./reference/infrastructure/infrastructure-manifests.md#node-port-check)
prints one line per ordered pair of nodes and exits 0 when all 65 ports are
open in both directions:

```bash
hack/lab-node-ports.sh
```

The migration ports lie in Linux's ephemeral port range, so an outgoing
connection on a node can hold one of them, and the check reports that port as
`listener bind failed`. Run the check again when that happens.

Then label the nodes, annotate their custom trait and create the trait in
Placement:

```bash
kubectl label node --all openstack.c5c3.io/chassis=true \
  openstack.c5c3.io/nova-compute-pool=lab \
  nova.openstack.cloud.sap/virt-driver=kvm \
  cobaltcore.cloud.sap/node-hypervisor-lifecycle=skip-tests
kubectl annotate node --all nova.openstack.cloud.sap/custom-traits=CUSTOM_C5C3_LAB
openstack --os-placement-api-version 1.6 trait create CUSTOM_C5C3_LAB
```

The labels select the nodes for the OVN chassis, the `NovaCompute` pool, the
libvirt DaemonSet and the hypervisor operator; onboarding waits for the trait.
The label table of
[Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors)
gives the reason behind each.

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
for node in "${nodes[@]}"; do
  kubectl wait --for=create "hypervisor/${node}" --timeout=10m
  kubectl patch hypervisor "${node}" --type merge -p '{"spec":{"highAvailability":false}}'
done
kubectl wait certificate --all -n hypervisor-system --for=condition=Ready --timeout=10m
kubectl wait pod -l app.kubernetes.io/name=libvirt -n openstack --for=condition=Ready --timeout=15m
```

The hypervisor operator creates a `Hypervisor` per node with
`spec.highAvailability: true`, and onboarding then waits for an HA service the
lab does not run, so the loop sets the field to `false`. The wait in the loop
fails at once with `the server doesn't have a resource type` while the
operator's chart still installs its CRD; run the loop again. A libvirt pod
turns `Ready` once kvm-node-agent has written its node's TLS files.

### Step 4: Wait for onboarding {#hv-onboarding}

```bash
kubectl wait hypervisor --all --timeout=20m \
  --for=jsonpath='{.status.conditions[?(@.type=="Onboarding")].reason}'=Succeeded
kubectl wait ovnchassis/lab-chassis neutronmetadataagent/lab-metadata-agent novacompute/lab \
  -n openstack --for=condition=Ready --timeout=20m
kubectl get hypervisor -o custom-columns='NAME:.metadata.name,LIBVIRTD:.status.conditions[?(@.type=="libvirtd.service")].status,LIBVIRT:.status.conditions[?(@.type=="LibVirtConnection")].status,TLS:.status.conditions[?(@.type=="TLSCertificateInstalled")].status'
openstack hypervisor list
openstack aggregate list
```

Each `Hypervisor` shows `True` in the `LIBVIRTD`, `LIBVIRT` and `TLS` columns.
The hypervisor list prints one row per node, and the aggregate list holds the
aggregate named after the nodes' zone, the availability zone the servers of
the next step name.

### Step 5: Boot a server on each node {#hv-boot}

Map the hosts into the cell, create a network without a router, and boot one
server on each node:

```bash
for node in "${nodes[@]}"; do
  tests/e2e/nova/discover-hosts.sh controlplane-nova openstack "${node}"
done
openstack network create lab-net
openstack subnet create lab-subnet --network lab-net --subnet-range 192.168.77.0/24
openstack network show lab-net -c mtu -f value
openstack server create lab-a --image cirros-kvm --flavor 1 --network lab-net \
  --availability-zone "${zone}:${nodes[0]}" --wait
openstack server create lab-b --image cirros-kvm --flavor 1 --network lab-net \
  --availability-zone "${zone}:${nodes[1]}" --wait
for pod in $(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt -o name); do
  kubectl exec -n openstack "${pod}" -c libvirtd -- virsh list
done
```

The network's MTU prints `1402`. Both servers reach `ACTIVE`, each on the node
its availability zone names, and each libvirt pod lists one running domain.

### Step 6: Check metadata and the tunnel from the console {#hv-console}

A server on a network without a router is reached through its console. Print
`lab-b`'s address, then open the console of `lab-a` in the libvirt pod of its
node:

```bash
openstack server show lab-b -c addresses -f value
host=$(openstack server show lab-a -c OS-EXT-SRV-ATTR:host -f value)
domain=$(openstack server show lab-a -c OS-EXT-SRV-ATTR:instance_name -f value)
libvirt_pod=$(kubectl get pod -n openstack -l app.kubernetes.io/name=libvirt \
  --field-selector "spec.nodeName=${host}" -o name)
kubectl exec -it -n openstack "${libvirt_pod}" -c libvirtd -- virsh console "${domain}"
```

Press Enter for the login prompt and log in as `cirros` with the password
`gocubsgo`. In a browser, the noVNC console of
[Expose the Console Proxy](./guides/nova/expose-the-console-proxy.md) reaches
the same server. On the console, with `<lab-b>` replaced by the address the
first command printed:

```sh
curl http://169.254.169.254/latest/meta-data/instance-id
ping -c 3 <lab-b>
ping -c 3 -s 1374 -M do <lab-b>
ping -c 1 -s 1375 -M do <lab-b>
```

The metadata call prints an instance ID, which proves the metadata agent on
`lab-a`'s node answers. The plain ping and the 1374-byte ping pass through the
Geneve tunnel between the nodes: 1374 bytes of payload and 28 bytes of ICMP and
IPv4 headers fill the network's MTU of 1402. The 1375-byte ping fails, because
the packet does not fit and `-M do` forbids fragmenting it. Leave the console
with `Ctrl+]`.

### Step 7: Live-migrate a server {#hv-migrate}

```bash
openstack server migrate --live-migration --wait lab-a
openstack server show lab-a -c OS-EXT-SRV-ATTR:host -f value
openstack server migration list --server lab-a
```

`lab-a` now runs on `nodes[1]`, beside `lab-b`, and the migration list shows the
migration `completed`. libvirt carried it over TLS between the nodes, with the
CPU model that `cpuModels` names in
`deploy/lab/metal-stack/hypervisor/compute.yaml`, which both nodes provide.

### Step 8: Evict a node {#hv-evict}

The step evicts whichever node holds the servers; after Step 7 both sit on
`nodes[1]`. Manual maintenance makes the hypervisor operator create an
`Eviction` that live-migrates every server off the node:

```bash
kubectl patch hypervisor "${nodes[1]}" --type merge \
  -p '{"spec":{"maintenance":"manual","maintenanceReason":"lab eviction check"}}'
kubectl wait --for=create "eviction/${nodes[1]}" --timeout=5m
kubectl wait "eviction/${nodes[1]}" --timeout=20m \
  --for=jsonpath='{.status.conditions[?(@.type=="Evicting")].reason}'=Succeeded
openstack server list --all-projects --host "${nodes[1]}"
kubectl patch hypervisor "${nodes[1]}" --type merge \
  -p '{"spec":{"maintenance":"","maintenanceReason":""}}'
openstack compute service list --service nova-compute
```

The operator disables the node's compute service in Nova before it creates
the `Eviction`, so the first wait gives the object time to appear; the second
wait alone exits 1 with `Error from server (NotFound)` while it is missing.
The server list of the evicted node is empty. The second patch clears
`maintenance`, which deletes the `Eviction`; sent before the wait succeeded,
it would leave the servers not yet moved on the node. After it, the node's
compute service is `enabled` and `up` again.

## Teardown

Delete the servers and the network, then the stack:

```bash
openstack server delete --wait lab-a lab-b
openstack subnet delete lab-subnet
openstack network delete lab-net
EXTERNAL_CLUSTER=true make teardown-infra
```

The teardown removes the hypervisors first, node labels included, while the
ControlPlane and the operators still run, and then the rest of the stack (see
[`make teardown-infra`](./reference/infrastructure/e2e-deployment.md#make-teardown-infra)).
A `NovaCompute` pool keeps its finalizer while Nova counts a server on its
nodes, so the teardown exits 1 with
`Delete the servers on the lab hypervisors first (openstack server list --all-projects).`
while a server is left. The node state under `/var/lib/nova`,
`/var/lib/libvirt` and `/etc/pki` stays on the nodes. Stop the port-forward of
Part 1 once the teardown has finished, and delete `gateway-ca.pem`.

## Caveats

Nothing on the lab is reachable from outside the cluster. MetalLB has no
address pool, so no Service gets an external address, and the APIs answer only
through the port-forward of the workstation that opened it. Tenant networks
have no router, so a server is reached through its console.

A server lives on its worker's local disk, under `/var/lib/nova`. A worker that
Gardener replaces, after a machine update or a failed health check, takes its
servers with it unless [Part 2, Step 8](#hv-evict) moved them to the other
worker first. With one worker left, no node can receive them.

## Proven by

Every `bash` block of this page but the `git clone` and the CA file of Part 1,
Step 7 ran in page order on
2026-10-02, from commit `715eafd3`, on shoot `forge` with two workers and
Kubernetes v1.35.6, from a bare cluster to a bare cluster. Part 1, Steps 3 and
4 ran again from a bare cluster on 2026-10-03, from commit `290752e0`, with the
wait at the end of Step 3: the deploy exited 0 and the apply of Step 4 exited 0
on its first attempt. Of the other blocks, that of Part 2, Step 3 took two
attempts, after the error its step names; the rest exited 0 on their first.
The console commands of Part 2, Step 6 were typed by a script. The teardown of
the 2026-10-02 run waited five minutes for the stack's objects in `openstack`
([#1186](https://github.com/c5c3/cobaltcore/issues/1186)) and then finished.
That run passed `--insecure` to every `openstack` command. The CA file of
Part 1, Step 7 replaced the flag afterwards and has not run on the lab: the
OpenStack CLI 8.2.0 verified a certificate of the Gateway's shape (self-signed,
`CA:FALSE`, empty subject) against such a file on a workstation.
No chainsaw suite runs against the lab, because CI has no metal-stack cluster.
The findings of the lab runs so far, upstream and in this repository, are
listed under
[Lab hypervisors](./reference/infrastructure/infrastructure-manifests.md#lab-hypervisors).

## Related references

- [Metal-stack lab](./reference/infrastructure/infrastructure-manifests.md#metal-stack-lab):
  the lab's manifests and the reason behind each setting, from the
  [node probe](./reference/infrastructure/infrastructure-manifests.md#node-probe)
  to the [node port check](./reference/infrastructure/infrastructure-manifests.md#node-port-check).
- [E2E Deployment](./reference/infrastructure/e2e-deployment.md): `make deploy-infra`
  and `make teardown-infra`, with every variable.
- [NovaCompute CRD](./reference/nova/novacompute-crd.md): the compute pool, its
  aggregates, live migration and the node drain.
- [Quick Start (ControlPlane)](./quick-start-controlplane.md): the same
  ControlPlane on kind, with every block of the CR explained.
