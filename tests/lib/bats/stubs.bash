# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash

# Command fakes for native bats files, loaded after common.bash:
#
#   load '../../lib/bats/common'
#   load '../../lib/bats/stubs'
#
# A test declares what a faked command answers to which arguments:
#
#   stub_on kubectl '*'                                # every call exits 0
#   stub_on kubectl 'get pods *' --out 'pod-a'         # this call prints pod-a
#   stub_on kubectl 'get pods *' --out '' --times 1    # but the first prints nothing
#   run with_stubs bash script-under-test.sh
#   assert_contains "it listed the pods" "$(stub_calls kubectl)" "get pods -n x"
#
# A call appends its arguments, joined by single spaces, to the call log of
# its command, then takes the newest rule whose shell pattern matches them
# and whose --times is not used up. Without such a rule it prints
# "stub <command>: no rule matches: <arguments>" to stderr and exits 99.
# docs/contributing/shell-unit-tests.md describes every option.
#
# STUB_DIR is $BATS_TEST_TMPDIR/stubs, created on first use and exported. It
# holds bin/ with the fakes, state/ for what an --exec body keeps between
# calls, rules/<command>/<n>/ with one directory per rule, and calls/ with
# one call log per command.
#
# A fake uses shell builtins only, because a test may fake sed, sleep or git
# too, and it holds the absolute path of its STUB_DIR, so it also answers
# when the script under test clears the environment.
#
# Runs under Bash 3.2, like every native test file.

# _stubs_init
# Sets and exports STUB_DIR and creates its directories.
_stubs_init() {
  STUB_DIR="${BATS_TEST_TMPDIR:?stubs.bash works inside a test only}/stubs"
  export STUB_DIR
  [ -d "$STUB_DIR/bin" ] ||
    mkdir -p "$STUB_DIR/bin" "$STUB_DIR/state" "$STUB_DIR/rules" "$STUB_DIR/calls"
}

# _stubs_name_ok <caller> <command>
# Fails with a message unless <command> is a name a fake can have.
_stubs_name_ok() {
  case "$2" in
    '' | */*)
      echo "$1: $2 is not a command name" >&2
      return 1
      ;;
  esac
}

# _stubs_header <command>
# Prints the start of a fake: the shebang, STUB_DIR, and the line that logs
# the call.
_stubs_header() {
  printf '#!/bin/bash\n'
  printf 'STUB_DIR=%q\n' "$STUB_DIR"
  printf 'export STUB_DIR\n'
  printf '_stub_ifs="$IFS"; IFS=" "; _stub_args="$*"; IFS="$_stub_ifs"\n'
  printf 'printf "%%s\\n" "$_stub_args" >>"$STUB_DIR/calls/%s"\n' "$1"
}

# _stubs_write_dispatcher <command>
# Writes bin/<command>, the fake that answers from the rules of <command>.
_stubs_write_dispatcher() {
  {
    _stubs_header "$1"
    printf '_stub_rules="$STUB_DIR/rules/%s"\n' "$1"
    printf '_stub_name=%q\n' "$1"
    cat <<'DISPATCH'
_stub_read() {
  _stub_value=
  IFS= read -r -d '' _stub_value <"$1"
}
_stub_read "$_stub_rules/count"
_stub_n="${_stub_value%
}"
while [ "$_stub_n" -gt 0 ]; do
  _stub_rule="$_stub_rules/$_stub_n"
  _stub_n=$((_stub_n - 1))
  _stub_read "$_stub_rule/pattern"
  # shellcheck disable=SC2254
  case "$_stub_args" in
    $_stub_value) ;;
    *) continue ;;
  esac
  if [ -f "$_stub_rule/times" ]; then
    _stub_read "$_stub_rule/times"
    _stub_left="${_stub_value%
}"
    _stub_claimed=
    _stub_k=1
    # A use is claimed by creating used.<k> with noclobber, so two calls at
    # once never take the same use.
    while [ "$_stub_k" -le "$_stub_left" ]; do
      if (set -C && : >"$_stub_rule/used.$_stub_k") 2>/dev/null; then
        _stub_claimed=1
        break
      fi
      _stub_k=$((_stub_k + 1))
    done
    [ -n "$_stub_claimed" ] || continue
  fi
  if [ -f "$_stub_rule/exec" ]; then
    . "$_stub_rule/exec"
    exit
  fi
  if [ -f "$_stub_rule/out" ]; then
    _stub_read "$_stub_rule/out"
    printf '%s' "$_stub_value"
  fi
  if [ -f "$_stub_rule/err" ]; then
    _stub_read "$_stub_rule/err"
    printf '%s' "$_stub_value" >&2
  fi
  _stub_read "$_stub_rule/rc"
  exit "${_stub_value%
}"
done
printf 'stub %s: no rule matches: %s\n' "$_stub_name" "$_stub_args" >&2
exit 99
DISPATCH
  } >"$STUB_DIR/bin/$1"
  chmod +x "$STUB_DIR/bin/$1"
}

# stub_on <command> <pattern> [<option>...]
# Adds a rule for <command>; the first rule of a command also writes the
# fake bin/<command>. <pattern> is a shell pattern matched with case against
# the arguments of a call joined by single spaces; '*' matches every call.
#
# Options:
#   --out <text>       print the text and a newline to stdout, nothing for an
#                      empty text
#   --out-raw <text>   print the text as it is
#   --out-file <file>  print a copy of the file, taken now
#   --err <text>       print the text and a newline to stderr
#   --rc <n>           exit with status n, default 0
#   --times <n>        answer n calls, then let older rules answer
#   --exec <body>      source the body in the fake, with the arguments of the
#                      call as "$@", and take its status; no other answer
#                      option goes with it
stub_on() {
  if [ "$#" -lt 2 ]; then
    echo 'stub_on: usage: stub_on <command> <pattern> [options]' >&2
    return 1
  fi
  local name="$1" pattern="$2" out='' out_set='' err='' err_set='' rc=0 rc_set='' times=''
  local exec_body='' exec_set=''
  shift 2
  _stubs_name_ok stub_on "$name" || return 1
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --out | --out-raw | --out-file | --err | --rc | --times | --exec)
        if [ "$#" -lt 2 ]; then
          echo 'stub_on: usage: stub_on <command> <pattern> [options]' >&2
          return 1
        fi
        ;;
    esac
    case "$1" in
      --out)
        out_set=1
        if [ -n "$2" ]; then
          out="$2
"
        else
          out=
        fi
        ;;
      --out-raw)
        out_set=1
        out="$2"
        ;;
      --out-file)
        if [ ! -f "$2" ]; then
          echo "stub_on: $2 does not exist" >&2
          return 1
        fi
        out_set=1
        out=
        IFS= read -r -d '' out <"$2" || true
        ;;
      --err)
        err_set=1
        err="$2
"
        ;;
      --rc)
        case "$2" in
          '' | *[!0-9]*)
            echo 'stub_on: --rc needs an integer from 0 to 255' >&2
            return 1
            ;;
        esac
        if [ "${#2}" -gt 3 ] || [ "$2" -gt 255 ]; then
          echo 'stub_on: --rc needs an integer from 0 to 255' >&2
          return 1
        fi
        rc_set=1
        rc="$2"
        ;;
      --times)
        case "$2" in
          '' | *[!0-9]* | 0*)
            echo 'stub_on: --times needs a positive integer' >&2
            return 1
            ;;
        esac
        times="$2"
        ;;
      --exec)
        exec_set=1
        exec_body="$2"
        ;;
      *)
        echo "stub_on: unknown option $1" >&2
        return 1
        ;;
    esac
    shift 2
  done
  if [ -n "$exec_set" ] && [ -n "$out_set$err_set$rc_set" ]; then
    echo 'stub_on: --exec takes no other answer option' >&2
    return 1
  fi

  _stubs_init
  local rules="$STUB_DIR/rules/$name" count=0 rule
  if [ -f "$rules/count" ]; then
    read -r count <"$rules/count"
  else
    mkdir -p "$rules"
    _stubs_write_dispatcher "$name"
  fi
  count=$((count + 1))
  rule="$rules/$count"
  mkdir "$rule"
  printf '%s' "$pattern" >"$rule/pattern"
  printf '%s\n' "$rc" >"$rule/rc"
  [ -z "$times" ] || printf '%s\n' "$times" >"$rule/times"
  [ -z "$out_set" ] || printf '%s' "$out" >"$rule/out"
  [ -z "$err_set" ] || printf '%s' "$err" >"$rule/err"
  [ -z "$exec_set" ] || printf '%s\n' "$exec_body" >"$rule/exec"
  # The count moves last, so a call never sees a rule half written.
  printf '%s\n' "$count" >"$rules/count"
}

# stub_script <command>
# Reads a body from stdin and writes it as bin/<command>, behind the shebang
# and the line that logs the call; the body sees the arguments as "$@" and
# STUB_DIR. For a fake whose logic fits no rule.
stub_script() {
  local body=
  if [ "$#" -lt 1 ] || [ -z "$1" ]; then
    echo 'stub_script: usage: stub_script <command>, body on stdin' >&2
    return 1
  fi
  _stubs_name_ok stub_script "$1" || return 1
  IFS= read -r -d '' body || true
  if [ -z "$body" ]; then
    echo 'stub_script: usage: stub_script <command>, body on stdin' >&2
    return 1
  fi
  _stubs_init
  {
    _stubs_header "$1"
    printf '%s' "$body"
  } >"$STUB_DIR/bin/$1"
  chmod +x "$STUB_DIR/bin/$1"
}

# stub_calls <command>
# Prints the call log of <command>, one line per call; nothing, with status
# 0, for a command that was never called or never declared.
stub_calls() {
  _stubs_init
  [ ! -f "$STUB_DIR/calls/$1" ] || cat "$STUB_DIR/calls/$1"
}

# with_stubs <command> [<argument>...]
# Runs the command with $STUB_DIR/bin in front of PATH. The PATH of the
# test itself does not change.
with_stubs() {
  _stubs_init
  PATH="$STUB_DIR/bin:$PATH" "$@"
}
