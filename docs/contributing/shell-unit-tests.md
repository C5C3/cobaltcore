---
title: Shell Unit Tests
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Shell Unit Tests

The shell unit tests under `tests/unit/` check the scripts, workflows and manifests of
the repository without a cluster. [bats-core](https://bats-core.readthedocs.io/en/stable/writing-tests.html)
runs them, `make test-shell` starts the run, and the CI job
[`test-shell`](../reference/ci-cd/ci-workflow.md#test-shell) runs it on every event.
This page describes the native form of a test: a bats file whose tests each run in a
process of their own and whose checks keep a description.

## Where a test lives

A test of `hack/foo.sh` or of a workflow sits in the directory of its area,
`tests/unit/<dir>/<name>.bats`, with `<dir>` one of `ci`, `deploy`, `docs`, `hack`,
`images`, `lab`, `load`, `renovate`, or `lib` for the tests of the helpers themselves.
`make test-shell` finds every `*.bats` under `tests/unit/` by itself, so a new file
needs no list. Shared code goes under `tests/lib/`; a `.sh` file under `tests/unit/`
fails the run unless its name ends in `_test.sh`.

`tests/unit/ci/` is the reference: its 70 tests were the first to move to the native
form. The other directories still hold scripts of the old form, `<name>_test.sh` files
that count their checks themselves. They keep that form until their own port, and the
run executes both kinds side by side.

## A complete example

The file below tests a small wait loop. It runs as it is when saved as
`tests/unit/lib/example.bats`:

```bash
#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify a wait loop: it polls a Deployment until kubectl reports it ready,
# gives up after three polls, and names the namespace in every call.
#
# Usage: bin/bats tests/unit/lib/example.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

setup() {
  SCRIPT="$BATS_TEST_TMPDIR/wait-ready.sh"
  cat >"$SCRIPT" <<'EOF'
for i in 1 2 3; do
  ready="$(kubectl get deployment api -n "$1" -o jsonpath='{.status.readyReplicas}')"
  [ "$ready" = 1 ] && { echo "ready after $i polls"; exit 0; }
  sleep 1
done
echo "not ready" >&2
exit 1
EOF
  stub_on sleep '*'
  stub_on kubectl '*' --out 1
}

@test "the loop reports a ready Deployment on the first poll" {
  run with_stubs bash "$SCRIPT" demo
  assert_eq "the loop exits 0" 0 "$status"
  assert_eq "the loop says when it saw the Deployment ready" "ready after 1 polls" "$output"
}

@test "the loop polls again while the Deployment is not ready" {
  stub_on kubectl 'get deployment api *' --out '' --times 2
  run with_stubs bash "$SCRIPT" demo
  assert_eq "the loop exits 0" 0 "$status"
  assert_contains "the loop saw the Deployment ready on the third poll" "$output" "after 3 polls"
}

@test "the loop gives up after three polls" {
  stub_on kubectl 'get deployment api *' --out 0
  run --separate-stderr with_stubs bash "$SCRIPT" demo
  assert_nonzero_exit "the loop fails" "$status"
  assert_eq "the loop says why on stderr" "not ready" "$stderr"
}

@test "every kubectl call names the namespace" {
  local call
  run with_stubs bash "$SCRIPT" demo
  while IFS= read -r call; do
    soft assert_contains "the call '$call' names the namespace" "$call" "-n demo"
  done <<EOF
$(stub_calls kubectl)
EOF
}
```

The file starts with the bats shebang, the SPDX pair, a header comment that says what
it verifies and how to run it, and the `load` lines. `common` always comes first;
`stubs` follows when the file fakes a command. Code at file level is limited to
`load`, `source`, constants, function definitions and `bats_test_function` loops:
bats evaluates it once to count the tests and once per test, so it must not create a
directory, set a trap or keep a counter.

Each `@test` is named by what it checks. The name shows on the `ok` or `not ok` line,
in the JUnit report, and is what `TEST_SHELL_NAME` selects. A test whose body is a loop
over values is registered once per value:

```bash
check_service() {
  assert_file_contains_fixed "$1 has a matrix entry" "$CI_YAML" "service: $1"
}
for service in nova cinder; do
  bats_test_function --description "the $service leg has a matrix entry" -- check_service "$service"
done
```

## Assertions

`tests/lib/bats/common.bash` loads [bats-support](https://github.com/bats-core/bats-support/tree/v0.3.0),
[bats-assert](https://github.com/bats-core/bats-assert/tree/v2.2.4) and
[bats-file](https://github.com/bats-core/bats-file/tree/v0.4.0), and defines ten
assertions that take a description as their first argument. A passed assertion prints
nothing. A failed one prints its description and the values it compared, each cut after
40 lines, and ends the test.

| Assertion | Arguments | Passes when |
| --- | --- | --- |
| `assert_eq` | description, expected, actual | the strings are equal |
| `assert_not_empty` | description, value | the value is not empty |
| `assert_contains` | description, haystack, needle | the haystack holds the needle as a fixed string |
| `assert_not_contains` | description, haystack, needle | the haystack does not hold the needle |
| `assert_starts_with` | description, actual, prefix | the value starts with the prefix |
| `assert_nonzero_exit` | description, status | the status is not 0 |
| `assert_gte` | description, actual, minimum | actual is an integer of at least the minimum |
| `assert_file_contains_fixed` | description, file, string | `grep -qF --` finds the string |
| `assert_file_matches` | description, file, expression | `grep -q` finds the basic regular expression |
| `assert_file_not_matches` | description, file, expression | `grep -q` does not find it |

The three file assertions fail on a file that does not exist. bats-file defines
`assert_file_contains` and `assert_file_not_contains` with the arguments file and
expression, which is why the two regular-expression assertions here carry other names.
The assertions of bats-assert, such as `assert_output` and `assert_line`, fit checks on
the result of `run`.

A check that none of the assertions covers keeps its condition and reports both
outcomes:

```bash
if grep -qE '^    if:' <<<"$block"; then
  grep -E '^    if:' <<<"$block"
  fail "test-shell carries no job-level if:"
else
  check_passed "test-shell carries no job-level if:"
fi
```

`check_passed` records the passed check in the ledger that the parity tool reads (see
[Porting an old script](#porting-an-old-script)). Every assertion of `common.bash`
records its own.

## A command that fails on purpose

A test runs with errexit, so a command whose failure is the point runs through `run`.
`run` keeps the status in `$status` and the output in `$output`; with
`run --separate-stderr`, stderr goes to `$stderr`:

```bash
run --separate-stderr bash "$PROJECT_ROOT/hack/test-shell-parity.sh" no-such-ref
assert_eq "the tool exits 1" 1 "$status"
assert_eq "the tool names the ref" "test-shell-parity: no-such-ref is not a commit" "$stderr"
```

Errexit does not apply inside `$( )`, in the condition of an `if`, or before `||`. A
command there may fail without ending the test, so a test that needs its status takes
it with `run`. bats strips the leading and trailing blanks of `$stderr`, so a check on
the exact text of stderr reads it from a file the command wrote it to.

## Checks over a list

A failed assertion ends its test, so a loop would report only its first bad item.
`soft` in front of a check records a failure and goes on; the default `teardown` then
fails the test with every recorded failure:

```bash
for file in "$PROJECT_ROOT"/.github/workflows/*.yaml; do
  soft assert_file_not_matches "$file pins no action by tag" "$file" 'uses: .*@v[0-9]'
done
```

`soft` runs its command in a subshell, so a check under it cannot set a variable of the
test. A file that defines a `teardown` of its own calls `common_teardown` in it.

## Faking a command

`tests/lib/bats/stubs.bash` fakes a command by rules. `stub_on <command> <pattern>`
adds a rule, where the pattern is a shell pattern matched against the arguments of a
call joined by single spaces. A call takes the newest matching rule, so the defaults of
`setup` lose against a rule a test declares, and a call that no rule matches exits 99
with `stub <command>: no rule matches: <arguments>` on stderr.

| Option | Answer |
| --- | --- |
| `--out <text>` | prints the text and a newline, nothing for an empty text |
| `--out-raw <text>` | prints the text as it is |
| `--out-file <file>` | prints a copy of the file, taken when the rule is declared |
| `--err <text>` | prints the text and a newline to stderr |
| `--rc <n>` | exits with status n, 0 by default |
| `--times <n>` | answers n calls, then the older rules answer again |
| `--exec <body>` | sources the body with the arguments of the call as `"$@"`; no other answer option goes with it |

A sequence is a lasting rule followed by a `--times` rule. Here the first read of the
events answers with an empty list and every later one with an event:

```bash
stub_on kubectl '*'
stub_on kubectl 'get events *' --out-file "$BATS_TEST_TMPDIR/one-event.json"
stub_on kubectl 'get events *' --out '{"items":[]}' --times 1
run with_stubs bash "$SCRIPT"
assert_eq "the script read the events twice" 2 "$(stub_calls kubectl | grep -c '^get events')"
```

The fakes live in `$STUB_DIR/bin`, with `STUB_DIR` set to `$BATS_TEST_TMPDIR/stubs`.
`with_stubs <command>` runs a command with that directory first on `PATH`, and leaves
the `PATH` of the test as it was. `stub_calls <command>` prints one line per call. An
`--exec` body keeps what it needs between calls under `$STUB_DIR/state`. A fake whose
logic fits no rule keeps a body of its own:

```bash
stub_script docker <<'BODY'
case "$1" in
  image) echo '[{"Id":"sha256:0"}]' ;;
  *) exit 1 ;;
esac
BODY
```

## Fixtures

A fixture that a test changes is built in `setup` under `$BATS_TEST_TMPDIR`, which bats
creates for each test and removes after it. A fixture that is slow to build and that no
test changes is built once in `setup_file` under `$BATS_FILE_TMPDIR`, with its variables
exported so that every test sees them. A test writes nowhere else and takes nothing from
a test before it.

`make test-shell` runs the tests of one file side by side. A file whose tests cannot
share a run sets `BATS_NO_PARALLELIZE_WITHIN_FILE=true` in `setup_file` and says why in
a comment.

## Skipping with a reason

A test that needs a tool the machine lacks skips and says why:

```bash
setup() {
  require_tool shellcheck jq
}
```

`require_tool` skips for the first tool that `command -v` does not find, and
`require_mikefarah_yq` skips unless `yq` is the mikefarah one. `skip "<reason>"` covers
any other case. bats prints the reason on the `ok` line, and a skip does not fail the
run.

## Running one file and one test

| Command | Runs |
| --- | --- |
| `bin/bats tests/unit/ci/test_shell_event_guard.bats` | one file, without the runner; `make install-bats` puts `bin/bats` in place |
| `make test-shell TEST_SHELL_FILTER=ci/test_shell_event_guard` | the files and old scripts whose path matches the extended regular expression |
| `make test-shell TEST_SHELL_NAME='on.push still covers'` | the native tests whose name matches; old scripts have no test names and do not run |
| `bin/bats --filter 'covers' tests/unit/ci/test_shell_event_guard.bats` | the matching tests of one file |

The variables of `make test-shell` and its JUnit report are described with the
[`test-shell`](../reference/ci-cd/ci-workflow.md#test-shell) job.

## Bash 3.2

A test runs under the Bash that macOS ships, 3.2, as do the helpers. That rules out
`mapfile` and `readarray`, `declare -A`, the case conversions `${x,,}` and `${x^^}`,
`|&`, `&>>` and `coproc`. A list goes through a `while IFS= read -r` loop over a
here-document instead of `mapfile`. `tests/unit/lib/native_form.bats` fails on each of
these constructs, and a quick check of one file runs in a container:

```bash
docker run --rm -v "$PWD:$PWD" -w "$PWD" bash:3.2 bash -n tests/lib/bats/common.bash
```

## Porting an old script

A port moves `tests/unit/<dir>/<name>_test.sh` to `tests/unit/<dir>/<name>.bats` with
`git mv`, so its history follows, and changes every reference to the old name in the
same change. It keeps every check, with its helper, its description and its arguments:

1. The header comment stays, with the `Usage:` line reading
   `bin/bats tests/unit/<dir>/<name>.bats`. The `PASS`, `FAIL` and `SKIP` counters, the
   source line of the old assertion file and the closing `Results:` block go. A shared
   library such as `tests/lib/ci_yaml.sh` is sourced as
   `source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"`.
2. Each `test_*` function becomes one `@test`, in the order of the calls at the end of
   the script. Its name is the text of its leading `echo "Test: ..."`, else the
   function name without `test_` and with spaces for underscores. A name holds no
   variable: a constant is written out and a `$` is escaped. An `echo "Test: ..."` in a
   loop or in a helper stays an `echo`. When git would no longer see the port as a
   rename with `@test` blocks (below 50 percent similarity), the functions stay and
   each is registered with `bats_test_function --description "<name>" -- <function>`,
   in the same order, with a comment that says why.
3. A function called with arguments from a loop is registered once per value with
   `bats_test_function`.
4. `mktemp -d` at file level and its `trap ... EXIT` go; the fixture moves into `setup`
   or `setup_file`, and what an earlier function left behind moves into `setup`.
5. `assert_file_contains` becomes `assert_file_matches`, and `assert_file_not_contains`
   becomes `assert_file_not_matches`. A check written by hand keeps its condition: the
   failing branch prints its details and calls `fail "<description>"`, the passing
   branch calls `check_passed` with the description of the `PASS:` line. An assertion
   function the script defines for itself is converted the same way.
6. `out="$(cmd 2>&1)"; rc=$?` becomes `run cmd`, and a helper that set `RUN_OUT` and
   `RUN_RC` calls `run`. A check in a loop that names one item per round gets `soft`.
7. A skip for a missing tool becomes `require_tool` or `skip "<reason>"` at the same
   place, in `setup` when the whole script skipped. A `SKIP` that marks an item a check
   does not apply to becomes a `continue`.
8. A command whose failure the old script ignored keeps that through an explicit
   `|| true`, and the pull request lists each such site. A port changes no script under
   test.
9. A kubectl fake becomes rules: each `case` branch a `stub_on kubectl '<pattern>'`, the
   defaults of a reset function rules in `setup`, a first answer that differs from the
   later ones a `--times` rule, an answer that depends on the arguments or on an earlier
   call an `--exec` body, the closing `exit 0` a `stub_on kubectl '*'` declared first.
   Other fakes keep their body through `stub_script`, and a fake that only exits 0
   becomes `stub_on <command> '*'`.

`hack/test-shell-parity.sh` shows that the port kept every check. It runs the old
scripts in a worktree of the base commit, runs the native files with the check ledger
on, and compares the descriptions per file:

```bash
hack/test-shell-parity.sh origin/main tests/unit/ci
```

It prints `ok <file>: <n> checks` per equal file, and `MISMATCH` with a `- ` line per
check only the old script has and a `+ ` line per check only the native file has. The
pull request of a port quotes its last line, such as
`test-shell-parity: 70 files, 8357 checks, 0 mismatched`, and each of its `note:` lines.
