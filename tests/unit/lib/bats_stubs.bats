#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify tests/lib/bats/stubs.bash, the rule-driven command fakes: a rule
# answers with stdout, stderr and a status, the newest matching rule wins, a
# --times rule falls back to the older one once used up, --out '' prints
# nothing and --out-raw no newline, --out-file prints the file as it was at
# declaration, --exec sees the arguments and $STUB_DIR/state, an unmatched
# call exits 99 with its message, stub_calls lists every call and nothing
# for an unknown command, stub_script runs its body and logs the call, a
# fake answers with the environment cleared, with_stubs leaves the PATH of
# the test alone, and every usage error has its message.
#
# Usage: bin/bats tests/unit/lib/bats_stubs.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

@test "a rule answers with stdout, stderr and its status" {
  stub_on kubectl 'get pods *' --out 'pod-a' --err 'warning: old' --rc 3

  run --separate-stderr with_stubs kubectl get pods -n chaos
  assert_failure 3
  assert_output 'pod-a'
  assert_equal "$stderr" 'warning: old'
}

@test "the newest matching rule wins and an older one answers what it does not match" {
  stub_on kubectl '*' --out 'default'
  stub_on kubectl 'get pods *' --out 'pods'

  run with_stubs kubectl get pods -n x
  assert_output 'pods'
  run with_stubs kubectl get nodes
  assert_output 'default'
}

@test "a --times rule answers its calls, then the older rule answers" {
  stub_on kubectl 'get events *' --out 'later'
  stub_on kubectl 'get events *' --out 'first' --times 2

  run with_stubs kubectl get events -A
  assert_output 'first'
  run with_stubs kubectl get events -A
  assert_output 'first'
  run with_stubs kubectl get events -A
  assert_output 'later'
}

@test "--out '' prints nothing and --out-raw prints no newline" {
  stub_on kubectl 'empty' --out ''
  stub_on kubectl 'raw' --out-raw 'no newline'
  stub_on kubectl 'lines' --out "$(printf 'one\ntwo')"

  with_stubs kubectl empty >"$BATS_TEST_TMPDIR/empty"
  assert_size_zero "$BATS_TEST_TMPDIR/empty"
  with_stubs kubectl raw >"$BATS_TEST_TMPDIR/raw"
  assert_equal "$(od -An -c "$BATS_TEST_TMPDIR/raw" | tr -s ' ')" ' n o n e w l i n e'
  run with_stubs kubectl lines
  assert_output "$(printf 'one\ntwo')"
}

@test "--out-file prints the file as it was at declaration" {
  printf 'first\n\n' >"$BATS_TEST_TMPDIR/answer"
  stub_on kubectl '*' --out-file "$BATS_TEST_TMPDIR/answer"
  echo changed >"$BATS_TEST_TMPDIR/answer"

  with_stubs kubectl get >"$BATS_TEST_TMPDIR/out"
  assert_equal "$(od -An -c "$BATS_TEST_TMPDIR/out" | tr -s ' ')" ' f i r s t \n \n'
}

@test "--exec sees the arguments and keeps state under STUB_DIR/state" {
  # shellcheck disable=SC2016
  stub_on kubectl 'get *' --exec '
    n=0
    [ ! -f "$STUB_DIR/state/n" ] || read -r n <"$STUB_DIR/state/n"
    n=$((n + 1))
    echo "$n" >"$STUB_DIR/state/n"
    echo "call $n: $2"
    [ "$n" -lt 2 ] || exit 7'

  run with_stubs kubectl get pods
  assert_success
  assert_output 'call 1: pods'
  run with_stubs kubectl get nodes
  assert_failure 7
  assert_output 'call 2: nodes'
}

@test "an unmatched call exits 99 with its message" {
  stub_on kubectl 'get pods'

  run --separate-stderr with_stubs kubectl delete pod x
  assert_failure 99
  assert_output ''
  assert_equal "$stderr" 'stub kubectl: no rule matches: delete pod x'
}

@test "stub_calls lists every call and nothing for an unknown command" {
  stub_on kubectl '*'
  with_stubs kubectl get pods -n "a b"
  with_stubs kubectl delete pod x

  run stub_calls kubectl
  assert_success
  assert_output "$(printf 'get pods -n a b\ndelete pod x')"
  run stub_calls helm
  assert_success
  assert_output ''
}

@test "stub_script runs its body with the arguments and logs the call" {
  stub_script docker <<'BODY'
echo "docker saw $# arguments: $*"
exit 4
BODY

  run with_stubs docker build -t x .
  assert_failure 4
  assert_output 'docker saw 4 arguments: build -t x .'
  run stub_calls docker
  assert_output 'build -t x .'
}

@test "a fake answers when the script under test clears the environment" {
  stub_on kubectl '*' --out 'answered'
  # shellcheck disable=SC2016
  stub_on kubectl 'state' --exec 'echo "$STUB_DIR"'

  run env -i "$STUB_DIR/bin/kubectl" version
  assert_success
  assert_output 'answered'
  run env -i "$STUB_DIR/bin/kubectl" state
  assert_output "$STUB_DIR"
  assert_equal "$(stub_calls kubectl)" "$(printf 'version\nstate')"
}

@test "with_stubs puts the fakes first and leaves the PATH of the test alone" {
  local before="$PATH"
  stub_on git '*' --out 'fake git'

  # Outside run, so a with_stubs that changed PATH for good would show.
  assert_equal "$(with_stubs git status)" 'fake git'
  with_stubs git status >/dev/null
  assert_equal "$PATH" "$before"
  run git --version
  assert_output --partial 'git version'
}

@test "stub_on reports each usage error with its message and status 1" {
  run stub_on kubectl
  assert_failure 1
  assert_output 'stub_on: usage: stub_on <command> <pattern> [options]'

  run stub_on kubectl '*' --out
  assert_failure 1
  assert_output 'stub_on: usage: stub_on <command> <pattern> [options]'

  run stub_on kubectl '*' --verbose yes
  assert_failure 1
  assert_output 'stub_on: unknown option --verbose'

  run stub_on '' '*'
  assert_failure 1
  assert_output 'stub_on:  is not a command name'

  run stub_on bin/kubectl '*'
  assert_failure 1
  assert_output 'stub_on: bin/kubectl is not a command name'

  local rc
  for rc in 256 -1 x ''; do
    run stub_on kubectl '*' --rc "$rc"
    assert_failure 1
    assert_output 'stub_on: --rc needs an integer from 0 to 255'
  done

  local times
  for times in 0 -2 x ''; do
    run stub_on kubectl '*' --times "$times"
    assert_failure 1
    assert_output 'stub_on: --times needs a positive integer'
  done

  run stub_on kubectl '*' --exec 'true' --out x
  assert_failure 1
  assert_output 'stub_on: --exec takes no other answer option'

  local option
  for option in --out-raw --err --rc; do
    run stub_on kubectl '*' "$option" 1 --exec 'true'
    assert_failure 1
    assert_output 'stub_on: --exec takes no other answer option'
  done
  : >"$BATS_TEST_TMPDIR/answer"
  run stub_on kubectl '*' --exec 'true' --out-file "$BATS_TEST_TMPDIR/answer"
  assert_failure 1
  assert_output 'stub_on: --exec takes no other answer option'

  run stub_on kubectl '*' --out-file "$BATS_TEST_TMPDIR/missing"
  assert_failure 1
  assert_output "stub_on: $BATS_TEST_TMPDIR/missing does not exist"

  # None of the refused rules left a fake behind.
  assert_file_not_exists "$BATS_TEST_TMPDIR/stubs/bin/kubectl"
}

@test "--rc takes the whole range from 0 to 255" {
  stub_on kubectl 'zero' --rc 0
  stub_on kubectl 'top' --rc 255

  run with_stubs kubectl zero
  assert_success
  run with_stubs kubectl top
  assert_failure 255
}

@test "stub_script reports a missing name and an empty body" {
  run stub_script </dev/null
  assert_failure 1
  assert_output 'stub_script: usage: stub_script <command>, body on stdin'

  run stub_script docker </dev/null
  assert_failure 1
  assert_output 'stub_script: usage: stub_script <command>, body on stdin'

  run stub_script a/b <<<'true'
  assert_failure 1
  assert_output 'stub_script: a/b is not a command name'
}
