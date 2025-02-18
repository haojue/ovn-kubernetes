package ovn

import (
	"context"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"sync"

	"k8s.io/klog/v2"
)

// SecondaryLocalnetNetworkController is created for logical network infrastructure and policy
// for a secondary localnet network
type SecondaryLocalnetNetworkController struct {
	BaseSecondaryLayer2NetworkController
}

// NewSecondaryLocalnetNetworkController create a new OVN controller for the given secondary localnet NAD
func NewSecondaryLocalnetNetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo,
	netconfInfo util.NetConfInfo) *SecondaryLocalnetNetworkController {
	stopChan := make(chan struct{})

	oc := &SecondaryLocalnetNetworkController{
		BaseSecondaryLayer2NetworkController{
			BaseSecondaryNetworkController: BaseSecondaryNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					NetConfInfo:                 netconfInfo,
					NetInfo:                     netInfo,
					lsManager:                   lsm.NewL2SwitchManager(),
					logicalPortCache:            newPortCache(stopChan),
					namespaces:                  make(map[string]*namespaceInfo),
					namespacesMutex:             sync.Mutex{},
					addressSetFactory:           addressset.NewOvnAddressSetFactory(cnci.nbClient),
					stopChan:                    stopChan,
					wg:                          &sync.WaitGroup{},
				},
			},
		},
	}

	// disable multicast support for secondary networks
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc
}

// Start starts the secondary localnet controller, handles all events and creates all needed logical entities
func (oc *SecondaryLocalnetNetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	if err := oc.Init(); err != nil {
		return err
	}

	return oc.Run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLocalnetNetworkController) Cleanup(netName string) error {
	return oc.cleanup(types.LocalnetTopology, netName)
}

func (oc *SecondaryLocalnetNetworkController) Init() error {
	switchName := oc.GetNetworkScopedName(types.OVNLocalnetSwitch)
	localnetNetConfInfo := oc.NetConfInfo.(*util.LocalnetNetConfInfo)

	logicalSwitch, err := oc.InitializeLogicalSwitch(switchName, localnetNetConfInfo.ClusterSubnets, localnetNetConfInfo.ExcludeSubnets)
	if err != nil {
		return err
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      oc.GetNetworkScopedName(types.OVNLocalnetPort),
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": oc.GetNetworkScopedName(types.LocalNetBridgeName),
		},
	}
	if localnetNetConfInfo.VLANID != 0 {
		intVlanID := localnetNetConfInfo.VLANID
		logicalSwitchPort.TagRequest = &intVlanID
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, logicalSwitch, &logicalSwitchPort)
	if err != nil {
		klog.Errorf("Failed to add logical port %+v to switch %s: %v", logicalSwitchPort, switchName, err)
		return err
	}

	return nil
}
