# Service notes for check-service-parity

Per-service findings from earlier audit runs that step 2 items 5 and 6 of [SKILL.md](../SKILL.md) would otherwise rediscover.

## Cinder: db purge row cap (item 5)

Cinder's `db purge` has no per-run row cap to check at either
release: `cinder-manage db purge <days>` takes the age alone at
27.0.0 and at 28.0.0, with no `--max_rows` counterpart to glance's
cap, and a run issues one unbounded delete per table. Item 5's shape
check records that as a deviation in the report rather than a
finding, and it needs no `ALLOWED_DEVIATIONS` token, because the
scripted P10 check passes on the projected CronJob. The other three
elements still apply, and cinder meets them. Cinder also runs
maintenance work item 5 never reaches: the
`<cinder>-<backend>-service-remove` Job fires from the detach
sequence a `CinderBackend` deletion starts, with no CronJob behind
it, so comparing projected CronJobs against the `cinder-manage`
subcommands does not see it.

## Cinder: component labels on Job pods (item 6)

Cinder is the first service with four Deployment kinds under one
instance label (API, scheduler, one `cinder-volume` per attached
backend, `cinder-backup`), which is why its API Service selector
carries the component key from the first pass. On item 6, the
db-purge CronJob carries `componentLabels(cinder, "db-purge")` on
the pod template under its job template, and the service-remove Job
reassigns `Spec.Template.Labels` to
`componentLabels(cinder, "service-remove")` after the shared builder
returns. The migration Jobs carry no component value at all:
`job.BuildMigrationJob` labels the Job object only and leaves the
pod template unlabelled, so the db-sync and schema-check pods hold
no `app.kubernetes.io` keys and miss the API Service selector by
absence. Item 6's grep therefore finds the two maintenance builders,
not the migration Job.
`tests/e2e/cinder/maintenance-endpoint-isolation` pins the pods it
covers: it samples the API Service EndpointSlices while a live
db-purge pod and a live service-remove pod each hold a pod IP, and
fails if either IP appears among the addresses.
