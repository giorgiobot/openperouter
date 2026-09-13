// SPDX-License-Identifier:Apache-2.0

package qemu_e2e

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/frr"
	"github.com/openperouter/openperouter/e2etests/pkg/infra"
	"github.com/openperouter/openperouter/e2etests/pkg/k8s"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// Trunk VF identifiers in the QEMU VM (2nd igb NIC, bound to grout via DPDK).
const (
	trunkVFPCI         = "0000:02:00.0"
	trunkVFNetlinkName = "toswitch1v1"
)

var _ = Describe("QEMU L2VNI VF-to-VF", Ordered, QEMUSupport, GroutSupport, func() {
	var cs clientset.Interface
	var routerPods []*corev1.Pod
	var nodes []corev1.Node

	qemuUnderlay := AcceleratedUnderlay

	l3vniRed := v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VNISpec{
			VRF: "red",
			VNI: 100,
			HostSession: &v1alpha1.HostSession{
				ASN:        64514,
				HostASN:    new(int64(64515)),
				LocalCIDRs: []string{"192.169.10.0/24"},
			},
		},
	}

	l2vniA := v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-a",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L2VNISpec{
			VNI:           110,
			RoutingDomain: l3vniRoutingDomain("red"),
			GatewayIPs:    []string{"10.110.0.1/24"},
			SRIOVVFPair: &v1alpha1.SRIOVVFPairConfig{
				PCIAddress: new(trunkVFPCI),
				VLAN:       33,
			},
		},
	}

	l2vniB := v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-b",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L2VNISpec{
			VNI:           120,
			RoutingDomain: l3vniRoutingDomain("red"),
			GatewayIPs:    []string{"10.120.0.1/24"},
			SRIOVVFPair: &v1alpha1.SRIOVVFPairConfig{
				PCIAddress: new(trunkVFPCI),
				VLAN:       44,
			},
		},
	}

	BeforeAll(func() {
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty(), "no router pods found")
		DumpPods("Router pods", routerPods)

		nodes, err = k8s.GetNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		By("Creating accelerated underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuUnderlay},
		})).To(Succeed())

		By("Verifying BGP sessions with leafkind1")
		leafExec := executor.ForContainer(infra.KindLeaf)
		for _, node := range nodes {
			neighborIP, err := infra.NeighborIP(infra.KindLeaf, node.Name)
			Expect(err).NotTo(HaveOccurred())
			validateSessionWithNeighbor(leafExec, validationParameters{
				fromName:    infra.KindLeaf,
				toName:      node.Name,
				neighborIP:  neighborIP,
				established: Established,
			})
		}
	})

	AfterAll(func() {
		Expect(Updater.CleanAll()).To(Succeed())
		Eventually(func() error {
			routers, err := openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		dumpIfFails(cs)
	})

	It("should create L2VNIs with VF-pair configuration", func() {
		By("Creating L3VNI red and L2VNIs with sriovVFPair")
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{l3vniRed},
			L2VNIs: []v1alpha1.L2VNI{l2vniA, l2vniB},
		})).To(Succeed())
	})

	It("should not have host-side bridges (no br-hs-* for VF-pair L2VNIs)", func() {
		for _, node := range nodes {
			exec := executor.ForNode(node.Name)
			out, err := exec.Exec("ip", "link", "show", "type", "bridge")
			Expect(err).NotTo(HaveOccurred(), "ip link show failed on %s: %s", node.Name, out)
			Expect(out).NotTo(ContainSubstring("br-hs-110"),
				"host bridge br-hs-110 should not exist with VF-pair mode on %s", node.Name)
			Expect(out).NotTo(ContainSubstring("br-hs-120"),
				"host bridge br-hs-120 should not exist with VF-pair mode on %s", node.Name)
		}
	})

	It("should have VXLAN interfaces for both VNIs in grout", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				if err != nil {
					return fmt.Errorf("grcli interface show failed on %s: %s", pod.Name, out)
				}

				var ifaces []groutInterface
				if err := json.Unmarshal([]byte(out), &ifaces); err != nil {
					return fmt.Errorf("failed to parse grcli output on %s: %w", pod.Name, err)
				}

				var hasVxlan110, hasVxlan120 bool
				for _, iface := range ifaces {
					if iface.Type != "vxlan" {
						continue
					}
					if iface.Name == "vni110" {
						hasVxlan110 = true
					}
					if iface.Name == "vni120" {
						hasVxlan120 = true
					}
				}

				if !hasVxlan110 {
					return fmt.Errorf("VXLAN vni110 not found on %s", pod.Name)
				}
				if !hasVxlan120 {
					return fmt.Errorf("VXLAN vni120 not found on %s", pod.Name)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should have EVPN VNIs provisioned in VRF red", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			for _, vni := range []int{110, 120} {
				Eventually(func() error {
					info, err := frr.EVPNVNIStatus(exec, vni)
					if err != nil {
						return fmt.Errorf(
							"failed to get EVPN VNI %d status on %s: %w",
							vni, pod.Name, err,
						)
					}
					if info == nil {
						return fmt.Errorf("EVPN VNI %d not provisioned on %s", vni, pod.Name)
					}
					if info.TenantVrf != "red" {
						return fmt.Errorf(
							"EVPN VNI %d on %s is in VRF %q, expected %q",
							vni, pod.Name, info.TenantVrf, "red",
						)
					}
					return nil
				}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
			}
		}
	})
})
