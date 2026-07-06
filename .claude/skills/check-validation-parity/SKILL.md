---
name: check-validation-parity
description: >-
  Audit whether every CR validation rule stays in parity across its four
  representations — the declarative kubebuilder markers and XValidation/CEL
  rules in operators/<op>/api/ and internal/common/types, the validating
  webhook in *_webhook.go, the webhook unit tests, and the invalid-cr e2e
  rejection corpus under tests/e2e/<op>/invalid-cr/. Use when asked to
  check validation parity, after adding or changing a validation rule or
  webhook, or after a CEL rule had to be demoted to webhook-only
  enforcement.
---

# Check validation parity

This skill verifies that the forge **CR validation logic stays in parity
across its four representations**: every admission rule declared as a
kubebuilder marker or CEL `XValidation` has a coherent webhook twin (or a
deliberate reason not to), every webhook-enforced rule is exercised by a
unit test and an invalid-cr rejection fixture, and every error substring
the invalid-cr Chainsaw suite asserts still anchors to a rule that
exists today.

It is repeatable — run it any time, especially after editing a
`*_types.go` validation marker, a `*_webhook.go` rule, or the invalid-cr
corpus, and when reviewing a PR that moves a rule between the CRD schema
and the webhook.

## What validation parity means here

A validation rule threads through four representations. Drift in any one
means a CR is rejected with a stale message, accepted when it should be
rejected, or rejected by a rule no test pins down:

| Representation | Where it lives | Source of truth |
|---|---|---|
| Declarative CRD validation | `+kubebuilder:validation:*` markers and `XValidation` CEL rules in `operators/<op>/api/v1alpha1/*_types.go` and the shared types under `internal/common/types/` | the marker/rule text, regenerated into the CRD by `make manifests` |
| Webhook validation | `operators/<op>/api/v1alpha1/*_webhook.go` (`ValidateCreate` / `ValidateUpdate` / `ValidateDelete`, accumulating `field.Invalid` / `field.Required` / `field.Forbidden` / `field.NotSupported` violations) | the Go validation functions |
| Webhook unit tests | `operators/<op>/api/v1alpha1/*_webhook_test.go` | test cases that drive each violation path |
| invalid-cr e2e corpus | `tests/e2e/<op>/invalid-cr/` — generated fixtures (`_generate.py`, `test_generate.py`) plus `chainsaw-test.yaml` asserting `contains($error, '…')` substrings | one fixture per rejection path, applied against a real API server |

The authoritative gates are `make verify-invalid-cr-fixtures` (generator
drift) and the webhook unit tests run by `make test-operator OPERATOR=<op>`
(the codecov `webhooks` component holds them to 90% coverage). This skill
defers to those gates and adds the cross-representation inventory checks
neither gate can express: a rule that silently lives in only one
representation passes both gates.

A parity finding is any rule enforced in one representation with no twin,
test, or fixture in the others — a CEL rule with no rejection fixture, a
webhook-only rule no unit test drives, or a Chainsaw error assertion
whose substring no longer matches any current rule.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-validation-parity/scripts/audit-validation-parity.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **V1** — the inventory: per API surface (each `operators/<op>/api/`
  plus `internal/common/types/`), the validation-marker families, every
  CEL rule with its message, the webhook error-helper counts, and the
  invalid-cr fixture count. This is the working sheet for step 2.
- **V2** — every `*_webhook.go` has a paired `*_webhook_test.go`. A
  missing test file means whole violation families can silently break.
- **V3** — every `contains($error, '…')` substring asserted by an
  invalid-cr Chainsaw suite anchors to the current rule set: a Go
  message/marker literal, a rejected value in a sibling fixture, a
  server-rendered field path whose segments are real json tags, or a
  known apimachinery phrase. A miss means a rule was reworded or removed
  and the e2e assertion now pins a message that can never appear.
- **V4** — every `XValidation:rule=` marker carries a `message=`. A CEL
  rule without a message rejects CRs with an opaque expression dump
  instead of an actionable error.
- **V5** — every operator whose `config/webhook/manifests.yaml` registers
  a `ValidatingWebhookConfiguration` has its invalid-cr corpus wired to a
  `chainsaw-test.yaml`. A corpus that is missing entirely is reported as
  an `[INFO] GAP` — grade it in the report rather than the script,
  because the corpus convention currently exists only for service
  operators.

### 2. Cross-reference the inventory

The script cannot judge rule semantics. Using the V1 inventory, classify
every rule and confirm its parity by hand (parallelize over operators
with sub-agents if the inventory is long):

1. For each CEL rule and each webhook rule, decide which of the three
   shapes it is: **CEL-only** (e.g. `self == oldSelf` immutability —
   transition rules cannot be expressed in a stateless webhook check),
   **webhook-only** (e.g. rules over preserve-unknown-fields maps, or
   stateful checks like one-ControlPlane-per-namespace that need a
   client), or **twinned** (enforced in both).
2. Every **CEL-only** rule needs an invalid-cr fixture — the webhook
   unit tests cannot evaluate CEL, so the e2e corpus is its only test.
3. Every **webhook-only** rule needs a unit-test case that drives its
   violation path *and* an invalid-cr fixture — the CRD schema will not
   catch it, so nothing else rejects a bad CR if the webhook regresses.
4. For **twinned** rules, confirm the two sides agree — same boundary
   values, compatible messages. A marker tightened without its webhook
   twin produces two different errors for the same mistake.
5. For each V5 `GAP`, decide whether the operator has admission rules
   worth pinning e2e (it does if `ValidateCreate`/`ValidateUpdate` can
   reject) and record the missing corpus as a finding.

### 3. Run the authoritative gates

The script runs no Go or Python. Run the real gates directly and report
their outcome:

```bash
make verify-invalid-cr-fixtures
make test-operator OPERATOR=keystone
make test-operator OPERATOR=c5c3
```

For rules that only a real API server evaluates (CEL, schema pruning),
the integration tests are the deeper gate: `make test-integration`.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — a webhook-only rule with no unit test and no invalid-cr
  fixture (nothing pins the rejection path); a Chainsaw `$error`
  substring that anchors to no current rule; `make
  verify-invalid-cr-fixtures` fails.
- **MEDIUM** — a CEL-only rule with no invalid-cr fixture; a twinned
  rule whose two sides disagree on boundary or message; a
  webhook-validated operator with no invalid-cr corpus at all (the V5
  `GAP`); an `XValidation` rule without a `message=`.
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
   test and a rejection fixture — the demotion is invisible unless
   something re-checks parity.
2. **Reworded message, stale e2e assertion.** A webhook or CEL message
   was improved; the Chainsaw `contains($error, '…')` substring still
   matches the old wording. The suite fails with an obscure mismatch —
   or worse, keeps passing because the substring accidentally matches a
   different rule's message.
3. **New rule, no rejection fixture.** A validation was added and its
   unit test written, but no invalid-cr fixture — the rule is never
   exercised against a real API server, where CEL evaluation, schema
   pruning, and webhook ordering can all change the outcome.
4. **Twinned rule drift.** A marker boundary was tightened (`Minimum=3`)
   but the webhook twin still checks the old boundary — users get the
   webhook's stale message for values the schema already rejects, and
   the webhook silently stops being a real guard.
5. **Webhook-validated CRD without a corpus.** An operator registers a
   `ValidatingWebhookConfiguration` and rejects CRs in code, but has no
   `tests/e2e/<op>/invalid-cr/` at all — every rejection path is pinned
   only by unit tests that never cross the wire (today: c5c3's
   ControlPlane webhook).

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (add the fixture, extend the webhook test, re-align the
  twin) as a separate, explicitly-scoped task.
- The webhook unit tests in this repo assert violations structurally
  rather than by message string, so V3 deliberately anchors the Chainsaw
  substrings against the Go sources instead of requiring messages to
  appear in `*_webhook_test.go`. Whether the unit tests cover each
  violation path is a step-2 judgement backed by the codecov `webhooks`
  component target (90%), not a grep.
- Pair this with [[check-crd-drift]] — that skill confirms the marker
  source regenerates cleanly into the CRD YAML and Helm copies; this
  skill confirms the rules the markers express keep their webhook, test,
  and fixture counterparts.
- Pair this with [[check-fixture-drift]] — that skill confirms fixtures
  are schema-valid, reachable, and generator-synced; this skill confirms
  the *rules* the fixtures exercise still exist and match.
- Pair this with [[check-condition-coverage]] — same audit shape, one
  layer up: conditions instead of validation rules.
