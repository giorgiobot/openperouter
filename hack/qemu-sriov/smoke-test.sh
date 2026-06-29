#!/usr/bin/env bash
set -euo pipefail

# Verifies that, after deploying openperouter with the grout dataplane onto the
# QEMU k3s node, the cluster is stable: the node stays Ready, every openperouter
# pod (controller, nodemarker, and the router pod's frr/reloader/grout
# containers) reaches Ready, and nothing crash-loops over an observation window.
#
# Talks to k3s over the port-forwarded API server via $KUBECONFIG.

KUBECTL="${KUBECTL:-kubectl}"
NS="${NAMESPACE:-openperouter-system}"
READY_TIMEOUT="${READY_TIMEOUT:-300s}"
STABILITY_WINDOW="${STABILITY_WINDOW:-30}"

k() { "$KUBECTL" "$@"; }

# Sum of restart counts across every container of every pod in the namespace.
restart_total() {
  k -n "$NS" get pods \
    -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}' \
    | awk '{s += $1} END {print s + 0}'
}

dump_diagnostics() {
  echo "::group::openperouter smoke-test diagnostics" >&2
  k get nodes -o wide >&2 || true
  k get pods -A -o wide >&2 || true
  k -n "$NS" describe pods >&2 || true
  k -n "$NS" get events --sort-by=.lastTimestamp >&2 || true
  for comp in controller nodemarker router; do
    echo "----- logs: $comp -----" >&2
    k -n "$NS" logs -l "app.kubernetes.io/component=$comp" --all-containers --prefix --tail=100 >&2 || true
  done
  echo "::endgroup::" >&2
}
trap 'echo "SMOKE TEST FAILED" >&2; dump_diagnostics' ERR

echo "== Waiting for the node to be Ready =="
k wait --for=condition=Ready node --all --timeout="$READY_TIMEOUT"

echo "== Waiting for the openperouter workloads to roll out =="
k -n "$NS" rollout status daemonset/openperouter-controller --timeout="$READY_TIMEOUT"
k -n "$NS" rollout status deployment/openperouter-nodemarker --timeout="$READY_TIMEOUT"
k -n "$NS" rollout status daemonset/openperouter-router --timeout="$READY_TIMEOUT"

echo "== Waiting for all $NS pods to be Ready =="
k -n "$NS" wait --for=condition=Ready pod --all --timeout="$READY_TIMEOUT"

echo "== Verifying the grout container is running in the router pod =="
grout_started=$(k -n "$NS" get pod -l app.kubernetes.io/component=router \
  -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="grout")].state.running.startedAt}')
if [ -z "$grout_started" ]; then
  echo "grout container is not running" >&2
  exit 1
fi
echo "grout container running since $grout_started"

echo "== Observing cluster stability for ${STABILITY_WINDOW}s =="
before=$(restart_total)
sleep "$STABILITY_WINDOW"
after=$(restart_total)
if [ "$after" -gt "$before" ]; then
  echo "container restarts increased during the stability window: $before -> $after" >&2
  exit 1
fi

# The node and every pod must still be Ready at the end of the window.
k wait --for=condition=Ready node --all --timeout=30s
k -n "$NS" wait --for=condition=Ready pod --all --timeout=30s

echo "Smoke test passed: cluster is stable with openperouter (grout) deployed (restarts: $after)."
