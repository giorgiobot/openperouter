#!/bin/bash
set -euo pipefail

INTERVAL="${MONITOR_INTERVAL:-10}"
OUTPUT_DIR="${KIND_EXPORT_LOGS:-/tmp/kind_logs}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-pe-kind}"
PIDFILE="${OUTPUT_DIR}/grout-memory-monitor.pid"

start() {
    mkdir -p "$OUTPUT_DIR"

    local nodes
    nodes=$(docker ps --filter "label=io.x-k8s.kind.cluster=${KIND_CLUSTER_NAME}" --format '{{.Names}}')
    if [[ -z "$nodes" ]]; then
        echo "No kind nodes found for cluster ${KIND_CLUSTER_NAME}"
        exit 1
    fi

    echo "Monitoring grout memory on nodes: $(echo "$nodes" | tr '\n' ' ')"
    echo "Interval: ${INTERVAL}s, output: ${OUTPUT_DIR}/grout-memory-*.log"

    for node in $nodes; do
        local logfile="${OUTPUT_DIR}/grout-memory-${node}.log"
        (
            # Capture the header once
            docker exec "$node" crictl stats --output table 2>/dev/null | head -1 > "$logfile"
            echo "# Monitoring started at $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$logfile"

            while true; do
                local line
                line=$(docker exec "$node" crictl stats --output table 2>/dev/null | grep grout || true)
                if [[ -n "$line" ]]; then
                    echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $line" >> "$logfile"
                fi
                sleep "$INTERVAL"
            done
        ) &
        echo "$!" >> "$PIDFILE"
    done

    echo "Monitor PIDs written to ${PIDFILE}"
}

stop() {
    if [[ ! -f "$PIDFILE" ]]; then
        echo "No pidfile found at ${PIDFILE}, nothing to stop"
        return 0
    fi
    while read -r pid; do
        kill "$pid" 2>/dev/null || true
    done < "$PIDFILE"
    rm -f "$PIDFILE"
    echo "Grout memory monitor stopped"
}

case "${1:-}" in
    start) start ;;
    stop)  stop ;;
    *)
        echo "Usage: $0 {start|stop}"
        exit 1
        ;;
esac
