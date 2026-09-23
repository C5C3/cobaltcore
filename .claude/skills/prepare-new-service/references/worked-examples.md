# Worked examples and the planning chain (issue states checked 2026-09-23)

Read this before drafting the meta (step 5), and in step 1 when the new
service resembles one below. The issues are the calibration for scope,
tone, and checkbox granularity; read the ones matching the profile.

## Which onboarding to read for which situation

| Onboarding | Issues | Read it for |
|---|---|---|
| Horizon | meta #552, pre-work #551 | no database, no tempest plugin, non-oslo config; the first generalization round |
| Glance | meta #656, pre-work #653 (Garage S3 test infra), #654 (c5c3 groundwork: role assignments, generalized catalog, cross-namespace credential delivery), #655 (common extractions) | a new backing store as pre-work, satellite backends, the Phase-0 decision record (D1–D10) worth imitating, and the alternative implementation mode — Phases 2–4 as one continuous stacked-PR arc instead of fanned-out sub-issues |
| Aurora dashboard | meta #758 (open), pre-work #757 (shared workload builder) | the first **non-OpenStack-upstream** service; its Phase-0 block records the release-decoupling decisions that [non-openstack-upstream-services.md](non-openstack-upstream-services.md) generalizes |
| Service registration | #846 | the worked example for the registration mechanism Phase 4 consumes: it replaced the ControlPlane's inline catalog table and service-account entries with the projected `KeystoneService` child |
| Neutron + OVN | meta #898 → #901 spike, #902 image, #903 ovn-operator, #904 neutron-operator, #905 CI/e2e, #906 ControlPlane, #907 docs; pre-work #895 (RabbitMQ backing service) | a service with a **message bus**: #895 delivered the broker, and `internal/common/messaging` was cut from it and built with the first consumer (#904); two operators and several CRDs in one meta; an agent in a privileged namespace; kernel-module-dependent suites (the chassis baseline runs blocking per the meta's D8) |
| Cinder | meta #979 → #985 spike, #986 image, #987 operator + satellites, #988 CI/e2e, #989 ControlPlane, #990 docs; pre-work #977 (satellite extraction), #978 (kind NFS server + csi-driver-nfs), #980 (c5c3 prune + bus delivery) | a **meta split across many sub-issues** (one per phase) with a spike and `/planwerk:decide`; satellite backends on the extracted mechanics; a kind-only backing store behind a flag (`WITH_NFS`); a `restricted` posture kept through config and downstream image patches (`patches/cinder/`) |
| Nova | meta #1014 → #1015 spike, #1016 image, #1017 operator, #1018 CI (nested meta → #1037 deploy stack, #1038 pipeline, #1039 e2e/chaos, #1040 tempest), #1019 ControlPlane, #1020 docs; pre-work #1012 (multi-database) | a service with **two databases** (read #1012 first); five workloads in one CR; hard cross-service dependencies (placement, neutron, glance); a published contract for another cluster; a fake-driver CI substrate; a meta body split into body + continuation comment |
| Compute clusters | meta #1013 → #1060–#1068 | a follow-on meta for the data plane on dedicated clusters, adopting external operators (openstack-hypervisor-operator, kvm-node-agent) as-is, with the upstream generalization tracked here (#1066) |

## The planning chain

This skill produces the meta (plus any pre-work issue); the author's
planwerk skills consume it:

1. `/planwerk:meta` splits the meta into the fewest self-contained
   sub-issues with native sub-issue links and blocked-by edges. The
   onboardings so far split one sub-issue per phase (#898, #979, #1014),
   with the Phase-0 spike first; the meta body then carries `(#N)` on its
   phase headings. Phase 3 came out blocked by Phase 2 despite the metas'
   "alongside" wording (#905←#904, #988←#987), because suites and pipeline
   lists reference the operator module.
2. Phase 0 is written as recommendations, not settled decisions. The spike
   sub-issue proves the runtime ones on a throw-away kind cluster, writes
   no code, and records its evidence as comments whose outcomes land in
   the meta's Phase 0 block (#985, #1015); `/planwerk:decide` then
   verifies the rest against the repository, puts the genuine judgment
   calls to the author, and records the outcomes in the meta and every
   sub-issue that assumed one (#979 on 2026-09-09, #1014 on 2026-09-15).
3. `/planwerk:elaborate` turns each sub-issue into a plan grounded in the
   repository before it is implemented. Author decisions taken during one
   elaboration can make siblings stale (#987 chose inline CSI volumes and
   pulled the CI operator lists into the operator PR, leaving #988–#990 to
   be corrected), so `/planwerk:revisit` re-checks a sub-issue before it is
   implemented.
4. A sub-issue that would not fit one reviewable PR is split again with
   `/planwerk:meta` into a nested meta: #1018 would have been ~200 files and
   2+ hours per CI fix round (the tempest job waits for every e2e and chaos
   leg), so it became #1037–#1040.
5. `/planwerk:implement` (or the unattended `planwerk-agent implement`)
   implements an elaborated sub-issue.

Size the meta for this chain: phases that map one-to-one onto sub-issues,
Phase-0 items phrased so `/planwerk:decide` can confirm or overturn each
one, and every checkbox concrete enough for `/planwerk:elaborate` to find
its files.
