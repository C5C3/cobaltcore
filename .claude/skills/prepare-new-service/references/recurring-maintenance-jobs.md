# Recurring maintenance jobs (verified 2026-09-23 at `12202e40`, re-verify at HEAD)

Read this when answering the profile's "recurring maintenance?" question
(step 1) and when writing the Phase 0 inventory and the Phase 2 checkboxes
(step 5).

Every OpenStack service that soft-deletes rows, caches artefacts, or ages
out records assumes an operator sweeps up on a schedule. Upstream ships
the commands and documents the cadence; scheduling them is the deployer's
job. Package deployments answer that with a cron entry on the controller
node — CobaltCore has no such place, so a maintenance task that the service
operator does not project simply never runs. That failure is invisible by
construction: the deployment stays Ready, every probe passes, and the
only symptom is a table, a cache, or a disk that keeps growing until it
becomes an incident: unless the operator projects a CronJob, no run ever
happens, and no probe, condition, or metric observes a purge that was
never scheduled.

Treat this as part of the initial implementation. Glance is the
cautionary tale: it onboarded without one, and adding `db purge` later
(#729) cost an API block, a sub-reconciler, a condition, a metric pair,
an admission bound on `metadata.name` in **two** operators plus a
hash-collapse fallback for the CRs admitted before that bound existed,
and three invalid-CR fixtures — all of which would have been a handful of
extra lines inside the original scaffolding PRs. Every service since has
shipped its task with the operator: barbican `db clean`
(`reconcile_dbclean.go`), cinder `db purge` (`reconcile_dbpurge.go`), nova
`db archive_deleted_rows` (`reconcile_dbarchive.go`), neutron's OVN
database sync (`reconcile_ovndbsync.go`) and the OVN database backup
(`operators/ovn/internal/controller/reconcile_backup.go`).

## Find the tasks

For each candidate ask: does upstream document a periodic invocation, and
what happens over a year without it? Sources, in order of authority: the
service's admin/operations guide for the pinned release, `<svc>-manage
--help` run against the built image (authoritative for the subcommand
names at that exact ref), and the upstream deployment projects
(openstack-k8s-operators, Yaook, kolla-ansible cron roles) for the
cadences they picked. Read the exit codes too, in the source at the
pinned ref: `nova-manage db archive_deleted_rows` answers 1 when it
archived rows (a success), so the nova CronJob wraps it in a script that
maps 0/1 to success; a CronJob that trusts the exit code fails every run
that did work.

The recurring shapes, with the commands to look for — a starting list to
verify, not a lookup table:

| Shape | Examples |
|---|---|
| Hard-delete soft-deleted rows | `glance-manage db purge` / `db purge_images_table` (implemented), `nova-manage db archive_deleted_rows` (implemented, without `--purge` or `--until-complete`, so the shadow tables keep the rows) + `db purge`, `cinder-manage db purge` (implemented; takes an age and no row cap), `heat-manage purge_deleted`, `barbican-manage db clean` (implemented) |
| Expire ephemeral records | `keystone-manage trust_flush` (implemented), token/session expiry sweeps — for Django-session services only when sessions are DB-backed (horizon's signed-cookie default needs none) |
| Prune caches and staging areas | glance image cache pruner/cleaner (implemented as the `cacheMaintenanceContainer` sidecar under `spec.imageCache`, not a CronJob), orphaned upload staging directories |
| Rotate key material | keystone fernet + credential rotation (`internal/common/rotation` — already generalized) |
| Reconcile or back up a second store | neutron's `spec.ovnDBSync` CronJob (repairs drift between the Neutron and OVN databases; opt-in, `True` when unset), the OVN database backup CronJob |
| Upstream daemons, not CronJobs | some services ship a long-running housekeeper (octavia) — model it as a Deployment, and say so in the profile rather than leaving the row blank |

A service with no maintenance task is a legitimate answer; record it in
the meta issue as a decision with its reasoning, so the next audit reads
it as considered rather than forgotten.

## Build sheet

`reconcile_dbpurge.go` (glance) and `reconcile_trustflush.go` (keystone)
are the two worked examples; both build on `internal/common/job`
(`reconcile_dbpurge.go` in cinder and `reconcile_dbarchive.go` in nova are
the multi-workload versions). One
maintenance task touches:

- **API** — an optional `spec.<task>` block (`DBPurgeSpec` in
  `operators/glance/api/v1alpha1/glance_types.go`) with `schedule`,
  retention/scope knobs, and `suspend`. Resolve the defaults at reconcile
  time in an `effective<Task>` helper, not in the defaulting webhook, so
  an unset field tracks the operator default across upgrades.
- **Sub-reconciler** — a parallel-group step calling `job.EnsureCronJob`
  through the resolved children client,
  plus `Owns(&batchv1.CronJob{}, engageLocal, engageNoProviders)` on the
  builder and the CronJob kind in the operator's `<Svc>RemoteChildKinds`
  list for the `AddRemoteChildWatches` leg.
- **Condition** — `<Task>Ready` in the operator's condition list and in
  the `subReconcilerConditionTypes` map (else the metric label reads
  `UNKNOWN`; [[check-condition-coverage]] gates this). Suspension keeps
  it `True` under its own reason — a pause is a posture, not a failure —
  but the message must name the backlog that stops draining.
- **Run visibility** — the CronJob controller prunes its Jobs by history
  limit, so list the Jobs by the CR's common labels, keep the ones the
  CronJob controls, and report on the newest that reached a terminal
  state (`job.TerminalCondition`). Feed
  `job.RecordJobTerminalState`, which dedupes on the Job UID, into a
  metric pair (`<svc>_operator_<task>_total` +
  `_duration_seconds`) and emit a Warning event on failure.
- **Webhook** — cron grammar via `validation.CronSchedule`
  (`internal/common/validation`; it accepts `@daily`-style descriptors
  that a CRD pattern would not), bounds on the retention knobs mirroring
  the CEL rules, and an update warning when a retention window shrinks,
  because the shortened window applies retroactively at the next firing.
- **Safety knobs on the CronJob** — `ConcurrencyPolicy: Forbid` (two
  purges deleting from the same tables contend: Galera certification
  failures, InnoDB lock timeouts), a per-invocation row cap
  (`--max_rows`) keeping each write-set Galera-friendly, and
  `ActiveDeadlineSeconds` so a wedged run turns into a terminal failure
  instead of an active Job that holds the condition `True` forever.
  Document what the cap means for throughput: it is the schedule, not
  the job, that sets the drain rate. Not every command offers a cap
  (`cinder-manage db purge` takes none); say so in the reference docs
  instead of inventing one.
- **The same runtime contract as the workload** — config volume, the
  `database.ConnectionEnvVar` override so the job reads dynamic
  credentials from the derived Secret rather than the ConfigMap, the
  db-TLS keypair mount under the same gate the Deployment uses, and pod
  labels from `naming.ComponentLabels` with the task's own component
  value (`trust-flush`, `db-purge`, `<kind>-rotation`): a superset of
  the selector labels so the NetworkPolicy covers the job pods, but
  **never** bare `commonLabels` — a Job pod whose labels satisfy the
  API Service selector becomes a Service endpoint with nothing
  listening (numeric `targetPort`, no readiness probe) and answers API
  traffic with `ECONNREFUSED` for its whole runtime, the #778 outage
  the component contract exists to prevent. The API PDB keeps the Job
  pods out via `naming.ExcludeJobPods`, not via the component label. A
  job that also talks to the API server needs its own egress rule
  (keystone's rotation CronJobs do).
- **Naming** — Kubernetes caps a CronJob name at 52 characters. Bound
  `metadata.name` in the webhook accordingly (see the CronJob gotcha in
  [known-gotchas.md](known-gotchas.md)) and
  mirror that bound in the c5c3 webhook for the projected child name
  (`validate<Svc>ChildName` + `<svc>ChildNameOverhead` in
  `controlplane_webhook.go`; glance through nova each carry one).
- **Tests** — unit tests pinning the rendered CronJob and every condition
  arm, invalid-CR fixtures for each new validation rule
  (fixtures `17`–`19` in `tests/e2e/glance/invalid-cr/` are the template), and a
  `basic-deployment` assertion that the CronJob exists. For the endpoint
  isolation itself, `tests/e2e/keystone/maintenance-endpoint-isolation`
  is the template (cinder and nova carry their own copies): a fast
  schedule, the API EndpointSlices sampled
  against the live Deployment's replica count, and a live maintenance
  pod required during the window so the suite proves isolation rather
  than an idle cluster. Assert what the command really does: the nova
  db-archive suite asserts rows moved into the shadow tables, not
  drained shadow tables.
- **Docs** — the spec block and the projected CronJob shape in
  `docs/reference/<svc>/<svc>-crd.md`, the step and its condition in
  `<svc>-reconciler.md`, the events plus an alerting rule in
  `<svc>-events.md`.

## Sub-issue checkbox

Phase 2 carries one checkbox per maintenance task, named after the
command it runs, e.g.:

> - [ ] `spec.dbPurge` + `reconcileDBPurge`: project the
>   `{name}-db-purge` CronJob (`<svc>-manage db purge`), report the
>   newest terminal run via `DBPurgeReady`, metric pair, invalid-CR
>   fixtures, CRD/reconciler/events docs
