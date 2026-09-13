#!/bin/bash
set -euo pipefail

# restore-csv-timestamp.sh
# Restores the committed createdAt timestamp in the CSV file if it's the only change.
# This prevents spurious diffs when operator-sdk regenerates the bundle.

CSV_FILE="${1:-}"

if [ ! -f "$CSV_FILE" ]; then
    echo "Error: CSV file not found: $CSV_FILE" >&2
    exit 1
fi

if git diff --quiet HEAD -- "$CSV_FILE"; then
    exit 0
fi

if ! git diff --exit-code -I'^    createdAt: ' HEAD -- "$CSV_FILE" >/dev/null 2>&1; then
    exit 0
fi

echo "Restoring createdAt timestamp from HEAD in $CSV_FILE"
git restore --source=HEAD --worktree "$CSV_FILE"
