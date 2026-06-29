#!/usr/bin/env bash
set -euo pipefail

ssh-keygen -t ed25519 -N "" -f id_ed25519

cat > meta-data <<'EOF'
instance-id: qemu-k8s-node
local-hostname: qemu-k8s-node
EOF

# Runs before k3s installs (see runcmd below), so kubelet picks up the VF's
# vfio-pci binding and the hugepage capacity from its very first start
# instead of needing a restart afterwards.
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
      echo 2 > /sys/bus/pci/devices/"\$PCI_DEV"/sriov_numvfs

      # Creating the VFs is asynchronous: the virtfn0 symlink and the VF's
      # default-driver binding can both lag behind the sriov_numvfs write,
      # which otherwise races the unbind/bind below into transient
      # "Permission denied"/"Device or resource busy" failures.
      for i in \$(seq 1 20); do
        [ -e /sys/bus/pci/devices/"\$PCI_DEV"/virtfn0 ] && break
        sleep 0.5
      done
      VF_ADDR=\$(basename "\$(readlink -f /sys/bus/pci/devices/"\$PCI_DEV"/virtfn0)")
      for i in \$(seq 1 20); do
        [ -e /sys/bus/pci/devices/"\$VF_ADDR"/driver ] && break
        sleep 0.5
      done

      # Bind the first VF to vfio-pci so a DPDK userspace driver can claim it;
      # the guest has no vIOMMU, so use VFIO's noiommu mode. The second VF is
      # left on its default in-kernel driver.
      echo 'options vfio enable_unsafe_noiommu_mode=1' > /etc/modprobe.d/vfio-noiommu.conf
      modprobe vfio enable_unsafe_noiommu_mode=1
      modprobe vfio-pci
      echo "\$VF_ADDR" > /sys/bus/pci/devices/"\$VF_ADDR"/driver/unbind || true
      echo vfio-pci > /sys/bus/pci/devices/"\$VF_ADDR"/driver_override
      for i in \$(seq 1 20); do
        echo "\$VF_ADDR" > /sys/bus/pci/drivers/vfio-pci/bind 2>/dev/null && break
        sleep 0.5
      done

      # 1536 * 2Mi = 3Gi of hugepages headroom for whatever dataplane workload
      # will later claim this VF.
      echo 1536 > /proc/sys/vm/nr_hugepages
      mountpoint -q /dev/hugepages || mount -t hugetlbfs hugetlbfs /dev/hugepages
runcmd:
  - /usr/local/sbin/configure-sriov.sh
  - curl -sfL https://get.k3s.io | sh -
EOF

cloud-localds seed.img user-data meta-data
