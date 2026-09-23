# The shared operator scaffold (verified 2026-09-23 at `12202e40`, re-verify at HEAD)

Read this in step 4 (generalization pre-check) and again when drafting the
Phase 2 checkboxes: everything below is consumption, not an extraction
candidate.

The nine service operators (barbican, cinder, glance, horizon, keystone,
neutron, nova, ovn, placement) no longer hand-write their pod
template, their scheme, or their webhook and instrumentation shims. A new
operator consumes the shared forms below; writing the pre-extraction
keystone shapes from memory (or from an older PR) reintroduces
boilerplate that has been deleted repo-wide, and review will send it
back.

| Consume | Instead of |
|---|---|
| `deployment.BuildWorkload(WorkloadParams{...})` — replicas/selector/strategy, pod-level knobs, container resources, restricted security context, preStop hook; everything else renders verbatim, **nilness included** | a hand-assembled `appsv1.Deployment` literal |
| `deployment.BuildService(ns, name, labels, selector, port, targetPort)` — `port` and `targetPort` stay separate so traffic can route to a sidecar | a hand-assembled `corev1.Service` literal |
| the component-label contract from `internal/common/naming`: API pod template labelled `ComponentLabels(app, instance, ComponentAPI)`, API Service selecting on `APISelectorLabels`, API PDB on `SelectorLabels` + `ExcludeJobPods()`, while `Deployment.spec.selector` and the NetworkPolicy `podSelector` stay on `SelectorLabels` (see [recurring-maintenance-jobs.md](recurring-maintenance-jobs.md) for why) — in a **single-workload** operator (barbican, glance, horizon, keystone, placement); a **multi-workload** operator (cinder, neutron, nova) puts `APISelectorLabels` on the API Deployment, its Service and its PDB, and a per-component selector (`SelectorLabels` + the component key) on every other Deployment | a name+instance Service selector that admits every pod of the instance — including maintenance Job pods — as an API endpoint |
| `bootstrap.NewScheme(<extra AddToScheme funcs>...)` in `main.go`, and `bootstrap.Run(bootstrap.ManagerConfig{...})` for the manager | `var scheme = runtime.NewScheme()` plus an `init()` block |
| one package-level `commonreconcile.Skeleton[*<Kind>, <Kind>Status]{SubConditionTypes, Conditions}` value whose `SetReady` / `UpdateStatus` / `MarkFailed` / `RunParallelGroup` the controller delegates to (`novaSkeleton` in `nova_controller.go`) | hand-written Ready aggregation, no-op-skipping status writes and parallel-group glue |
| `mcbuilder.ControllerManagedBy(mgr)` (sigs.k8s.io/multicluster-runtime) with `commonmulticluster.EngageLocalCluster` / `EngageNoProviderClusters`, remote child watches via `AddRemoteChildWatches` (+ `ClusterServesKind` for optional kinds), and every child access routed through `ResolveChildrenClient` — see [placement-on-target-clusters.md](placement-on-target-clusters.md) | a single-cluster `ctrl.NewControllerManagedBy` with `Owns()` watches and direct `r.Client` child writes |
| embedding `webhook.NoopDeleteValidator[T]` | a per-webhook `ValidateDelete` method |
| passing the bound method `instrumenter.Instrument` into the pipeline | a package-local `instrumentSubReconciler` wrapper |
| referencing `commonreconcile.*` / `healthcheck.*` requeue constants directly | package-local aliases in a `requeue_intervals.go` (keep that file only for genuinely operator-specific waits; horizon has none and therefore no file) |
| `internal/common/satellite` (`Collect`, `ResolveParentChildren`, `SectionProjected` / `ObserveParams`) plus `validation.ExtraOptions` / `ExtraOptionsRules` and `validation.AttachedSiblings` for a satellite backend CRD (#977) | a fourth copy of the `GlanceBackend` / `BarbicanSecretStore` watch, aggregation and webhook mechanics |
| `internal/common/messaging` for a bus consumer (`ReconcileTransportURLSecret`, `TransportURLEnvVar`, `RabbitSection`, `EgressPort`; built with Neutron, #904) | a hand-rendered `transport_url` in the ConfigMap |
| `ProvisionFlowParams.AdditionalDatabaseNames` and a derived instance name (`<cr>-api`) per extra `DatabaseSpec` block (#1012; recipe paragraph "A second database block" in `docs/contributing/adding-a-new-operator.md`) | a second hand-rolled database provisioning flow |

**Satellite backends.** If users attach a variable number of backends
(image stores, identity domains), model them as a **satellite CRD**
mirroring `KeystoneIdentityBackend`/`GlanceBackend`: inverted
attachment via `<svc>Ref`, dedicated per-backend controller + an
aggregating sub-reconciler on the parent, curated `backends[]`
projection from c5c3 with prefix-guarded pruning, plus the three suites
the pattern demands (multi-instance, default/aggregation-switch with
last-good retention, `invalid-<child>-cr`). `BarbicanSecretStore`,
`CinderBackend` and `CinderBackupBackend` are the later copies, all on
`internal/common/satellite` since #977.

What stays service-specific is the residue the builder cannot know:
conditional volume/mount/container appends (db-TLS keypair, backend
config, sidecars), the launch command, probes, and any pod annotation
that must roll the Deployment when a secret rotates.

Copy the Service-selector latch with the label contract, not just the
labels: every single-workload `reconcileDeployment` gates the narrow
`APISelectorLabels` selector on `deployment.TemplateConverged` and latches
it one-way via
`deployment.APISelectorNarrowed`, read through the uncached
`mgr.GetAPIReader()` (the informer cache can predate the narrowing
write). A new operator's pods carry the component label from birth, so
the latch closes on the first converged rollout — but the gate is what
keeps the Service from ever selecting zero serving pods during a
rollout, and horizon adopted the full shape for exactly that reason
(#785) despite projecting no maintenance pods at all. A multi-workload
operator skips the latch and narrows every selector on the first pass:
one CR owns several Deployments, so a selector without the component key
would let them adopt each other's pods and route API traffic to a worker
that serves no HTTP, and no Deployment predating the component label
exists to migrate off (`apiSelectorLabels` in cinder, neutron and nova;
cinder never used `deployment.APISelectorNarrowed`). OVN projects no API
Service at all.

**Pin the rendered objects.** Every API-serving operator (eight of the
nine; ovn pins each workload in its own `reconcile_<step>_pin_test.go`)
carries a
`reconcile_deployment_pin_test.go`: the full Deployment and Service
marshalled with `sigs.k8s.io/yaml` and compared as a plain string, one
golden per input that perturbs the pod template (launch mode, TLS,
sidecar, autoscaling, hash annotations). Generate the new operator's pins
in the same commit that adds its builders — they are what makes the next
change to the shared `BuildWorkload` provably byte-neutral for this
service, and they cost nothing to write while the builder is fresh
(#1012 used the six operators' pins as its byte-neutrality proof).
