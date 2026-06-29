# QEMU SR-IOV smoke test

Boots a single-node QEMU VM with an emulated Intel `igb` SR-IOV NIC and 2Mi
hugepages, installs k3s on it, and verifies that one SR-IOV VF is bound to
`vfio-pci` and the hugepages are reserved. It does **not** deploy the SR-IOV
device plugin, grout, or any peer/dataplane pod — it only proves the
substrate (VFs + hugepages + k3s node) is up and `Ready`.

## Local prerequisites

- `qemu-system-x86` / `qemu-utils`
- `cloud-image-utils` (for `cloud-localds`)
- access to `/dev/kvm` (KVM acceleration is required)

On Debian/Ubuntu:

```sh
sudo apt-get install qemu-system-x86 qemu-utils cloud-image-utils
```

## Usage

```sh
make qemu-sriov-test    # up + verify + down
# or step by step:
make qemu-sriov-up
make qemu-sriov-verify
make qemu-sriov-down
```

`make qemu-sriov-up` downloads/builds the VM and waits for the k3s node to
become `Ready`. Once it returns, the node's kubeconfig is available at
`hack/qemu-sriov/kubeconfig`:

```sh
export KUBECONFIG=hack/qemu-sriov/kubeconfig
kubectl get nodes
```

`make qemu-sriov-down` powers off the VM and kills the QEMU process.

## Notes

- The cloud-init script installs k3s via `curl https://get.k3s.io` at boot.
- One SR-IOV VF is bound to `vfio-pci` using VFIO's unsafe noiommu mode,
  since the guest has no vIOMMU. Both are intentional and required for this
  lane to work.
