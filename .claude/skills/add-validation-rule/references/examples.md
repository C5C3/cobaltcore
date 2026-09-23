# Worked examples

Three rules from the history, one per shape the SKILL.md decision table
distinguishes. Read the commits with `git show --stat <sha>` and
`git show <sha> -- <path>`; the file lists below are the validation-relevant
part of each.

## A. Twinned enum marker, schema answers (neutron, 2026-09-22)

`c5344317` `feat(neutron): add novaMetadata.protocol to the metadata agent`,
docs in `2cca3b2e`.

| Layer | Edit |
|---|---|
| Marker | `+kubebuilder:validation:Enum=http;https` on `NovaMetadataSpec.Protocol` in `operators/neutron/api/v1alpha1/neutronmetadataagent_types.go` |
| Regenerated | `config/crd/bases/…_neutronmetadataagents.yaml` and the Helm copy under `helm/neutron-operator/crds/` |
| Defaulting | `Default()` fills `DefaultNovaMetadataProtocol` (`http`) inside a present block and leaves a nil block nil |
| Webhook twin | `validate()` appends `field.Invalid(novaPath.Child("protocol"), p, "protocol must be http or https")` for a non-empty value outside the enum |
| Unit tests | a defaulting assertion plus a row `unsupported novaMetadata protocol rejected` in `TestNeutronMetadataAgentValidateCreate_RejectionTable` |
| Ownership | `nova_metadata_protocol` joins `MetadataAgentOwnedConfigKeys`, so `spec.extraConfig` cannot set it behind the typed field |
| Fixture | `10-novametadata-protocol-invalid.yaml` in `tests/e2e/neutron/invalid-neutronmetadataagent-cr/`; `_EXPECTED_FIXTURE_COUNT` 10 → 11 |
| Chainsaw | asserts `novaMetadata.protocol` and `Unsupported value`: the schema's enum phrase, because the API server rejects the CR before the webhook runs |
| Docs | the field row, the enum in the marker list, and the webhook message table row in `docs/reference/neutron/neutron-metadata-agent-crd.md` |

The lesson: the webhook message is written and unit-tested, but the e2e
assertion pins the schema phrase.

## B. CEL rule, exported validator, ControlPlane delegation (glance, 2026-07-25)

`7e7d392a` `feat: add the Glance importFiltering spec surface and admission
rules`, then `5cdfa822` `feat: project importFiltering from the ControlPlane`.

Service side (`7e7d392a`):

- three type-level `XValidation` rules on `ImportFilteringSpec` in
  `glance_types.go` reject a non-empty allow-list beside its deny-list, because
  glance ignores the deny-list then (`allowedSchemes and disallowedSchemes are
  mutually exclusive: …`); item markers bound the scheme enum, host length,
  port range and a 64-item cap;
- `ValidateImportFiltering` in `glance_webhook.go` repeats all of it with the
  same message text, and adds the newline check on hosts, which no marker
  carries;
- fixtures `13-importfiltering-allow-and-deny-hosts.yaml` (asserts the CEL
  message) and `14-importfiltering-host-control-char.yaml` (asserts the
  webhook message, the only gate) in `tests/e2e/glance/invalid-cr/`;
- the `importFiltering` row and section in `docs/reference/glance/glance-crd.md`.

The validator is exported because the next commit needs it.

ControlPlane side (`5cdfa822`):

- `ServiceGlanceSpec.ImportFiltering` is typed
  `*glancev1alpha1.ImportFilteringSpec`, so controller-gen copies the three
  CEL rules and the item markers into `c5c3.io_controlplanes.yaml` (136 lines
  in the base CRD and again in the Helm copy);
- `validateGlance` in `controlplane_webhook.go` calls
  `glancev1alpha1.ValidateImportFiltering(glPath.Child("importFiltering"), …)`
  instead of carrying a mirror;
- `reconcile_glance.go` projects the block, with a unit test;
- fixture `62-glance-importfiltering-allow-and-deny-hosts.yaml` in
  `tests/e2e/c5c3/invalid-cr/` asserts the same CEL message on the
  ControlPlane.

Both CRDs ship in separate charts, so "admitted by the ControlPlane, accepted
by the child" holds while both come from one release; the field doc records
that caveat.

## C. Webhook-only, create-only name bound, mirrored on the ControlPlane (nova, 2026-09-16 to 09-22)

| Commit | Edit |
|---|---|
| `974269fe` `feat(nova): add the Nova API types, webhook and config ownership` | `MaxNovaNameLength = MaxCronJobNameLength - len("-db-archive")` (41) and `validateNameLength`, called from `ValidateCreate` only; the unit test `TestNovaValidateUpdate_OverlongNameStaysUpdatable` pins that an update does not trip it |
| `38b96179` `test(nova): add the invalid-cr rejection corpus` | new corpus, both Makefile lines, `19-name-too-long.yaml` one character past the bound; `test_generate.py` holds every other fixture's name at or under 41 |
| `699fc369` `feat(c5c3): admit services.nova` | `validateNovaChildName` bounds the ControlPlane name so `{cp}-nova` fits `novav1alpha1.MaxNovaNameLength` (36 characters); called on create and on the update that newly sets `services.nova` |
| `6b83b922` `test(e2e): reject the fourteen invalid services.nova fixtures` | `104-nova-name-too-long.yaml` asserts `the projected Nova child CR name would be 42 characters` and `when spec.services.nova is set`; the corpus crossed prefix 99 here, so the commit widened `_FIXTURE_FILENAME_PATTERN` to `[0-9]{2,3}` |
| `2cca3b2e` `docs: describe services.nova, the notifier and the agent protocol` | the "Projected Nova name bound" row in `docs/reference/c5c3/controlplane-crd.md`, tagged **Webhook-only** |

Why webhook-only and create-only: `metadata.name` is immutable, so an update
rule could only fire against a CR an older operator admitted, including the
finalizer-removal update that completes its deletion. The controller stays
total for a grandfathered over-long name by collapsing it onto a
content-stable hash. Without the ControlPlane mirror a long ControlPlane name
is admitted, the Nova child is rejected on every reconcile, and only deleting
and recreating the ControlPlane recovers.
