#!/usr/bin/env bash
set -euo pipefail

ssh-keygen -t ed25519 -N "" -f id_ed25519

cat > meta-data <<'EOF'
instance-id: qemu-k8s-node
local-hostname: qemu-k8s-node
EOF

# Runs before k3s installs (see runcmd below), so kubelet picks up the
# SR-IOV VFs and the hugepage capacity from its very first start instead
# of needing a restart afterwards.
cat > user-data <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat id_ed25519.pub)
package_update: true
write_files:
  - path: /usr/local/sbin/configure-sriov.sh
    permissions: '0755'
    content: |
      #!/usr/bin/env bash
      set -euo pipefail

      # Find the SR-IOV-capable igb PCI device (the emulated PF) and bring
      # its link up; the PF has no IP config of its own (only its VFs are
      # used), so it never comes up on its own, and the VFs need the PF's
      # link up to get carrier on the embedded switch.
      PCI_DEV=\$(for d in /sys/bus/pci/devices/*/; do [ -f "\$d/sriov_totalvfs" ] && basename "\$d"; done)
      PF_IFACE=\$(basename /sys/bus/pci/devices/"\$PCI_DEV"/net/*)
      ip link set "\$PF_IFACE" up
      echo 8 > /sys/bus/pci/devices/"\$PCI_DEV"/sriov_numvfs

      # Creating the VFs is asynchronous: the virtfn0 symlink can lag behind
      # the sriov_numvfs write, so wait for it to appear before continuing.
      for i in \$(seq 1 20); do
        [ -e /sys/bus/pci/devices/"\$PCI_DEV"/virtfn0 ] && break
        sleep 0.5
      done

      # 1536 * 2Mi = 3Gi of hugepages headroom for whatever dataplane workload
      # will later run on this node.
      echo 1536 > /proc/sys/vm/nr_hugepages
      mountpoint -q /dev/hugepages || mount -t hugetlbfs hugetlbfs /dev/hugepages
runcmd:
  - /usr/local/sbin/configure-sriov.sh
  - curl -sfL https://get.k3s.io | sh -
EOF

cloud-localds seed.img user-data meta-data
