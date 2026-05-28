# CC-0107 — Enforce mTLS client-cert auth on OpenBao listener

Closes #321.

## Summary

OpenBao previously ran with **server-side TLS only**: the listener proved
OpenBao's identity to clients, but the transport layer placed no
requirement on the client's identity — anything with network reach to
`openbao.openbao-system.svc:8200` and a valid Kubernetes ServiceAccount
token (or, for the Raft inter-node path, merely the leader CA) was
admitted. This PR adds **transport-layer mutual TLS** to the OpenBao API
listener so every connection must present a client certificate that
chains to the same self-signed CA as the server cert *before* any
application-layer auth (Kubernetes JWT, AppRole, root token) runs.

Scope is deliberately limited to the transport layer; the existing
Kubernetes-ServiceAccount-token authentication / authorization model
(`auth.kubernetes` on `kubernetes/management`, the `eso-management`
role, and the CC-0009 / CC-0083 audit invariants in
`deploy/openbao/policies/{eso-management,push-keystone-keys}.hcl`) is
preserved unchanged. Switching ESO (or any client) to the `cert`
AuthMethod is an explicit Non-Goal.

The change touches:

- `deploy/flux-system/releases/openbao.yaml` — adds
  `tls_client_ca_file = "/openbao/tls/ca.crt"` and
  `tls_require_and_verify_client_cert = true` to the HA listener; adds
  `leader_client_cert_file` / `leader_client_key_file` to all three
  `retry_join` stanzas; adds the `client-tls` volume / volumeMount.
- `deploy/kind/base/kustomization.yaml` — applies the same
  `tls_client_ca_file` + require-and-verify mode to the kind
  `standalone.config` listener (the two-listener consistency footgun
  per **Apply bug fixes to all instances of the same pattern**).
- `deploy/flux-system/infrastructure/openbao-client-tls-cert.yaml` (new) —
  two cert-manager `Certificate` resources (`openbao-client-tls`,
  `eso-openbao-client-tls`) issued by `selfsigned-cluster-issuer` with
  `usages: ["client auth"]`, registered in the infrastructure
  `kustomization.yaml`.
- `deploy/eso/clustersecretstore.yaml` — adds the Vault-provider
  `tls.certSecretRef` / `tls.keySecretRef` pointing at
  `eso-openbao-client-tls`; the `auth.kubernetes` block and `caProvider`
  remain byte-unchanged.
- `deploy/openbao/bootstrap/{common.sh,init-unseal.sh}` and
  `hack/deploy-infra.sh` — forward `VAULT_CLIENT_CERT` /
  `VAULT_CLIENT_KEY` through every `bao`-invoking wrapper (`bao_exec`,
  `bao_exec_stdin`, private `kube_exec`, `openbao_kube_exec`).
- `tests/unit/deploy/openbao_mtls_test.sh` (new) — full mTLS-wiring
  unit suite (70 assertions, all PASS); `production_posture_test.sh`
  extends its `:(exclude)` carve-out to cover `openbao.yaml` and the
  header records the CC-0107 deviation mirroring CC-0097 chaos-mesh.
- `tests/e2e/infrastructure/infra-stack-health/chainsaw-test.yaml` —
  Step 4 keeps the `ClusterSecretStore` + three `ExternalSecret`
  `Ready=True` assertions, now annotated as running under enforced mTLS.
- `docs/reference/infrastructure/{openbao-bootstrap,infrastructure-manifests}.md`
  — TLS Configuration / Certificate-SANs tables, `VAULT_CLIENT_*` env
  vars, runnable operator negative-check probe, OpenBao Helm-values
  table and narrative all updated.

## Architecture submodule follow-up (Task 7.3)

`.gitmodules` pins `architecture → https://github.com/C5C3/C5C3` at
commit `7beebd6f2d66a1012f16a52c3990bed9033b5af8`. That revision's
`docs/09-implementation/09-openbao-deployment.md` documents the
OpenBao HelmRelease — and at the pinned commit it carries **only the
server-side TLS configuration**, with no notion of client-cert
verification or the mTLS trust chain this PR adds:

```yaml
# architecture/docs/09-implementation/09-openbao-deployment.md, ~L42-L88
# At submodule pin 7beebd6f the listener block has no
# tls_client_ca_file / tls_require_and_verify_client_cert, the three
# retry_join stanzas carry only leader_api_addr (no
# leader_client_cert_file / leader_client_key_file), and the
# server.volumes / server.volumeMounts block mounts only the
# `tls` (server) Secret at /openbao/tls — there is no `client-tls`
# volume backing /openbao/client-tls.
listener "tcp" {
  tls_disable     = 0
  address         = "[::]:8200"
  cluster_address = "[::]:8201"
  tls_cert_file   = "/openbao/tls/tls.crt"
  tls_key_file    = "/openbao/tls/tls.key"
}
...
volumes:
  - name: tls
    secret:
      secretName: openbao-tls
volumeMounts:
  - name: tls
    mountPath: /openbao/tls
    readOnly: true
```

Per repository rule `NO_SUBMODULE_MODIFICATIONS` (and boundary #11 of
the CC-0107 elaboration: the `architecture/` submodule is READ-ONLY
from this worktree's perspective and serves exclusively as a source of
information), this worktree does **not** touch submodule content.
The design-rationale correction must land via a separate upstream PR
to `https://github.com/C5C3/C5C3` against
`docs/09-implementation/09-openbao-deployment.md`, and must mirror the
transport-layer wiring this PR establishes in the repository:

1. **HA listener block** (currently L45-L51 at pin `7beebd6f`) — add:

    ```hcl
    tls_client_ca_file               = "/openbao/tls/ca.crt"
    tls_require_and_verify_client_cert = true
    ```

   alongside the existing `tls_cert_file` / `tls_key_file` lines, and
   record the rationale: the listener rejects any TLS handshake
   without a valid client cert before any application-layer auth runs;
   `tls_client_ca_file` reuses the self-signed CA bundle so client
   certs minted by `selfsigned-cluster-issuer` chain to it.

2. **All three `retry_join` stanzas** (currently L55-L63 at pin
   `7beebd6f`) — add:

    ```hcl
    leader_client_cert_file = "/openbao/client-tls/tls.crt"
    leader_client_key_file  = "/openbao/client-tls/tls.key"
    ```

   to each stanza (not just one — Raft `retry_join` targets the API
   listener on `:8200`, so once that listener requires client certs
   every peer must present one or the cluster will not form).

3. **`server.volumes` / `server.volumeMounts`** (currently L81-L88 at
   pin `7beebd6f`) — add a second volume mounting the in-pod client
   keypair Secret at a path distinct from the server-cert mount:

    ```yaml
    volumes:
      - name: tls
        secret:
          secretName: openbao-tls
      - name: client-tls          # CC-0107
        secret:
          secretName: openbao-client-tls
    volumeMounts:
      - name: tls
        mountPath: /openbao/tls
        readOnly: true
      - name: client-tls          # CC-0107
        mountPath: /openbao/client-tls
        readOnly: true
    ```

   The two mount paths are intentionally distinct so that server-cert
   and client-cert lifecycles (separate cert-manager `Certificate`
   resources, separate Secrets, independent rotations) cannot collide.

4. **Client-cert `Certificate` resources** — document that two
   additional `cert-manager.io/v1` `Certificate`s are required:

   - `openbao-client-tls` (Secret `openbao-client-tls` in
     `openbao-system`) — mounted into the StatefulSet for Raft
     `retry_join` peer auth and in-pod `bao` exec via the bootstrap
     scripts.
   - `eso-openbao-client-tls` (Secret `eso-openbao-client-tls` in
     `openbao-system`) — referenced by the ESO
     `ClusterSecretStore/openbao-cluster-store` via
     `spec.provider.vault.tls.certSecretRef` / `keySecretRef`.

   Both are issued by `selfsigned-cluster-issuer` with
   `usages: ["client auth"]` and share the `openbao-tls` `duration` /
   `renewBefore` (`8760h` / `720h`) so server and client rotation
   cadences stay aligned. SANs on the client certs are identifiers
   only — the listener does not verify SANs on client auth, only
   chain-to-CA.

5. **ESO `ClusterSecretStore`** — note that the existing K8s-token
   auth method (`auth.kubernetes` on `kubernetes/management`, role
   `eso-management`) is **unchanged**: mTLS is purely a transport-layer
   admission gate layered *underneath* the existing app-layer auth.
   The CC-0009 / CC-0083 audit invariants in
   `deploy/openbao/policies/{eso-management,push-keystone-keys}.hcl`
   are preserved.

6. **Operator interface** — document the `VAULT_CLIENT_CERT` /
   `VAULT_CLIENT_KEY` env vars (defaulting to
   `/openbao/client-tls/tls.crt` and `/openbao/client-tls/tls.key`) and
   add a runnable mTLS-enforcement probe consistent with
   `docs/reference/infrastructure/openbao-bootstrap.md#verify-mtls-enforcement-cc-0107`
   (no-client-cert `curl` from inside the pod, expected to fail at the
   TLS handshake; same call with the client keypair, expected
   `http_code=200`).

Once the upstream PR merges, bump the submodule pin here in a
follow-up worktree to pick up the corrected architecture doc.

Tracking: this upstream follow-up must be opened against
`https://github.com/C5C3/C5C3` (target file
`docs/09-implementation/09-openbao-deployment.md`) before CC-0107 can
be considered fully propagated across the documentation surface
(REQ-011). The follow-up PR link will be added to this PR description
once filed, per the **Verify cross-file consistency across the PR
scope** project pattern and the CC-0093 / CC-0097 / CC-0098 precedents
for cross-repo design-doc handoff.
