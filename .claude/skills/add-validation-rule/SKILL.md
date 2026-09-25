---
name: add-validation-rule
description: >-
  Add or change a CR admission rule in a CobaltCore operator end to end:
  choose the layer (kubebuilder marker, CEL XValidation, webhook-only, or
  twinned), then move every representation together — the *_types.go marker
  and the regenerated CRD plus its Helm copy, the validating webhook and its
  unit tests, the CRD-only envtest, the invalid-cr rejection fixture with its
  generator, Chainsaw step and Makefile wiring, the ControlPlane mirror when
  operators/c5c3 projects the field, and the reference docs. Use when adding,
  tightening, loosening, or moving a validation rule or webhook check, when a
  new CRD field needs bounds, or when a CEL rule has to be demoted to the
  webhook.
---

# Add a validation rule

The procedural counterpart of [[check-validation-parity]]: that skill audits
whether a rule's representations agree, this one walks the edits that keep
them agreeing. One rule touches up to eight places, and CI checks each
separately (`verify-codegen`, the unit tests, `verify-invalid-cr-fixtures`,
`test-integration`, the `e2e-operator (<op>)` leg), so a missed place shows
up as a red job one layer away from the edit.

## How the API server answers (read first)

Admission runs in this order: decode with the structural schema defaults
(`+kubebuilder:default`), the defaulting webhook (`Default`), schema
validation (OpenAPI markers and CEL `XValidation`), and only then the
validating webhook (`ValidateCreate` / `ValidateUpdate`). Four consequences
decide what a test can observe:

1. **The schema answers first.** When both layers carry a rule, a violating
   CR never reaches the webhook, so the Chainsaw step pins the schema's
   message and the webhook twin is defense in depth for objects that bypass
   the schema. The schema speaks in go-openapi and apiserver phrases, as
   asserted across the corpora today: `should match` (Pattern),
   `should be at least N chars long` (MinLength), `should be greater than or
   equal to` / `should be less than or equal to` (Minimum / Maximum),
   `should have at least N items` / `N properties` (MinItems /
   MinProperties), `Unsupported value` (Enum),
   `Too long` (MaxLength), and a CEL rule's `message=` verbatim.
2. **The defaulting webhook masks `Required value`.** Its typed round trip
   writes a non-pointer struct, or a string without `omitempty`, back as
   `{}` / `""`, so the `required` list is satisfied and the next rule answers
   (the XOR CEL rule, the pattern). Precedents:
   `tests/e2e/placement/invalid-cr/00-openstackrelease-missing.yaml`,
   `tests/e2e/nova/invalid-cr/08-messaging-missing.yaml`. Only the
   webhook-less envtest ever shows `Required value`.
3. **Schema defaults fill before the webhook runs.** `DeploymentSpec.Replicas`
   defaults to 3 as soon as a `deployment` block is present, so a CEL cap such
   as cinder's `volume.deployment.replicas == 1` rejects a present block that
   omits `replicas`; fixtures and ControlPlane projections must state it.
4. **The defaulter can remove the violation.** Nova's defaulting webhook drops
   a disabled console proxy's `deployment` block, so the CEL rule against it
   is unobservable on a cluster: it has no fixture and is pinned by the
   CRD-only envtest instead (`nova_types.go`, `_generate.py` docstring).

## 1. Choose the layer

| Rule shape | Layer | Why |
|---|---|---|
| One field, static bound (range, length, pattern, enum, item count) | marker, usually twinned in the webhook | the schema holds while the webhook is down |
| Cross-field on one object (XOR, A requires B, a derived size such as `size(self.database) <= 58`) | CEL `XValidation` with `message=`, twinned | same; the twin repeats the message verbatim |
| Transition (immutability, frozen mode) | CEL with `oldSelf`, twinned in `ValidateUpdate` | runs on UPDATE only; `validation.TargetClusterRefImmutable` and nova's `validateDatabaseImmutable` are the twins |
| Keys or values of a preserve-unknown-fields map (`extraConfig`) | webhook-only | the API server cannot build CEL type information for it (`horizon_types.go`) |
| A grammar no regex expresses (cron with `@daily`) | webhook-only | `validation.CronSchedule` |
| Floors on a `resource.Quantity` or duration | webhook-only | no `Minimum` marker applies (glance `ValidateStaging`, `ValidateImageCache`) |
| Needs a client: a PriorityClass exists, sibling CRs, one ControlPlane per namespace | webhook-only | reads through `w.Client` (the manager's API reader); a nil reader skips the lookup |
| `metadata.name` bounded by a child's name (`{name}-db-archive` CronJob, 52-char cap) | webhook-only, `ValidateCreate` only | the name is immutable, so an update rule would wedge the finalizer-removal update |
| Option-catalog membership (`extraConfig` unknown option) | webhook-only, re-run on update only when its inputs changed | `extraConfigCatalogInputsChanged` keeps a regenerated catalog from rejecting an unrelated edit |

A rule on a type in `internal/common/types/` lands in every CRD embedding it;
its webhook twin belongs in `internal/common/validation/` (one implementation,
see the package's DECISION comment) with a case in `validation_test.go`.

## 2. Inventory

```bash
bash .claude/skills/add-validation-rule/scripts/next-fixture-number.sh <op>
```

Per corpus under `tests/e2e/<op>/invalid-*/`: the next free prefix, whether
the `_EXPECTED_FIXTURE_COUNT` pin matches the `FIXTURES` entries, whether
`make verify-invalid-cr-fixtures` runs the corpus, and whether a corpus past
99 accepts three-digit prefixes. It then lists the operator's API symbols the
ControlPlane API embeds or calls. Pass a corpus directory instead of `<op>`
for one corpus.

## 3. Edit, in this order

1. **Markers and CEL** in `operators/<op>/api/v1alpha1/<kind>_types.go` (or
   `internal/common/types/*.go`). Every CEL rule carries `message=`. Guard
   optional fields with `has()`, resolve defaults inline
   (`(has(self.x) ? self.x : 'Static')`), and under a parent marked
   `+kubebuilder:default={}` guard both `self` and `oldSelf`: the API server
   evaluates the rule against the empty default when the CRD is installed and
   rejects the whole CRD with `no such key` (`ovncentral_types.go`,
   `OVNDatabaseSpec`). The comment above the marker states the failure the
   rule prevents; the reference docs quote it.
2. **Regenerate.** `make generate` when fields changed, then `make sync-crds`
   (it runs `make manifests` first). Run both without `OPERATOR=` when the type
   lives in `internal/common/types/` or the ControlPlane embeds it:
   controller-gen copies the schema into `c5c3.io_controlplanes.yaml` too.
3. **Webhook** in `operators/<op>/api/v1alpha1/<kind>_webhook.go`: append to
   `validate()` (shared by create and update) with `field.Invalid` /
   `Required` / `Forbidden` / `NotSupported` on the exact field path, one
   aggregated error per request. Create-only and transition rules go into
   `ValidateCreate` / `ValidateUpdate` beside the existing ones. A webhook
   rule re-runs on every update, so a tightened rule rejects the next edit of
   every stored CR that violates it, including the finalizer-removal update
   of a deleting one (nova's `ValidateUpdate` admits a deleting CR with an
   unchanged spec for this reason). Export the validator
   (`ValidateImportFiltering`) or the bound (`MaxNovaNameLength`) when the
   ControlPlane needs it. Declare an unexported constant or helper in
   the same commit as its first consumer: golangci-lint's `unused` check fails
   a commit that stages it.
4. **Defaulting**, if the field gets a default: `Default()` fills zero values
   only; a default written into the stored CR freezes today's value, so
   render-time resolution is the alternative (glance `importFiltering`).
5. **Unit tests** in `<kind>_webhook_test.go`: a case in the
   `…ValidateCreate_RejectionTable` where the operator has one (mutate,
   `wantPath`, `wantSub`), otherwise a test beside its siblings; the tests
   assert that `err.Error()` contains the field path and a message substring.
   Add the accepted boundary value too. Helper-level tests assert
   `errs[0].Type` / `Field` instead (glance). Codecov holds
   `operators/*/api/**` to 90%.
6. **CEL on a real API server** in `operators/<op>/api/v1alpha1/integration_test.go`:
   a test on `setupEnvTestNoWebhook` (named `TestIntegration_CRD_CELOnly_*`
   in most operators), for every CEL rule the webhook would otherwise mask and
   for every transition rule (create, then update). envtest installs the CRD
   from `config/crd/bases/`, so step 2 must precede it.
7. **Rejection fixture** in `tests/e2e/<op>/invalid-cr/` (primary kind) or
   `invalid-<kind>-cr/` (secondary kinds). Never hand-edit a fixture:
   - add a `Fixture(filename="NN-<rule>.yaml", comment=…, <field>=…)` to
     `FIXTURES` in `_generate.py`, mutating one aspect of the scaffold so the
     rule under test is the only one that fails; the comment names the layer
     that answers (nova's `test_generate.py` also holds every other fixture's
     name under the webhook's name bound);
   - `python3 tests/e2e/<op>/<corpus>/_generate.py` writes it;
   - bump `_EXPECTED_FIXTURE_COUNT` in `test_generate.py`;
   - add the step to `chainsaw-test.yaml`: `apply.file` plus `expect.check`
     on `($error != null)`, `contains($error, '<field path>')` and
     `contains($error, '<message of the answering layer>')`;
   - a transition rule needs an accepted base fixture and an update fixture
     with the same `metadata.name`, applied in order in one step (keystone
     `13-immutable-base` then `14-immutable-database-name`); c5c3 gives each
     such wave its own `Test` document, so the persisted base gets its own
     namespace;
   - no `metadata.namespace`: Chainsaw runs each Test in an ephemeral one;
   - at prefix 100 widen `_FIXTURE_FILENAME_PATTERN` to `[0-9]{2,3}` and any
     `[:2]` or `[0-9]{2}-` in `test_generate.py` (c5c3, commit `6b83b922`);
   - a new corpus adds both its lines to `verify-invalid-cr-fixtures`.
8. **ControlPlane side**, when `operators/c5c3` projects the field
   (`Service<Svc>Spec` in `controlplane_types.go`, projected in
   `operators/c5c3/internal/controller/reconcile_<svc>.go`). Without a mirror
   the ControlPlane admits a value the child CRD rejects, and the reconcile
   fails to apply the child on every pass. Pick the matching shape:
   - **embedded type** (`glancev1alpha1.ImportFilteringSpec`): the schema is
     copied, and `validateGlance` delegates to the exported validator;
   - **local copy** (`ServiceNovaSpec`): duplicate the marker or CEL on the
     c5c3 type and mirror the check in `validate<Svc>` in
     `controlplane_webhook.go` (nova's console-route path);
   - **shared validator** (`validation.CronSchedule` for `dbArchive.schedule`);
   - **derived bound** (`validateNovaChildName` from `MaxNovaNameLength`,
     create-and-newly-enabled only).
   Add a case to `controlplane_webhook_test.go` and a fixture to
   `tests/e2e/c5c3/invalid-cr/`. Keys added to `config_ownership.go` reach the
   ControlPlane's `extraConfig` admission through
   `controlplane_extraconfig.go` without further wiring.
9. **Docs**: the validation section of `docs/reference/<op>/<kind>-crd.md`
   (`Defaulting and validation`, `Immutability and Validation Summary` or
   `Validation Rules`: CEL rules as rule/message tables, webhook rules in
   prose or a message table), its `Chainsaw E2E Tests` summary where present,
   and for a ControlPlane mirror a row in the `Validation Rules` tables of
   `docs/reference/c5c3/controlplane-crd.md` (field, `field.*` type, and a
   **Webhook-only.** tag where no schema rule backs it). Follow
   `STYLE_GUIDE.md`.

## 4. Verify

```bash
export PATH="$(go env GOPATH)/bin:$PATH"     # controller-gen and setup-envtest
make generate                                 # only when fields changed
make sync-crds                                # runs make manifests first
make verify-crd-sync
make test-operator OPERATOR=<op>              # plus OPERATOR=c5c3 for a mirror
make test-common                              # when internal/common changed
make verify-invalid-cr-fixtures
make test-integration OPERATOR=<op>           # CEL and CRD install on envtest
make lint OPERATOR=<op>
chainsaw lint test -f tests/e2e/<op>/<corpus>/chainsaw-test.yaml
git status --short                            # CI verify-codegen fails on any diff
```

For a tight CEL loop run one test:
`KUBEBUILDER_ASSETS=$(setup-envtest use 1.35 -p path) go test -tags=integration -run TestIntegration_CRD_CELOnly ./operators/<op>/api/v1alpha1/`.
The c5c3 envtest suite runs about 22 minutes; its webhook unit tests are the
fast gate for a mirror.

The corpus only meets a real API server in the `e2e-operator (<op>)` CI leg,
and `tests/e2e/chainsaw-config.yaml` sets `failFast: true`, so one wrong
substring hides every later step. To check a whole corpus
without a cluster, write a throw-away `//go:build integration` test in
`operators/<op>/api/v1alpha1/` that applies each fixture through
`setupEnvTest` (webhook served) and matches the `contains($error, '…')`
strings parsed out of `chainsaw-test.yaml`; delete it before committing.

## 5. Hand off

Run [[check-validation-parity]] on the result. It exits 0 at HEAD, so a V3
`[FAIL]` after the change is real and gets fixed, not waived: a reworded
message a Chainsaw step still quotes, a moved marker boundary a
`should be …` assertion still names (`stale boundary`), or a value that no
longer fits its `fmt` template. A V5 `GAP` for a kind the webhook now
validates means its rejection corpus is missing. Pair with
[[check-crd-drift]] (regeneration) and [[check-fixture-drift]] (fixture
schema validity and reachability).

## Notes

- Worked examples from the history, one per shape (twinned enum marker, CEL
  plus exported validator plus ControlPlane delegation, create-only name bound
  mirrored on the ControlPlane): [references/examples.md](references/examples.md).
- Demoting a CEL rule to the webhook (Horizon `extraConfig`) removes the only
  gate a webhook-down cluster had; the demotion commit must add the webhook
  unit test and the fixture, and change the fixture's asserted message.
- The script is read-only; it prints paths and wiring status and writes
  nothing.
