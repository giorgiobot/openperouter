// SPDX-License-Identifier:Apache-2.0

package qemu_e2e

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/frr"
	"github.com/openperouter/openperouter/e2etests/pkg/ipfamily"
	"github.com/openperouter/openperouter/e2etests/pkg/networklayerprotocol"
	"github.com/openperouter/openperouter/e2etests/triage"
	corev1 "k8s.io/api/core/v1"
)

type validationParameters struct {
	fromName                string
	toName                  string
	neighborIP              string
	receivedAddressFamilies []networklayerprotocol.NLP
	established             bool
}

func validateSessionWithNeighbor(exec executor.Executor, parameters validationParameters) {
	Eventually(func() error {
		neigh, err := frr.NeighborInfo(parameters.neighborIP, exec)
		if err != nil {
			return err
		}
		if !parameters.established && neigh.BgpState == "Established" {
			return fmt.Errorf("neighbor from %s to %s - %s is established", parameters.fromName, parameters.toName, parameters.neighborIP)
		}
		if parameters.established && neigh.BgpState != "Established" {
			return fmt.Errorf("neighbor %s to %s - %s is not established", parameters.fromName, parameters.toName, parameters.neighborIP)
		}

		if !parameters.established {
			return nil
		}
		for _, expectedReceivedAF := range parameters.receivedAddressFamilies {
			isRxReceived := false
			for pathName, addPath := range neigh.NeighborCapabilities.AddPath {
				if strings.ToLower(pathName) == fmt.Sprintf("%s%s", expectedReceivedAF.AFI, expectedReceivedAF.SAFI) {
					isRxReceived = addPath.RxReceived
					break
				}
			}
			if isRxReceived {
				continue
			}
			return fmt.Errorf("neighbor %s to %s - %s is established but expectedReceivedAF %s not found",
				parameters.fromName, parameters.toName, parameters.neighborIP, expectedReceivedAF)
		}

		return nil
	}, 5*time.Minute, time.Second).ShouldNot(HaveOccurred())
}

func waitForType5Route(exec executor.Executor, prefix string) {
	Eventually(func() error {
		evpn, err := frr.EVPNInfo(exec)
		if err != nil {
			return err
		}
		if !evpn.ContainsType5Prefix(prefix) {
			return fmt.Errorf("Type-5 route for %s not yet present", prefix)
		}
		return nil
	}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
}

func l3vniRoutingDomain(name string) *v1alpha1.RoutingDomain {
	return &v1alpha1.RoutingDomain{
		Type:  v1alpha1.RoutingDomainTypeL3VNI,
		L3VNI: &v1alpha1.L3VNIReference{Name: name},
	}
}

type groutInterface struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func canPingFromPod(exec executor.Executor, ip string) {
	ginkgo.GinkgoHelper()
	Eventually(func(g Gomega) {
		ginkgo.By(fmt.Sprintf("pinging %s via net1", ip))
		out, err := exec.Exec("ping", "-c", "1", "-W", "2", "-I", "net1", ip)
		g.Expect(err).ToNot(HaveOccurred(), "ping to %s failed: %s", ip, out)
	}).
		WithTimeout(40 * time.Second).
		WithPolling(time.Second).
		Should(Succeed())
}

func removeGatewayFromPod(pod *corev1.Pod) error {
	exec := executor.ForPod(pod.Namespace, pod.Name, "agnhost")

	var podIPs []string
	for _, podIP := range pod.Status.PodIPs {
		podIPs = append(podIPs, podIP.IP)
	}

	family, err := ipfamily.ForAddresses(podIPs...)
	if err != nil {
		return fmt.Errorf("failed to detect IP family for pod %s: %w", pod.Name, err)
	}

	if family == ipfamily.IPv4 || family == ipfamily.DualStack {
		output, err := exec.Exec("ip", "route", "del", "default", "dev", "eth0")
		if err != nil {
			return fmt.Errorf("failed to remove ipv4 gateway from pod %s: %s: %w", pod.Name, output, err)
		}
	}

	if family == ipfamily.IPv6 || family == ipfamily.DualStack {
		nextHopIPv6, err := findNextHopIPv6(exec, "default", "eth0")
		if err != nil {
			return fmt.Errorf("failed to find IPv6 next hop for pod %s: %w", pod.Name, err)
		}
		output, err := exec.Exec("ip", "-6", "route", "del", "default", "via", nextHopIPv6, "dev", "eth0")
		if err != nil {
			return fmt.Errorf("failed to remove ipv6 gateway from pod %s: %s: %w", pod.Name, output, err)
		}
	}

	return nil
}

func findNextHopIPv6(exec executor.Executor, destination, device string) (string, error) {
	output, err := exec.Exec("ip", "-6", "route", "show", destination)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(fmt.Sprintf(`via +([0-9a-fA-F:]+) dev %s`, device))
	match := re.FindStringSubmatch(output)
	if len(match) == 0 {
		return "", fmt.Errorf("cannot extract ipv6 default gateway for dev eth0 from output: %s", output)
	}
	return strings.TrimSpace(match[1]), nil
}

func DumpPods(name string, pods []*corev1.Pod) {
	triage.DumpPods(name, pods)
}
