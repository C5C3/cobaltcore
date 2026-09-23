# Non-OpenStack-upstream services (verified 2026-07-27 pre-aurora; line references re-verified 2026-09-23 at `12202e40`, re-verify at HEAD)

Read this in step 1 when the service does not come from
`opendev.org/openstack` (own org, own release cadence, usually not Python),
and when a service image bundles a non-OpenStack artefact.

A service from outside the OpenStack upstream (own org and repo, own
release cadence, usually not Python — worked example: Aurora dashboard,
meta #758, still open) keeps **layers 2–5 unchanged**: the operator is Go regardless
of the workload, and the horizon subtraction (no
database/job/release/rotation/tls/keystoneauth) usually applies. Layer 1
breaks structurally. Profile these instead of the Python questions:

- **Upstream artifacts first:** check what upstream actually publishes
  (Aurora: npm packages only, via Changesets; both upstream Dockerfiles
  are dev-grade). Default posture: CobaltCore builds the production image from
  source at a pinned upstream git ref.
- **Release decoupling — do NOT add a `releases/*/source-refs.yaml`
  key.** The keys *are* the build/test/verify matrix
  (`hack/ci-generate-build-matrix.sh:61-69`), imply per-OpenStack-release
  versioning, and obligate `verify_release_config.sh`. Use the
  `keystone-federation-proxy` precedent instead: dedicated
  build/merge jobs outside the matrix, the verify script run inside the
  build job (`build-images.yaml:592-695`),
  image tagged with the service's own version. `backup-shifter`
  (`:696-799`) and `ovn` (`:800-967`, its own `verify-ovn-image` job, the
  tag read from `ARG OVN_VERSION` by `hack/ci-resolve-ovn-version.sh`) are
  two more decoupled images.
- **Source org:** two clone sites hardcode `openstack/<service>` —
  `.github/actions/checkout-service-source/action.yaml` and
  `hack/ci-build-service-image.sh:94-95`. Parameterize them or bypass both
  via the dedicated job.
- **Renovate:** the source-refs customManager templates
  `opendev.org/openstack/{{depName}}` (`renovate.json:26`). A decoupled pin
  needs its own customManager (github-tags/releases datasource) plus a
  packageRules entry — gate with [[check-renovate-coverage]]. A
  non-OpenStack artefact bundled into an OpenStack image follows the noVNC
  shape of #1016 (`images/nova/Dockerfile`): `ARG <X>_VERSION` plus
  `ARG <X>_COMMIT`, fetched by commit with an in-build guard that the tag
  still matches, and one Renovate github-tags manager capturing tag and
  digest in a single matchString so a bump cannot move one without the
  other.
- **The Python gotchas don't transfer:** upper-constraints,
  `extra-packages.yaml`, uv/PBR/WSGI, the `python-base`/`venv-builder`
  lineage, and the `openstack`-user assertion in
  `verify_deviation_comments.sh` are all Python-specific. A non-Python
  image writes its own `verify_<svc>.sh` contract (runtime present, built
  assets present, non-root, no build toolchain in the final image) and its
  own deviation-comment function (`test_ovn_deviation_comment` and
  `test_backup_shifter_deviation_comment` are the shape). No shared base
  image for a new language
  before the rule of three.
- **e2e inversion:** instead of one service version per OpenStack release,
  **one** service version runs against **N** OpenStack releases — the
  `basic-deployment` fork pair carries the same service tag against both
  stacks, and the kind image-load legs (the ci.yaml `e2e-operator`
  `Resolve E2E images` step, where `keystone-federation-proxy:dev` and
  `ovn:${OVN_VERSION}` are the precedents)
  need an entry outside `hack/ci-service-image-releases.sh`'s release
  loop; `hack/ci-resolve-e2e-images.sh` names the federation proxy
  explicitly for the same reason.
- **Docs that go stale the moment this lands:**
  `docs/reference/ci-cd/build-images-workflow.md` § Adding a New Service
  and `docs/reference/ci-cd/container-images.md` both claim source-refs is
  the sole registration and assume pip/venv — extending them is part of
  the first decoupled service's Phase 5, not follow-up work.
