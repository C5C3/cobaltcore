---
name: check-go-workspace-deps
description: >-
  Audit the CobaltCore Go workspace for dependency-version drift between
  the operators/<op>/go.mod files and internal/common/go.mod —
  controller-runtime, multicluster-runtime, the k8s.io family, every other
  direct requirement two modules share, the Go directive, and any toolchain
  directive must stay in lockstep so the workspace builds the same versions
  everywhere. Use when asked to check workspace deps, after running
  `go get` in one module, after Renovate bumps a dependency in only some of
  the modules, or when PR CI shows sum.golang.org errors that do not
  reproduce on the branch.
---

# Check Go workspace consistency

This skill verifies that the CobaltCore **Go workspace's shared dependencies
stay in lockstep** across all modules. `go.work` lists `internal/common`
and every operator module under `operators/` (eleven members at the time
of writing; the audit reads the list, it does not assume it). In
workspace mode the *build* picks one version per dependency by minimum
version selection, but each module's `go.mod` declares its own pin. If
those pins drift, `go mod tidy` per module rewrites them, and a CI leg
that builds one module sees a different graph than the laptop that built
the workspace.

It is repeatable — run it any time, especially after a `go get` in one
module, or after a Renovate PR that touched only some `go.mod` files.

## What workspace consistency means here

| Layer | Where it lives | Source of truth |
|---|---|---|
| Workspace member set | `go.work` (`use (…)` block) | the directories listed are exactly the modules participating in workspace mode |
| Go directive | `go.work` `go <ver>` and every member `go.mod` | one version string shared by all modules (`setup-go` in CI reads `go-version-file: go.work`) |
| Toolchain directive | `toolchain <ver>` in `go.work` / any `go.mod` | none is declared today; if one appears, it must be identical everywhere |
| Shared dependency versions | each `go.mod` `require` block | identical version per module for the fixed k8s/controller-runtime/openbao list (direct or `// indirect`) and for every module two or more members require directly |
| Workspace sum file | `go.work.sum` | untracked on purpose (`.gitignore`): the go command appends to it per command that runs, and Renovate does not write it |

The authoritative gates are `make verify-go-tidy` (every member's
`go.mod`/`go.sum` equals what `go mod tidy -diff` would write; the first
step of the CI `verify-codegen` job) and a workspace build. This skill
adds the cross-module pin diff neither expresses: a divergent `go.mod` is
still valid Go and still tidy — the workspace silently picks the higher
pin.

A drift finding is any place two `go.mod` files pin different versions
of the same shared dependency, a `go`/`toolchain` directive disagrees,
or the workspace member set diverges from the modules on disk.

## Procedure

Work through these steps in order and report findings at the end.

### 1. Run the deterministic audit

```bash
bash .claude/skills/check-go-workspace-deps/scripts/audit-go-workspace-deps.sh
```

Exit code `1` means at least one `[FAIL]`. Interpret:

- **W1** — every directory in `go.work`'s `use (…)` block exists and
  contains a `go.mod`. A stale entry breaks every workspace build.
- **W2** — every `go.mod` under `operators/` or `internal/` is listed in
  `go.work`. An unlisted module builds with its own resolution and is
  invisible to workspace-wide `go test`/`golangci-lint` runs.
- **W3** — the `go` directive in `go.work` matches every member's
  `go.mod` exactly.
- **W3b** — a `toolchain` directive, where any file declares one, is the
  same everywhere. With none declared the check passes.
- **W4** — (a) each dependency in the script's `SHARED_DEPS` list
  (controller-runtime, multicluster-runtime, `k8s.io/api`,
  `apimachinery`, `client-go`, `apiextensions-apiserver`,
  `github.com/dc-tec/openbao-operator`) carries one version across
  every module that requires it, direct or `// indirect`; (b) every
  other module that two or more members require **directly** (gateway-api,
  mariadb-operator, external-secrets, prometheus, gomega, …) carries one
  version too. Indirect pins outside the fixed list are ignored on
  purpose: `go mod tidy` computes them per module graph and they differ
  legitimately.
- **W5** — `go.work.sum` is not tracked by git and `.gitignore` covers it.
- The **inventory** table lists the fixed shared deps side by side per
  module.

### 2. Cross-reference the inventory

The script does not run the Go toolchain. For each finding:

1. For a W4 divergence, decide which version is canonical (usually the
   newer one, the one `main` already carries) and propagate it:
   ```bash
   cd operators/<lagging-op>
   go get <module>@<version>
   go mod tidy
   ```
   Indirect k8s pins such as `k8s.io/apiextensions-apiserver` flow from
   `internal/common`'s version: `go get` them explicitly, a plain tidy
   keeps the old indirect version.
2. For W3/W3b deltas, bump the lagging directive so all files match.
3. After any change, run the gates in step 3.

### 3. Run the authoritative gates

```bash
make verify-go-tidy       # every member is tidy (CI: verify-codegen, first step)
go build ./...            # workspace build from the repo root
make test                 # per-module unit tests through the workspace
```

`make tidy` rewrites every module when `verify-go-tidy` fails.
`controller-gen` and `setup-envtest` live in `$(go env GOPATH)/bin`;
export it onto `PATH` before codegen targets.

### 4. Report

Group findings by severity:

- **HIGH** — a workspace build fails; the `go` directive disagrees
  between `go.work` and a member; a workspace member directory does not
  exist; `make verify-go-tidy` fails.
- **MEDIUM** — two modules pin different versions of the same shared
  dependency; a module on disk is missing from `go.work`; `go.work.sum`
  is tracked or not ignored.
- **LOW** — a member without a `require` entry for a dependency all its
  siblings need (usually fine; the module may not import it).

One line per finding with the dependency, the per-module versions, and
the suggested fix. End with a per-dependency verdict.

## Drift patterns

1. **Partial Renovate bump.** Renovate bumped a dependency in some
   modules only (a new module added on a branch after the grouping rule
   ran is the usual cause). The workspace resolves to the higher pin; the
   lagging module's own CI leg builds the lower one. A bump in
   `internal/common` that raises an indirect pin of the operators is not
   this pattern: Renovate's `gomodTidyAll` tidies every module that
   replaces `internal/common` in the same PR.
2. **PR CI tests the merge with `main`.** `pull_request` CI checks out
   the merge of the head with current `main`. A branch that keeps (or
   adds) a module at a version `main` has since bumped yields a mixed
   workspace: `test (<op>)` legs fail `[setup failed]` with
   `verifying go.mod: reading https://sum.golang.org/…`. Nothing
   reproduces on the branch. Reproduce in a worktree merged with
   `origin/main`, bump the lagging module, and run `go mod tidy` **in the
   merged tree**. A module that is new on the branch is never covered by
   a tidy commit on `main`, so rebasing alone does not fix it; its tidy
   has to land on the branch.
3. **`go get` in one module only.** A dependency was added to one
   operator while `internal/common` (or a sibling) needs the same package
   for tests; without a tidy there the indirect resolution differs.
4. **Directive drift.** A `go.work` edit bumped the `go` directive but a
   member `go.mod` still names the old version; a module-local build then
   uses a different toolchain than the workspace.
5. **Workspace member missing or stale.** A new `operators/<new>/` was
   added without a `use` entry (W2), or a deleted module is still listed
   (W1). A new operator also needs the `operators/Dockerfile` COPY lines
   and the Makefile `OPERATORS` default; [[check-service-parity]] P2
   checks those.

## Notes

- This skill is read-only; the script edits nothing. Apply fixes
  (`go get`, `go mod tidy`, `go.work` edits) as a separate, explicitly
  scoped task.
- To align another cross-cutting dependency even where modules only
  require it indirectly, add it to `SHARED_DEPS` at the top of the
  script. Direct requirements shared by two modules need no entry.
- `go.work.sum` stays untracked; do not commit it. Tracked, it failed
  `verify-codegen` on every Go module PR Renovate opened.
- Pair this with [[check-renovate-coverage]] — that skill ensures
  Renovate has a manager for these deps; this skill ensures Renovate's
  bumps land in every module.
