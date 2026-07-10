---
title: Nix Development Environment
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Nix Development Environment

The repository ships a Nix flake so that entering the development environment
yields the same toolchain a CI run gets, pinned to the versions the pipeline
pins. It is a second, equally complete entry point next to
`make install-test-deps` — the dependency-installation script and the CI setup
steps are unchanged, and no pipeline behavior depends on the flake.

```bash
nix develop
```

The one command drops you into a shell where `controller-gen`, `gofumpt`,
`golangci-lint`, `chainsaw`, `kind`, `kubectl`, `flux`, `kustomize`, the
`helm-unittest` plugin, and the envtest assets are all on `PATH` (via a
gitignored `bin/`) at the versions CI uses.

## What you get

- **Base runtimes** — Go, Node, Python 3, Helm, `shellcheck`, `yq`, `jq`, and a
  GNU userland — come from the `nixpkgs` revision pinned in `flake.lock`. The
  GNU userland is deliberate: it makes macOS behave like the Linux-only CI
  runner (for example, `make chainsaw-lint` relies on GNU `xargs`).
- **Exact CI-pinned tools** are installed by `hack/nix-devshell-hook.sh`, which
  the flake sources on entry. Rather than duplicate any version into the flake,
  the hook reads each pin **where it already lives**, so a Renovate bump to a
  canonical file self-heals the shell on the next entry.

Print the pins the hook resolves without installing anything:

```bash
bash hack/nix-devshell-hook.sh --print-pins
```

## Where each version comes from

The hook installs these at the **exact** version CI uses, reading the pin from
its authoritative location:

| Tool | Pin source |
|------|------------|
| `controller-gen` | `.github/workflows/ci.yaml` env `CONTROLLER_GEN_VERSION` |
| `gofumpt` | `.github/workflows/ci.yaml` env `GOFUMPT_VERSION` (mirrored in the `Makefile`) |
| `golangci-lint` | `.github/workflows/ci.yaml` env `GOLANGCI_LINT_VERSION` |
| `setup-envtest` | `.github/workflows/ci.yaml` (`setup-envtest@<ref>`) |
| envtest assets | `Makefile` `ENVTEST_K8S_VERSION` (the pinned Kubernetes minor) |
| `kustomize` | `.github/workflows/ci.yaml` (`KUSTOMIZE_VERSION=`) |
| `helm-unittest` | `.github/workflows/ci.yaml` (`helm plugin install … --version`) |
| `chainsaw`, `kind`, `kubectl`, `flux` | `hack/install-test-deps.sh` (reused as-is, with its SHA256 verification) |

These come from `nixpkgs` and track it, because the pipeline itself does **not**
pin them to an exact patch:

| Tool | Note |
|------|------|
| Go | `go_1_26` from nixpkgs; `go.work` sets only the minimum the modules need |
| Node | `nodejs_24` from nixpkgs (CI pins the major only, `node-version: 24`) |
| Python 3 | the runtime the Helm-schema generator and the docs site need |
| Helm | CI uses `azure/setup-helm` with no version input, so it floats |
| `shellcheck`, `jq`, GNU userland | runner-preinstalled and unpinned in `ci.yaml` |
| `yq` | pinned (`v4.53.3`) in `verify-container-images.yaml` and `build-images.yaml`; `ci.yaml` uses the runner's floating `yq`, so the flake tracks the nixpkgs `yq-go` |

## Prerequisites

- **Nix ≥ 2.19** with the `nix-command` and `flakes` experimental features
  enabled. If they are not enabled globally, pass them per invocation:

  ```bash
  nix --extra-experimental-features 'nix-command flakes' develop
  ```

- **Docker** is deliberately **not** provided by the flake — a running daemon
  cannot be nix-provisioned, and `kind` needs one. Install Docker separately
  (see the [Quick Start (Extended)](../quick-start-extended.md) prerequisites).

## First entry and offline behavior

The first `nix develop` performs network installs (`go install` for the Go
tools, tarball downloads for `kustomize`/`chainsaw`/`kind`/`kubectl`/`flux`, the
`helm-unittest` plugin, and the envtest assets). This is impure by design and is
the same kind of work CI does per job. Subsequent entries skip anything already
installed at the pinned version.

If you enter the shell offline, provisioning **degrades loudly** — each tool
that could not be installed is listed in a warning summary and the shell still
opens. Re-run `source hack/nix-devshell-hook.sh` once you are back online.

## Keeping in sync

- **Tool pins** are bumped by Renovate in their canonical files (`ci.yaml`, the
  `Makefile`, `hack/install-test-deps.sh`). The devshell re-reads those files on
  entry, so it self-heals with no flake edit — see
  [Dependency Management](./dependency-management.md).
- **`flake.lock`** (the `nixpkgs` revision) is maintained by Renovate's native
  `nix` manager, which opens a grouped weekly re-lock PR for review. The
  `nixos-unstable` re-lock is not automerged — a human confirms the base-runtime
  bump the devshell inherits.

## Optional: direnv

To enter the shell automatically when you `cd` into the checkout:

```bash
echo 'use flake' > .envrc
direnv allow
```

`.envrc` is untracked and `.direnv/` is gitignored.

## Running the CI checks inside the shell

Because the shell holds the CI tool versions, the same targets CI runs work
locally against the same tools:

```bash
make format-check     # gofumpt at the pinned version
make lint             # golangci-lint at the pinned version
make verify-helm-schema
make chainsaw-lint
make test-integration OPERATOR=keystone   # uses the pinned envtest assets
```

## See also

- [Dependency Management](./dependency-management.md) — how Renovate keeps the
  pinned versions (and `flake.lock`) fresh.
- [Quick Start (Extended)](../quick-start-extended.md) — the
  `make install-test-deps` path and the full prerequisites.
