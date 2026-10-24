<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# Chainsaw e2e suites

Happy-path end-to-end tests, driven by
[chainsaw](https://kyverno.github.io/chainsaw/) against a kind cluster
deployed with `make deploy-infra`. Run everything with `make e2e`, or a
single suite with:

```bash
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/<suite>/
```

## Layout and conventions

- **Directory per suite.** Every suite is one directory holding a
  `chainsaw-test.yaml` plus its fixtures. Chainsaw discovers suites
  recursively — a new directory needs **no** Makefile or CI registration
  (the chaos tree is the exception, see `tests/e2e-chaos/README.md`).
- **Tree layout:**
  - `tests/e2e/<operator>/` — per-CR suites for one operator
    (`keystone/`, `glance/`, `c5c3/`).
  - `tests/e2e/<operator>-operator/` — operator-chart-level suites
    (e.g. `keystone-operator/network-policy-egress`,
    `glance-operator/metrics`); the `e2e-operator`
    CI job includes this directory only when it exists.
  - `tests/e2e/infrastructure/` — suites against the bare infra stack,
    run by the `e2e-infra` CI job.
- **Fixtures are numbered** in apply order: `00-keystone-cr.yaml`,
  `01-patch-upgrade.yaml`, …
- **Test naming:** `metadata.name` in `chainsaw-test.yaml` is
  `<operator>-<feature>` (e.g. `keystone-scale`). CI failure logs report
  this name (`--- FAIL: chainsaw/<name>`), so keep it equal to the
  directory's feature part.
- **Per-release variants** are hand-maintained copies named
  `<suite>-<release-slug>` (e.g. `basic-deployment-2026-1`) with the
  image tag and resource names updated — remember them when adding or
  retiring an OpenStack release.
- **Chassis suites.** A suite that renders an `OVNChassis`, a
  `NeutronMetadataAgent` or a `NovaCompute` labels the node itself with
  `openstack.c5c3.io/chassis=true`, removes the label again in its step
  `cleanup`, and sets `spec.concurrent: false`. The chassis DaemonSets
  run with `hostNetwork` and own `/run/openvswitch` and `/run/ovn` on
  the node, a `NovaCompute`'s pods own `/var/lib/nova` there too, and
  `hack/kind-config.yaml` declares a single node, so two such suites
  running at the same time contend over the same host paths.
  `tests/e2e/ovn/chassis-single-node/` is the model, and
  `tests/e2e/nova/compute-node-pool/` also labels the node with the zone
  and the pool label its `NovaCompute` selects.
- **Shared scripts.** `tests/e2e/lib/` holds scripts that the suites of
  several operators run:
  - `assert-namespace-scoped-operator.sh`, the proof every
    `namespace-scoped-rbac` suite runs against the operator release it
    installed;
  - `assert-operator-env-clean.sh`, the scan every `env-key-material`
    suite runs to show that the operator's pod spec and process
    environment hold no key material;
  - `assert-operator-metric-families.sh`, which 18 suites run to read the
    operator release's own Prometheus families through the API server's
    pod proxy, with the per-CR samples their Jobs, rotations or re-mints
    record;
  - `assert-steady-state-events.sh`, which ends each of the nine
    `basic-deployment` suites: the Ready CR may gain no event in a
    120-second window.

  Chainsaw runs script steps in the suite directory, so a suite calls them
  by relative path (`../../lib/<script>.sh`). Each script is unit-tested
  under `tests/unit/ci/` against a stubbed `kubectl`. The `tests_e2e_<op>`
  path filter of every operator whose suites run a script lists it, so an
  edit to a script runs those `e2e-operator` jobs. The seven operators
  that run the proof list `tests/e2e/lib/**`. ovn and neutron list the
  scan, the metric check and the event budget, and c5c3 lists the scan
  and the metric check.

## Configuration

`tests/e2e/chainsaw-config.yaml` applies to the whole tree: `parallel: 4`,
`failFast: true` (everything after the first failure may be cascade or
skipped), `assert: 120s`, `cleanup: 3m` (deletion suites wait on the
MariaDB operator serializing Database/User/Grant deletions), and a
JUnit report written to `_output/reports/`.

Chainsaw injects `NAMESPACE=<test namespace>` into every `script:` step.
Helper scripts must use dedicated variables (e.g. `OPENBAO_NAMESPACE`)
for anything that is not the test namespace.

## Generated suites — do not hand-edit

`tests/e2e/keystone/invalid-cr/`,
`tests/e2e/keystone/invalid-identitybackend-cr/`,
`tests/e2e/horizon/invalid-cr/`, `tests/e2e/c5c3/invalid-cr/`,
`tests/e2e/glance/invalid-cr/`,
`tests/e2e/glance/invalid-glancebackend-cr/`,
`tests/e2e/barbican/invalid-cr/`,
`tests/e2e/barbican/invalid-barbicansecretstore-cr/`,
`tests/e2e/ovn/invalid-cr/`,
`tests/e2e/ovn/invalid-ovnchassis-cr/`,
`tests/e2e/neutron/invalid-cr/`,
`tests/e2e/neutron/invalid-neutronmetadataagent-cr/`,
`tests/e2e/cinder/invalid-cr/`,
`tests/e2e/cinder/invalid-cinderbackend-cr/`,
`tests/e2e/cinder/invalid-cinderbackupbackend-cr/`,
`tests/e2e/nova/invalid-cr/`, and
`tests/e2e/nova/invalid-novacompute-cr/` are each
generated by their `_generate.py` (unit-tested by `test_generate.py`). Never
edit the numbered fixtures by hand; change the generator and re-run it.
`make verify-invalid-cr-fixtures` is the CI gate that catches drift.

## Related trees

- `tests/e2e-chaos/` — Chaos Mesh fault-injection suites (own config and
  conventions, see its README).
- `tests/e2e-operator-upgrade/` — the operator helm-upgrade-in-place
  suites, one per operator, live **outside** `tests/e2e/` on purpose:
  each manages its operator's release itself and must run against a
  cluster without a pre-deployed operator, so `make e2e` must not sweep
  them up.
- `tests/e2e-nova-libvirt/` — the suite that boots a server through
  Nova's libvirt driver lives **outside** `tests/e2e/` on purpose: it
  needs libvirtd on an amd64 node and runs in the non-blocking
  `e2e-nova-libvirt` job, so neither `make e2e` nor the blocking nova
  leg of `e2e-operator` may sweep it up.
- `tests/tempest/` — Tempest API test configuration per service and
  release.

Debugging a red e2e CI job? Use the `debug-e2e-failure` skill
(`.claude/skills/debug-e2e-failure/`).
