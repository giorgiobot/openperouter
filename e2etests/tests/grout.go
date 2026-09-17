// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/infra"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

type groutInterface struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

var _ = Describe("Grout datapath", GroutSupport, Ordered, func() {
	var cs clientset.Interface
	var routers openperouter.Routers

	vni := v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VNISpec{
			VRF: "red",
			HostSession: &v1alpha1.HostSession{
				ASN:        64514,
				HostASN:    new(int64(64515)),
				LocalCIDRs: []string{"192.169.10.0/24"},
			},
			VNI: 100,
		},
	}

	BeforeAll(func() {
		if !GroutMode {
			Skip("Skipping grout datapath tests in non-grout mode")
		}

		err := Updater.CleanAll()
		Expect(err).NotTo(HaveOccurred())

		cs = k8sclient.New()
		Eventually(func() error {
			routers, err = openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())

		err = Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{
				infra.Underlay,
			},
			L3VNIs: []v1alpha1.L3VNI{vni},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			routers, err = openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())

		for router := range routers.GetExecutors() {
			validateSessionWithNeighbor(router, validationParameters{
				fromName:    router.Name(),
				toName:      "leafKind",
				neighborIP:  "192.168.11.2",
				established: Established,
			})
		}
	})

	AfterAll(func() {
		err := Updater.CleanAll()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		dumpIfFails(cs)
	})

	It("should have grout interfaces configured", func() {
		for router := range routers.GetExecutors() {
			By("checking grout interfaces on " + router.Name())
			out, err := router.Exec("grcli", "--err-exit", "--json", "interface", "show")
			Expect(err).NotTo(HaveOccurred(), "grcli interface show failed on %s: %s", router.Name(), out)

			var ifaces []groutInterface
			Expect(json.Unmarshal([]byte(out), &ifaces)).To(Succeed(), "failed to parse grcli output on %s", router.Name())

			types := map[string]bool{}
			for _, iface := range ifaces {
				types[iface.Type] = true
			}

			Expect(types).To(HaveKey("vrf"), "no grout VRF found on %s", router.Name())
			Expect(types).To(HaveKey("vxlan"), "no grout VXLAN found on %s", router.Name())
			Expect(types).To(HaveKey("port"), "no grout port found on %s", router.Name())
		}
	})

	It("should have routes installed in grout and not in the kernel", func() {
		for router := range routers.GetExecutors() {
			By("checking grout has routes for VRF " + vni.Spec.VRF + " on " + router.Name())
			out, err := router.Exec("grcli", "--err-exit", "--json", "route", "show", "vrf", vni.Spec.VRF, "ipv4")
			Expect(err).NotTo(HaveOccurred(), "grcli route show failed on %s: %s", router.Name(), out)

			var routes []json.RawMessage
			Expect(json.Unmarshal([]byte(out), &routes)).To(Succeed(), "failed to parse grcli route output on %s", router.Name())
			Expect(routes).NotTo(BeEmpty(), "no grout routes found in VRF %s on %s", vni.Spec.VRF, router.Name())

			By("checking kernel has no BGP routes for VRF " + vni.Spec.VRF + " on " + router.Name())
			out, err = router.Exec("ip", "route", "show", "vrf", vni.Spec.VRF, "proto", "bgp")
			if err != nil && strings.Contains(out, "does not exist") {
				continue
			}
			Expect(err).NotTo(HaveOccurred(), "ip route show failed on %s: %s", router.Name(), out)
			Expect(strings.TrimSpace(out)).To(BeEmpty(),
				"kernel BGP routes found in VRF %s on %s, expected grout datapath only", vni.Spec.VRF, router.Name())
		}
	})
})
