---
title: Connect a Compute Cluster
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Connect a Compute Cluster

The control plane runs no `nova-compute`. A compute node belongs to a
hypervisor, and hypervisors run in clusters of their own. What the projected
Nova publishes for them is the compute contract, one Secret carrying the
configuration fragment and the credentials a `nova-compute` joins the control
plane with. This guide is written for the owner of a compute cluster: it
describes what the Secret carries, how to get it onto the compute cluster
today, which of its addresses do not resolve from there yet and how to override
each, and what the compute side has to provide in return.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through the **Boot a first server** checks in Step 6, so
the projected `controlplane-nova` child is `Ready` in `openstack` and has
published `controlplane-nova-compute-config`. Every resource name in the
examples below is one that devstack produces.
:::

- A compute cluster you can reach with `kubectl`. The commands below name it
  through two substitution rules: `COMPUTE_CONTEXT` is the kubeconfig context of
  that cluster, and `COMPUTE_NAMESPACE` is the namespace its `nova-compute` pods
  run in. Export both before you copy a command.
- `jq` on `PATH`.

## What the contract carries

The Secret is `controlplane-nova-compute-config` in `openstack`, and the child
names it in `status.computeConfigSecretRef`. It keeps that name for the lifetime
of the Nova and is updated in place.

| Key | Content |
| --- | --- |
| `nova-compute.conf` | The `nova.conf` fragment a `nova-compute` reads |
| `transport_url` | The `rabbit://` URL of the message bus, with the broker user and password |
| `password` | The password of the `nova` service user every client section authenticates as |
| `metadata_proxy_shared_secret` | The value the Neutron metadata agent signs proxied instance requests with |
| `cell_name` | `cell1`, the cell the compute is mapped into |
| `ca.crt` | The broker's CA bundle, only while the bus uses TLS |

The fragment carries `[DEFAULT]`, `[keystone_authtoken]`, `[service_user]`,
`[placement]`, `[neutron]`, `[glance]`, `[oslo_messaging_rabbit]`,
`[oslo_messaging_notifications]`, `[upgrade_levels]` and `[vnc]`, plus `[cinder]`,
`[key_manager]` and `[barbican]` while the ControlPlane declares block storage
and the key manager. It carries no database, cache, scheduler, conductor or
`libvirt` section, no `[DEFAULT] host`, no `state_path` and none of the
ControlPlane's `extraConfig`: those belong to the compute deployment. The
[Compute contract](../../reference/nova/nova-crd.md#compute-contract) reference
lists every omission with its reason.

The mount path is part of the contract. `[oslo_messaging_rabbit] ssl_ca_file`
points at `/etc/nova/compute-config/ca.crt`, so a compute that mounts the Secret
anywhere else finds no CA bundle where its own configuration says one is.

::: warning Three of the keys are credentials
`transport_url`, `password` and `metadata_proxy_shared_secret` are credentials:
the broker login, the `nova` service user's password, and the secret that
authenticates every metadata request. Treat the Secret and every copy of it like
the admin credential. Only `nova-compute.conf`, `cell_name` and `ca.crt`, a
public CA bundle, carry nothing secret. Do not print the Secret's data to a
terminal you share, and do not attach it to a ticket.
:::

## Copy the Secret to the compute cluster

The ControlPlane is meant to mirror the contract into every compute cluster it
serves, but no mirror target exists yet, so the copy is manual. Copy the data
and the name, and nothing of the source object's metadata:

```bash
kubectl get secret controlplane-nova-compute-config -n openstack -o json \
  | jq '{apiVersion, kind, type, data, metadata: {name: .metadata.name}}' \
  | kubectl --context "$COMPUTE_CONTEXT" apply --server-side -n "$COMPUTE_NAMESPACE" -f -
kubectl --context "$COMPUTE_CONTEXT" annotate secret controlplane-nova-compute-config \
  -n "$COMPUTE_NAMESPACE" kubectl.kubernetes.io/last-applied-configuration-
```

Both commands keep the credentials out of the copy's metadata. A client-side
`kubectl apply` stores the whole object, data included, in the
`kubectl.kubernetes.io/last-applied-configuration` annotation, where tools that
redact `data` but show annotations would print it. `--server-side` does not
create that annotation, but once a client-side apply or a GitOps tool has left
it on the copy, the API server rewrites it with every `kubectl apply
--server-side`, the new credentials included. The `annotate` command removes it;
on a copy that never carried it, the command changes nothing.

The copy is a snapshot. The source is updated in place whenever a value in it
changes: a rotated service-user password, a rotated broker credential, a new
metadata shared secret, or a ControlPlane change that alters the fragment, such
as publishing the console proxy. Repeat the copy after every such change, then
restart the compute pods: `nova-compute` reads its configuration and the
environment variables sourced from the Secret once, at start.

## Addresses that do not resolve from another cluster

The contract names three things by their address inside the management
cluster. A `nova-compute` in the same cluster, like the fake compute of the
[Run a Fake Compute for Testing](./run-a-fake-compute-for-testing.md) guide,
reaches all three. One in another cluster reaches none of them until you
override each.

### The message bus

`transport_url` names the management cluster's broker Service. The connection
carries the broker login of the whole cell and every Nova RPC message, and the
quick-start devstack's bus is plaintext, so expose the broker to the compute
cluster only once it serves TLS:

- Set `spec.infrastructure.messaging.tls` on the ControlPlane. The fragment then
  verifies the broker against the contract's `ca.crt`.
- Serve TLS on the broker with a certificate that names the address the compute
  dials. The ControlPlane configures only the client side; TLS on the broker
  itself is the platform's to set up.
- Admit only the compute nodes' addresses on the LoadBalancer Service
  (`loadBalancerSourceRanges`) or the Gateway TCP listener that exposes it.

Then give the compute a URL with the reachable address, the broker's TLS port
and the same credentials by setting `OS_DEFAULT__TRANSPORT_URL` on the
`nova-compute` container. oslo.config reads an `OS_<GROUP>__<OPTION>` variable
over the file, so the variable wins over the fragment.

### The Keystone endpoint

Every `auth_url` in the fragment is the in-cluster Keystone URL the child runs
with, `spec.keystoneEndpoint`:

```bash
kubectl get nova controlplane-nova -n openstack \
  -o jsonpath='{.spec.keystoneEndpoint}{"\n"}'
```

It appears in `[keystone_authtoken]`, `[service_user]`, `[placement]` and
`[neutron]`, and in `[cinder]` and as `[barbican] auth_endpoint` when those are
rendered. Override each with the public Keystone URL, which on the devstack is
`https://keystone.127-0-0-1.nip.io:8443/v3` and elsewhere the address your
Keystone Gateway publishes.

### The catalog interface

The client sections resolve the services they call from the Keystone catalog
with `valid_interfaces = internal`, and the internal rows the ControlPlane
registers are Service URLs such as `http://controlplane-placement.openstack.svc:8778`.
Overlay `valid_interfaces = public` in `[placement]`, `[neutron]` and `[glance]`,
change `[cinder] catalog_info` to `block-storage:cinder:publicURL` and
`[barbican] barbican_endpoint_type` to `public` when those are rendered, or pin
each service with `endpoint_override`.

The Keystone URL and the interface overrides are ordinary configuration, so they
fit in one overlay file beside the fragment. Load the fragment with
`--config-file` and the overlay with a later `--config-dir`, the way the fake
compute loads its own overlay: oslo.config reads the directory after the file,
so the overlay's values win.

```ini
# An overlay beside the fragment; the Keystone URL is the devstack's.
[keystone_authtoken]
auth_url = https://keystone.127-0-0-1.nip.io:8443/v3

[service_user]
auth_url = https://keystone.127-0-0-1.nip.io:8443/v3

[placement]
auth_url = https://keystone.127-0-0-1.nip.io:8443/v3
valid_interfaces = public

[neutron]
auth_url = https://keystone.127-0-0-1.nip.io:8443/v3
valid_interfaces = public

[glance]
valid_interfaces = public
```

## The metadata path

An instance asks `169.254.169.254` for its metadata. The Neutron metadata agent
on the compute cluster answers, signs the request with the shared secret, and
forwards it to the Nova metadata API on the control plane. Publish that API on a
hostname of its own with `services.nova.metadataGateway`. On the devstack the
Gateway already carries the listener `https-nova-metadata` for it:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"metadataGateway":{"parentRef":{"name":"openstack-gw"},"hostname":"nova-metadata.127-0-0-1.nip.io"}}}}}'
kubectl wait nova/controlplane-nova -n openstack --timeout=5m \
  --for=jsonpath='{.status.conditions[?(@.type=="MetadataHTTPRouteReady")].reason}'=HTTPRouteAccepted
```

`MetadataHTTPRouteReady` reads `True` under `HTTPRouteNotRequired` while no
metadata gateway is set, so the wait keys on the reason an accepted route sets.

The `NeutronMetadataAgent` on the compute cluster then points at that hostname
over HTTPS and signs with the key the contract carries. Its `sharedSecretRef`
names the Secret copied above, in the agent's own namespace:

```yaml
spec:
  novaMetadata:
    host: nova-metadata.127-0-0-1.nip.io   # nova-metadata.<your domain> elsewhere
    port: 443
    protocol: https
    sharedSecretRef:
      name: controlplane-nova-compute-config
      key: metadata_proxy_shared_secret
```

`port: 443` assumes a Gateway on the standard port; on the devstack's
`KIND_HOST_PORT=8443` mapping it is 8443. The agent verifies the listener's
certificate, so the compute cluster has to trust the issuer behind it.

## What the compute side owes

The contract is half of the handover. The compute cluster provides the rest:

- **One `nova-compute` pod per hypervisor node**, with `[DEFAULT] host` set to
  the node's name. The OVN chassis on the node registers under that name, and
  neutron binds a port only to a host with a live chassis.
- **A `state_path` that survives a restart.** nova-compute writes a UUID to
  `state_path/compute_id` on its first start and refuses to start without it
  while its compute node record exists, so the path needs storage that outlives
  the pod.
- **Its own `[libvirt]` section**, and whatever else configures the hypervisor.
  The contract carries nothing about the host it runs on.
- Under [openstack-hypervisor-operator](https://github.com/cobaltcore-dev/openstack-hypervisor-operator)
  (as read at `d99cffc` on 2026-09-11), the agent pods run in one of the
  namespaces its `--agent-namespaces` flag lists and do not tolerate the
  `kvm.cloud.sap/offboarding:NoExecute` taint, so offboarding a node evicts them.
- **A service user for that operator** with admin reach into Nova and
  Placement. Its client takes the first public endpoint of each service from the
  catalog.
- **The fixtures its smoke test expects:** a project `test` in the domain
  `cc3test`, an image `cirros-kvm`, a flavor `1`, a volume type `premium` and a
  non-shared network. Set `spec.skipTests` on its resources to skip the test
  instead.
- **One aggregate per value of the nodes' `topology.kubernetes.io/zone` label**,
  created before the first node joins, plus an aggregate named
  `tenant_filter_tests`.

### Onboarding latency

A new compute registers itself when it starts, but the scheduler places nothing
on it until it has a host mapping. The scheduler's discovery periodic writes the
mapping within 300 seconds. A consumer that polls for it adds its own interval:
openstack-hypervisor-operator polls every 60 seconds, so it sees a new node as
mapped up to 360 seconds after the compute registered.
[Nova Cells](../../reference/nova/nova-cells.md#host-discovery) describes the
periodic and the command that maps a host at once.

### Offboarding

Removing a node goes through the compute API. `openstack compute service delete`
on the node's service record also removes its host mapping, its resource
provider and its aggregate membership, and is refused while instances remain on
it. openstack-hypervisor-operator issues that delete itself. Nothing removes the
node's chassis record from the OVN Southbound database: that stays behind for
whoever runs the network side to clean up.

## What this repository tests

Only the in-cluster half. The fake compute of
[Run a Fake Compute for Testing](./run-a-fake-compute-for-testing.md) consumes
the same Secret the way a compute cluster would, from the management cluster
itself, and the suites below boot servers on it. No suite copies the Secret to
another cluster, exposes the broker, overrides the Keystone endpoint or the
catalog interface, runs a real hypervisor, or runs openstack-hypervisor-operator.
Everything in the sections on addresses and on what the compute side owes is
therefore the contract as written, not as tested.

## Tested by

The full-ControlPlane suite asserts the contract Secret the ControlPlane's Nova
publishes and runs the fake compute against it; the Nova suite does the same
against a standalone `Nova` CR.

```bash
chainsaw test --test-dir tests/e2e/c5c3/full-controlplane-keystone
chainsaw test --test-dir tests/e2e/nova/basic-deployment
```
