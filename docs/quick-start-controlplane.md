---
title: Deploy a ControlPlane
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Deploy a ControlPlane {#quick-start-controlplane-c5c3-k-orc-on-kind}

A ControlPlane describes the OpenStack services you want CobaltCore to run.
CobaltCore deploys those services, connects them to their databases and message
bus, and registers their APIs in the service catalog.

This tutorial adds image storage, networking, secrets and compute APIs to the
identity service and dashboard from the [Quick Start](./quick-start.md). You
will verify the APIs by creating and removing sample resources.

Use the same `cobaltcore` kind cluster and the same `controlplane` resource in
`openstack`. Applying the expanded configuration updates that ControlPlane;
keep the Quick Start environment running.

The local environment tests OpenStack APIs. It has no KVM hypervisors; the
optional fake compute simulates a server without running a virtual machine.
For real servers and networking, follow
[Deploy on bare metal](./quick-start-metal-stack.md).

## Prerequisites

- A running CobaltCore operator stack on kind, from the Quick Start or
  [Deploy on local Kubernetes](./deployment/local-kubernetes.md). If you are
  starting from scratch, complete Steps 1 and 2 below. The stack must use
  `WITH_CONTROLPLANE=true`.
- The [local toolchain](./deployment/local-kubernetes.md), including `make`,
  `kubectl`, `jq` and `yq` v4.40.1 or newer.
- The OpenStack CLI with `osc-placement` and `python-barbicanclient` for the
  service checks. Install them in a virtual environment:

  ```bash
  python3 -m venv .venv-openstack
  . .venv-openstack/bin/activate
  python -m pip install python-openstackclient osc-placement python-barbicanclient
  ```

- At least 4 CPU cores, 16 GB of memory available to the container runtime,
  and 10 GB of free disk. Allow extra capacity for the optional fake compute.

This tutorial uses the `Minimal` profile: one replica per service and a small,
single-instance database. `Standard` provides multiple replicas and a larger
Galera database for HA deployments, which also need suitable nodes and storage.
Choose the profile before creating the database: its replica count and storage
size are immutable. See
[Advanced configuration](./guides/advanced-configuration.md) for sizing and
production requirements.

## Step 1 — Get the example files {#step-1-—-clone}

If you already cloned the repository for the Quick Start, use that checkout.
Otherwise:

```bash
git clone https://github.com/c5c3/cobaltcore.git
cd cobaltcore
```

Run the remaining commands from the repository root. Install the tools after
entering the checkout:

```bash
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"
```

## Step 2 — Prepare your local environment {#step-2-—-cluster-controlplane-stack}

If the Quick Start or local deployment guide already installed the operator
stack, select that cluster and continue to Step 3:

```bash
kind export kubeconfig --name cobaltcore
kubectl config use-context kind-cobaltcore
kubectl get nodes
```

For a fresh local environment, start your container runtime and run:

```bash
EXTERNAL_CLUSTER=false KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
kubectl config use-context kind-cobaltcore
```

This installs CobaltCore and its dependencies. It leaves the ControlPlane for
you to create in Step 4. The HTTPS APIs use local port `8443`. Allow 5 to 10
minutes for the operators to become ready. For Podman setup and other local
options, see [Deploy on local Kubernetes](./deployment/local-kubernetes.md).

An older local setup with a standalone Keystone uses a different deployment
mode. Preserve any data you need, then remove that disposable environment with
the local deployment guide's cleanup instructions before starting this one.
The current Quick Start already uses ControlPlane mode and needs no cleanup.

If a download or image pull fails, fix the reported error and rerun the same
command after selecting `kind-cobaltcore`. The script reuses the cluster. A
transient failure does not require deleting it.

For the optional Cinder volume check, install NFS storage before Step 4. This
also works on the existing Quick Start cluster:

```bash
kubectl config use-context kind-cobaltcore
EXTERNAL_CLUSTER=false KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

NFS adds a local server and CSI driver. On Linux, the helper loads the required
kernel modules through sudo. See the
[NFS storage reference](./reference/infrastructure/infrastructure-manifests.md#nfs-storage-stack-opt-in)
for details.

Before applying any examples, confirm the context:

```bash
test "$(kubectl config current-context)" = kind-cobaltcore
```

These examples use kind hostnames and storage. For another cluster, start with
[Deploy on an existing Kubernetes cluster](./deployment/existing-kubernetes.md)
and use that environment's endpoints and backing services.

## Step 3 — Enable networking {#step-3-—-deploy-the-ovn-control-plane}

Neutron needs an OVN control plane to create networks. Save this as
`controlplane-ovn.yaml`, then apply it:

```yaml
# controlplane-ovn.yaml
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNCentral
metadata:
  name: controlplane-ovn
  namespace: openstack
spec:
  tls:
    issuerRef:
      name: openstack-ovn-ca-issuer
  northbound:
    replicas: 1
  southbound:
    replicas: 1
  northd:
    deployment:
      replicas: 1
```

```bash
kubectl apply -f controlplane-ovn.yaml
kubectl wait ovncentral/controlplane-ovn -n openstack \
  --for=condition=Ready --timeout=10m
```

The ControlPlane references this OVN instance. Deleting only the ControlPlane
leaves OVN running.

## Step 4 — Choose your services {#step-4-—-create-the-controlplane-cr}

Save the following as `controlplane.yaml`. It enables these services:

| Service | What you can do |
| --- | --- |
| Keystone and Horizon | Authenticate and use the OpenStack dashboard |
| Glance | Store images in the bundled S3 object store |
| Placement and Nova | Track capacity and use the compute API |
| Barbican | Store and retrieve secrets |
| Neutron | Create networks using the OVN instance from Step 3 |

The name and namespace match the Quick Start. Keep them unchanged to extend
that ControlPlane. The endpoints below use its local HTTPS port, `8443`.

```yaml
# controlplane.yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  openStackRelease: "2026.1"
  infrastructure:
    messaging: {}
  services:
    keystone:
      publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
      gateway:
        parentRef:
          name: openstack-gw
        hostname: keystone.127-0-0-1.nip.io
        path: /
    horizon:
      gateway:
        parentRef:
          name: openstack-gw
        hostname: horizon.127-0-0-1.nip.io
    glance:
      publicEndpoint: https://glance.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw
        hostname: glance.127-0-0-1.nip.io
      backends:
        - name: default
          type: S3
          isDefault: true
          s3:
            endpoint: http://garage.shared-services.svc.cluster.local:3900
            bucket: glance-images
            region: garage
            credentialsSecretRef:
              name: garage-s3-credentials
    placement:
      publicEndpoint: https://placement.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw
        hostname: placement.127-0-0-1.nip.io
    barbican:
      secretStore:
        dedicated: {}
      publicEndpoint: https://barbican.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw
        hostname: barbican.127-0-0-1.nip.io
    neutron:
      publicEndpoint: https://neutron.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw
        hostname: neutron.127-0-0-1.nip.io
      ovn:
        centralRef:
          name: controlplane-ovn
    nova:
      publicEndpoint: https://nova.127-0-0-1.nip.io:8443
      gateway:
        parentRef:
          name: openstack-gw
        hostname: nova.127-0-0-1.nip.io
  sizing:
    profile: Minimal
```

```bash
kubectl apply -f controlplane.yaml
```

CobaltCore supplies the database and cache configuration. The `messaging`
block enables the message bus required by Neutron and Nova. Nova also requires
Glance, Placement and Neutron, which this example includes.

For an identity-only starting point, the repository provides
`deploy/kind/controlplane/controlplane.yaml`; the Quick Start applies that
manifest with the local port configured. The expanded example above adds the
services used by the checks below.

::: details Optional: block storage (needs WITH_NFS=true in Step 2)
Add this fragment under `spec.services` in `controlplane.yaml`, alongside
`neutron`, then apply the file again. It stores volumes and backups on the NFS
server installed in Step 2.

```yaml
# block-storage.yaml
cinder:
  publicEndpoint: https://cinder.127-0-0-1.nip.io:8443
  gateway:
    parentRef:
      name: openstack-gw
    hostname: cinder.127-0-0-1.nip.io
  backends:
    - name: nfs1
      type: NFS
      nfs:
        server: nfs-server.openstack.svc.cluster.local
        path: /volumes
  backupBackend:
    name: nfsbk
    type: NFS
    nfs:
      server: nfs-server.openstack.svc.cluster.local
      path: /backups
```

Without the NFS overlay, Cinder cannot mount its storage and
`CinderReady` remains false.
:::

## Step 5 — Enable database access {#step-5-—-onboard-the-openbao-database-engine-tenant}

CobaltCore uses OpenBao to issue database credentials for each service. Run
this setup after adding the services in Step 4, including when you continued
from the Quick Start. The Quick Start configured credentials only for the
services present at that time.

Wait for the database, then configure access for every service now declared in
the ControlPlane:

```bash
kubectl wait mariadb/openstack-db -n openstack --for=condition=Ready --timeout=10m

export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n shared-services \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')
deploy/openbao/bootstrap/setup-database-tenant.sh openstack controlplane
unset BAO_TOKEN
```

The arguments are the ControlPlane namespace and name. Repeating the setup
refreshes the configuration in place. Rerun it whenever you enable another
service that needs a database.

The bootstrap token command is for this local environment. An existing
OpenBao deployment needs an authorized token with access to
`database/mariadb/*`; see [Advanced configuration](./guides/advanced-configuration.md).

If a service waits for credentials and its ExternalSecret reports
`SecretSyncedError` with an unknown database role, run this setup again.
External Secrets retries the request automatically.

## Step 6 — Wait for readiness {#step-6-—-watch-the-chain-reconcile}

```bash
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=Ready --timeout=15m
```

`Ready=True` means all 21 sub-conditions are satisfied, including the enabled
services and their catalog registrations. If the wait times out, inspect the
condition reasons:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
kubectl get pods -n openstack
kubectl get events -n openstack --sort-by=.lastTimestamp
```

<details>
<summary>Readiness conditions</summary>

These conditions help identify the dependency that still needs attention.
Several services can become ready at the same time.

| Condition | Waits for |
| --- | --- |
| `SizingReady` | Nothing |
| `NamespacesReady` | `SizingReady`. Reads `True/NoDedicatedNamespaces` here: no service has a namespace of its own |
| `InfrastructureReady` | `NamespacesReady`, then the backing services of `spec.infrastructure` |
| `ESOTenantStoreReady` | `InfrastructureReady`, then the secret store `openbao-tenant-store` |
| `DBCredentialsReady` | `ESOTenantStoreReady`, then the onboarding of Step 5 |
| `AdminPasswordReady` | `DBCredentialsReady`, then the admin password from OpenBao |
| `KeystoneReady` | `AdminPasswordReady`, then the Keystone child `controlplane-keystone` |
| `HorizonReady` | `KeystoneReady` |
| `KORCReady` | Keystone is ready |
| `AdminCredentialReady` | `KORCReady` |
| `CatalogReady` | `AdminCredentialReady` |
| `GlanceReady` | `KeystoneReady` and its own `KeystoneService` registration |
| `PlacementReady` | `KeystoneReady` and its own registration |
| `BarbicanReady` | `KeystoneReady` and its own registration |
| `OVNReady` | The referenced OVNCentral: `controlplane-ovn` from Step 3 |
| `NeutronReady` | `KeystoneReady`, `OVNReady`, its registration and the message bus in its namespace |
| `CinderReady` | `KeystoneReady`, its registration and the message bus. Reads `True/CinderNotManaged` without the block-storage block |
| `NovaReady` | `KeystoneReady`, `PlacementReady`, its registration and the message bus |
| `ServiceAccountsReady` | The registrations of glance, placement, barbican, neutron, neutron-nova, cinder when present, and nova |
| `RegistrationTenantStoresReady` | Nothing. Reads `True/NoRegistrationNamespaces` here: no other namespace registers a service against this ControlPlane |
| `NamespaceAssignmentsReady` | Nothing. Reads `True/NoNamespaceAssignments` here: no namespace is assigned to a service owner |

</details>

For the complete dependency graph, see
[ControlPlane reconciliation](./reference/c5c3/controlplane-reconciler.md#reconciliation-flow).

## Step 7 — Verify your services {#step-7-—-verify}

Authenticate through the local gateway:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret controlplane-keystone-admin-credentials -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
openstack --insecure catalog list
```

The token command should return an ID and expiry time. The catalog should
include `identity`, `image`, `placement`, `key-manager`, `network` and
`compute`, plus `block-storage` if you enabled Cinder.

Keep these `OS_*` variables exported for the checks below. `--insecure` accepts
the local gateway's self-signed certificate; use trusted certificates outside
this local environment. If `*.nip.io` does not resolve, follow the
[local DNS troubleshooting](./quick-start-extended.md#access-keystone-from-your-local-machine).

### Upload a first image

Upload a throwaway file and wait for its status to become `active`:

```bash
dd if=/dev/urandom of=/tmp/first.img bs=1024 count=1
openstack --insecure image create --disk-format raw --container-format bare \
  --file /tmp/first.img first-image
for _ in $(seq 30); do
  [ "$(openstack --insecure image show first-image -f value -c status)" = active ] && break
  sleep 2
done
test "$(openstack --insecure image show first-image -f value -c status)" = active \
  && echo "OK: image uploaded and active"
openstack --insecure image delete first-image
```

The upload completes asynchronously. An `active` image confirms that Glance
can write to its object store. This sample file is not a bootable VM image.

### List placement resource classes

```bash
openstack --insecure resource class list
```

Expect classes such as `VCPU`, `MEMORY_MB` and `DISK_GB`. If the CLI does not
recognize the command, install the `osc-placement` plugin from the prerequisites.

### Store and retrieve a first secret

Use a disposable payload to verify Barbican:

```bash
PAYLOAD='cobaltcore-quick-start-payload'
HREF=$(openstack --insecure secret store --name first-secret \
  --payload "$PAYLOAD" -f value -c 'Secret href')
test "$(openstack --insecure secret get -p "$HREF" -f value -c Payload)" = "$PAYLOAD" \
  && echo "OK: payload came back unchanged"
openstack --insecure secret delete "$HREF"
```

The command needs `python-barbicanclient`. Use only test data here: command
arguments can appear in process listings and shell history.

### Create a first network

```bash
openstack --insecure network create demo-net
openstack --insecure subnet create --network demo-net --subnet-range 192.0.2.0/24 demo-subnet
openstack --insecure network show demo-net -c status -f value
```

Expect `ACTIVE`. This confirms that Neutron can create the network in OVN;
it does not attach a real compute host. Remove the sample resources:

```bash
openstack --insecure subnet delete demo-subnet
openstack --insecure network delete demo-net
```

### Create a first volume

Skip this check unless you enabled the block-storage block of Step 4 and NFS
in Step 2. Volume creation is asynchronous:

```bash
openstack --insecure volume create --size 1 demo-vol
for _ in $(seq 60); do
  [ "$(openstack --insecure volume show demo-vol -c status -f value)" = available ] && break
  sleep 2
done
test "$(openstack --insecure volume show demo-vol -c status -f value)" = available \
  && echo "OK: volume is available"
```

Keep `demo-vol` if you are continuing to
[Configure NFS backups](./guides/cinder/configure-nfs-backups.md). Otherwise,
remove it:

```bash
openstack --insecure volume delete demo-vol
```

### Boot a first server

Check the compute API:

```bash
openstack --insecure compute service list
```

Expect `nova-scheduler` and `nova-conductor` to report `up`. No `nova-compute`
service is present yet: the local ControlPlane runs compute APIs, but has no
hypervisor to run virtual machines.

::: details Optional: boot a server on a fake compute
This adds a simulated compute host for testing scheduling and the server
lifecycle. Its instances do not run a guest operating system.

```bash
kubectl apply -k deploy/kind/fake-compute
kubectl rollout status deploy/controlplane-fake-compute -n openstack --timeout=5m
```

Wait for the compute service, then register it with Nova's cell:

```bash
for _ in $(seq 24); do
  [ "$(openstack --insecure compute service list --service nova-compute \
    --host cobaltcore-control-plane -f value -c State)" = up ] && break
  sleep 5
done
openstack --insecure compute service list --service nova-compute
kubectl exec -n openstack deploy/controlplane-nova-conductor -c conductor -- \
  nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 discover_hosts --verbose
```

`openstack --insecure hypervisor list` should show `cobaltcore-control-plane`.
Create a test flavor and image, then request a server:

```bash
openstack --insecure flavor create --vcpus 1 --ram 128 --disk 1 m1.nano
dd if=/dev/zero of=/tmp/boot-image.raw bs=1024 count=1024
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

Expect `ACTIVE`. Use `--nic none`: this example has no OVN chassis to bind a
network port. If the server reports `No valid host was found`, confirm the
compute service is `up`, rerun host discovery, and recreate the server.

Keep the server if you are continuing to
[Expose the console proxy](./guides/nova/expose-the-console-proxy.md).
Otherwise, remove the test resources:

```bash
openstack --insecure server delete --wait demo-server
openstack --insecure image delete boot-image
openstack --insecure flavor delete m1.nano
```

The fake compute remains running. See
[Run a fake compute for testing](./guides/nova/run-a-fake-compute-for-testing.md)
for removal and further checks. To run real VMs, follow
[Deploy on bare metal](./quick-start-metal-stack.md).
:::

### Open the Horizon dashboard

Open [Horizon](https://horizon.127-0-0-1.nip.io:8443/) in your browser. Accept
the local self-signed certificate, then sign in with user `admin`, domain
`Default`, and the password from `controlplane-keystone-admin-credentials`.

Use the Identity panel to inspect users and projects. The Project panel shows
the images, networks and other resources managed through the APIs.

## Teardown

Keep the cluster for [Provision resources](./quick-start-orders.md) or other
tutorials. When you are finished, this command deletes the local environment
and its data:

```bash
kubectl config use-context kind-cobaltcore
EXTERNAL_CLUSTER=false make teardown-infra
```

## Related references

- [Provision resources](./quick-start-orders.md): request databases, credentials
  and other resources for an application.
- [Advanced configuration](./guides/advanced-configuration.md): sizing, storage
  and deployment options.
- [ControlPlane API reference](./reference/c5c3/controlplane-crd.md): all fields
  and status conditions.
- [ControlPlane reconciliation](./reference/c5c3/controlplane-reconciler.md):
  dependencies between services.
