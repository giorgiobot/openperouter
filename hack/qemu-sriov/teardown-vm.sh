#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"

ssh_cmd sudo poweroff || true
sleep 5
[ -f qemu.pid ] && kill "$(cat qemu.pid)" 2>/dev/null || true
