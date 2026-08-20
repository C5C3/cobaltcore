---
title: Service Accounts on Application Credentials
quadrant: operator
---

# Service Accounts on Application Credentials

> **Status: idea sketch — not implemented.** Service accounts authenticate with
> passwords today. The admin credential already runs on a Keystone application
> credential; this page sketches extending that pattern to every service
> account. It is based on an upstream analysis of keystone, placement,
> keystonemiddleware, and keystoneauth (as of August 2026).

## Motivation

Every service account (glance, placement, barbican) authenticates its
`[keystone_authtoken]` section with a password. Rotating that password is a
Secret flip with a built-in outage window: between K-ORC applying the new
password in Keystone and the last pod rolling onto the new Secret,
keystonemiddleware fails token validation with 401. The
`CredentialRotation` flow keeps the window short, but cannot remove it.

Application credentials remove it. A user can hold several at once, so a new
credential can be minted, rolled out, and the old one deleted after the
rollout — no moment where the running config is invalid. They also survive
password changes, carry an optional `expires_at`, and can be narrowed with
access rules. Keystone's own user guide makes the same argument: changing a
service user's password "results in immediate downtime for any application
using that password."

## Baseline: what exists today

- The defaulting webhook injects a service account per declared service, with
  the role `service` (the role Keystone's `identity:validate_token` policy
  accepts for token validation).
- K-ORC provisions the user, project, and role assignment. The operator
  generates a 256-bit password, round-trips it through OpenBao, and an
  ExternalSecret materializes `password` and a password-based `clouds.yaml`
  in the service namespace.
- The shared `keystoneauth.Section()` helper renders
  `[keystone_authtoken]` with `auth_type = password` for all service
  operators; the password itself reaches the pod as the
  `OS_KEYSTONE_AUTHTOKEN__PASSWORD` environment override.
- Rotation is the `serviceAccountPassword` target of the
  [CredentialRotation CR](../reference/c5c3/controlplane-crd.md): a generation
  bump produces a new password Secret, K-ORC re-applies it, pods roll on the
  Secret digest.
- The **admin credential** already runs on an application credential. K-ORC
  mints it from a password-based bootstrap `clouds.yaml`
  (`{cp}-admin-password-cloud`) and re-mints it on rotation. This sketch
  repeats that shape per service account.

## The upstream constraint

Keystone only allows a user to create application credentials **for
themselves**. The check is enforced in code (`keystone/api/users.py`), after
policy evaluation: the token's user must match the target user, and the
credential's project and roles are taken from the token, not from the request.
Admins may read and delete another user's credentials, but never create one.
There is no upstream plan to change this; the one related RFE has been dormant
since 2020, and the Keystone FAQ rejects detaching credentials from their
owning user on security grounds.

The consequence for provisioning: the operator must authenticate **as the
service user** to mint the credential. The password therefore stays, demoted
to a bootstrap and rotation credential that only the provisioning layer uses.
The running service never sees it.

The consumer side needs no upstream change. keystonemiddleware loads its auth
plugin through keystoneauth's generic loading, so the target configuration is
available today:

```ini
[keystone_authtoken]
auth_type = v3applicationcredential
application_credential_id = <id>
application_credential_secret = <secret>   # via environment override
service_type = placement
# No project/domain/system scope options: Keystone rejects an explicit
# scope with application credentials; tokens are always scoped to the
# credential's project.
```

A project-scoped token whose roles include `service` passes
`identity:validate_token`, and keystoneauth re-authenticates on expiry
without operator involvement.

Further constraints that shape the design:

- A token from a *restricted* application credential cannot create or delete
  application credentials. Self-rotation would require `unrestricted=true`,
  which upstream discourages. Rotation therefore always runs over the
  operator's password session.
- Deleting **or disabling** the service user destroys its application
  credentials permanently; re-enabling does not restore them.
- The embedded roles are intersected with the user's current assignments at
  every token issuance. If the user loses the `service` role, the credential
  stops producing useful tokens.

## Sketched design

Per service account, the admin pattern repeats:

1. **User and password as today.** K-ORC provisioning is unchanged.
2. **A per-account password `clouds.yaml` Secret**, the service-account
   sibling of `{cp}-admin-password-cloud`.
3. **A managed K-ORC `ApplicationCredential`** whose `cloudCredentialsRef`
   points at that Secret, so the mint authenticates as the service user.
   Restricted, with optional `expiresAt` and access rules.
4. **Delivery of id and secret** through the existing OpenBao round-trip. The
   consumer Secret carries an application-credential `clouds.yaml`; the
   builder for that document already exists for the admin flow.
5. **Config switch.** `keystoneauth.Section()` gains an
   application-credential branch, and the secret reaches the pod as
   `OS_KEYSTONE_AUTHTOKEN__APPLICATION_CREDENTIAL_SECRET`. Pods roll on the
   existing Secret digest.
6. **Overlapping rotation.** A new `CredentialRotation` target mints a second
   credential, flips the Secret, and deletes the old credential after the
   rollout. Password rotation becomes a separate concern with no service
   impact.

| Aspect | Password (today) | Application credential (sketch) |
|---|---|---|
| Rotation | Secret flip with a 401 window | Overlapping, no outage window |
| Password change | Breaks the service immediately | No service impact |
| Expiry | None | `expires_at`, token lifetime capped to it |
| Least privilege | Full user rights | Access rules possible |
| Provisioning | Admin sets the password | Bootstrap password still required |

The change surface in the repository is contained: the `Section()` helper in
`internal/common/keystoneauth`, the two service-account provisioning call
sites (inline ControlPlane accounts and the standalone `KeystoneService`),
the three `ServiceUserSpec` CRD surfaces with their config-ownership
catalogs, and the rotation target.

## Risks and open questions

- **K-ORC mint as a non-admin.** The `ApplicationCredential` actuator has only
  been exercised with admin credentials. Whether it behaves correctly when
  authenticated as the service user itself (including list and adoption
  behavior) is the open spike before anything else.
- **User disable is destructive.** An accidental disable of a service user
  deletes its credentials with no recovery path. The operator should have no
  disable path for service accounts, and the re-mint flow must heal the case
  regardless.
- **Glance backend gap.** `glance_store` supports application credentials for
  the Swift and Cinder backends only from OpenStack 2026.1. On 2025.2 only
  `[keystone_authtoken]` could switch; deployments using those backends need
  a per-release distinction.
- **Keystone hardening.** Upstream tightened the application-credential trust
  model through 2026 (token rescoping blocked, an EC2 escalation fixed,
  delegation fixes in flight). Running service identities on application
  credentials raises the stakes on picking those fixes up promptly.
- **`service_type` should be set** in `[keystone_authtoken]` in any case:
  without it, keystonemiddleware rejects incoming user tokens that were issued
  from application credentials with access rules.

## Suggested next steps

Triage the spike first: mint an application credential through K-ORC
authenticated as a service user, against a kind cluster. If that holds, the
rest splits along the change surface above: the shared config helper, the
provisioning call sites, the rotation target, and the per-service CRD fields.
