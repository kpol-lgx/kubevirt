package network

import (
	"context"
	"fmt"

	expect "github.com/google/goexpect"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"kubevirt.io/kubevirt/tests/util"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/kubevirt/tests"
	"kubevirt.io/kubevirt/tests/console"
	"kubevirt.io/kubevirt/tests/libnet"
	"kubevirt.io/kubevirt/tests/libvmi"
)

const (
	specifyingVMReadinessProbe = "Specifying a VMI with a readiness probe"
	specifyingVMLivenessProbe  = "Specifying a VMI with a liveness probe"
)

var _ = SIGDescribe("[ref_id:1182]Probes", func() {
	var (
		err           error
		virtClient    kubecli.KubevirtClient
		vmi           *v1.VirtualMachineInstance
		blankIPFamily = *new(corev1.IPFamily)
	)

	BeforeEach(func() {
		virtClient, err = kubecli.GetKubevirtClient()
		util.PanicOnError(err)

		tests.BeforeTestCleanup()
	})

	buildProbeBackendPodSpec := func(probe *v1.Probe) (*corev1.Pod, func() error) {
		var probeBackendPod *corev1.Pod
		if isHTTPProbe(*probe) {
			port := probe.HTTPGet.Port.IntVal
			probeBackendPod = tests.StartHTTPServerPod(int(port))
		} else {
			port := probe.TCPSocket.Port.IntVal
			probeBackendPod = tests.StartTCPServerPod(int(port))
		}
		return probeBackendPod, func() error {
			return virtClient.CoreV1().Pods(util.NamespaceTestDefault).Delete(context.Background(), probeBackendPod.Name, metav1.DeleteOptions{})
		}
	}

	Context("for readiness", func() {
		const (
			period         = 5
			initialSeconds = 5
			timeoutSeconds = 1
			port           = 1500
		)

		tcpProbe := createTCPProbe(period, initialSeconds, port)
		httpProbe := createHTTPProbe(period, initialSeconds, port)
		guestAgentPingProbe := createGuestAgentPingProbe(period, initialSeconds)

		isVMIReady := func() bool {
			readVmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(vmi.Name, &metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			return vmiReady(readVmi) == corev1.ConditionTrue
		}

		table.DescribeTable("should succeed", func(readinessProbe *v1.Probe, IPFamily corev1.IPFamily, isExecProbe bool, disableEnableCycle bool) {
			checkStatus := func(ready bool, condition corev1.ConditionStatus, timeout int) {
				By("Checking that the VMI and the pod will be marked as ready to receive traffic")
				Eventually(isVMIReady, timeout, 1).Should(Equal(ready))
				Expect(tests.PodReady(tests.GetRunningPodByVirtualMachineInstance(vmi, util.NamespaceTestDefault))).To(Equal(condition))
			}

			if IPFamily == corev1.IPv6Protocol {
				libnet.SkipWhenNotDualStackCluster(virtClient)
				By("Create a support pod which will reply to kubelet's probes ...")
				probeBackendPod, supportPodCleanupFunc := buildProbeBackendPodSpec(readinessProbe)
				defer func() {
					Expect(supportPodCleanupFunc()).To(Succeed(), "The support pod responding to the probes should be cleaned-up at test tear-down.")
				}()

				By("Attaching the readiness probe to an external pod server")
				readinessProbe, err = pointProbeToSupportPod(probeBackendPod, IPFamily, readinessProbe)
				Expect(err).ToNot(HaveOccurred(), "should attach the backend pod with readiness probe")

				By(specifyingVMReadinessProbe)
				vmi = createReadyCirrosVMIWithReadinessProbe(readinessProbe)
			} else if !isExecProbe {
				By(specifyingVMReadinessProbe)
				vmi = createReadyCirrosVMIWithReadinessProbe(readinessProbe)

				assertPodNotReady(virtClient, vmi)

				By("Starting the server inside the VMI")
				serverStarter(vmi, readinessProbe, 1500)
			} else {
				By(specifyingVMReadinessProbe)
				vmi = tests.NewRandomFedoraVMIWithGuestAgent()
				vmi.Spec.ReadinessProbe = readinessProbe
				vmi = tests.VMILauncherIgnoreWarnings(virtClient)(vmi)

				By("Waiting for agent to connect")
				tests.WaitAgentConnected(virtClient, vmi)
			}

			checkStatus(true, corev1.ConditionTrue, 120)

			if disableEnableCycle {
				By("Disabling the guest-agent")
				Expect(console.LoginToFedora(vmi)).To(Succeed())

				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "sudo systemctl stop qemu-guest-agent -\n"},
					&expect.BExp{R: console.PromptExpression},
				}, 120)).ToNot(HaveOccurred())

				// Marking the status to not ready can take a little more time
				checkStatus(false, corev1.ConditionFalse, 300)

				By("Enabling the guest-agent again")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "sudo systemctl start qemu-guest-agent -\n"},
					&expect.BExp{R: console.PromptExpression},
				}, 120)).ToNot(HaveOccurred())

				checkStatus(true, corev1.ConditionTrue, 120)
			}
		},
			table.Entry("[test_id:1202][posneg:positive]with working TCP probe and tcp server on ipv4", tcpProbe, corev1.IPv4Protocol, false, false),
			table.Entry("[test_id:1202][posneg:positive]with working TCP probe and tcp server on ipv6", tcpProbe, corev1.IPv6Protocol, false, false),
			table.Entry("[test_id:1200][posneg:positive]with working HTTP probe and http server on ipv4", httpProbe, corev1.IPv4Protocol, false, false),
			table.Entry("[test_id:1200][posneg:positive]with working HTTP probe and http server on ipv6", httpProbe, corev1.IPv6Protocol, false, false),
			table.Entry("[test_id:TODO]with working Exec probe", createExecProbe(period, initialSeconds, timeoutSeconds, "uname", "-a"), blankIPFamily, true, false),
			table.Entry("[test_id:6739]with GuestAgentPing", guestAgentPingProbe, blankIPFamily, true, false),
			table.Entry("[test_id:6741]status change with guest-agent availability", guestAgentPingProbe, blankIPFamily, true, true),
		)

		table.DescribeTable("should fail", func(readinessProbe *v1.Probe, vmiFactory func(opts ...libvmi.Option) *v1.VirtualMachineInstance) {
			By(specifyingVMReadinessProbe)
			vmi = vmiFactory()
			vmi.Spec.ReadinessProbe = readinessProbe
			vmi = tests.VMILauncherIgnoreWarnings(virtClient)(vmi)

			By("Checking that the VMI is consistently non-ready")
			Consistently(isVMIReady).Should(Equal(false))
		},
			table.Entry("[test_id:1220][posneg:negative]with working TCP probe and no running server", tcpProbe, libvmi.NewCirros),
			table.Entry("[test_id:1219][posneg:negative]with working HTTP probe and no running server", httpProbe, libvmi.NewCirros),
			table.Entry("[test_id:TODO]with working Exec probe and invalid command", createExecProbe(period, initialSeconds, timeoutSeconds, "exit", "1"), libvmi.NewFedora),
			table.Entry("[test_id:TODO]with working Exec probe and infinitely running command", createExecProbe(period, initialSeconds, timeoutSeconds, "tail", "-f", "/dev/null"), libvmi.NewFedora),
		)
	})

	Context("for liveness", func() {
		const (
			period         = 5
			initialSeconds = 90
			timeoutSeconds = 1
			port           = 1500
		)

		tcpProbe := createTCPProbe(period, initialSeconds, port)
		httpProbe := createHTTPProbe(period, initialSeconds, port)

		table.DescribeTable("should not fail the VMI", func(livenessProbe *v1.Probe, IPFamily corev1.IPFamily, isExecProbe bool) {

			if IPFamily == corev1.IPv6Protocol {
				libnet.SkipWhenNotDualStackCluster(virtClient)

				By("Create a support pod which will reply to kubelet's probes ...")
				probeBackendPod, supportPodCleanupFunc := buildProbeBackendPodSpec(livenessProbe)
				defer func() {
					Expect(supportPodCleanupFunc()).To(Succeed(), "The support pod responding to the probes should be cleaned-up at test tear-down.")
				}()

				By("Attaching the liveness probe to an external pod server")
				livenessProbe, err = pointProbeToSupportPod(probeBackendPod, IPFamily, livenessProbe)
				Expect(err).ToNot(HaveOccurred(), "should attach the backend pod with livness probe")

				By(specifyingVMLivenessProbe)
				vmi = createReadyCirrosVMIWithLivenessProbe(livenessProbe)
			} else if !isExecProbe {
				By(specifyingVMLivenessProbe)
				vmi = createReadyCirrosVMIWithLivenessProbe(livenessProbe)

				By("Starting the server inside the VMI")
				serverStarter(vmi, livenessProbe, 1500)
			} else {
				By(specifyingVMLivenessProbe)
				vmi = tests.NewRandomFedoraVMIWithGuestAgent()
				vmi.Spec.LivenessProbe = livenessProbe
				vmi = tests.VMILauncherIgnoreWarnings(virtClient)(vmi)

				By("Waiting for agent to connect")
				tests.WaitAgentConnected(virtClient, vmi)
			}

			By("Checking that the VMI is still running after a while")
			Consistently(func() bool {
				vmi, err := virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Get(vmi.Name, &metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vmi.IsFinal()
			}, 120, 1).Should(Not(BeTrue()))
		},
			table.Entry("[test_id:1199][posneg:positive]with working TCP probe and tcp server on ipv4", tcpProbe, corev1.IPv4Protocol, false),
			table.Entry("[test_id:1199][posneg:positive]with working TCP probe and tcp server on ipv6", tcpProbe, corev1.IPv6Protocol, false),
			table.Entry("[test_id:1201][posneg:positive]with working HTTP probe and http server on ipv4", httpProbe, corev1.IPv4Protocol, false),
			table.Entry("[test_id:1201][posneg:positive]with working HTTP probe and http server on ipv6", httpProbe, corev1.IPv6Protocol, false),
			table.Entry("[test_id:TODO]with working Exec probe", createExecProbe(period, initialSeconds, timeoutSeconds, "uname", "-a"), blankIPFamily, true),
		)

		table.DescribeTable("should fail the VMI", func(livenessProbe *v1.Probe, vmiFactory func(opts ...libvmi.Option) *v1.VirtualMachineInstance) {
			By("Specifying a VMI with a livenessProbe probe")
			vmi = vmiFactory()
			vmi.Spec.LivenessProbe = livenessProbe
			vmi = tests.VMILauncherIgnoreWarnings(virtClient)(vmi)

			By("Checking that the VMI is in a final state after a while")
			Eventually(func() bool {
				vmi, err := virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Get(vmi.Name, &metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return vmi.IsFinal()
			}, 120, 1).Should(BeTrue())
		},
			table.Entry("[test_id:1217][posneg:negative]with working TCP probe and no running server", tcpProbe, libvmi.NewCirros),
			table.Entry("[test_id:1218][posneg:negative]with working HTTP probe and no running server", httpProbe, libvmi.NewCirros),
			table.Entry("[test_id:TODO]with working Exec probe and invalid command", createExecProbe(period, initialSeconds, timeoutSeconds, "exit", "1"), libvmi.NewFedora),
		)
	})
})

func createReadyCirrosVMIWithReadinessProbe(probe *v1.Probe) *v1.VirtualMachineInstance {
	vmi := libvmi.NewCirros()
	vmi.Spec.ReadinessProbe = probe

	return tests.RunVMIAndExpectLaunchIgnoreWarnings(vmi, 180)
}

func createReadyCirrosVMIWithLivenessProbe(probe *v1.Probe) *v1.VirtualMachineInstance {
	vmi := libvmi.NewCirros()
	vmi.Spec.LivenessProbe = probe

	return tests.RunVMIAndExpectLaunchIgnoreWarnings(vmi, 180)
}

func vmiReady(vmi *v1.VirtualMachineInstance) corev1.ConditionStatus {
	for _, cond := range vmi.Status.Conditions {
		if cond.Type == v1.VirtualMachineInstanceConditionType(corev1.PodReady) {
			return cond.Status
		}
	}
	return corev1.ConditionFalse
}

func assertPodNotReady(virtClient kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance) {
	Expect(tests.PodReady(tests.GetRunningPodByVirtualMachineInstance(vmi, util.NamespaceTestDefault))).To(Equal(corev1.ConditionFalse))
	readVmi, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(vmi.Name, &metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	Expect(vmiReady(readVmi)).To(Equal(corev1.ConditionFalse))
}

func createTCPProbe(period int32, initialSeconds int32, port int) *v1.Probe {
	httpHandler := v1.Handler{
		TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromInt(port),
		},
	}
	return createProbeSpecification(period, initialSeconds, 1, httpHandler)
}

func createGuestAgentPingProbe(period int32, initialSeconds int32) *v1.Probe {
	handler := v1.Handler{GuestAgentPing: &v1.GuestAgentPing{}}
	return createProbeSpecification(period, initialSeconds, 1, handler)
}

func patchProbeWithIPAddr(existingProbe *v1.Probe, ipHostIP string) *v1.Probe {
	if isHTTPProbe(*existingProbe) {
		existingProbe.HTTPGet.Host = ipHostIP
	} else {
		existingProbe.TCPSocket.Host = ipHostIP
	}
	return existingProbe
}

func createHTTPProbe(period int32, initialSeconds int32, port int) *v1.Probe {
	httpHandler := v1.Handler{
		HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt(port),
		},
	}
	return createProbeSpecification(period, initialSeconds, 1, httpHandler)
}

func createExecProbe(period int32, initialSeconds int32, timeoutSeconds int32, command ...string) *v1.Probe {
	execHandler := v1.Handler{Exec: &corev1.ExecAction{Command: command}}
	return createProbeSpecification(period, initialSeconds, timeoutSeconds, execHandler)
}

func createProbeSpecification(period int32, initialSeconds int32, timeoutSeconds int32, handler v1.Handler) *v1.Probe {
	return &v1.Probe{
		PeriodSeconds:       period,
		InitialDelaySeconds: initialSeconds,
		Handler:             handler,
		TimeoutSeconds:      timeoutSeconds,
	}
}

func isHTTPProbe(probe v1.Probe) bool {
	return probe.Handler.HTTPGet != nil
}

func serverStarter(vmi *v1.VirtualMachineInstance, probe *v1.Probe, port int) {
	if isHTTPProbe(*probe) {
		tests.StartHTTPServer(vmi, port)
	} else {
		tests.StartTCPServer(vmi, port)
	}
}

func pointProbeToSupportPod(pod *corev1.Pod, IPFamily corev1.IPFamily, probe *v1.Probe) (*v1.Probe, error) {
	supportPodIP := libnet.GetPodIpByFamily(pod, IPFamily)
	if supportPodIP == "" {
		return nil, fmt.Errorf("pod's %s %s IP address does not exist", pod.Name, IPFamily)
	}

	return patchProbeWithIPAddr(probe, supportPodIP), nil
}
