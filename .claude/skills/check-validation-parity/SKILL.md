---
name: check-validation-parity
description: >-
  Audit whether every CR validation rule stays in parity across its four
  representations — the declarative kubebuilder markers and XValidation/CEL
  rules in operators/<op>/api/ and internal/common/types, the validating
  webhook in *_webhook.go, the webhook unit tests, and the e2e rejection
  corpora under tests/e2e/<op>/invalid-cr/ and invalid-<kind>-cr/. Use when
  asked to check validation parity, after adding or changing a validation
  rule or webhook, after moving a marker boundary, or after a CEL rule had
  to be demoted to webhook-only enforcement.
---

# Check validation parity

This skill verifies that the CobaltCore **CR validation logic stays in parity
across its four representations**: every admission rule declared as a
kubebuilder marker or CEL `XValidation` has a coherent webhook twin (or a
deliberate reason not to), every webhook-enforced rule is exercised by a
unit test and a rejection fixture, and every error substring a rejection
corpus asserts still anchors to a rule that exists today.

It is repeatable — run it any time, especially after editing a
`*_types.go` validation marker, a `*_webhook.go` rule, or a rejection
corpus, and when reviewing a PR that moves a rule between the CRD schema
and the webhook. To make such a change, follow [[add-validation-rule]];
this skill audits the result.

## What validation parity means here

A validation rule threads through four representations. Drift in any one
means a CR is rejected with a stale message, accepted when it should be
rejected, or rejected by a rule no test pins down:

| Representation | Where it lives | Source of truth |
|---|---|---|
| Declarative CRD validation | `+kubebuilder:validation:*` markers and `XValidation` CEL rules in `operators/<op>/api/v1alpha1/*_types.go` and the shared types under `internal/common/types/` | the marker/rule text, regenerated into `operators/<op>/config/crd/bases/` by `make manifests` |
| Webhook validation | `operators/<op>/api/v1alpha1/*_webhook.go` (`ValidateCreate` / `ValidateUpdate` / `ValidateDelete`, accumulating `field.Invalid` / `field.Required` / `field.Forbidden` / `field.NotSupported` violations); shared validators in `internal/common/validation/` | the Go validation functions |
| Webhook unit tests | `operators/<op>/api/v1alpha1/*_webhook_test.go` | test cases that drive each violation path |
| Rejection corpora | `tests/e2e/<op>/invalid-cr/` plus one `invalid-<kind>-cr/` per further kind (`tests/e2e/cinder/invalid-cinderbackend-cr/`, `tests/e2e/c5c3/invalid-keystoneservice-cr/`, …) — generated fixtures (`_generate.py`, `test_generate.py`, two- or three-digit prefixes) plus `chainsaw-test.yaml` asserting `contains($error, '…')` substrings | one fixture per rejection path, applied against a real API server |

The authoritative gates are `make verify-invalid-cr-fixtures` (generator
drift) and the webhook unit tests run by `make test-operator OPERATOR=<op>`
(the codecov `webhooks` component holds `operators/*/api/**` to 90%). This
skill defers to those gates and adds the cross-representation inventory
checks neither gate can express: a rule that silently lives in only one
representation passes both gates.

A parity finding is any rule enforced in one representation with no twin,
test, or fixture in the others — a CEL rule with no rejection fixture, a
webhook-only rule no unit test drives, or a Chainsaw error assertion
whose substring no longer matches any current rule.

## Which layer answers

Admission runs in a fixed order: structural-schema defaults on decode, the
defaulting webhook, schema validation (OpenAPI markers and CEL
`XValidation`), and only then the validating webhook. What a Chainsaw step
can observe follows from that order:

- **Schema before webhook.** The API server validates the structural schema
  and its CEL rules before it calls the validating webhook, so where both
  carry a rule the schema message is the one a Chainsaw assertion must pin;
  the webhook message is asserted only for rules without a schema
  counterpart. The corpus headers state this
  (`tests/e2e/barbican/invalid-cr/chainsaw-test.yaml`).
- **The defaulting webhook masks `Required value`.** Its typed round trip
  writes an omitted non-pointer struct, or a string without `omitempty`,
  back as `{}` / `""`: the `required` list is satisfied and the next rule
  answers (the XOR CEL rule, the pattern). Precedents:
  `tests/e2e/placement/invalid-cr/00-openstackrelease-missing.yaml`,
  `tests/e2e/nova/invalid-cr/08-messaging-missing.yaml`. The schema's
  `Required value` for such a field shows only on a webhook-less envtest
  (`setupEnvTestNoWebhook`); V3 prints an `[INFO]` for every corpus that
  asserts it.
- **The defaulter can remove the violation.** A value the defaulter
  normalizes never reaches the schema: barbican's `01-replicas-below-minimum`
  uses `-1` because a `0` is defaulted away.
- **CEL under a defaulted parent.** A transition rule on a type that a
  parent marks `+kubebuilder:default={}` must guard `self` and `oldSelf`
  with `has()`. Unguarded, the API server evaluates the rule against the
  `{}` default when the CRD is installed and rejects the whole CRD with
  `no such key: … evaluating rule` (the guarded form: `OVNDatabaseSpec` in
  `operators/ovn/api/v1alpha1/ovncentral_types.go`). Unit tests cannot see
  it; `make test-integration` does.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-validation-parity/scripts/audit-validation-parity.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. It runs in a few
seconds, writes its extracted Go and CRD records to a `mktemp` directory it
removes on exit, and leaves V3's matching to
`.claude/skills/check-validation-parity/scripts/anchor-error-substrings.awk`.
Interpret:

- **V1** — the inventory: per API surface (each `operators/<op>/api/`
  plus `internal/common/types/` and `internal/common/validation/`), the
  validation-marker families, every CEL rule with its message, the webhook
  error-helper counts, and the fixture count of every rejection corpus.
  This is the working sheet for step 2.
- **V2** — every `*_webhook.go` has a paired `*_webhook_test.go`. A
  missing test file means whole violation families can silently break.
- **V3** — every `contains($error, '…')` substring of every rejection
  corpus (commented-out steps excluded) anchors to the current rule set.
  The per-suite line counts the anchor categories, first hit wins:
  - `server-phrase` — a `field.ErrorType` string apimachinery renders
    (`Not found`, `Required value`, `Duplicate value`, `Invalid value`,
    `Unsupported value`, `Forbidden`, `Too long`, `Too many`, `Too few`,
    `Too short`, `Internal error`, or a first word of one), the
    `supported values` detail, or the API server's `admission webhook`
    denial prefix.
  - `schema-phrase` — a go-openapi / apiextensions phrase (`should be at
    least N chars long`, `should be greater than or equal to N`, `should
    have at least N items`, `must have at most N items`, `should match`, …)
    **whose boundary exists**: a `+kubebuilder:validation:Minimum=N` (etc.)
    marker in the Go corpus or a `minimum: N` / `minLength:` / `maxItems:` /
    `pattern:` value in the generated CRD of the kind the suite applies.
    A phrase whose boundary nothing carries is a `stale boundary` `[FAIL]`:
    the marker moved and the assertion did not.
  - `go` — inside a Go string literal (`+`-concatenations joined) or a
    `+kubebuilder` marker line of the operator's corpus: its own `api/`,
    every `operators/<Y>/api` its non-test code imports (the c5c3 webhook
    delegates to the service APIs), and all non-test Go under
    `internal/common/` except `testutil/`. Prose comments do not count.
  - `template` — consistent with a `fmt` verb template of the corpus
    (`name must be at most %d characters: …`, `%s is managed via %s and
    must not be set in extraConfig`): the literal parts match verbatim,
    a `%d` value is numeric, a `%s`/`%v`/`%q` value either starts with a
    digit (a quantity, duration, or release such as `1Mi`, `2025.2`) or
    occurs verbatim in the corpus or a fixture (the key/owner pair of a
    `config_ownership.go` table), and at least 8 literal characters over two
    words take part.
  - `field-path` — a server-rendered path such as `cache.servers[0]` or
    `resources.requests.memory`: every segment, index stripped and a
    leading `spec.` / `metadata.` / `status.` allowed, is a json tag of the
    corpus, a property of the kind's generated CRD (which covers embedded
    Kubernetes types), or a key of a sibling fixture (map keys).
  - `fixture` — a value echoed from a numbered fixture of the suite, YAML
    comments ignored.

  A `[FAIL]` names the substring and the reason. It means a rule was
  reworded, removed, or re-bounded and the e2e assertion now pins a
  message that can never appear.
- **V4** — every `XValidation:rule=` marker carries a `message=`. A CEL
  rule without a message rejects CRs with an opaque expression dump
  instead of an actionable error.
- **V5** — every operator whose `config/webhook/manifests.yaml` registers
  a `ValidatingWebhookConfiguration` has every rejection corpus wired to a
  `chainsaw-test.yaml` that references its fixtures, and every resource the
  configuration validates is applied (`kind:`) by a fixture of some corpus.
  A missing corpus or an unexercised validated kind is an `[INFO] GAP` —
  grade it in the report rather than the script, because a freshly
  webhook-validated kind may legitimately lag its corpus by a PR.

### 2. Cross-reference the inventory

The script cannot judge rule semantics. Using the V1 inventory, classify
every rule and confirm its parity by hand (parallelize over operators
with sub-agents if the inventory is long):

1. For each CEL rule and each webhook rule, decide which of the three
   shapes it is: **CEL-only** (a rule no webhook repeats; transition rules
   such as `self == oldSelf` immutability often stay CEL-only, though the
   repo also twins them in `ValidateUpdate`, e.g.
   `validation.TargetClusterRefImmutable` in `internal/common/validation/`
   and nova's `validateDatabaseImmutable`),
   **webhook-only** (e.g. rules over preserve-unknown-fields maps, floors
   on a `resource.Quantity` or duration, or stateful checks like
   one-ControlPlane-per-namespace that need a client), or **twinned**
   (enforced in both).
2. Every **CEL-only** rule needs a rejection fixture — the webhook unit
   tests cannot evaluate CEL, so the e2e corpus (or a CRD-only envtest case
   through `setupEnvTestNoWebhook` when the defaulter makes the violation
   unobservable on a cluster) is its only test.
3. Every **webhook-only** rule needs a unit-test case that drives its
   violation path *and* a rejection fixture — the CRD schema will not
   catch it, so nothing else rejects a bad CR if the webhook regresses.
4. For **twinned** rules, confirm the two sides agree — same boundary
   values, compatible messages. The fixture asserts the schema side (see
   § Which layer answers); only the unit test sees the webhook side.
5. For each V5 `GAP`, decide whether the kind has admission rules worth
   pinning e2e (it does if `ValidateCreate`/`ValidateUpdate` can reject)
   and record the missing corpus as a finding.

### 3. Run the authoritative gates

The script runs no Go or Python. Run the real gates directly and report
their outcome:

```bash
make verify-invalid-cr-fixtures
for op in $(printf '%s\n' operators/*/api/v1alpha1/*_webhook.go | cut -d/ -f2 | sort -u); do
  make test-operator OPERATOR="${op}" || echo "FAILED: ${op}"
done
```

Every operator with an `api/` ships a `*_webhook.go` today (barbican,
c5c3, cinder, glance, horizon, keystone, neutron, nova, ovn, placement),
so the loop covers all ten; run only the touched operators when time is
short. For rules that only a real API server evaluates (CEL, schema
pruning, defaulting before validation, a CRD that fails to install), the
envtest suites are the deeper gate: `make test-integration OPERATOR=<op>`
(needs `setup-envtest`; `operators/<op>/api/v1alpha1/integration_test.go`,
present for every operator but c5c3, runs CRD-only cases through
`setupEnvTestNoWebhook` and, for most, webhook-served ones through
`setupEnvTest`).
The corpora themselves meet a real API server only in the
`e2e-operator (<op>)` CI leg, where `tests/e2e/chainsaw-config.yaml` sets
`failFast: true`: one wrong substring hides every later step.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — a webhook-only rule with no unit test and no rejection
  fixture (nothing pins the rejection path); a V3 `[FAIL]` (stale
  assertion or stale boundary) confirmed by hand; `make
  verify-invalid-cr-fixtures` fails.
- **MEDIUM** — a CEL-only rule with no rejection fixture; a twinned
  rule whose two sides disagree on boundary or message; a V5 `GAP`; an
  `XValidation` rule without a `message=`; a corpus asserting `Required
  value` for a field the defaulting webhook materializes.
- **LOW** — a webhook message worded differently from its CEL twin
  without behavioural difference; inventory asymmetries worth a look
  (e.g. an operator whose webhook has many `field.Required` calls but
  whose corpus only exercises `field.Invalid` paths).

For each finding give one line with a `file:line` reference for both the
rule side and the missing/stale counterpart side. End with a per-operator
parity verdict.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **CEL rule demoted to webhook-only.** A CEL rule cannot be kept on
   the CRD (e.g. the API server cannot build type information for a
   CEL rule over a preserve-unknown-fields map, as with Horizon's
   `extraConfig` in PR #558) and moves into the webhook. The schema no
   longer rejects the bad CR, so the webhook path *must* gain a unit
   test and a rejection fixture, and the fixture's asserted message
   changes from the CEL text to the webhook text.
2. **Reworded message, stale e2e assertion.** A webhook or CEL message
   was improved; the Chainsaw `contains($error, '…')` substring still
   matches the old wording. The suite fails with an obscure mismatch —
   or worse, keeps passing because the substring accidentally matches a
   different rule's message.
3. **New rule, no rejection fixture.** A validation was added and its
   unit test written, but no rejection fixture — the rule is never
   exercised against a real API server, where CEL evaluation, schema
   pruning, and webhook ordering can all change the outcome.
4. **Moved boundary.** A marker boundary was tightened (`Minimum=3`) but
   the webhook twin still checks the old boundary, or the Chainsaw step
   still quotes `should be greater than or equal to 1`. V3 reports the
   second as a stale boundary; the first is a step-2 finding.
5. **Assertion on a layer that never answers.** A step pins the webhook
   message for a rule the schema also carries, or `Required value` for a
   field the defaulting webhook fills in. The e2e leg fails on a cluster
   while the webhook-less envtest passes.
6. **Validated kind without a corpus.** An operator validates a second
   kind (a backend, a store, an agent) but only `invalid-cr/` exists for
   the first. V5 reports the kind as a `GAP`.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (add the fixture, extend the webhook test, re-align the
  twin) as a separate, explicitly-scoped task — [[add-validation-rule]].
- V3 answers "does the rule behind this substring still exist", not "does
  this exact error appear": a `%d` value is not checked against the
  computed bound, field-path segments are checked one by one rather than as
  a path, and a one-word substring such as `replicas` anchors to its json
  tag. The e2e leg pins the exact text.
- The webhook unit tests match messages too (`gomega.ContainSubstring` on
  the returned error), but they only see the webhook side; V3 anchors the
  Chainsaw substrings against the Go sources and the generated CRDs
  because the schema answers first for twinned rules.
- Pair this with [[check-crd-drift]] — that skill confirms the marker
  source regenerates cleanly into the CRD YAML and Helm copies; this
  skill confirms the rules the markers express keep their webhook, test,
  and fixture counterparts. V3's schema-phrase check reads the generated
  CRD, so run `check-crd-drift` first when both report.
- Pair this with [[check-fixture-drift]] — that skill confirms fixtures
  are schema-valid, reachable, and generator-synced; this skill confirms
  the *rules* the fixtures exercise still exist and match.
- Pair this with [[check-condition-coverage]] — same audit shape, one
  layer up: conditions instead of validation rules.
