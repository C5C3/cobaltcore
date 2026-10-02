#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/derive-sizing-figures.py on TSV fixtures written into a temp
# directory, one recommendations.tsv per job run, the way
# `gh run download --pattern 'sizing-*'` lays out the artifacts:
#   - the shared memory formula fits the largest normalized target at every
#     process count, and memoryBase + p × defaultMemoryPerProcess covers every
#     formula row;
#   - Nova scheduler rows without --processes count one process in the
#     ControlPlane jobs and two in tempest, and --threads normalizes by 32Mi
#     per extra thread of each process;
#   - the CPU median takes the upper middle key, counts a key measured in two
#     legs once at its larger value, rounds up to 10m and never drops below
#     10m;
#   - a container with a fixed memory figure, cinder-backup or the Neutron
#     metadata agent, stays out of the memory fit;
#   - the backing-service figures keep their memory floors, and a backing row
#     of another job is ignored;
#   - the budget projection lowers minimalServiceCPURequest in 5m steps until
#     it fits, and exits 1 when it cannot or when memory is over budget;
#   - incomplete input exits 1 and unparsable input exits 2.
#
# Usage: bash tests/unit/hack/derive_sizing_figures_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DERIVE="$PROJECT_ROOT/hack/derive-sizing-figures.py"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

HEADER="namespace kind workload replicas owner_kind owner_name app component container processes threads cpu_target_m memory_target_mi cpu_upper_m memory_upper_mi cpu_request_m memory_request_mi memory_limit_mi snapshot"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_tsv <dir> <job> <leg>
# Writes <dir>/recommendations.tsv the way the collector does: the comment
# line, the header row, then the rows read from stdin with their
# space-separated fields turned into tabs.
write_tsv() {
  local dir="$1" job="$2" leg="$3"
  mkdir -p "$dir"
  {
    echo "# run=42 attempt=1 sha=abc job=$job leg=$leg recommender=vpa-recommender:1.8.0"
    tr ' ' '\t' <<<"$HEADER"
    grep -v '^$' | tr -s ' ' '\t'
  } >"$dir/recommendations.tsv"
}

# The fixture of one complete run. The formula rows peak at T = 300Mi at one
# process, 430Mi at two and 700Mi at four.
write_controlplane() {
  write_tsv "$1/sizing-e2e-controlplane" e2e-controlplane - <<'EOF'
openstack Deployment keystone-api 1 Keystone keystone keystone api keystone-api 1 1 30 300 - - 100 368 368 t
openstack Deployment neutron-api 1 Neutron neutron neutron api neutron-api 2 1 40 430 - - 100 512 512 t
openstack Deployment nova-api 1 Nova nova nova api nova-api 4 1 20 700 - - 100 800 800 t
openstack Deployment glance-api 1 Glance glance glance api glance-api - - 50 500 - - 100 624 624 t
openstack StatefulSet openstack-db 1 MariaDB openstack-db mariadb database mariadb - - 30 700 - - 100 1024 1024 t
openstack StatefulSet openstack-memcached 1 Memcached openstack-memcached memcached cache memcached - - 5 40 - - 50 96 96 t
openstack StatefulSet shared-rabbitmq 1 RabbitmqCluster shared-rabbitmq rabbitmq messaging rabbitmq - - 100 300 - - 200 512 512 t
openstack StatefulSet barbican-openbao 1 OpenBaoCluster barbican-openbao openbao secretstore openbao - - 12 180 - - 50 128 128 t
openstack DaemonSet ovn-chassis - OVNChassis chassis ovnchassis ovs ovs-vswitchd - - 30 100 - - 100 256 256 t
EOF
}

write_sso() {
  write_tsv "$1/sizing-e2e-controlplane-sso" e2e-controlplane-sso - <<'EOF'
openstack Deployment keystone-api 1 Keystone keystone keystone api keystone-api 1 1 30 290 - - 100 368 368 t
openstack Deployment keystone-api 1 Keystone keystone keystone api federation-proxy - - 12 100 - - 25 256 256 t
EOF
}

write_tempest() {
  # The MariaDB of a tempest leg is a backing row of another job: ignored.
  write_tsv "$1/sizing-tempest-keystone-2025.2" tempest keystone-2025.2 <<'EOF'
openstack Deployment keystone 2 Keystone keystone keystone api keystone-api 4 1 80 650 - - 100 800 800 t
openstack StatefulSet openstack-db 1 MariaDB openstack-db mariadb database mariadb - - 300 2000 - - 100 1024 1024 t
EOF
  write_tsv "$1/sizing-tempest-glance-2025.2" tempest glance-2025.2 <<'EOF'
openstack Deployment glance 1 Glance glance glance api glance-api - - 60 700 - - 100 624 624 t
EOF
}

write_complete_run() {
  write_controlplane "$1"
  write_sso "$1"
  write_tempest "$1"
}

# derive <dir> [--budget-total value]
# Runs the script; stdout and stderr go to $OUT and $ERR, the exit code to $RC.
derive() {
  local dir="$1"
  shift
  python3 "$DERIVE" "$@" "$dir" >"$OUT" 2>"$ERR"
  RC=$?
}

# value_of <key> — the value of one key=value line of the last run.
value_of() {
  awk -F= -v k="$1" '$1 == k {print $2; exit}' "$OUT"
}

# input_row <job> <workload> <container> — the p, t and T columns of one row
# of the Markdown input table, space-separated.
input_row() {
  awk -F' [|] ' -v j="$1" -v w="$2" -v c="$3" '
    $1 == "| " j && $6 == w && $7 == c {print $9, $10, $13; exit}' "$OUT" | sed 's/ |$//'
}

new_tmp() {
  TMP="$(mktemp -d)"
  OUT="$TMP/out"
  ERR="$TMP/err"
}

# ---------------------------------------------------------------------------
# Test 1: the figures of a complete run
# ---------------------------------------------------------------------------
test_complete_run() {
  echo "Test: a complete run derives every figure"
  new_tmp
  write_complete_run "$TMP/run"

  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the slope 700-430 over two processes rounds up to 144Mi" "144Mi" "$(value_of defaultMemoryPerProcess)"
  assert_eq "300 - 144 = 156 rounds up to a 160Mi base" "160Mi" "$(value_of memoryBase)"
  assert_eq "Glance at (500 - 160) / 1 process rounds up to 352Mi" "352Mi" "$(value_of glanceMemoryPerProcess)"
  assert_eq "the tempest keys 60m and 80m take the upper middle, 80m" "80m" "$(value_of defaultCPURequest)"
  assert_eq "the ControlPlane keys 20, 30, 40, 50m take 40m" "40m" "$(value_of minimalServiceCPURequest)"
  assert_eq "MariaDB at a 700Mi target keeps its 1Gi floor; the tempest MariaDB is ignored" \
    "30m,1Gi" "$(value_of minimalDatabase)"
  assert_eq "Memcached at a 40Mi target keeps its 96Mi floor, CPU its 10m floor" \
    "10m,96Mi" "$(value_of minimalCache)"
  assert_eq "the broker keeps its 512Mi floor" "100m,512Mi" "$(value_of minimalMessaging)"
  assert_eq "OpenBao at a 180Mi target rounds up to 192Mi, 12m to 15m" \
    "15m,192Mi" "$(value_of minimalSecretStore)"
  assert_eq "the sidecar CPU keeps its 25m floor" "25m" "$(value_of sidecarCPURequest)"
  assert_eq "the sidecar memory keeps its 256Mi floor" "256Mi" "$(value_of sidecarMemory)"
  assert_eq "the keys come in the order of the constants they replace" \
    "memoryBase defaultMemoryPerProcess glanceMemoryPerProcess defaultCPURequest minimalServiceCPURequest minimalDatabase minimalCache minimalMessaging minimalSecretStore sidecarCPURequest sidecarMemory " \
    "$(awk -F= 'NF == 2 && $1 ~ /^[a-zA-Z]+$/ {printf "%s ", $1}' "$OUT")"
  # Four service rows drop 60m each, MariaDB 70m, Memcached 40m and OpenBao
  # 35m (-385m); the formula moves each service by -64Mi (Glance -112Mi) and
  # OpenBao rises 64Mi (-240Mi).
  assert_eq "the projection adds the changed requests to the budget total" \
    "projected: 2615m 11760Mi of 4000m 16384Mi" "$(grep '^projected:' "$OUT")"
  assert_contains "the Markdown lists the input rows" "$(cat "$OUT")" "### Input rows"
  assert_contains "the chassis row is printed as ignored" "$(cat "$OUT")" \
    "| e2e-controlplane | - | openstack | OVNChassis | ovs | ovn-chassis | ovs-vswitchd | ignored |"

  # memoryBase + p × defaultMemoryPerProcess covers every formula row.
  local uncovered
  uncovered="$(awk -F' [|] ' '$8 == "formula" && $13+0 > 160 + $9 * 144 {print $6}' "$OUT")"
  assert_eq "memoryBase + p × defaultMemoryPerProcess covers every formula row" "" "$uncovered"
  assert_eq "the check above saw the five formula rows" "5" \
    "$(awk -F' [|] ' '$8 == "formula"' "$OUT" | wc -l | tr -d ' ')"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 2: process and thread counts
# ---------------------------------------------------------------------------
test_process_counts() {
  echo "Test: Nova scheduler workers follow the job, and threads normalize by 32Mi"
  new_tmp
  write_complete_run "$TMP/run"
  cat >>"$TMP/run/sizing-e2e-controlplane/recommendations.tsv" <<EOF
$(printf 'openstack Deployment nova-scheduler 1 Nova nova nova scheduler nova-scheduler - - 10 200 - - 100 368 368 t' | tr ' ' '\t')
EOF
  cat >>"$TMP/run/sizing-tempest-keystone-2025.2/recommendations.tsv" <<EOF
$(printf 'openstack Deployment nova-scheduler 1 Nova nova nova scheduler nova-scheduler - - 10 400 - - 100 512 512 t' | tr ' ' '\t')
$(printf 'openstack Deployment keystone-threads 1 Keystone keystone keystone api keystone-api 4 2 10 1000 - - 100 928 928 t' | tr ' ' '\t')
EOF

  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "a scheduler without --processes counts 1 process in e2e-controlplane" \
    "1 1 200" "$(input_row e2e-controlplane nova-scheduler nova-scheduler)"
  assert_eq "a scheduler without --processes counts 2 processes in tempest" \
    "2 1 400" "$(input_row tempest nova-scheduler nova-scheduler)"
  assert_eq "--processes 4 --threads 2 at 1000Mi normalizes to 1000 - 4 × 32 = 872" \
    "4 2 872" "$(input_row tempest keystone-threads keystone-api)"
  assert_eq "eventlet Glance counts 2 processes in tempest" \
    "2 1 700" "$(input_row tempest glance glance-api)"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 3: the render-time CPU median
# ---------------------------------------------------------------------------
test_cpu_median() {
  echo "Test: the render-time CPU request is the upper median of the per-key maxima"
  new_tmp
  write_controlplane "$TMP/run"
  write_sso "$TMP/run"
  # Four keys: keystone at 44m in one leg and 63m in the other counts once as
  # 63m. Sorted 12, 41, 63, 90: the upper middle is 63m, which rounds up to
  # 70m. Counting keystone twice, or taking the lower middle, gives 50m.
  write_tsv "$TMP/run/sizing-tempest-keystone-2025.2" tempest keystone-2025.2 <<'EOF'
openstack Deployment keystone 1 Keystone keystone keystone api keystone-api 4 1 44 650 - - 100 800 800 t
openstack Deployment glance 1 Glance glance glance api glance-api - - 41 700 - - 100 624 624 t
EOF
  write_tsv "$TMP/run/sizing-tempest-keystone-2026.1" tempest keystone-2026.1 <<'EOF'
openstack Deployment keystone 1 Keystone keystone keystone api keystone-api 4 1 63 650 - - 100 800 800 t
openstack Deployment neutron 1 Neutron neutron neutron api neutron-api 2 1 12 400 - - 100 512 512 t
openstack Deployment nova 1 Nova nova nova api nova-api 2 1 90 400 - - 100 512 512 t
EOF
  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the upper middle of 12, 41, 63, 90 rounds up to 70m" "70m" "$(value_of defaultCPURequest)"
  assert_contains "the key table carries the larger keystone value once" "$(cat "$OUT")" \
    '| `Keystone/api/keystone-api` | 63 |'
  assert_contains "the median sentence counts four keys" "$(cat "$OUT")" "Median of 4 keys: 63m"

  write_tsv "$TMP/run/sizing-tempest-keystone-2025.2" tempest keystone-2025.2 <<'EOF'
openstack Deployment keystone 1 Keystone keystone keystone api keystone-api 4 1 3 650 - - 100 800 800 t
openstack Deployment glance 1 Glance glance glance api glance-api - - 1 700 - - 100 624 624 t
EOF
  rm -rf "$TMP/run/sizing-tempest-keystone-2026.1"
  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "a median of 3m becomes the 10m floor" "10m" "$(value_of defaultCPURequest)"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 4: a database above its floor
# ---------------------------------------------------------------------------
test_database_above_floor() {
  echo "Test: MariaDB at 1100Mi rounds up to 1104Mi"
  new_tmp
  write_complete_run "$TMP/run"
  sed -i.bak 's/\tmariadb\t-\t-\t30\t700\t/\tmariadb\t-\t-\t30\t1100\t/' \
    "$TMP/run/sizing-e2e-controlplane/recommendations.tsv"

  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "MariaDB at 1100Mi yields 1104Mi" "30m,1104Mi" "$(value_of minimalDatabase)"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 4a: a fixed memory figure stays out of the fit
# cinder-backup's memory is the operator's backupMemory, not the formula, so a
# 1400Mi target at one process must not raise memoryBase (it would reach
# 1264Mi). Its CPU still counts: 70m moves the tempest median from 80m to 70m
# and 35m moves the ControlPlane median from 40m to 35m.
# ---------------------------------------------------------------------------
test_fixed_memory_stays_out_of_the_fit() {
  echo "Test: cinder-backup counts for CPU but stays out of the memory fit"
  new_tmp
  write_complete_run "$TMP/run"
  write_tsv "$TMP/run/sizing-e2e-controlplane" e2e-controlplane - <<'EOF'
openstack Deployment keystone-api 1 Keystone keystone keystone api keystone-api 1 1 30 300 - - 100 368 368 t
openstack Deployment neutron-api 1 Neutron neutron neutron api neutron-api 2 1 40 430 - - 100 512 512 t
openstack Deployment nova-api 1 Nova nova nova api nova-api 4 1 20 700 - - 100 800 800 t
openstack Deployment glance-api 1 Glance glance glance api glance-api - - 50 500 - - 100 624 624 t
openstack Deployment cinder-backup 1 Cinder cinder cinder backup backup 1 1 35 1400 - - 100 2048 2048 t
openstack StatefulSet openstack-db 1 MariaDB openstack-db mariadb database mariadb - - 30 700 - - 100 1024 1024 t
openstack StatefulSet openstack-memcached 1 Memcached openstack-memcached memcached cache memcached - - 5 40 - - 50 96 96 t
openstack StatefulSet shared-rabbitmq 1 RabbitmqCluster shared-rabbitmq rabbitmq messaging rabbitmq - - 100 300 - - 200 512 512 t
openstack StatefulSet barbican-openbao 1 OpenBaoCluster barbican-openbao openbao secretstore openbao - - 12 180 - - 50 128 128 t
EOF
  write_tsv "$TMP/run/sizing-tempest-cinder-2025.2" tempest cinder-2025.2 <<'EOF'
openstack Deployment cinder-backup 1 Cinder cinder cinder backup backup 1 1 70 1500 - - 100 2048 2048 t
EOF

  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the backup rows leave defaultMemoryPerProcess at 144Mi" "144Mi" "$(value_of defaultMemoryPerProcess)"
  assert_eq "the backup rows leave memoryBase at 160Mi" "160Mi" "$(value_of memoryBase)"
  assert_eq "the tempest backup CPU joins the render-time median (60, 70, 80m)" \
    "70m" "$(value_of defaultCPURequest)"
  assert_eq "the ControlPlane backup CPU joins the Minimal median (20, 30, 35, 40, 50m)" \
    "35m" "$(value_of minimalServiceCPURequest)"
  assert_contains "the projection keeps the backup memory and moves its CPU" "$(cat "$OUT")" \
    "| cinder-backup | backup | 1 | 100 → 35 | 2048 → 2048 |"
  assert_contains "the formula section names the fixed container" "$(cat "$OUT")" \
    "These containers have a fixed memory figure and stay out of the fit: \`Cinder/backup/backup\`."
  assert_contains "the input table marks the backup row" "$(cat "$OUT")" \
    "| tempest | cinder-2025.2 | openstack | Cinder | backup | cinder-backup | backup | formula, fixed memory | 1 | 1 | 70 | 1500 | 1500 |"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 4b: the metadata agent's memory stays out of the fit
# The agent's memory is the operator's metadataAgentMemory, which follows the
# networks on its node. A 900Mi target at one process, above every other
# one-process row, must not raise memoryBase (it would reach 768Mi).
# ---------------------------------------------------------------------------
test_agent_memory_stays_out_of_the_fit() {
  echo "Test: the metadata agent stays out of the memory fit"
  new_tmp
  write_complete_run "$TMP/run"
  derive "$TMP/run" --budget-total 3000m,12000Mi
  local base per_process
  base="$(value_of memoryBase)"
  per_process="$(value_of defaultMemoryPerProcess)"

  tr ' ' '\t' >>"$TMP/run/sizing-e2e-controlplane/recommendations.tsv" <<'EOF'
openstack DaemonSet neutron-agent-metadata-agent - NeutronMetadataAgent neutron-agent neutronmetadataagent metadata-agent metadata-agent - - 12 900 - - 70 368 368 t
EOF

  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the run without the agent row derives memoryBase" "160Mi" "$base"
  assert_eq "the agent row leaves memoryBase where it is" "$base" "$(value_of memoryBase)"
  assert_eq "the agent row leaves defaultMemoryPerProcess where it is" \
    "$per_process" "$(value_of defaultMemoryPerProcess)"
  assert_contains "the formula section names the agent" "$(cat "$OUT")" \
    "These containers have a fixed memory figure and stay out of the fit: \`NeutronMetadataAgent/metadata-agent/metadata-agent\`."
  assert_contains "the input table marks the agent row" "$(cat "$OUT")" \
    "| e2e-controlplane | - | openstack | NeutronMetadataAgent | metadata-agent | neutron-agent-metadata-agent | metadata-agent | formula, fixed memory | 1 | 1 | 12 | 900 | 900 |"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 5: the budget projection lowers the Minimal service CPU
# ---------------------------------------------------------------------------
test_budget_lowers_minimal_cpu() {
  echo "Test: a CPU overshoot lowers minimalServiceCPURequest in 5m steps"
  new_tmp
  write_sso "$TMP/run"
  write_tempest "$TMP/run"
  # Four service rows request 10m and measure 80m, so Minimal raises each by
  # 70m: +280m, less 145m for the backing services, on 3990m is 4125m. Every
  # 5m step takes 20m off; seven steps reach 45m and 3985m.
  write_tsv "$TMP/run/sizing-e2e-controlplane" e2e-controlplane - <<'EOF'
openstack Deployment keystone-api 1 Keystone keystone keystone api keystone-api 1 1 80 300 - - 10 368 368 t
openstack Deployment neutron-api 1 Neutron neutron neutron api neutron-api 2 1 80 430 - - 10 512 512 t
openstack Deployment nova-api 1 Nova nova nova api nova-api 4 1 80 700 - - 10 800 800 t
openstack Deployment glance-api 1 Glance glance glance api glance-api - - 80 500 - - 10 624 624 t
openstack StatefulSet openstack-db 1 MariaDB openstack-db mariadb database mariadb - - 30 700 - - 100 1024 1024 t
openstack StatefulSet openstack-memcached 1 Memcached openstack-memcached memcached cache memcached - - 5 40 - - 50 96 96 t
openstack StatefulSet shared-rabbitmq 1 RabbitmqCluster shared-rabbitmq rabbitmq messaging rabbitmq - - 100 300 - - 200 512 512 t
openstack StatefulSet barbican-openbao 1 OpenBaoCluster barbican-openbao openbao secretstore openbao - - 12 180 - - 50 128 128 t
EOF
  # The sso keystone measured 30m; drop it so the four ControlPlane keys stay
  # at 80m.
  write_tsv "$TMP/run/sizing-e2e-controlplane-sso" e2e-controlplane-sso - <<'EOF'
openstack Deployment keystone-api 1 Keystone keystone keystone api federation-proxy - - 12 100 - - 25 256 256 t
EOF

  derive "$TMP/run" --budget-total 3990m,15000Mi
  assert_eq "exit code is 0" "0" "$RC"
  assert_eq "the Minimal service CPU steps down to 45m" "45m" "$(value_of minimalServiceCPURequest)"
  assert_eq "the projection fits" "projected: 3985m 14760Mi of 4000m 16384Mi" "$(grep '^projected:' "$OUT")"
  assert_eq "every 5m step from 80m to 45m is printed" \
    "80m 75m 70m 65m 60m 55m 50m 45m " \
    "$(awk -F' [|] ' '$1 ~ /^\| [0-9]+m$/ {sub(/^\| /, "", $1); printf "%s ", $1}' "$OUT")"

  derive "$TMP/run" --budget-total 4200m,15000Mi
  assert_eq "an unfittable projection exits 1" "1" "$RC"
  assert_contains "the message names the 10m floor" "$(cat "$ERR")" \
    "derive-sizing-figures: the Minimal figures cannot fit 4000m CPU even at 10m per service container"
  assert_contains "the figures are still printed for the report" "$(cat "$OUT")" "### Budget projection"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 6: memory over budget
# ---------------------------------------------------------------------------
test_memory_over_budget() {
  echo "Test: a memory projection above 16384Mi exits 1"
  new_tmp
  write_complete_run "$TMP/run"
  # The complete run moves memory by -240Mi: 16700 - 240 = 16460, 76 over.
  derive "$TMP/run" --budget-total 3000m,16700Mi
  assert_eq "exit code is 1" "1" "$RC"
  assert_contains "the message names the overshoot" "$(cat "$ERR")" \
    "derive-sizing-figures: memory over budget by 76Mi with every figure at its measured target"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 7: incomplete input
# ---------------------------------------------------------------------------
test_incomplete_input() {
  echo "Test: incomplete input exits 1 with the missing piece"
  new_tmp

  mkdir -p "$TMP/empty"
  derive "$TMP/empty" --budget-total 3000m,12000Mi
  assert_eq "an empty directory exits 1" "1" "$RC"
  assert_contains "an empty directory has no process count" "$(cat "$ERR")" \
    "the formula needs rows at two process counts, found none"

  write_complete_run "$TMP/single"
  sed -i.bak -e '/neutron-api/d' -e '/nova-api/d' "$TMP/single/sizing-e2e-controlplane/recommendations.tsv"
  rm -rf "$TMP/single/sizing-tempest-keystone-2025.2"
  derive "$TMP/single" --budget-total 3000m,12000Mi
  assert_eq "a single process count exits 1" "1" "$RC"
  assert_contains "the message lists the one count" "$(cat "$ERR")" \
    "the formula needs rows at two process counts, found 1"

  write_complete_run "$TMP/noglance"
  sed -i.bak '/glance-api/d' "$TMP/noglance/sizing-e2e-controlplane/recommendations.tsv"
  rm -rf "$TMP/noglance/sizing-tempest-glance-2025.2"
  derive "$TMP/noglance" --budget-total 3000m,12000Mi
  assert_eq "no Glance row exits 1" "1" "$RC"
  assert_contains "the message names Glance" "$(cat "$ERR")" "no Glance row was measured"

  write_complete_run "$TMP/nosso"
  rm -rf "$TMP/nosso/sizing-e2e-controlplane-sso"
  derive "$TMP/nosso" --budget-total 3000m,12000Mi
  assert_eq "no federation-proxy row exits 1" "1" "$RC"
  assert_contains "the message names the missing job" "$(cat "$ERR")" \
    "no federation-proxy row was measured (did e2e-controlplane-sso run?)"

  write_complete_run "$TMP/nocache"
  sed -i.bak '/openstack-memcached/d' "$TMP/nocache/sizing-e2e-controlplane/recommendations.tsv"
  derive "$TMP/nocache" --budget-total 3000m,12000Mi
  assert_eq "a missing backing service exits 1" "1" "$RC"
  assert_contains "the message names the kind" "$(cat "$ERR")" \
    "no Memcached row was measured in e2e-controlplane"

  write_complete_run "$TMP/notempest"
  rm -rf "$TMP/notempest/sizing-tempest-keystone-2025.2" "$TMP/notempest/sizing-tempest-glance-2025.2"
  derive "$TMP/notempest" --budget-total 3000m,12000Mi
  assert_eq "no tempest row exits 1" "1" "$RC"
  assert_contains "the message names the render-time request" "$(cat "$ERR")" \
    "no tempest row was measured for the render-time CPU request"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Test 8: unparsable input
# ---------------------------------------------------------------------------
test_parse_errors() {
  echo "Test: unparsable input and a missing --budget-total exit 2"
  new_tmp
  write_complete_run "$TMP/run"

  derive "$TMP/run"
  assert_eq "a missing --budget-total exits 2" "2" "$RC"
  assert_contains "the message says what to pass" "$(cat "$ERR")" \
    "derive-sizing-figures: cannot parse --budget-total: the option is required"

  derive "$TMP/run" --budget-total 3000,12Gi
  assert_eq "a malformed --budget-total exits 2" "2" "$RC"
  assert_contains "the message quotes the value" "$(cat "$ERR")" "cannot parse --budget-total: '3000,12Gi'"

  sed -i.bak 's/\tkeystone-api\t1\t1\t30\t300\t/\tkeystone-api\t1\t1\tabc\t300\t/' \
    "$TMP/run/sizing-e2e-controlplane/recommendations.tsv"
  derive "$TMP/run" --budget-total 3000m,12000Mi
  assert_eq "a non-numeric target exits 2" "2" "$RC"
  assert_contains "the message names the file, the line and the column" "$(cat "$ERR")" \
    "cannot parse $TMP/run/sizing-e2e-controlplane/recommendations.tsv:3: cpu_target_m 'abc' is not a whole number"

  write_complete_run "$TMP/nojob"
  sed -i.bak '1s/job=tempest/job=nightly/' "$TMP/nojob/sizing-tempest-glance-2025.2/recommendations.tsv"
  derive "$TMP/nojob" --budget-total 3000m,12000Mi
  assert_eq "an unknown job exits 2" "2" "$RC"
  assert_contains "the message names the job" "$(cat "$ERR")" "job=nightly is none of"
  rm -rf "$TMP"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP: python3 not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_complete_run
  test_process_counts
  test_cpu_median
  test_database_above_floor
  test_fixed_memory_stays_out_of_the_fit
  test_agent_memory_stays_out_of_the_fit
  test_budget_lowers_minimal_cpu
  test_memory_over_budget
  test_incomplete_input
  test_parse_errors
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
