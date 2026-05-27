---
name: check-go-workspace-deps
description: >-
  Audit the forge Go workspace for dependency-version drift between
  the operators/<op>/go.mod files and internal/common/go.mod —
  controller-runtime, k8s.io/api, k8s.io/apimachinery, k8s.io/client-go,
  the Go directive, and the toolchain pin must stay in lockstep so
  the workspace builds the same versions everywhere. Use when asked to
  check workspace deps, after running `go get` in one module, or after
  Renovate bumps controller-runtime in only one of the modules.
---

# Check Go workspace consistency

This skill verifies that the forge **Go workspace's shared dependencies
stay in lockstep** across all modules. The repo uses a Go workspace
(`go.work` lists `internal/common`, `operators/c5c3`, `operators/keystone`),
which means the *build* picks one version per dep — but each module's
`go.mod` declares its own pin. If those pins drift, `go mod tidy` per
module rewrites them, and the next CI matrix leg sees a different
build than the laptop.

It is repeatable — run it any time, especially after a `go get` in one
module, or after a Renovate PR that touched only one `go.mod`.

## What workspace consistency means here

The repo declares three things that have to agree:

| Layer | Where it lives | Source of truth |
|---|---|---|
| Workspace member set | `go.work` (`use (…)` block) | the directories listed are exactly the modules participating in workspace mode |
| Go directive | `go.work` `go <ver>` and each `operators/<op>/go.mod` / `internal/common/go.mod` `go <ver>` | a single version string (e.g. `1.25.10`) shared across all modules |
| Shared dependency versions | each `go.mod` `require ( … )` block — specifically the k8s.io/* and sigs.k8s.io/controller-runtime entries | identical version per module (controller-runtime, k8s.io/api, k8s.io/apimachinery, k8s.io/client-go, k8s.io/apiextensions-apiserver) |
| Workspace sum file | `go.work.sum` | a *tracked* file per CC-0001 REQ-009 (gitignore intentionally does *not* exclude it) |

The authoritative gate is `go build ./...` from each module root, which
fails if a workspace member references a missing or incompatible
dependency. This skill defers to that as the source of truth and adds
the per-module version-pin diff the build does not surface (a divergent
`go.mod` is still valid Go — the workspace just silently picks one of
the pins to build against).

A drift finding is any place two `go.mod` files pin different versions
of the same dep, the `go.work` `go` directive disagrees with any
member's `go` directive, or the workspace member set diverges from the
directories on disk.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-go-workspace-deps/scripts/audit-go-workspace-deps.sh
```

The script catches the mechanically-checkable gaps and prints an
inventory. Exit code `1` means at least one `[FAIL]`. Interpret:

- **W1** — every directory listed in `go.work`'s `use (…)` block
  exists on disk and contains a `go.mod`. A stale entry breaks
  `go build` from the workspace root.
- **W2** — every directory containing a `go.mod` under `operators/`
  or `internal/` is listed in `go.work`. An unlisted module silently
  builds with its own resolution path — workspace replace directives
  do not apply.
- **W3** — the `go` directive in `go.work` matches the `go` directive
  in every member's `go.mod` (exact string match, e.g. `1.25.10`).
  A delta is a real toolchain hazard: the workspace uses one Go
  version, the per-module CI legs use another.
- **W4** — for each shared dep (controller-runtime, k8s.io/api,
  k8s.io/apimachinery, k8s.io/client-go, k8s.io/apiextensions-apiserver),
  every module that requires it (direct or indirect) pins the same
  version. A divergent pin is the most common workspace drift: one
  module bumped, the others did not.
- **W5** — `go.work.sum` is present (CC-0001 REQ-009).
- The **inventory** lists, per shared dep, the pin in each module
  side-by-side.

### 2. Cross-reference the inventory

The script does not run `go mod tidy`. Using the printed inventory,
confirm:

1. For each `[FAIL]` from W4, decide which version is canonical
   (usually the newer one) and propagate to every module:
   ```bash
   cd operators/<lagging-op>
   go get <module>@<version>
   go mod tidy
   ```
2. For W3 deltas, bump the lagging module's `go` directive (or the
   `go.work` directive) so they match. The Go compiler's behaviour
   under workspace mode does *not* mix toolchain versions per-module
   at build time — only the workspace-root `go.work` toolchain wins.
3. After any change, run `go build ./...` from the repo root to
   confirm the workspace still resolves cleanly.

### 3. Run the authoritative gate

The script does not invoke the Go toolchain. Run the real gates and
report the exact outcomes:

```bash
go build ./...            # workspace-root build
make test                 # per-module tests via the workspace
go mod tidy && git diff   # in each module — any diff is W4 drift
```

`go build ./...` is the authoritative gate for workspace resolution.
`go mod tidy && git diff` per module is the authoritative gate for
per-module pin consistency; an empty diff means the `go.mod` already
reflects what tidy would compute.

### 4. Report

Produce a concise summary grouped by severity:

- **HIGH** — `go build ./...` fails; the `go` directive disagrees
  between `go.work` and any member; a workspace member directory does
  not exist.
- **MEDIUM** — two modules pin different versions of the same shared
  dep; a module under `operators/` or `internal/` is missing from
  the workspace; `go.work.sum` is absent.
- **LOW** — a workspace member that has no `require` entry for a
  shared dep that all siblings need (likely OK, but worth a check —
  the module may legitimately not depend on it).

For each finding give one line with the dep, the per-module versions,
and the suggested fix. End with a per-shared-dep verdict.

## Drift patterns

These recurring shapes are worth grepping for first:

1. **Single-module Renovate bump.** Renovate created a PR bumping
   `sigs.k8s.io/controller-runtime` in only one module's `go.mod`.
   The workspace silently still resolves to the older version (or
   the newer, depending on which module wins the workspace minimum-version
   selection). CI passes; the build's actual dependency does not
   match what humans think they pinned.
2. **`go get` ran in one module only.** A developer ran `go get …`
   in `operators/keystone` to add a feature, but the same package is
   needed in `internal/common` for tests. `go mod tidy` in
   `internal/common` would add it; without the tidy, the indirect
   resolution differs.
3. **Toolchain drift.** A `go.work` edit bumped the `go` directive
   to `1.26.x` but one of the `operators/<op>/go.mod` still says
   `1.25.x`. `go build` from the workspace root uses `1.26.x`;
   running tests from inside the module without the workspace uses
   `1.25.x`.
4. **Workspace member missing.** A new `operators/<new>/` was added
   with its own `go.mod` but the developer forgot to add it to
   `go.work`'s `use (…)` block. The new module compiles in isolation
   but the workspace ignores it.
5. **Stale workspace member.** An old `operators/<dead>/` was deleted
   but `go.work` still references it. `go build ./...` fails with a
   missing-directory error.

## Notes

- This skill is read-only; the deterministic script edits nothing.
  Apply fixes (`go get`, `go mod tidy`, edit `go.work`) as a separate,
  explicitly-scoped task.
- The shared-dep list at the top of the script is opinionated —
  controller-runtime + the k8s.io/* family. If forge later adds
  another cross-cutting dep (e.g. a metrics library, a tracing
  client), extend that list so the W4 check covers it.
- `go.work.sum` is intentionally tracked per CC-0001 REQ-009. Do not
  add it to `.gitignore`.
- Pair this with [[check-renovate-coverage]] — that skill ensures
  Renovate has a manager for these deps in the first place; this
  skill ensures Renovate's bumps land consistently.
