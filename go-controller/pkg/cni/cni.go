package cni

import (
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	minRsrc           = resource.MustParse("1k")
	maxRsrc           = resource.MustParse("1P")
	BandwidthNotFound = &notFoundError{}
)

type direction int

func (d direction) String() string {
	if d == Egress {
		return "egress"
	}
	return "ingress"
}

const (
	Egress direction = iota
	Ingress
)

type notFoundError struct{}

func (*notFoundError) Error() string {
	return "not found"
}

func validateBandwidthIsReasonable(rsrc *resource.Quantity) error {
	if rsrc.Value() < minRsrc.Value() {
		return fmt.Errorf("resource is unreasonably small (< 1kbit)")
	}
	if rsrc.Value() > maxRsrc.Value() {
		return fmt.Errorf("resoruce is unreasonably large (> 1Pbit)")
	}
	return nil
}

func extractPodBandwidth(podAnnotations map[string]string, dir direction) (int64, error) {
	annotation := "kubernetes.io/ingress-bandwidth"
	if dir == Egress {
		annotation = "kubernetes.io/egress-bandwidth"
	}

	str, found := podAnnotations[annotation]
	if !found {
		return 0, BandwidthNotFound
	}
	bwVal, err := resource.ParseQuantity(str)
	if err != nil {
		return 0, err
	}
	if err := validateBandwidthIsReasonable(&bwVal); err != nil {
		return 0, err
	}
	return bwVal.Value(), nil
}

func (pr *PodRequest) String() string {
	return fmt.Sprintf("[%s/%s %s]", pr.PodNamespace, pr.PodName, pr.SandboxID)
}

// checkOrUpdatePodUID validates the given pod UID against the request's existing
// pod UID. If the existing UID is empty the runtime did not support passing UIDs
// and the best we can do is use the given UID for the duration of the request.
// But if the existing UID is valid and does not match the given UID then the
// sandbox request is for a different pod instance and should be terminated.
// Static pod UID is a hash of the pod itself that does not match
// the UID of the mirror kubelet creates on the api /server.
// We will use the UID of the mirror.
// The hash is annotated in the mirror pod (kubernetes.io/config.hash)
// and we could match against it, but let's avoid that for now as it is not
// a published standard.
func (pr *PodRequest) checkOrUpdatePodUID(pod *kapi.Pod) error {
	if pr.PodUID == "" || IsStaticPod(pod) {
		// Runtime didn't pass UID, or the pod is a static pod, use the one we got from the pod object
		pr.PodUID = string(pod.UID)
	} else if string(pod.UID) != pr.PodUID {
		// Exit early if the pod was deleted and recreated already
		return fmt.Errorf("pod deleted before sandbox %v operation began", pr.Command)
	}
	return nil
}

func (pr *PodRequest) cmdAdd(kubeAuth *KubeAPIAuth, clientset *ClientSet, useOVSExternalIDs bool) (*Response, error) {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	kubecli := &kube.Kube{KClient: clientset.kclient}
	annotCondFn := isOvnReady
	vfNetdevName := ""
	if pr.CNIConf.DeviceID != "" {
		var err error

		vfNetdevName, err = util.GetNetdevNameFromDeviceId(pr.CNIConf.DeviceID)
		if err != nil {
			return nil, fmt.Errorf("failed in cmdAdd while getting VF Netdevice name: %v", err)
		}
		if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
			// Add DPU connection-details annotation so ovnkube-node running on DPU
			// performs the needed network plumbing.
			if err = pr.addDPUConnectionDetailsAnnot(kubecli, clientset.podLister, vfNetdevName); err != nil {
				return nil, err
			}
			annotCondFn = isDPUReady
		}
		// In the case of SmartNIC (CX5), we store the VFNetdevname in the VF representor's
		// OVS interface's external_id column. This is done in ConfigureInterface().
	}
	// Get the IP address and MAC address of the pod
	// for DPU, ensure connection-details is present
	pod, annotations, podNADAnnotation, err := GetPodWithAnnotations(pr.ctx, clientset, namespace, podName,
		pr.nadName, annotCondFn)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}
	if err = pr.checkOrUpdatePodUID(pod); err != nil {
		return nil, err
	}
	podInterfaceInfo, err := PodAnnotation2PodInfo(annotations, podNADAnnotation, useOVSExternalIDs, pr.PodUID, vfNetdevName,
		pr.nadName, pr.netName, pr.CNIConf.MTU)
	if err != nil {
		return nil, err
	}

	response := &Response{KubeAuth: kubeAuth}
	if !config.UnprivilegedMode {
		response.Result, err = pr.getCNIResult(clientset, podInterfaceInfo)
		if err != nil {
			return nil, err
		}
	} else {
		response.PodIFInfo = podInterfaceInfo
	}

	return response, nil
}

func (pr *PodRequest) cmdDel(clientset *ClientSet) (*Response, error) {
	// assume success case, return an empty Result
	response := &Response{}
	response.Result = &current.Result{}

	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	vfNetdevName := ""
	if pr.CNIConf.DeviceID != "" {
		if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
			pod, err := clientset.getPod(pr.PodNamespace, pr.PodName)
			if err != nil {
				klog.Warningf("Failed to get pod %s/%s: %v", pr.PodNamespace, pr.PodName, err)
				return response, nil
			}
			dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, pr.nadName)
			if err != nil {
				klog.Warningf("Failed to get DPU connection details annotation for pod %s/%s NAD %s: %v", pr.PodNamespace,
					pr.PodName, pr.nadName, err)
				return response, nil
			}
			vfNetdevName = dpuCD.VfNetdevName
		} else {
			// Find the the hostInterface name
			condString := "external-ids:sandbox=" + pr.SandboxID
			if pr.netName != types.DefaultNetworkName {
				condString += fmt.Sprintf(" external_ids:%s=%s", types.NADExternalID, pr.nadName)
			} else {
				condString += fmt.Sprintf(" external_ids:%s{=}[]", types.NADExternalID)
			}
			ovsIfNames, err := ovsFind("Interface", "name", condString)
			if err != nil || len(ovsIfNames) != 1 {
				klog.Warningf("Couldn't find the OVS interface for pod %s/%s NAD %s: %v",
					pr.PodNamespace, pr.PodName, pr.nadName, err)
			} else {
				ovsIfName := ovsIfNames[0]
				out, err := ovsGet("interface", ovsIfName, "external_ids", "vf-netdev-name")
				if err != nil {
					klog.Warningf("Couldn't find the original VF Netdev name from OVS interface %s for pod %s/%s: %v",
						ovsIfName, pr.PodNamespace, pr.PodName, err)
				} else {
					vfNetdevName = out
				}
			}
		}
	}

	podInterfaceInfo := &PodInterfaceInfo{
		IsDPUHostMode: config.OvnKubeNode.Mode == types.NodeModeDPUHost,
		VfNetdevName:  vfNetdevName,
	}
	if !config.UnprivilegedMode {
		err := pr.UnconfigureInterface(podInterfaceInfo)
		if err != nil {
			return nil, err
		}
	} else {
		// pass the isDPU flag and vfNetdevName back to cniShim
		response.Result = nil
		response.PodIFInfo = podInterfaceInfo
	}
	return response, nil
}

func (pr *PodRequest) cmdCheck() error {
	// noop...CMD check is not considered useful, and has a considerable performance impact
	// to pod bring up times with CRIO. This is due to the fact that CRIO currently calls check
	// after CNI ADD before it finishes bringing the container up
	return nil
}

// HandlePodRequest is the callback for all the requests
// coming to the cniserver after being processed into PodRequest objects
// Argument '*PodRequest' encapsulates all the necessary information
// kclient is passed in so that clientset can be reused from the server
// Return value is the actual bytes to be sent back without further processing.
func HandlePodRequest(request *PodRequest, clientset *ClientSet, useOVSExternalIDs bool, kubeAuth *KubeAPIAuth) ([]byte, error) {
	var result, resultForLogging []byte
	var response *Response
	var err, err1 error

	klog.Infof("%s %s starting CNI request %+v", request, request.Command, request)
	switch request.Command {
	case CNIAdd:
		response, err = request.cmdAdd(kubeAuth, clientset, useOVSExternalIDs)
	case CNIDel:
		response, err = request.cmdDel(clientset)
	case CNICheck:
		err = request.cmdCheck()
	default:
	}

	if response != nil {
		if result, err1 = response.Marshal(); err1 != nil {
			return nil, fmt.Errorf("%s %s CNI request %+v failed to marshal result: %v",
				request, request.Command, request, err1)
		}
		if resultForLogging, err1 = response.MarshalForLogging(); err1 != nil {
			klog.Errorf("%s %s CNI request %+v, %v", request, request.Command, request, err1)
		}
	}

	klog.Infof("%s %s finished CNI request %+v, result %q, err %v",
		request, request.Command, request, string(resultForLogging), err)

	if err != nil {
		// Prefix errors with request info for easier failure debugging
		return nil, fmt.Errorf("%s %v", request, err)
	}
	return result, nil
}

// getCNIResult get result from pod interface info.
// PodInfoGetter is used to check if sandbox is still valid for the current
// instance of the pod in the apiserver, see checkCancelSandbox for more info.
// If kube api is not available from the CNI, pass nil to skip this check.
func (pr *PodRequest) getCNIResult(getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := pr.ConfigureInterface(getter, podInterfaceInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to configure pod interface: %v", err)
	}

	gateways := map[string]net.IP{}
	for _, gw := range podInterfaceInfo.Gateways {
		if gw.To4() != nil && gateways["4"] == nil {
			gateways["4"] = gw
		} else if gw.To4() == nil && gateways["6"] == nil {
			gateways["6"] = gw
		}
	}

	// Build the result structure to pass back to the runtime
	ips := []*current.IPConfig{}
	for _, ipcidr := range podInterfaceInfo.IPs {
		ip := &current.IPConfig{
			Interface: current.Int(1),
			Address:   *ipcidr,
		}
		var ipVersion string
		if utilnet.IsIPv6CIDR(ipcidr) {
			ipVersion = "6"
		} else {
			ipVersion = "4"
		}
		ip.Gateway = gateways[ipVersion]
		ips = append(ips, ip)
	}

	return &current.Result{
		Interfaces: interfacesArray,
		IPs:        ips,
	}, nil
}
