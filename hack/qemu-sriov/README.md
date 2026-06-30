# QEMU SR-IOV smoke test

Boots a single-node QEMU VM with an emulated Intel `igb` SR-IOV NIC and 2Mi
hugepages, installs k3s on it, and verifies that the SR-IOV VFs are created
and the hugepages are reserved. It then deploys openperouter with the
**grout** DPDK dataplane onto that node and smoke-tests that the cluster
stays stable — this is the substrate (real SR-IOV VFs + hugepages) that grout
needs and that the kind-based lanes cannot provide.

## Local prerequisites

- `qemu-system-x86` / `qemu-utils`
- `cloud-image-utils` (for `cloud-localds`)
- access to `/dev/kvm` (KVM acceleration is required)
- `docker` (to build and import the `main-grout` image into the VM's k3s)

On Debian/Ubuntu:

```sh
sudo apt-get install qemu-system-x86 qemu-utils cloud-image-utils
```

## Usage

```sh
make qemu-sriov-test    # up + verify + deploy-grout + smoke + underlay-test + down
# or step by step:
make qemu-sriov-up
make qemu-sriov-verify
make qemu-sriov-deploy-grout
make qemu-sriov-smoke
make qemu-sriov-underlay-test
make qemu-sriov-down
```

`make qemu-sriov-up` downloads/builds the VM and waits for the k3s node to
become `Ready`. Once it returns, the node's kubeconfig is available at
`hack/qemu-sriov/kubeconfig`:

```sh
export KUBECONFIG=hack/qemu-sriov/kubeconfig
kubectl get nodes
```

`make qemu-sriov-deploy-grout` builds the `main-grout` image, imports it into
the VM's k3s, prepares the node, and runs `make deploy-grout-helm` against the
kubeconfig above. `make qemu-sriov-smoke` then asserts the node and every
openperouter pod (including the router pod's `grout` container) reach `Ready`
and that nothing crash-loops over a short observation window.

`make qemu-sriov-underlay-test` creates an `Underlay` object targeting the
first SR-IOV VF and checks that the perouter controller moves that NIC out of
the host root netns and into the `perouter` named netns.

`make qemu-sriov-down` powers off the VM and kills the QEMU process.

## Notes

- The cloud-init script installs k3s via `curl https://get.k3s.io` at boot.
- The emulated `igb` PF is brought up and 2 SR-IOV VFs are created; the VFs
  are left on their default in-kernel driver (`grout` runs in `--test-mode`
  and does not claim a `vfio-pci` device on this lane).
- `main-grout` is built from `Dockerfile.grout` and is never published to a
  registry, so it is streamed into the VM's k3s containerd image store and the
  helm release is installed with `pullPolicy=IfNotPresent`.
- k3s keeps its containerd socket at `/run/k3s/containerd/containerd.sock`;
  `prepare-perouter-host.sh` bind-mounts it onto `/run/containerd/containerd.sock`,
  the path the openperouter controller expects.
