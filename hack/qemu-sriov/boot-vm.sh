#!/usr/bin/env bash
set -euo pipefail

qemu-img create -f qcow2 -F qcow2 -b "$(pwd)/base.img" disk.qcow2
qemu-img resize disk.qcow2 +6G

# 6144Mi leaves ~3Gi for the kernel/k3s/OS after cloud-init's
# configure-sriov.sh reserves 3Gi (1536 * 2Mi) of hugepages.
qemu-system-x86_64 \
  -machine q35,accel=kvm \
  -cpu host \
  -m 6144 -smp 2 \
  -drive file=disk.qcow2,if=virtio,format=qcow2 \
  -drive file=seed.img,if=virtio,format=raw \
  -netdev user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::6443-:6443 \
  -device virtio-net-pci,netdev=net0 \
  -netdev user,id=net1,net=10.0.3.0/24 \
  -device pcie-root-port,id=pcie_port.1 \
  -device igb,bus=pcie_port.1,netdev=net1 \
  -display none \
  -serial file:console.log \
  -daemonize \
  -pidfile qemu.pid
