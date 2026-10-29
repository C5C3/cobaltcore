---
title: Attach an LDAP Domain Backend to Keystone
quadrant: operator
---

# Attach an LDAP Domain Backend to Keystone

This guide walks through attaching an LDAP/AD-backed identity domain to a
running Keystone with the `KeystoneIdentityBackend` CRD: create the bind
credentials Secret, apply one CR, watch the conditions converge, and verify
an LDAP user can authenticate. The worked example uses the same
`docker-test-openldap` fixture the e2e suite deploys (seeded
`dc=planetexpress,dc=com` tree), so every value is copy-pasteable against a
kind cluster.

For the full field reference, see the
[KeystoneIdentityBackend CRD API Reference](../../reference/keystone/identity-backend-crd.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a Keystone CR named
`keystone` is `Ready` in the `openstack` namespace. Every resource name in the
examples below is one that devstack produces.
:::

The LDAP server must be reachable from the cluster, with a bind DN allowed to
search the user/group trees. Step 1 deploys a seeded fixture directory on the
kind devstack; for an existing LDAP server, use your own directory settings.

## Step 1 — Set up the LDAP directory and bind credentials

::: info Service users stay SQL-backed
OpenStack service accounts and the bootstrap admin must remain in the SQL-backed
`Default` domain. The CRD rejects attaching a backend to `Default` for this
reason. Use the LDAP domain for human users and `Default` for services.
The LDAP backend is read-only by default.
:::

On the kind devstack, deploy the same OpenLDAP directory the e2e suite uses.
It is a plain namespace-pinned manifest, so `kubectl apply` runs it verbatim:

```bash
kubectl apply -f tests/e2e/keystone/ldap-domain-backend/00-openldap.yaml
kubectl -n openstack rollout status deploy/openldap
```

This ships, all in the `openstack` namespace:

- a seeded `dc=planetexpress,dc=com` tree (users under `ou=people` with
  `objectClass: inetOrgPerson`, `uid`/`mail` attributes, and `userPassword`
  equal to the `uid`);
- a Service `openldap` on port `10389`
  (`ldap://openldap.openstack.svc.cluster.local:10389`);
- a bind-credentials Secret `openldap-bind` holding
  `cn=admin,dc=planetexpress,dc=com` / `GoodNewsEveryone`.

The fixture creates `openldap-bind`, which the backend CR in Step 2 references.
No separate Secret creation is needed for the kind devstack.

To connect to an existing LDAP server instead, skip the fixture deployment and
create `corp-ldap-bind` in the same namespace as the backend CR. Replace the
example credentials below with your server's bind DN and password. The operator
reads the fixed data keys `username` and `password`:

```bash
kubectl create secret generic corp-ldap-bind -n openstack \
  --from-literal=username='cn=admin,dc=planetexpress,dc=com' \
  --from-literal=password='GoodNewsEveryone'
```

In Step 2, change `bindCredentialsSecretRef.name` to `corp-ldap-bind` and replace
the LDAP URL, suffix, and user mappings with your directory's settings.

The operator watches the referenced bind Secret. Rotating its credentials
re-renders the per-domain config automatically.

## Step 2 — Apply the backend CR

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: corp-ldap
  namespace: openstack
spec:
  keystoneRef:
    name: keystone
  domain:
    name: planetexpress
    mode: Manage           # the operator creates the domain
    deletionPolicy: Retain # keep the domain when this CR is deleted
  type: LDAP
  ldap:
    url: ldap://openldap.openstack.svc.cluster.local:10389
    bindCredentialsSecretRef:
      name: openldap-bind
    suffix: dc=planetexpress,dc=com
    readOnly: true         # the default, spelled out: keystone never writes to LDAP
    users:
      treeDN: ou=people,dc=planetexpress,dc=com
      objectClass: inetOrgPerson
      idAttribute: uid
      nameAttribute: uid
      mailAttribute: mail
```

::: warning Existing Keystone domains
Pick `mode: Adopt` when the domain already exists in Keystone. Adopt resolves
the domain by name and never mutates it. Adopted domains are always retained
when the backend CR is deleted.
:::

For `ldaps://` endpoints, add certificate verification:

```yaml
    tls:
      caBundleSecretRef:
        name: corp-ldap-ca   # data key must be "ca.crt"
```

## Step 3 — Watch the conditions converge

```bash
kubectl get keystoneidentitybackends -n openstack
NAME        READY   DOMAIN          KEYSTONE   AGE
corp-ldap   True    planetexpress   keystone   1m
```

`kubectl describe keystoneidentitybackend corp-ldap -n openstack` shows the
progression: `DomainReady=True` (the domain exists in Keystone), then
`ConfigProjected=True` (the rendered `keystone.planetexpress.conf` is mounted
in the Keystone Deployment at `/etc/keystone/domains/`), then `Ready=True`.
The Keystone CR itself reports the aggregate across all attached backends via
its `IdentityBackendsReady` condition; a pending backend holds the Keystone's
`Ready` at False so a half-attached domain never goes unnoticed.

The figure shows that order for every satellite kind. Its gate,
`CredentialsReady`, is `DomainReady` on an identity backend.
[The handshake](../../reference/backend/kubernetes-packages.md#satellite-handshake)
lists the five steps and what differs per kind.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

The domains Secret is content-hashed, so the attach rolls the Keystone
Deployment once; subsequent reconciles are no-ops until the spec or the bind
credentials change.

## Step 4 — Verify an LDAP user can authenticate

Use the admin credentials from the Quick Start to list users and grant
`professor` the `member` role on the LDAP domain. The role assignment is stored
in SQL and does not modify LDAP. A domain-scoped token requires a role
assignment on that domain.

```bash
# List the LDAP-backed users through the Keystone API (admin credentials):
openstack user list --domain planetexpress

openstack role add --user professor --user-domain planetexpress \
  --domain planetexpress member

# Issue a token as an LDAP user (docker-test-openldap seeds password == uid):
(
  # Clear inherited project/system scope without changing the admin shell:
  unset OS_PROJECT_ID OS_PROJECT_NAME OS_PROJECT_DOMAIN_ID OS_PROJECT_DOMAIN_NAME
  unset OS_SYSTEM_SCOPE
  openstack --os-username professor --os-password professor \
    --os-user-domain-name planetexpress \
    --os-domain-name planetexpress token issue
)
```

Without the openstack CLI, use the `urllib`/`curl` requests in
[Step 4 in the e2e test](https://github.com/C5C3/cobaltcore/blob/main/tests/e2e/keystone/ldap-domain-backend/chainsaw-test.yaml#L161)
for an in-cluster variant. The test obtains an admin token, lists LDAP users,
grants the domain role, and requests an LDAP user's token.

## Deleting a backend

`kubectl delete keystoneidentitybackend corp-ldap -n openstack` de-projects the config
first (keystone never runs with config pointing at a dead domain) and then
applies `spec.domain.deletionPolicy`:

| Mode / policy | Effect on the Keystone domain |
| --- | --- |
| `Manage` + `Retain` (default) | Domain stays, including its users' assignments. |
| `Manage` + `Delete` | Domain is disabled, then deleted. |
| `Adopt` (any policy) | Domain always stays — the operator never destroys what it did not create. |

`deletionPolicy` is mutable, so you can decide at teardown time.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `DomainReady=False/WaitingForKeystoneAPI` | The referenced Keystone is not serving yet — check its `KeystoneAPIReady` condition. |
| `DomainReady=False/DomainAlreadyExists` | A same-named domain exists that this CR did not create. The operator never seizes foreign domains; use `mode: Adopt` to attach to it. |
| `DomainReady=False/DomainNotFound` | `mode: Adopt` but no domain with that name exists — Adopt never creates. |
| `IdentityBackendsReady=False/WaitingForBackends` with `IdentityBackendSkipped` Warning events | The bind Secret is missing or lacks the `username`/`password` keys; healthy sibling backends keep working. |
| LDAP users not listed | Check the tree/attribute mapping against your directory (`user_tree_dn`, `user_objectclass`, `user_id_attribute`) — unset fields fall back to keystone's defaults, which may not match your schema. |
| `GET /v3/users` returns HTTP 400 `'enabled' is a required property` | The projected `user_enabled_invert`/`user_enabled_default` fallback was suppressed by a `user_enabled_*` key in `extraOptions` that does not match the directory — with no matching attribute, keystone omits `enabled` from the user model and its own response validation rejects the reply. Align the `user_enabled_*` options with the directory schema (or drop them to restore the fallback). |
| Admission rejects the CR | The message names the exact rule: Default-domain protection, domain-name uniqueness per Keystone, the `extraOptions` denylist, or the fixed Secret data-key contract. |

## Tested by

Attaching the LDAP domain, watching the conditions converge, and authenticating
an LDAP user against the seeded `dc=planetexpress,dc=com` directory are asserted
end-to-end on the CI e2e kind cluster by this chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/keystone/ldap-domain-backend
```

::: details The backend CR the suite applies
The suite isolates its Keystone instance from the parallel suite pool, so its
backend CR name (`planetexpress-ldap`) and `keystoneRef` (`keystone-ldap`)
differ from the `corp-ldap` / `keystone` names used in the
walkthrough above.

<<< @/../tests/e2e/keystone/ldap-domain-backend/02-backend-cr.yaml#backend-cr
:::
