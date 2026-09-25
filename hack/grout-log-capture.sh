#!/usr/bin/env bash
#
# Follows the grout container of every router pod for the whole e2e run and
# keeps a copy of its log that survives kubelet log rotation.
#
# A grout error loop can log ~150k lines/s ("rte_pktmbuf_alloc: pool
# exhausted"), which rotates the container log away within seconds, so the
# log collected at the end of the job no longer holds the first occurrence
# or what preceded it. This copy keeps the first MAX_REPEATS occurrences of
# every distinct message and then only counts them, so the onset stays
# readable and the file stays small.
#
# It also raises the grout log levels in LOG_LEVELS on every new grout
# container, so pool allocations and releases are logged.
#
# Runs until killed. Start it with setsid so it leads its own process group,
# and stop it with `kill -- -$(cat "$PID_FILE")` to stop the log streams too.
# Reads NAMESPACE, SELECTOR, OUT_DIR, MAX_REPEATS, REPORT_EVERY, LOG_LEVELS,
# KUBECTL and PID_FILE from the environment.

set -uo pipefail

NAMESPACE="${NAMESPACE:-openperouter-system}"
SELECTOR="${SELECTOR:-app=router}"
OUT_DIR="${OUT_DIR:-/tmp/kind_logs/grout-logs}"
MAX_REPEATS="${MAX_REPEATS:-20}"
REPORT_EVERY="${REPORT_EVERY:-100000}"
LOG_LEVELS="${LOG_LEVELS:-grout.mempool:debug grout.port:debug}"
KUBECTL="${KUBECTL:-kubectl}"
PID_FILE="${PID_FILE:-/tmp/grout-log-capture.pid}"
POLL_SECONDS=5

declare -A followers
declare -A levels_set

main() {
  mkdir -p "${OUT_DIR}"
  echo $$ >"${PID_FILE}"
  echo "capturing grout logs of ${NAMESPACE}/${SELECTOR} into ${OUT_DIR}"

  local pod node cid
  while true; do
    while read -r pod node cid; do
      [ -n "${cid}" ] || continue
      ensure_follower "${pod}" "${node}" "${cid}"
      ensure_log_levels "${pod}" "${cid}"
    done < <(running_grout_containers)
    sleep "${POLL_SECONDS}"
  done
}

# running_grout_containers prints "pod node containerID" for every router pod
# whose grout container is running.
running_grout_containers() {
  "${KUBECTL}" get pods -n "${NAMESPACE}" -l "${SELECTOR}" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{" "}{range .status.containerStatuses[?(@.name=="grout")]}{.state.running.startedAt}{"/"}{.containerID}{end}{"\n"}{end}' 2>/dev/null |
    awk '$3 ~ /^[^\/]+\/.+:\/\// { sub(/^[^\/]*\//, "", $3); sub(/^.*:\/\//, "", $3); print $1, $2, $3 }'
}

# ensure_follower streams the logs of the given container unless a stream for
# it is already running. A stream that ended while the container still runs is
# started again, so a dropped connection does not end the capture; it only
# asks for the last minute, so a flooding container is not copied again from
# the start.
ensure_follower() {
  local pod="$1" node="$2" cid="$3"
  local pid="${followers[${cid}]:-}"
  local since=()

  if [ -n "${pid}" ]; then
    kill -0 "${pid}" 2>/dev/null && return
    since=(--since=60s)
  fi

  local out="${OUT_DIR}/${node}_${pod}_${cid:0:12}.log"
  echo "$(date -u +%FT%TZ) [grout-log-capture] following ${pod} on ${node}, container ${cid:0:12} ${since[*]}" >>"${out}"
  "${KUBECTL}" logs -f --timestamps "${since[@]}" -n "${NAMESPACE}" -c grout "${pod}" 2>&1 |
    dedup_repeats >>"${out}" &
  followers[${cid}]=$!
}

# ensure_log_levels raises the grout log levels once per container. It is
# retried on the next poll until grcli succeeds, since grout may still be
# starting.
ensure_log_levels() {
  local pod="$1" cid="$2"

  [ -z "${LOG_LEVELS}" ] && return
  [ -n "${levels_set[${cid}]:-}" ] && return

  # LOG_LEVELS is a space separated list, split on purpose.
  # shellcheck disable=SC2086
  if "${KUBECTL}" exec -n "${NAMESPACE}" -c grout "${pod}" -- grcli log level set ${LOG_LEVELS} >/dev/null 2>&1; then
    levels_set[${cid}]=1
    echo "set grout log levels on ${pod} (${cid:0:12}): ${LOG_LEVELS}"
  fi
}

# dedup_repeats prints the first MAX_REPEATS occurrences of every message, then
# only a running count every REPORT_EVERY occurrences. The leading timestamp
# added by kubectl is ignored when comparing messages.
dedup_repeats() {
  awk -v max="${MAX_REPEATS}" -v every="${REPORT_EVERY}" '
    {
      msg = substr($0, index($0, " ") + 1)
      n = ++seen[msg]
      if (n <= max) {
        print
        if (n == max) {
          print $1 " [grout-log-capture] seen " max " times, counting further repeats: " msg
        }
        fflush()
      } else if (n % every == 0) {
        print $1 " [grout-log-capture] seen " n " times: " msg
        fflush()
      }
    }'
}

main "$@"
