---
title: Run a Fake Compute for Testing
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Run a Fake Compute for Testing

This guide runs a `nova-compute` on nova's fake virt driver beside the control
plane, maps it into the cell, boots and resizes a server on it, and removes it
again. The fake compute is the first consumer of the compute contract: it reads
everything it needs from the Secret `controlplane-nova-compute-config`, the way a
compute cluster builds its own `nova-compute`, and runs no hypervisor behind it.
Use it to exercise the scheduling, placement and instance-lifecycle paths of a
kind control plane that has no real hypervisor.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through the `nova` block of Step 3 and the
**Boot a first server** catalog and service checks in Step 6, so the projected
`controlplane-nova` child is `Ready` in `openstack` and the `OS_*` variables of
the token-issue step are exported. Every resource name in the examples below is
one that devstack produces.
:::

- The `openstack` CLI with the `osc-placement` plugin, as the quick start's
  prerequisites list it, for the resource-provider checks.
- Room for one more pod: the fake compute requests 50m CPU and 128Mi.

## The manifest

The manifest lives in `deploy/kind/fake-compute/`, an opt-in kind overlay that
`make deploy-infra` never applies. It holds a ConfigMap with the compute's own
overlay and the Deployment that mounts it beside the contract Secret:

<<< @/../deploy/kind/fake-compute/fake-compute.yaml

The names assume the quick start's ControlPlane. For a ControlPlane named `<cp>`,
replace every `controlplane-` prefix in the file with `<cp>-`, and for a release
other than `2025.2` set both image tags to that release.

Three settings in the manifest make a compute run without a hypervisor:

- **`compute_driver = fake.FakeDriverWithoutFakeNodes`.** The fake driver accepts
  every instance operation and allocates nothing. This variant reports a single
  node named after `[DEFAULT] host`, so the hypervisor, the compute service and
  the resource provider share one name.
- **A preseeded `compute_id`.** nova-compute writes a UUID to
  `state_path/compute_id` on its first start and identifies its compute node
  record by it from then on. `state_path` is an `emptyDir` here, so the file dies
  with the pod. Without the seed, a restarted pod generates a fresh UUID, tries
  to create a second compute node record under the same host and node name, and
  fails to start with `Duplicate compute node record`. The `node-identity` init
  container writes the fixed UUID `d13d13d1-0000-4000-8000-000000000003` before
  every start.
- **vif plugging off** (`vif_plugging_is_fatal = false`,
  `vif_plugging_timeout = 0`). No OVS interface backs a port on this pod, so no
  chassis claims one and `network-vif-plugged` never arrives. A compute that
  waited for it would time out on the boot of every server that carries a port.

`[DEFAULT] host` comes from `OS_DEFAULT__HOST`, set to the pod's node name,
`cobaltcore-control-plane` on the devstack (`<CLUSTER_NAME>-control-plane` when
you set `CLUSTER_NAME` for `make deploy-infra`). The OVN chassis on a node
registers under the node's name, and neutron binds a port only to a host that
has a live chassis, so a compute under any other name would leave every port
unbound once a chassis runs there.

## Steps

### 1. Apply the manifest

The Deployment mounts `controlplane-nova-compute-config`, which the nova operator
publishes once the projected Nova is up. Wait for `NovaReady`, then apply:

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=NovaReady --timeout=15m
kubectl apply -k deploy/kind/fake-compute
kubectl rollout status deploy/controlplane-fake-compute -n openstack --timeout=5m
```

If you already applied the overlay in the quick start's optional server boot,
the apply changes nothing.

### 2. Verify the compute registered

The compute reports itself over the message bus. Its service row reads `up`
within a minute of the rollout:

```bash
openstack --insecure compute service list --service nova-compute
openstack --insecure hypervisor list
```

Both list `cobaltcore-control-plane`: the service row is the process, the
hypervisor row is the compute node record the driver wrote. The compute also
publishes its capacity to Placement as a resource provider of the same name:

```bash
RP=$(openstack --insecure resource provider list \
  --name cobaltcore-control-plane -f value -c uuid)
openstack --insecure resource provider inventory list "$RP"
```

The inventory lists `VCPU`, `MEMORY_MB` and `DISK_GB`. Without the provider the
scheduler has nothing to claim against, and a boot fails with
`No valid host was found` instead of anything that names the compute.

### 3. Map the host into the cell

The scheduler places an instance only on a host that has a host mapping, and the
scheduler's discovery periodic writes it within 300 seconds. To map the host now,
run the discovery by hand in the conductor pod:

```bash
kubectl exec -n openstack deploy/controlplane-nova-conductor -c conductor -- \
  nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 discover_hosts --verbose
```

`--verbose` names the host it mapped. A run that maps nothing either found the
host already mapped or ran before the compute registered; repeat it once the
service row reads `up`. [Nova Cells](../../reference/nova/nova-cells.md#host-discovery)
describes the periodic.

### 4. Boot a server

A flavor the fake driver can satisfy, a 1 MiB image it never reads, and a server
without a network. If you kept `demo-server` from the quick start's optional
server boot, it is already this server; skip to step 5.

```bash
openstack --insecure flavor create --vcpus 1 --ram 128 --disk 1 m1.nano
truncate -s 1M /tmp/boot-image.raw
openstack --insecure image create --disk-format raw --container-format bare \
  --file /tmp/boot-image.raw boot-image
for _ in $(seq 30); do
  [ "$(openstack --insecure image show boot-image -f value -c status)" = active ] && break
  sleep 2
done
openstack --insecure server create --image boot-image --flavor m1.nano \
  --nic none --wait demo-server
openstack --insecure server show demo-server -c status -f value
```

The last command prints `ACTIVE`. The loop before the boot waits for the image
to reach `active`: `image create` returns before the store write completes, and
nova refuses to boot from an image that is still `saving`. `--nic none` is what
lets the boot finish on this devstack: it runs no `OVNChassis`, so a port on a
network could never bind.

A server in `ERROR` whose fault reads `No valid host was found` (see
`openstack --insecure server show demo-server -c fault`) was booted before the
host was mapped. Run the discovery from step 3, delete the server, and boot it
again.

### 5. (Optional) Resize the server

The fake compute is the only host, so a resize has to land on the host the
server already runs on. nova leaves that host out of the candidates unless
`allow_resize_to_same_host` is set, and the resize then finds no valid host.
Set it on the ControlPlane, which projects it into the child's
`spec.extraConfig`:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"extraConfig":{"DEFAULT":{"allow_resize_to_same_host":"true"}}}}}}'
ok=
for _ in $(seq 120); do
  CM=$(kubectl get deploy/controlplane-nova -n openstack \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')
  kubectl get configmap "$CM" -n openstack -o jsonpath='{.data.nova\.conf}' \
    | grep -Fxq 'allow_resize_to_same_host = true' && { ok=1; break; }
  sleep 5
done
if [ -n "$ok" ]; then
  kubectl rollout status deploy/controlplane-nova -n openstack --timeout=10m
else
  echo "controlplane-nova does not mount the option after 10m: check the Nova CR's DatabaseReady condition" >&2
fi
```

The option lands in a new config ConfigMap, and the nova operator re-runs the
db-sync Job against it before it touches the API Deployment. Until that Job
finishes, the Deployment still mounts the old ConfigMap and describes the old
pods, and a rollout check passes at once. The loop therefore waits until the
ConfigMap the Deployment mounts carries the option, and the rollout check then
waits for the API pods to run on it. A failed db-sync Job holds the Deployment
on the old ConfigMap, so the loop gives up after ten minutes and names the
condition that reports the Job. On a second run the option is already mounted
and the loop ends at once. The option is read by the compute API when it plans a
resize, so the fake compute does not need it. Then resize onto a second flavor
and confirm:

```bash
openstack --insecure flavor create --vcpus 1 --ram 256 --disk 1 m1.small
openstack --insecure server resize --flavor m1.small --wait demo-server
openstack --insecure server show demo-server -c status -f value
openstack --insecure server resize confirm demo-server
```

The status reads `VERIFY_RESIZE` before the confirm and `ACTIVE` after it. To
take the option out again, set the override to `null` with the same patch shape:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"extraConfig":null}}}}'
```

## What the fake compute cannot do

- **Serve a console.** The fake driver reports `fakevncconsole.com:6969` as the
  VNC address of every instance. The console proxy resolves the token and dials
  that address, which answers nothing.
  [Expose the Console Proxy](./expose-the-console-proxy.md) walks the proxy up to
  that point.
- **Bind a port.** A server with a network gets a port that stays unbound: that
  needs an `OVNChassis` on the node, which in turn needs the Open vSwitch kernel
  modules of a `WITH_OVN_KERNEL_MODULES=true` bring-up.
  [Label a Node as a Compute or Network Node](../ovn/label-a-chassis-node.md)
  covers the chassis side.

## Removing the fake compute

Remove it in this order:

```bash
# 1. The servers on it. The service delete below is refused while any remain.
openstack --insecure server delete --wait demo-server

# 2. The compute itself. Wait until its pod is gone, so it cannot report again.
kubectl delete -k deploy/kind/fake-compute
kubectl wait --for=delete pod -l app.kubernetes.io/name=nova-fake-compute \
  -n openstack --timeout=2m

# 3. Its service record, which also drops the resource provider and the host
#    mapping.
SERVICE_ID=$(openstack --insecure compute service list --service nova-compute \
  --host cobaltcore-control-plane -f value -c ID)
openstack --insecure compute service delete "$SERVICE_ID"
```

The order matters twice. `openstack compute service delete` is refused while the
host carries instances. And a compute that is still running when its service
record goes registers again on its next report, which leaves a fresh service
record behind that no host mapping points at.

Then remove what step 4 and step 5 created:

```bash
openstack --insecure image delete boot-image
openstack --insecure flavor delete m1.nano
openstack --insecure flavor delete m1.small   # only if you ran step 5
```

## Tested by

The full-ControlPlane suite runs this Deployment: it applies it against the
ControlPlane's contract Secret, maps the host with the same discovery command
and boots a server on it. The Nova suite runs a Deployment of its own against a
standalone `Nova` CR, under the fixed host name `fake-1`: it waits for the
scheduler's discovery periodic instead of running the command, and boots,
resizes and deletes a server with `allow_resize_to_same_host` set on the `Nova`
CR from the start. No suite runs step 5's ControlPlane patch or the removal
sequence.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
chainsaw test --test-dir tests/e2e/nova/basic-deployment
```

::: details The fixture the full-ControlPlane suite applies
The suite names its ControlPlane `controlplane-keystone`, so its fixture carries
that prefix: `controlplane-keystone-nova-compute-config`,
`controlplane-keystone-fake-compute`. The manifest above is this fixture with the
prefix replaced by `controlplane`, and a unit test pins the two to each other.

<<< @/../tests/e2e/c5c3/full-controlplane-keystone/06-fake-compute.yaml
:::
