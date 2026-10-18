---
name: check-nix-lock-drift
description: >-
  Audit whether every lock under nix/locks/ and every image pin still
  matches the pins it derives from, without Nix — the source and test
  refs under releases/<version>/, the patches, extra-packages.yaml, the
  base pins of nix/base-requirements.txt, the constraint overrides merged
  into upper-constraints.txt, the image pins under images/, and the
  operator vendor hashes. Use when a Renovate pull request is red on
  nix-lock-check, after a bump of a release pin under releases/ or
  overrides/, after a hand edit under nix/locks/, or as a sweep before a
  release.
---

# Check Nix lock drift

`hack/nix-lock.sh` turns the release pins into the locks the Nix image
builds read: `nix/locks/<release>/<name>/` with `sources.json`,
`pyproject.toml` and `uv.lock` for every `source-refs.yaml` key and for
Tempest. It also writes the image pins under `images/` and the operator
vendor hashes. The build reads `releases/` and `overrides/` only through
the lock, so a pin that moved without a new lock builds the old inputs.

This skill reads the committed locks and pins and names every lock that no
longer matches what it derives from. It needs no Nix and no network and
runs in seconds. It is repeatable: run it after any pin bump and before a
release.

## What lock drift means here

| Layer | Lock or pin | Derived from |
|---|---|---|
| Inventory | the three files of `nix/locks/<release>/<name>/` | the `releases/<release>/` directories, their `source-refs.yaml` keys, and `tempest` |
| Source ref | `ref`, `project`, `rev`, `hash` and `patches` of `sources.json` | `source-refs.yaml` (the `tempest` key of `test-refs.yaml` for the Tempest lock) and `patches/<name>/<release>/*.patch` |
| Project inputs | `[project].dependencies`, the dependency group of a derived image and `[tool.uv.sources]` in `pyproject.toml` | `extra-packages.yaml` (`pip_extras`, `pip_packages`, a block without a `source-refs.yaml` key such as `nova-compute`), `test-refs.yaml`, `nix/base-requirements.txt`, the `rev` of `sources.json` |
| Constraints | `[tool.uv].constraint-dependencies` of `pyproject.toml` | `upper-constraints.txt` with `overrides/<release>/constraints.txt` merged by `scripts/apply-constraint-overrides.sh` |
| Image pins | `images/*/source.json`, `images/nova/novnc.json` | the upstream commits they name, prefetched by `make nix-lock` |
| Operators | `operators/<op>/vendor.json` | the `go.mod` and `go.sum` files `hack/nix-lock.sh --check-operators` hashes |

The authoritative gate is `make nix-lock-check`, the `nix-lock-check` job
of `.github/workflows/build-images.yaml`. This skill defers to it.

## Procedure

### 1. Run the audit

```bash
bash .claude/skills/check-nix-lock-drift/scripts/audit-nix-lock-drift.sh
```

Exit code `1` means at least one `[FAIL]`. Interpret:

- **L1** — every lock of every release has its three files, and every
  directory under `nix/locks/` is a lock some release pins. An orphan
  (`no release pins it`) is a lock `make nix-lock` deletes; a missing file
  is a lock it never wrote.
- **L2** — `sources.json` carries the ref the release pins, a `project`
  (`tempest` for Tempest), a 40-character `rev`, a `sha256-` hash, and the
  sorted paths of the release's patches, in the order the build applies
  them. A lagging ref means the image builds the old upstream tree; a
  patch missing from `patches` is a patch the build never applies.
- **L3** — `[project].dependencies` opens with the project and its
  `pip_extras` (`keystone[ldap]`), then lists the `pip_packages` of
  `extra-packages.yaml` and the base pins and nothing else, a base pin by
  name alone where the merged constraints pin it. `[tool.uv.sources]`
  fetches the project at the `rev` of `sources.json`. The Tempest lock
  opens with `tempest==<version>` and holds the five `test-refs.yaml` pins
  `hack/nix-lock.sh` installs and the base pins. A block of
  `extra-packages.yaml` without a `source-refs.yaml` key must be the
  dependency group of exactly one lock, with the block's extras and pip
  packages.
- **L4** — `constraint-dependencies` equals the `name==version` form of
  the `name===version` lines of `upper-constraints.txt` after
  `scripts/apply-constraint-overrides.sh` merged the overrides into a copy.
  A Renovate bump of `upper-constraints.txt` or of an override fails here.
- **L5** — every image pin has a 40-character `rev` and a hash; the ovn pin
  also `ovs.rev` and `ovs.hash`, the pins beside a `manager.nix` a
  `vendorHash`. L5 checks the shape only: the prefetch of
  `make nix-lock-check` alone shows whether a hash matches its commit.
- **L6** — `hack/nix-lock.sh --check-operators`, one line with the stale
  `vendor.json` files it printed.
- **L7** — `[INFO]` only: a lock whose newest commit is older than the
  newest commit of a pin file it derives from, with the file and both
  dates. A pin commit that changes nothing the lock reads (a comment) is
  legitimate, so this is a hint where to look. A shallow clone skips it.

### 2. Run the gate

```bash
make nix-lock-check
```

`make nix-lock-check` is the gate to trust over the script. It regenerates
every lock in the `lock-tools` shell and fails when anything under
`nix/locks/`, an image pin or an operator vendor hash changed. Only the full
regeneration sees a `uv.lock` that re-resolves on unchanged inputs or a
hash that does not match its commit; the script sees neither. It needs Nix
and network access.

The fix for every finding is the same: run `make nix-lock` and commit the
result, on the Renovate branch when a Renovate pull request moved the pin.
`make nix-lock-check-operators` repeats the L6 gate alone and needs neither.

### 3. Report

Group findings by severity, one line each, with the lock and the pin file:

- **HIGH** — a lagging ref, an empty project or a patch list that differs
  (L2), a `[tool.uv.sources]` rev other than the one of `sources.json`
  (L3), and constraint drift (L4): the image builds other inputs than the
  release pins name.
- **MEDIUM** — an orphan or incomplete lock (L1), a missing or extra
  dependency or a derived image without its group (L3), a malformed image
  pin (L5), a stale vendor hash (L6).
- **LOW** — an L7 hint that `make nix-lock-check` did not confirm.

## Notes

- The script is read-only: the constraint merge runs on a copy under
  `mktemp -d`. It runs under Bash 3.2 and needs `yq` (mikefarah) v4.49.1 or
  newer, which also reads the TOML of L3 and L4, and `jq`; without them it
  fails at once. L7 needs git and prints an `[INFO] … skipped` line without
  it.
- `scripts/apply-constraint-overrides.sh` edits with the `sed -i` and the
  `I` flag of GNU sed. With the sed of macOS or busybox the script skips L4,
  and L3 checks only that the pip packages are present, without the base
  pins; run it inside the `lock-tools` shell there:
  `nix develop .#lock-tools --command bash .claude/skills/check-nix-lock-drift/scripts/audit-nix-lock-drift.sh`.
- Releases come from `ls releases/`, locks from the `source-refs.yaml`
  keys, derived images from the blocks of `extra-packages.yaml` without a
  key, image pins from `images/*/`. The five Tempest pins, the constraint
  merge, the base-pin rule and the PEP 503 names copy
  `collect_tempest_inputs`, `merge_constraints`, `add_base_pins` and
  `normalize` of `hack/nix-lock.sh`, whose comments name the script: change
  both.
- [[check-renovate-coverage]] says which Renovate manager moves a pin;
  [[check-release-wiring]] audits the release directories this skill reads.
