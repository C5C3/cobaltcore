# CC-0093 — Parameterize OpenBao RemoteKey per Keystone CR

Closes #279.

## Summary

The Fernet- and credential-keys backup `PushSecret` builders previously
hard-coded their OpenBao `RemoteKey` to the two cluster-global, flat
KV-v2 paths `openstack/keystone/fernet-keys` and
`openstack/keystone/credential-keys`. Two Keystone CRs in the same
Kubernetes namespace therefore shared exactly two backing OpenBao
objects — whichever CR's ESO push landed last won, and either CR's
deletion could race with another CR's next push. The flake was dormant
in single-Keystone production clusters but load-bearing in the
E2E suite (`tests/e2e/chainsaw-config.yaml` runs `parallel: 4` across
tests that all share the `openstack` namespace) and was reproduced on
`feature/CC-0088` CI run 24903037791/72927386373 at
`2026-04-24T17:53:33Z`.

This PR switches both builders to a per-CR layout
`openstack/keystone/{keystone.Name}/<leaf>`, broadens
`push-keystone-keys.hcl` to a `+` single-segment glob on the CR-name
position (still denying `openstack/keystone/db`), updates the
deletion-cleanup chainsaw test to assert the CR-scoped paths, adds a
positive KV-isolation assertion to the `concurrent-cr-conflicts`
chainsaw test, adds two envtest integration tests
(`TestIntegrationKeystone_PushSecretRemoteKeyIsPerCR` and
`TestIntegrationKeystone_CredentialPushSecretRemoteKeyIsPerCR`), and
rewrites the six affected path references plus the `Known limitation`
block in `docs/reference/keystone-reconciler.md`.

## CC-0093 Completeness Proof (Task 5.1)

Verification command (run at HEAD `4ed7dcc6`):

```
$ rg 'openstack/keystone/(fernet|credential)-keys' docs/ deploy/ operators/
deploy/openbao/policies/push-keystone-keys.hcl:17:# replacing the previous flat leaves `openstack/keystone/fernet-keys` and
deploy/openbao/policies/push-keystone-keys.hcl:18:# `openstack/keystone/credential-keys`. The policy therefore switches from
docs/reference/keystone-reconciler.md:789:KV-v2 paths `kv-v2/openstack/keystone/fernet-keys` and
docs/reference/keystone-reconciler.md:790:`kv-v2/openstack/keystone/credential-keys`. Starting with CC-0093 the
docs/reference/keystone-reconciler.md:809:bao kv metadata delete kv-v2/openstack/keystone/fernet-keys
docs/reference/keystone-reconciler.md:810:bao kv metadata delete kv-v2/openstack/keystone/credential-keys
docs/reference/keystone-reconciler.md:1042:`kv-v2/openstack/keystone/fernet-keys`. The race itself is path-shape
operators/keystone/internal/controller/integration_test.go:3668:// path openstack/keystone/fernet-keys, causing concurrent two-CR
operators/keystone/internal/controller/integration_test.go:3720:// path openstack/keystone/credential-keys, causing concurrent two-CR
operators/keystone/internal/controller/reconcile_credential_test.go:440:         g.Expect(got).NotTo(Equal("openstack/keystone/credential-keys"), "must not fall back to legacy flat path")
operators/keystone/internal/controller/reconcile_fernet_test.go:435:         g.Expect(got).NotTo(Equal("openstack/keystone/fernet-keys"), "must not fall back to legacy flat path")
```

Every remaining match is either **(a)** inside the new
`Migration note: legacy flat paths (CC-0093)` subsection of
`docs/reference/keystone-reconciler.md`, or **(b)** a comment /
negative test assertion that explicitly documents the legacy form as
legacy. Breakdown:

| Location | Category | Justification |
| --- | --- | --- |
| `deploy/openbao/policies/push-keystone-keys.hcl:17-18` | (b) | Header-comment rationale: `"replacing the previous flat leaves ..."` — documents the pre-CC-0093 form the `+` glob supersedes. |
| `docs/reference/keystone-reconciler.md:789-790` | (a) | First sentence of the migration note subsection (section header at `:786`). |
| `docs/reference/keystone-reconciler.md:809-810` | (a) | `bao kv metadata delete` cleanup commands inside the migration note's `sh` code block. |
| `docs/reference/keystone-reconciler.md:1042` | (b) | CC-0091 motivating-race paragraph: explicitly calls the flat path `the now-legacy flat path` and pins the predating CI run 24842115250. |
| `operators/keystone/internal/controller/integration_test.go:3668` | (b) | Godoc of `TestIntegrationKeystone_PushSecretRemoteKeyIsPerCR` — `"Regression guard: before CC-0093, both PushSecrets wrote to the shared path openstack/keystone/fernet-keys"`. |
| `operators/keystone/internal/controller/integration_test.go:3720` | (b) | Symmetric credential-keys regression-guard godoc. |
| `operators/keystone/internal/controller/reconcile_credential_test.go:440` | (b) | Negative assertion: `NotTo(Equal(...), "must not fall back to legacy flat path")`. |
| `operators/keystone/internal/controller/reconcile_fernet_test.go:435` | (b) | Symmetric Fernet negative assertion. |

No active-code reference to the flat path remains. The test suite
pins the legacy form as a forbidden regression target, not as a
production path.

## Architecture submodule follow-up (Task 5.2)

`.gitmodules` pins `architecture → https://github.com/C5C3/C5C3` at
commit `7beebd6f2d66a1012f16a52c3990bed9033b5af8`. That revision
still carries a stale flat-path reference:

```
architecture/docs/09-implementation/04-keystone-reconciler.md:279:
  **OpenBao backup** (optional) — Creates a PushSecret CR to back up
  Fernet keys to `kv-v2/openstack/keystone/fernet-keys` in OpenBao.
```

Per repository rule `NO_SUBMODULE_MODIFICATIONS`, this worktree does
**not** touch submodule content. The correction must land via a
separate upstream PR to `https://github.com/C5C3/C5C3`
(`docs/09-implementation/04-keystone-reconciler.md:279`), changing the
path string to `kv-v2/openstack/keystone/{keystone.Name}/fernet-keys`
(and, for completeness, replacing any symmetric `credential-keys`
wording if added later — at this submodule pin only the Fernet
bullet exists). Once the upstream PR merges, bump the submodule pin
here in a follow-up worktree to pick up the corrected architecture
doc.

Tracking: this follow-up must be opened against
`github.com/C5C3/C5C3` before CC-0093 can be considered fully
propagated across the documentation surface (REQ-008).
