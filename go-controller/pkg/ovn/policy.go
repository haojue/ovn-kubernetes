package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type netpolDefaultDenyACLType string

const (
	// netpolDefaultDenyACLType is used to distinguish default deny and arp allow acls create for the same port group
	defaultDenyACL netpolDefaultDenyACLType = "defaultDeny"
	arpAllowACL    netpolDefaultDenyACLType = "arpAllow"
	// port groups suffixes
	// ingressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	ingressDefaultDenySuffix = "ingressDefaultDeny"
	// egressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	egressDefaultDenySuffix = "egressDefaultDeny"
	// arpAllowPolicyMatch is the match used when creating default allow ARP ACLs for a namespace
	arpAllowPolicyMatch   = "(arp || nd)"
	allowHairpinningACLID = "allow-hairpinning"
	// ovnStatelessNetPolAnnotationName is an annotation on K8s Network Policy resource to specify that all
	// the resulting OVN ACLs must be created as stateless
	ovnStatelessNetPolAnnotationName = "k8s.ovn.org/acl-stateless"
)

// defaultDenyPortGroups is a shared object and should be used by only 1 thread at a time
type defaultDenyPortGroups struct {
	// portName: map[portName]sets.String(policyNames)
	// store policies that are using every port in the map
	// these maps should be atomically updated with db operations
	// if adding a port to db for a policy fails, map shouldn't be changed
	ingressPortToPolicies map[string]sets.Set[string]
	egressPortToPolicies  map[string]sets.Set[string]
	// policies is a map of policies that use this port group
	// policy keys must be unique, and it can be retrieved with (np *networkPolicy) getKey()
	policies map[string]bool
}

// addPortsForPolicy adds port-policy association for default deny port groups and
// returns lists of new ports to add to the default deny port groups.
// If port should be added to ingress and/or egress default deny port group depends on policy spec.
func (sharedPGs *defaultDenyPortGroups) addPortsForPolicy(np *networkPolicy,
	portNamesToUUIDs map[string]string) (ingressDenyPorts, egressDenyPorts []string) {
	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}

	if np.isIngress {
		for portName, portUUID := range portNamesToUUIDs {
			// if this is the first NP referencing this pod, then we
			// need to add it to the port group.
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 {
				ingressDenyPorts = append(ingressDenyPorts, portUUID)
				sharedPGs.ingressPortToPolicies[portName] = sets.Set[string]{}
			}
			// increment the reference count.
			sharedPGs.ingressPortToPolicies[portName].Insert(np.getKey())
		}
	}
	if np.isEgress {
		for portName, portUUID := range portNamesToUUIDs {
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 {
				// again, reference count is 0, so add to port
				egressDenyPorts = append(egressDenyPorts, portUUID)
				sharedPGs.egressPortToPolicies[portName] = sets.Set[string]{}
			}
			// bump reference count
			sharedPGs.egressPortToPolicies[portName].Insert(np.getKey())
		}
	}
	return
}

// deletePortsForPolicy deletes port-policy association for default deny port groups,
// and returns lists of port UUIDs to delete from the default deny port groups.
// If port should be deleted from ingress and/or egress default deny port group depends on policy spec.
func (sharedPGs *defaultDenyPortGroups) deletePortsForPolicy(np *networkPolicy,
	portNamesToUUIDs map[string]string) (ingressDenyPorts, egressDenyPorts []string) {
	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}

	if np.isIngress {
		for portName, portUUID := range portNamesToUUIDs {
			// Delete and Len can be used for zero-value nil set
			sharedPGs.ingressPortToPolicies[portName].Delete(np.getKey())
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 {
				ingressDenyPorts = append(ingressDenyPorts, portUUID)
				delete(sharedPGs.ingressPortToPolicies, portName)
			}
		}
	}
	if np.isEgress {
		for portName, portUUID := range portNamesToUUIDs {
			sharedPGs.egressPortToPolicies[portName].Delete(np.getKey())
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 {
				egressDenyPorts = append(egressDenyPorts, portUUID)
				delete(sharedPGs.egressPortToPolicies, portName)
			}
		}
	}
	return
}

type networkPolicy struct {
	// For now networkPolicy has
	// 3 types of global events (those use oc.networkPolicies to get networkPolicy object)
	// 1. Create network policy - create networkPolicy resources,
	// enable local events, and Update namespace loglevel event
	// 2. Update namespace loglevel - update ACLs for defaultDenyPortGroups and portGroup
	// 3. Delete network policy - disable local events, and Update namespace loglevel event,
	// send deletion signal to already running event handlers, delete resources
	//
	// 2 types of local events (those use the same networkPolicy object there were created for):
	// 1. localPod events - update portGroup, defaultDenyPortGroups and localPods
	// 2. peerNamespace events - add/delete gressPolicy address set, update ACLs for portGroup
	//
	// Delete network policy conflict with all other handlers, therefore we need to make sure it only runs
	// when no other handlers are executing, and that no other handlers will try to work with networkPolicy after
	// Delete network policy was called. This can be done with RWLock, if Delete network policy takes Write lock
	// and sets deleted field to true, and all other handlers take RLock and return immediately if deleted is true.
	// Create network Policy can also take Write lock while it is creating required resources.
	//
	// The only other conflict between handlers here is Update namespace loglevel and peerNamespace, since they both update
	// portGroup ACLs, but this conflict is handled with namespace lock, because both these functions need to lock
	// namespace to create/update ACLs with correct loglevel.
	//
	// We also need to make sure handlers of the same type can be executed in parallel, if this is not true, every
	// event handler can have it own additional lock to sync handlers of the same type.
	//
	// Allowed order of locking is namespace Lock -> oc.networkPolicies key Lock -> networkPolicy.Lock
	// Don't take namespace Lock while holding networkPolicy key lock to avoid deadlock.
	// Don't take RLock from the same goroutine twice, it can lead to deadlock.
	sync.RWMutex

	name            string
	namespace       string
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	isIngress       bool
	isEgress        bool

	// network policy owns only 1 local pod handler
	localPodHandler *factory.Handler
	// peer namespace handlers
	nsHandlerList []*factory.Handler
	// peerAddressSets stores PodSelectorAddressSet keys for peers that this network policy was successfully added to.
	// Required for cleanup.
	peerAddressSets []string

	// localPods is a map of pods affected by this policy.
	// It is used to update defaultDeny port group port counters, when deleting network policy.
	// Port should only be added here if it was successfully added to default deny port group,
	// and local port group in db.
	// localPods may be updated by multiple pod handlers at the same time,
	// therefore it uses a sync map to handle simultaneous access.
	// map of portName(string): portUUID(string)
	localPods sync.Map

	portGroupName string
	// this is a signal for related event handlers that they are/should be stopped.
	// it will be set to true before any networkPolicy infrastructure is deleted,
	// therefore every handler can either do its work and be sure all required resources are there,
	// or this value will be set to true and handler can't proceed.
	// Use networkPolicy.RLock to read this field and hold it for the whole event handling.
	deleted bool
}

func NewNetworkPolicy(policy *knet.NetworkPolicy) *networkPolicy {
	policyTypeIngress, policyTypeEgress := getPolicyType(policy)
	np := &networkPolicy{
		name:            policy.Name,
		namespace:       policy.Namespace,
		ingressPolicies: make([]*gressPolicy, 0),
		egressPolicies:  make([]*gressPolicy, 0),
		isIngress:       policyTypeIngress,
		isEgress:        policyTypeEgress,
		nsHandlerList:   make([]*factory.Handler, 0),
		localPods:       sync.Map{},
	}
	return np
}

func (oc *DefaultNetworkController) syncNetworkPolicies(networkPolicies []interface{}) error {
	// find network policies that don't exist in k8s anymore, but still present in the dbs, and cleanup.
	// Peer address sets and network policy's port groups (together with acls) will be cleaned up.
	// Delete port groups with acls first, since address sets may be referenced in these acls, and
	// cause SyntaxError in ovn-controller, if address sets deleted first, but acls still reference them.
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range networkPolicies {
		policy, ok := npInterface.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("spurious object in syncNetworkPolicies: %v", npInterface)
		}
		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}
	}

	// cleanup port groups based on acl search
	// netpol-owned port groups first
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicy, oc.controllerName, nil)
	p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	netpolACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("cannot find NetworkPolicy ACLs: %v", err)
	}
	stalePGs := sets.Set[string]{}
	for _, netpolACL := range netpolACLs {
		// policy-owned acl
		namespace, policyName, err := parseACLPolicyKey(netpolACL.ExternalIDs[libovsdbops.ObjectNameKey.String()])
		if err != nil {
			return fmt.Errorf("failed to sync stale network policies: acl IDs parsing failed: %w", err)
		}
		if !expectedPolicies[namespace][policyName] {
			// policy doesn't exist on k8s, cleanup
			portGroupName, _ := getNetworkPolicyPGName(namespace, policyName)
			stalePGs.Insert(portGroupName)
		}
	}
	// default deny port groups
	predicateIDs = libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, oc.controllerName, nil)
	p = libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	netpolACLs, err = libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	if err != nil {
		return fmt.Errorf("cannot find default deny NetworkPolicy ACLs: %v", err)
	}
	for _, netpolACL := range netpolACLs {
		// default deny acl
		namespace := netpolACL.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if _, ok := expectedPolicies[namespace]; !ok {
			// no policies in that namespace are found, delete default deny port group
			stalePGs.Insert(defaultDenyPortGroupName(namespace, ingressDefaultDenySuffix))
			stalePGs.Insert(defaultDenyPortGroupName(namespace, egressDefaultDenySuffix))
		}
	}
	if len(stalePGs) > 0 {
		sets.List[string](stalePGs)
		err = libovsdbops.DeletePortGroups(oc.nbClient, sets.List[string](stalePGs)...)
		if err != nil {
			return fmt.Errorf("error removing stale port groups %v: %v", stalePGs, err)
		}
		klog.Infof("Network policy sync cleaned up %d stale port groups", len(stalePGs))
	}

	// add default hairpin allow acl
	err = oc.addHairpinAllowACL()
	if err != nil {
		return fmt.Errorf("failed to create allow hairping acl: %w", err)
	}

	return nil
}

func getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNode, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nodeName,
			libovsdbops.IpKey:         mgmtPortIP,
		})
}

// There is no delete function for this ACL type, because the ACL is applied on a node switch.
// When the node is deleted, switch will be deleted by the node sync, and the dependent ACLs will be
// garbage-collected.
func (oc *DefaultNetworkController) addAllowACLFromNode(nodeName string, mgmtPortIP net.IP) error {
	ipFamily := "ip4"
	if utilnet.IsIPv6(mgmtPortIP) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())
	dbIDs := getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP.String(), oc.controllerName)
	nodeACL := BuildACL(dbIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportIngress)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to create or update ACL %v: %v", nodeACL, err)
	}

	ops, err = libovsdbops.AddACLsToLogicalSwitchOps(oc.nbClient, ops, nodeName, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL %v to switch %s: %v", nodeACL, nodeName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func (oc *DefaultNetworkController) getDefaultDenyPolicyACLIDs(ns string, aclDir aclDirection,
	defaultACLType netpolDefaultDenyACLType) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
			// in the same namespace there can be 2 default deny port groups, egress and ingress,
			// every port group has default deny and arp allow acl.
			libovsdbops.PolicyDirectionKey: string(aclDir),
			libovsdbops.TypeKey:            string(defaultACLType),
		})
}

func defaultDenyPortGroupName(namespace, gressSuffix string) string {
	return hashedPortGroup(namespace) + "_" + gressSuffix
}

func (oc *DefaultNetworkController) buildDenyACLs(namespace, pg string, aclLogging *ACLLoggingLevels,
	aclDir aclDirection) (denyACL, allowACL *nbdb.ACL) {
	denyMatch := getACLMatch(pg, "", aclDir)
	allowMatch := getACLMatch(pg, arpAllowPolicyMatch, aclDir)
	aclPipeline := aclDirectionToACLPipeline(aclDir)

	denyACL = BuildACL(oc.getDefaultDenyPolicyACLIDs(namespace, aclDir, defaultDenyACL),
		types.DefaultDenyPriority, denyMatch, nbdb.ACLActionDrop, aclLogging, aclPipeline)
	allowACL = BuildACL(oc.getDefaultDenyPolicyACLIDs(namespace, aclDir, arpAllowACL),
		types.DefaultAllowPriority, allowMatch, nbdb.ACLActionAllow, nil, aclPipeline)
	return
}

func (oc *DefaultNetworkController) addPolicyToDefaultPortGroups(np *networkPolicy, aclLogging *ACLLoggingLevels) error {
	return oc.sharedNetpolPortGroups.DoWithLock(np.namespace, func(pgKey string) error {
		sharedPGs, loaded := oc.sharedNetpolPortGroups.LoadOrStore(pgKey, &defaultDenyPortGroups{
			ingressPortToPolicies: map[string]sets.Set[string]{},
			egressPortToPolicies:  map[string]sets.Set[string]{},
			policies:              map[string]bool{},
		})
		if !loaded {
			// create port groups with acls
			err := oc.createDefaultDenyPGAndACLs(np.namespace, np.name, aclLogging)
			if err != nil {
				oc.sharedNetpolPortGroups.Delete(pgKey)
				return fmt.Errorf("failed to create default deny port groups: %v", err)
			}
		}
		sharedPGs.policies[np.getKey()] = true
		return nil
	})
}

func (oc *DefaultNetworkController) delPolicyFromDefaultPortGroups(np *networkPolicy) error {
	return oc.sharedNetpolPortGroups.DoWithLock(np.namespace, func(pgKey string) error {
		sharedPGs, found := oc.sharedNetpolPortGroups.Load(pgKey)
		if !found {
			return nil
		}
		delete(sharedPGs.policies, np.getKey())
		if len(sharedPGs.policies) == 0 {
			// last policy was deleted, delete port group
			err := oc.deleteDefaultDenyPGAndACLs(np.namespace)
			if err != nil {
				return fmt.Errorf("failed to delete defaul deny port group: %v", err)
			}
			oc.sharedNetpolPortGroups.Delete(pgKey)
		}
		return nil
	})
}

// createDefaultDenyPGAndACLs creates the default port groups and acls for a namespace
// must be called with defaultDenyPortGroups lock
func (oc *DefaultNetworkController) createDefaultDenyPGAndACLs(namespace, policy string, aclLogging *ACLLoggingLevels) error {
	ingressPGName := defaultDenyPortGroupName(namespace, ingressDefaultDenySuffix)
	ingressDenyACL, ingressAllowACL := oc.buildDenyACLs(namespace, ingressPGName, aclLogging, aclIngress)
	egressPGName := defaultDenyPortGroupName(namespace, egressDefaultDenySuffix)
	egressDenyACL, egressAllowACL := oc.buildDenyACLs(namespace, egressPGName, aclLogging, aclEgress)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, ingressDenyACL, ingressAllowACL, egressDenyACL, egressAllowACL)
	if err != nil {
		return err
	}

	ingressPG := libovsdbops.BuildPortGroup(ingressPGName, ingressPGName, nil, []*nbdb.ACL{ingressDenyACL, ingressAllowACL})
	egressPG := libovsdbops.BuildPortGroup(egressPGName, egressPGName, nil, []*nbdb.ACL{egressDenyACL, egressAllowACL})
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, ingressPG, egressPG)
	if err != nil {
		return err
	}

	recordOps, txOkCallBack, _, err := oc.AddConfigDurationRecord("networkpolicy", namespace, policy)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}
	txOkCallBack()

	return nil
}

// deleteDefaultDenyPGAndACLs deletes the default port groups and acls for a namespace
// must be called with defaultDenyPortGroups lock
func (oc *DefaultNetworkController) deleteDefaultDenyPGAndACLs(namespace string) error {
	ingressPGName := defaultDenyPortGroupName(namespace, ingressDefaultDenySuffix)
	egressPGName := defaultDenyPortGroupName(namespace, egressDefaultDenySuffix)

	ops, err := libovsdbops.DeletePortGroupsOps(oc.nbClient, nil, ingressPGName, egressPGName)
	if err != nil {
		return err
	}
	// No need to delete ACLs, since they will be garbage collected with deleted port groups
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact deleteDefaultDenyPGAndACLs: %v", err)
	}

	return nil
}

// must be called with namespace lock
func (oc *DefaultNetworkController) updateACLLoggingForPolicy(np *networkPolicy, aclLogging *ACLLoggingLevels) error {
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}

	// Predicate for given network policy ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicy, oc.controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: getACLPolicyKey(np.namespace, np.name),
	})
	p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	return UpdateACLLoggingWithPredicate(oc.nbClient, p, aclLogging)
}

func (oc *DefaultNetworkController) updateACLLoggingForDefaultACLs(ns string, nsInfo *namespaceInfo) error {
	return oc.sharedNetpolPortGroups.DoWithLock(ns, func(pgKey string) error {
		_, loaded := oc.sharedNetpolPortGroups.Load(pgKey)
		if !loaded {
			// shared port group doesn't exist, nothing to update
			return nil
		}
		predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, oc.controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: ns,
				libovsdbops.TypeKey:       string(defaultDenyACL),
			})
		p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
		defaultDenyACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
		if err != nil {
			return fmt.Errorf("failed to find netpol default deny acls for namespace %s: %v", ns, err)
		}
		if err := UpdateACLLogging(oc.nbClient, defaultDenyACLs, &nsInfo.aclLogging); err != nil {
			return fmt.Errorf("unable to update ACL logging for namespace %s: %w", ns, err)
		}
		return nil
	})
}

// handleNetPolNamespaceUpdate should update all network policies related to given namespace.
// Must be called with namespace Lock, should be retriable
func (oc *DefaultNetworkController) handleNetPolNamespaceUpdate(namespace string, nsInfo *namespaceInfo) error {
	// update shared port group ACLs
	if err := oc.updateACLLoggingForDefaultACLs(namespace, nsInfo); err != nil {
		return fmt.Errorf("failed to update default deny ACLs for namespace %s: %v", namespace, err)
	}
	// now update network policy specific ACLs
	klog.V(5).Infof("Setting network policy ACLs for ns: %s", namespace)
	for npKey := range nsInfo.relatedNetworkPolicies {
		err := oc.networkPolicies.DoWithLock(npKey, func(key string) error {
			np, found := oc.networkPolicies.Load(npKey)
			if !found {
				klog.Errorf("Netpol was deleted from cache, but not from namespace related objects")
				return nil
			}
			return oc.updateACLLoggingForPolicy(np, &nsInfo.aclLogging)
		})
		if err != nil {
			return fmt.Errorf("unable to update ACL for network policy %s: %v", npKey, err)
		}
		klog.Infof("ACL for network policy: %s, updated to new log level: %s", npKey, nsInfo.aclLogging.Allow)
	}
	return nil
}

// getPolicyType returns whether the policy is of type ingress and/or egress
func getPolicyType(policy *knet.NetworkPolicy) (bool, bool) {
	var policyTypeIngress bool
	var policyTypeEgress bool

	for _, policyType := range policy.Spec.PolicyTypes {
		if policyType == knet.PolicyTypeIngress {
			policyTypeIngress = true
		} else if policyType == knet.PolicyTypeEgress {
			policyTypeEgress = true
		}
	}

	return policyTypeIngress, policyTypeEgress
}

// getNewLocalPolicyPorts will find and return port info for every given pod obj, that is not found in
// np.localPods.
// if there are problems with fetching port info from logicalPortCache, pod will be added to errObjs.
func (oc *DefaultNetworkController) getNewLocalPolicyPorts(np *networkPolicy,
	objs ...interface{}) (policyPortsToUUIDs map[string]string, policyPortUUIDs []string, errObjs []interface{}) {

	policyPortUUIDs = make([]string, 0, len(objs))
	policyPortsToUUIDs = map[string]string{}

	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		if _, ok := np.localPods.Load(logicalPortName); ok {
			// port is already added for this policy
			continue
		}

		if pod.Spec.NodeName == "" {
			// pod is not yet scheduled, will receive update event for it
			continue
		}

		// Skip pods that will never be present in logicalPortCache,
		// e.g. hostNetwork pods, overlay node pods, or completed pods
		if !oc.podExpectedInLogicalCache(pod) {
			continue
		}

		// Add pod to errObjs for retry if
		// 1. getting pod LSP from the cache fails,
		// 2. the gotten LSP is scheduled for removal (stateful-sets).
		portInfo, err := oc.logicalPortCache.get(pod, types.DefaultNetworkName)
		if err != nil {
			klog.Warningf("Failed to get LSP for pod %s/%s for networkPolicy %s, err: %v",
				pod.Namespace, pod.Name, np.name, err)
			errObjs = append(errObjs, pod)
			continue
		}

		// Add pod to errObjs if LSP is scheduled for deletion
		if !portInfo.expires.IsZero() {
			klog.Warningf("Stale LSP %s for network policy %s found in cache",
				portInfo.name, np.name)
			errObjs = append(errObjs, pod)
			continue
		}

		// LSP get succeeded and LSP is up to fresh
		klog.V(5).Infof("Fresh LSP %s for network policy %s found in cache",
			portInfo.name, np.name)

		policyPortUUIDs = append(policyPortUUIDs, portInfo.uuid)
		policyPortsToUUIDs[portInfo.name] = portInfo.uuid
	}
	return
}

// getExistingLocalPolicyPorts will find and return port info for every given pod obj, that is present in np.localPods.
// if there are problems with fetching port info from logicalPortCache, pod will be added to errObjs.
func (oc *DefaultNetworkController) getExistingLocalPolicyPorts(np *networkPolicy,
	objs ...interface{}) (policyPortsToUUIDs map[string]string, policyPortUUIDs []string, errObjs []interface{}) {
	klog.Infof("Processing NetworkPolicy %s/%s to delete %d local pods...", np.namespace, np.name, len(objs))

	policyPortUUIDs = make([]string, 0, len(objs))
	policyPortsToUUIDs = map[string]string{}
	for _, obj := range objs {
		pod := obj.(*kapi.Pod)

		logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
		loadedPortUUID, ok := np.localPods.Load(logicalPortName)
		if !ok {
			// port is already deleted for this policy
			continue
		}
		portUUID := loadedPortUUID.(string)

		policyPortsToUUIDs[logicalPortName] = portUUID
		policyPortUUIDs = append(policyPortUUIDs, portUUID)
	}
	return
}

// denyPGAddPorts adds ports to default deny port groups.
// It also can take existing ops e.g. to add port to network policy port group and transact it.
// It only adds new ports that do not already exist in the deny port groups.
func (oc *DefaultNetworkController) denyPGAddPorts(np *networkPolicy, portNamesToUUIDs map[string]string, ops []ovsdb.Operation) error {
	var err error
	ingressDenyPGName := defaultDenyPortGroupName(np.namespace, ingressDefaultDenySuffix)
	egressDenyPGName := defaultDenyPortGroupName(np.namespace, egressDefaultDenySuffix)

	pgKey := np.namespace
	// this lock guarantees that sharedPortGroup counters will be updated atomically
	// with adding port to port group in db.
	oc.sharedNetpolPortGroups.LockKey(pgKey)
	pgLocked := true
	defer func() {
		if pgLocked {
			oc.sharedNetpolPortGroups.UnlockKey(pgKey)
		}
	}()
	sharedPGs, ok := oc.sharedNetpolPortGroups.Load(pgKey)
	if !ok {
		// Port group doesn't exist
		return fmt.Errorf("port groups for ns %s don't exist", np.namespace)
	}

	ingressDenyPorts, egressDenyPorts := sharedPGs.addPortsForPolicy(np, portNamesToUUIDs)
	// counters were updated, update back to initial values on error
	defer func() {
		if err != nil {
			sharedPGs.deletePortsForPolicy(np, portNamesToUUIDs)
		}
	}()

	if len(ingressDenyPorts) != 0 || len(egressDenyPorts) != 0 {
		// db changes required
		ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, ingressDenyPGName, ingressDenyPorts...)
		if err != nil {
			return fmt.Errorf("unable to get add ports to %s port group ops: %v", ingressDenyPGName, err)
		}

		ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, ops, egressDenyPGName, egressDenyPorts...)
		if err != nil {
			return fmt.Errorf("unable to get add ports to %s port group ops: %v", egressDenyPGName, err)
		}
	} else {
		// shared pg was updated and doesn't require db changes, no need to hold the lock
		oc.sharedNetpolPortGroups.UnlockKey(pgKey)
		pgLocked = false
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("unable to transact add ports to default deny port groups: %v", err)
	}
	return nil
}

// denyPGDeletePorts deletes ports from default deny port groups.
// Set useLocalPods = true, when deleting networkPolicy to remove all its ports from defaultDeny port groups.
// It also can take existing ops e.g. to delete ports from network policy port group and transact it.
func (oc *DefaultNetworkController) denyPGDeletePorts(np *networkPolicy, portNamesToUUIDs map[string]string, useLocalPods bool,
	ops []ovsdb.Operation) error {
	var err error
	if useLocalPods {
		portNamesToUUIDs = map[string]string{}
		np.localPods.Range(func(key, value interface{}) bool {
			portNamesToUUIDs[key.(string)] = value.(string)
			return true
		})
	}
	if len(portNamesToUUIDs) != 0 {
		ingressDenyPGName := defaultDenyPortGroupName(np.namespace, ingressDefaultDenySuffix)
		egressDenyPGName := defaultDenyPortGroupName(np.namespace, egressDefaultDenySuffix)

		pgKey := np.namespace
		// this lock guarantees that sharedPortGroup counters will be updated atomically
		// with adding port to port group in db.
		oc.sharedNetpolPortGroups.LockKey(pgKey)
		pgLocked := true
		defer func() {
			if pgLocked {
				oc.sharedNetpolPortGroups.UnlockKey(pgKey)
			}
		}()
		sharedPGs, ok := oc.sharedNetpolPortGroups.Load(pgKey)
		if !ok {
			// Port group doesn't exist, nothing to clean up
			klog.Infof("Skip delete ports from default deny port group: port group doesn't exist")
		} else {
			ingressDenyPorts, egressDenyPorts := sharedPGs.deletePortsForPolicy(np, portNamesToUUIDs)
			// counters were updated, update back to initial values on error
			defer func() {
				if err != nil {
					sharedPGs.addPortsForPolicy(np, portNamesToUUIDs)
				}
			}()

			if len(ingressDenyPorts) != 0 || len(egressDenyPorts) != 0 {
				// db changes required
				ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, ingressDenyPGName, ingressDenyPorts...)
				if err != nil {
					return fmt.Errorf("unable to get del ports from %s port group ops: %v", ingressDenyPGName, err)
				}

				ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, ops, egressDenyPGName, egressDenyPorts...)
				if err != nil {
					return fmt.Errorf("unable to get del ports from %s port group ops: %v", egressDenyPGName, err)
				}
			} else {
				// shared pg was updated and doesn't require db changes, no need to hold the lock
				oc.sharedNetpolPortGroups.UnlockKey(pgKey)
				pgLocked = false
			}
		}
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("unable to transact del ports from default deny port groups: %v", err)
	}

	return nil
}

// handleLocalPodSelectorAddFunc adds a new pod to an existing NetworkPolicy, should be retriable.
func (oc *DefaultNetworkController) handleLocalPodSelectorAddFunc(np *networkPolicy, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolLocalPodEvent("add", duration)
		}()
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	// get info for new pods that are not listed in np.localPods
	portNamesToUUIDs, policyPortUUIDs, errPods := oc.getNewLocalPolicyPorts(np, objs...)
	// for multiple objects, try to update the ones that were fetched successfully
	// return error for errPods in the end
	if len(portNamesToUUIDs) > 0 {
		var err error
		// add pods to policy port group
		var ops []ovsdb.Operation
		if !PortGroupHasPorts(oc.nbClient, np.portGroupName, policyPortUUIDs) {
			ops, err = libovsdbops.AddPortsToPortGroupOps(oc.nbClient, nil, np.portGroupName, policyPortUUIDs...)
			if err != nil {
				return fmt.Errorf("unable to get ops to add new pod to policy port group: %v", err)
			}
		}
		// add pods to default deny port group
		// make sure to only pass newly added pods
		// ops will be transacted by denyPGAddPorts
		if err = oc.denyPGAddPorts(np, portNamesToUUIDs, ops); err != nil {
			return fmt.Errorf("unable to add new pod to default deny port group: %v", err)
		}
		// all operations were successful, update np.localPods
		for portName, portUUID := range portNamesToUUIDs {
			np.localPods.Store(portName, portUUID)
		}
	}

	if len(errPods) > 0 {
		var errs []error
		for _, errPod := range errPods {
			pod := errPod.(*kapi.Pod)
			errs = append(errs, fmt.Errorf("unable to get port info for pod %s/%s", pod.Namespace, pod.Name))
		}
		return kerrorsutil.NewAggregate(errs)
	}
	return nil
}

// handleLocalPodSelectorDelFunc handles delete event for local pod, should be retriable
func (oc *DefaultNetworkController) handleLocalPodSelectorDelFunc(np *networkPolicy, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolLocalPodEvent("delete", duration)
		}()
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}

	portNamesToUUIDs, policyPortUUIDs, errPods := oc.getExistingLocalPolicyPorts(np, objs...)

	if len(portNamesToUUIDs) > 0 {
		var err error
		// del pods from policy port group
		var ops []ovsdb.Operation
		ops, err = libovsdbops.DeletePortsFromPortGroupOps(oc.nbClient, nil, np.portGroupName, policyPortUUIDs...)
		if err != nil {
			return fmt.Errorf("unable to get ops to add new pod to policy port group: %v", err)
		}
		// delete pods from default deny port group
		if err = oc.denyPGDeletePorts(np, portNamesToUUIDs, false, ops); err != nil {
			return fmt.Errorf("unable to add new pod to default deny port group: %v", err)
		}
		// all operations were successful, update np.localPods
		for portName := range portNamesToUUIDs {
			np.localPods.Delete(portName)
		}
	}

	if len(errPods) > 0 {
		pod := errPods[0].(*kapi.Pod)
		return fmt.Errorf("unable to get port info for pod %s/%s", pod.Namespace, pod.Name)
	}
	return nil
}

// This function starts a watcher for local pods. Sync function and add event for every existing pod
// will be executed sequentially first, and an error will be returned if something fails.
// LocalPodSelectorType uses handleLocalPodSelectorAddFunc on Add and Update,
// and handleLocalPodSelectorDelFunc on Delete.
func (oc *DefaultNetworkController) addLocalPodHandler(policy *knet.NetworkPolicy, np *networkPolicy) error {
	// NetworkPolicy is validated by the apiserver
	sel, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	if err != nil {
		klog.Errorf("Could not set up watcher for local pods: %v", err)
		return err
	}

	// Add all local pods in a syncFunction to minimize db ops.
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any pod that wasn't properly handled will be retried individually.
		_ = oc.handleLocalPodSelectorAddFunc(np, objs...)
		return nil
	}
	retryLocalPods := oc.newRetryFrameworkWithParameters(
		factory.LocalPodSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{
			np: np,
		})

	podHandler, err := retryLocalPods.WatchResourceFiltered(policy.Namespace, sel)
	if err != nil {
		klog.Errorf("WatchResource failed for addLocalPodHandler: %v", err)
		return err
	}

	np.localPodHandler = podHandler
	return nil
}

func getNetworkPolicyPGName(namespace, name string) (pgName, readablePGName string) {
	readableGroupName := fmt.Sprintf("%s_%s", namespace, name)
	return hashedPortGroup(readableGroupName), readableGroupName
}

type policyHandler struct {
	gress             *gressPolicy
	namespaceSelector *metav1.LabelSelector
}

// createNetworkPolicy creates a network policy, should be retriable.
// If network policy with given key exists, it will try to clean it up first, and return an error if it fails.
// No need to log network policy key here, because caller of createNetworkPolicy should prepend error message with
// that information.
func (oc *DefaultNetworkController) createNetworkPolicy(policy *knet.NetworkPolicy, aclLogging *ACLLoggingLevels) (*networkPolicy, error) {
	// To avoid existing connections disruption, make sure to apply allow ACLs before applying deny ACLs.
	// This requires to start peer handlers before local pod handlers.
	// 1. Cleanup old policy if it failed to be created
	// 2. Build gress policies, create addressSets for peers
	// 3. Add policy to default deny port group.
	// 4. Build policy ACLs and port group. All the local pods that this policy
	// selects will be eventually added to this port group.
	// Pods are not added to default deny port groups yet, this is just a preparation step
	// 5. Unlock networkPolicy before starting pod handlers to avoid deadlock
	// since pod handlers take np.RLock
	// 6. Start peer handlers to update all allow rules first
	// 7. Start local pod handlers, that will update networkPolicy and default deny port groups with selected pods.

	npKey := getPolicyKey(policy)
	var np *networkPolicy
	var policyHandlers []*policyHandler

	// network policy will be annotated with this
	// annotation -- [ "k8s.ovn.org/acl-stateless": "true"] for the ingress/egress
	// policies to be added as stateless OVN ACL's.
	// if the above annotation is not present or set to false in network policy,
	// then corresponding egress/ingress policies will be added as stateful OVN ACL's.
	var statelessNetPol bool
	if config.OVNKubernetesFeature.EnableStatelessNetPol {
		// look for stateless annotation if the statlessNetPol feature flag is enabled
		val, ok := policy.Annotations[ovnStatelessNetPolAnnotationName]
		if ok && val == "true" {
			statelessNetPol = true
		}
	}

	err := oc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		oldNP, found := oc.networkPolicies.Load(npKey)
		if found {
			// 1. Cleanup old policy if it failed to be created
			if cleanupErr := oc.cleanupNetworkPolicy(oldNP); cleanupErr != nil {
				return fmt.Errorf("cleanup for retrying network policy create failed: %v", cleanupErr)
			}
		}
		np, found = oc.networkPolicies.LoadOrStore(npKey, NewNetworkPolicy(policy))
		if found {
			// that should never happen, because successful cleanup will delete np from oc.networkPolicies
			return fmt.Errorf("network policy is found in the system, "+
				"while it should've been cleaned up, obj: %+v", np)
		}
		np.Lock()
		npLocked := true
		// we unlock np in the middle of this function, use npLocked to track if it was already unlocked explicitly
		defer func() {
			if npLocked {
				np.Unlock()
			}
		}()
		// no need to check np.deleted, since the object has just been created
		// now we have a new np stored in oc.networkPolicies
		var err error

		if aclLogging.Deny != "" || aclLogging.Allow != "" {
			klog.Infof("ACL logging for network policy %s in namespace %s set to deny=%s, allow=%s",
				policy.Name, policy.Namespace, aclLogging.Deny, aclLogging.Allow)
		}

		// 2. Build gress policies, create addressSets for peers

		// Consider both ingress and egress rules of the policy regardless of this
		// policy type. A pod is isolated as long as as it is selected by any
		// namespace policy. Since we don't process all namespace policies on a
		// given policy update that might change the isolation status of a selected
		// pod, we have created the allow ACLs derived from the policy rules in case
		// the selected pods become isolated in the future even if that is not their
		// current status.

		// Go through each ingress rule.  For each ingress rule, create an
		// addressSet for the peer pods.
		for i, ingressJSON := range policy.Spec.Ingress {
			klog.V(5).Infof("Network policy ingress is %+v", ingressJSON)

			ingress := newGressPolicy(knet.PolicyTypeIngress, i, policy.Namespace, policy.Name, oc.controllerName, statelessNetPol)
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.ingressPolicies = append(np.ingressPolicies, ingress)

			// Each ingress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range ingressJSON.Ports {
				ingress.addPortPolicy(&portJSON)
			}

			for _, fromJSON := range ingressJSON.From {
				handler, err := oc.setupGressPolicy(np, ingress, fromJSON)
				if err != nil {
					return err
				}
				if handler != nil {
					policyHandlers = append(policyHandlers, handler)
				}
			}
		}

		// Go through each egress rule.  For each egress rule, create an
		// addressSet for the peer pods.
		for i, egressJSON := range policy.Spec.Egress {
			klog.V(5).Infof("Network policy egress is %+v", egressJSON)

			egress := newGressPolicy(knet.PolicyTypeEgress, i, policy.Namespace, policy.Name, oc.controllerName, statelessNetPol)
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.egressPolicies = append(np.egressPolicies, egress)

			// Each egress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range egressJSON.Ports {
				egress.addPortPolicy(&portJSON)
			}

			for _, toJSON := range egressJSON.To {
				handler, err := oc.setupGressPolicy(np, egress, toJSON)
				if err != nil {
					return err
				}
				if handler != nil {
					policyHandlers = append(policyHandlers, handler)
				}
			}
		}
		klog.Infof("Policy %s added to peer address sets %v", npKey, np.peerAddressSets)

		// 3. Add policy to default deny port group
		// Pods are not added to default deny port groups yet, this is just a preparation step
		err = oc.addPolicyToDefaultPortGroups(np, aclLogging)
		if err != nil {
			return err
		}

		// 4. Build policy ACLs and port group. All the local pods that this policy
		// selects will be eventually added to this port group.
		portGroupName, readableGroupName := getNetworkPolicyPGName(policy.Namespace, policy.Name)
		np.portGroupName = portGroupName
		ops := []ovsdb.Operation{}

		acls := oc.buildNetworkPolicyACLs(np, aclLogging)
		ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, acls...)
		if err != nil {
			return fmt.Errorf("failed to create ACL ops: %v", err)
		}

		pg := libovsdbops.BuildPortGroup(np.portGroupName, readableGroupName, nil, acls)
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, pg)
		if err != nil {
			return fmt.Errorf("failed to create ops to add port to a port group: %v", err)
		}

		var recordOps []ovsdb.Operation
		var txOkCallBack func()
		recordOps, txOkCallBack, _, err = oc.AddConfigDurationRecord("networkpolicy", policy.Namespace, policy.Name)
		if err != nil {
			klog.Errorf("Failed to record config duration: %v", err)
		}
		ops = append(ops, recordOps...)

		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to run ovsdb txn to add ports to port group: %v", err)
		}
		txOkCallBack()

		// 5. Unlock network policy before starting pod handlers to avoid deadlock,
		// since pod handlers take np.RLock
		np.Unlock()
		npLocked = false

		// 6. Start peer handlers to update all allow rules first
		for _, handler := range policyHandlers {
			// For each peer namespace selector, we create a watcher that
			// populates ingress.peerAddressSets
			err = oc.addPeerNamespaceHandler(handler.namespaceSelector, handler.gress, np)
			if err != nil {
				return fmt.Errorf("failed to start peer handler: %v", err)
			}
		}

		// 7. Start local pod handlers, that will update networkPolicy and default deny port groups with selected pods.
		err = oc.addLocalPodHandler(policy, np)
		if err != nil {
			return fmt.Errorf("failed to start local pod handler: %v", err)
		}

		return nil
	})
	return np, err
}

func (oc *DefaultNetworkController) setupGressPolicy(np *networkPolicy, gp *gressPolicy,
	peer knet.NetworkPolicyPeer) (*policyHandler, error) {
	// Add IPBlock to ingress network policy
	if peer.IPBlock != nil {
		gp.addIPBlock(peer.IPBlock)
		return nil, nil
	}
	if peer.PodSelector == nil && peer.NamespaceSelector == nil {
		// undefined behaviour
		klog.Errorf("setupGressPolicy failed: all fields unset")
		return nil, nil
	}
	gp.hasPeerSelector = true

	podSelector := peer.PodSelector
	if podSelector == nil {
		// nil pod selector is equivalent to empty pod selector, which selects all
		podSelector = &metav1.LabelSelector{}
	}
	podSel, _ := metav1.LabelSelectorAsSelector(podSelector)
	nsSel, _ := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)

	if podSel.Empty() && (peer.NamespaceSelector == nil || !nsSel.Empty()) {
		// namespace-based filtering
		if peer.NamespaceSelector == nil {
			// nil namespace selector means same namespace
			_, err := gp.addNamespaceAddressSet(np.namespace, oc.addressSetFactory)
			if err != nil {
				return nil, fmt.Errorf("failed to add namespace address set for gress policy: %w", err)
			}
		} else if !nsSel.Empty() {
			// namespace selector, use namespace address sets
			handler := &policyHandler{
				gress:             gp,
				namespaceSelector: peer.NamespaceSelector,
			}
			return handler, nil
		}
	} else {
		// use podSelector address set
		// np.namespace will be used when fromJSON.NamespaceSelector = nil
		asKey, ipv4as, ipv6as, err := oc.EnsurePodSelectorAddressSet(
			podSelector, peer.NamespaceSelector, np.namespace, np.getKeyWithKind())
		// even if GetPodSelectorAddressSet failed, add key for future cleanup or retry.
		np.peerAddressSets = append(np.peerAddressSets, asKey)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure pod selector address set %s: %v", asKey, err)
		}
		gp.addPeerAddressSets(ipv4as, ipv6as)
	}
	return nil, nil
}

// addNetworkPolicy creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects using OVN Port Groups
// if addNetworkPolicy fails, create or delete operation can be retried
func (oc *DefaultNetworkController) addNetworkPolicy(policy *knet.NetworkPolicy) error {
	klog.Infof("Adding network policy %s", getPolicyKey(policy))
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolEvent("add", duration)
		}()
	}
	// To not hold nsLock for the whole process on network policy creation, we do the following:
	// 1. save required namespace information to use for netpol create
	// 2. create network policy without ns Lock
	// 3. lock namespace
	// 4. check if namespace information related to network policy has changed, run the same function as on namespace update
	// 5. subscribe to namespace update events
	// 6. unlock namespace

	// 1. save required namespace information to use for netpol create,
	npKey := getPolicyKey(policy)
	nsInfo, nsUnlock := oc.getNamespaceLocked(policy.Namespace, true)
	if nsInfo == nil {
		return fmt.Errorf("unable to get namespace for network policy %s: namespace doesn't exist", npKey)
	}
	aclLogging := nsInfo.aclLogging
	nsUnlock()

	// 2. create network policy without ns Lock, cleanup on failure
	var np *networkPolicy
	var err error

	np, err = oc.createNetworkPolicy(policy, &aclLogging)
	defer func() {
		if err != nil {
			klog.Infof("Create network policy %s failed, try to cleanup", npKey)
			// try to cleanup network policy straight away
			// it will be retried later with add/delete network policy handlers if it fails
			cleanupErr := oc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
				np, ok := oc.networkPolicies.Load(npKey)
				if !ok {
					klog.Infof("Deleting policy %s that is already deleted", npKey)
					return nil
				}
				return oc.cleanupNetworkPolicy(np)
			})
			if cleanupErr != nil {
				klog.Infof("Cleanup for failed create network policy %s returned an error: %v",
					npKey, cleanupErr)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("failed to create Network Policy %s: %v", npKey, err)
	}
	klog.Infof("Create network policy %s resources completed, update namespace loglevel", npKey)

	// 3. lock namespace
	nsInfo, nsUnlock = oc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo == nil {
		// namespace was deleted while we were adding network policy,
		// try to cleanup network policy
		// expect retry to be handled by delete event that should come
		err = fmt.Errorf("unable to get namespace at the end of network policy %s creation: %v", npKey, err)
		return err
	}
	// 6. defer unlock namespace
	defer nsUnlock()

	// 4. check if namespace information related to network policy has changed,
	// network policy only reacts to namespace update ACL log level.
	// Run handleNetPolNamespaceUpdate sequence, but only for 1 newly added policy.
	if nsInfo.aclLogging.Deny != aclLogging.Deny {
		if err = oc.updateACLLoggingForDefaultACLs(policy.Namespace, nsInfo); err != nil {
			return fmt.Errorf("network policy %s failed to be created: update default deny ACLs failed: %v", npKey, err)
		} else {
			klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
				npKey, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
	}
	if nsInfo.aclLogging.Allow != aclLogging.Allow {
		if err = oc.updateACLLoggingForPolicy(np, &nsInfo.aclLogging); err != nil {
			return fmt.Errorf("network policy %s failed to be created: update policy ACLs failed: %v", npKey, err)
		} else {
			klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
				npKey, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
	}

	// 5. subscribe to namespace update events
	nsInfo.relatedNetworkPolicies[npKey] = true
	return nil
}

// buildNetworkPolicyACLs builds the ACLS associated with the 'gress policies
// of the provided network policy.
func (oc *DefaultNetworkController) buildNetworkPolicyACLs(np *networkPolicy, aclLogging *ACLLoggingLevels) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	for _, gp := range np.ingressPolicies {
		acl, _ := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}
	for _, gp := range np.egressPolicies {
		acl, _ := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}

	return acls
}

// deleteNetworkPolicy removes a network policy
// It only uses Namespace and Name from given network policy
func (oc *DefaultNetworkController) deleteNetworkPolicy(policy *knet.NetworkPolicy) error {
	npKey := getPolicyKey(policy)
	klog.Infof("Deleting network policy %s", npKey)
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolEvent("delete", duration)
		}()
	}
	// First lock and update namespace
	nsInfo, nsUnlock := oc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo != nil {
		// unsubscribe from namespace events
		delete(nsInfo.relatedNetworkPolicies, npKey)
		nsUnlock()
	}
	// Next cleanup network policy
	err := oc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		np, ok := oc.networkPolicies.Load(npKey)
		if !ok {
			klog.Infof("Deleting policy %s that is already deleted", npKey)
			return nil
		}
		if err := oc.cleanupNetworkPolicy(np); err != nil {
			return fmt.Errorf("deleting policy %s failed: %v", npKey, err)
		}
		return nil
	})
	return err
}

// cleanupNetworkPolicy should be retriable
// It takes and releases networkPolicy lock.
// It updates oc.networkPolicies on success, should be called with oc.networkPolicies key locked.
// No need to log network policy key here, because caller of cleanupNetworkPolicy should prepend error message with
// that information.
func (oc *DefaultNetworkController) cleanupNetworkPolicy(np *networkPolicy) error {
	npKey := np.getKey()
	klog.Infof("Cleaning up network policy %s", npKey)
	np.Lock()
	defer np.Unlock()

	// signal to local pod/peer handlers to ignore new events
	np.deleted = true

	// stop handlers, retriable
	oc.shutdownHandlers(np)
	var err error

	// delete from peer address set
	for i, asKey := range np.peerAddressSets {
		if err := oc.DeletePodSelectorAddressSet(asKey, np.getKeyWithKind()); err != nil {
			// remove deleted address sets from the list
			np.peerAddressSets = np.peerAddressSets[i:]
			return fmt.Errorf("failed to delete network policy from peer address set %s: %v", asKey, err)
		}
	}
	np.peerAddressSets = nil

	// Delete the port group, idempotent
	ops, err := libovsdbops.DeletePortGroupsOps(oc.nbClient, nil, np.portGroupName)
	if err != nil {
		return fmt.Errorf("failed to get delete network policy port group %s ops: %v", np.portGroupName, err)
	}
	recordOps, txOkCallBack, _, err := oc.AddConfigDurationRecord("networkpolicy", np.namespace, np.name)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)

	err = oc.denyPGDeletePorts(np, nil, true, ops)
	if err != nil {
		return fmt.Errorf("unable to delete ports from defaultDeny port group: %v", err)
	}
	// transaction was successful, exec callback
	txOkCallBack()
	// cleanup local pods, since they were deleted from port groups
	np.localPods = sync.Map{}

	err = oc.delPolicyFromDefaultPortGroups(np)
	if err != nil {
		return fmt.Errorf("unable to delete policy from default deny port groups: %v", err)
	}

	// finally, delete netpol from existing networkPolicies
	// this is the signal that cleanup was successful
	oc.networkPolicies.Delete(npKey)
	return nil
}

type NetworkPolicyExtraParameters struct {
	np *networkPolicy
	gp *gressPolicy
}

func (oc *DefaultNetworkController) handlePeerNamespaceSelectorAdd(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolPeerNamespaceEvent("add", duration)
		}()
	}
	np.RLock()
	if np.deleted {
		np.RUnlock()
		return nil
	}
	updated := false
	var errors []error
	for _, obj := range objs {
		namespace := obj.(*kapi.Namespace)
		// addNamespaceAddressSet is safe for concurrent use, doesn't require additional synchronization
		nsUpdated, err := gp.addNamespaceAddressSet(namespace.Name, oc.addressSetFactory)
		if err != nil {
			errors = append(errors, err)
		} else if nsUpdated {
			updated = true
		}
	}
	np.RUnlock()
	// unlock networkPolicy, before calling peerNamespaceUpdate
	if updated {
		err := oc.peerNamespaceUpdate(np, gp)
		if err != nil {
			errors = append(errors, err)
		}
	}
	return kerrorsutil.NewAggregate(errors)

}

func (oc *DefaultNetworkController) handlePeerNamespaceSelectorDel(np *networkPolicy, gp *gressPolicy, objs ...interface{}) error {
	if config.Metrics.EnableScaleMetrics {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolPeerNamespaceEvent("delete", duration)
		}()
	}
	np.RLock()
	if np.deleted {
		np.RUnlock()
		return nil
	}
	updated := false
	for _, obj := range objs {
		namespace := obj.(*kapi.Namespace)
		// delNamespaceAddressSet is safe for concurrent use, doesn't require additional synchronization
		if gp.delNamespaceAddressSet(namespace.Name) {
			updated = true
		}
	}
	np.RUnlock()
	// unlock networkPolicy, before calling peerNamespaceUpdate
	if updated {
		return oc.peerNamespaceUpdate(np, gp)
	}
	return nil
}

// peerNamespaceUpdate updates gress ACLs, for this purpose it need to take nsInfo lock and np.RLock
// make sure to pass unlocked networkPolicy
func (oc *DefaultNetworkController) peerNamespaceUpdate(np *networkPolicy, gp *gressPolicy) error {
	// Lock namespace before locking np
	// this is to make sure we don't miss update acl loglevel event for namespace.
	// The order of locking is strict: namespace first, then network policy, otherwise deadlock may happen
	nsInfo, nsUnlock := oc.getNamespaceLocked(np.namespace, true)
	var aclLogging *ACLLoggingLevels
	if nsInfo == nil {
		aclLogging = &ACLLoggingLevels{
			Allow: "",
			Deny:  "",
		}
	} else {
		defer nsUnlock()
		aclLogging = &nsInfo.aclLogging
	}
	np.RLock()
	defer np.RUnlock()
	if np.deleted {
		return nil
	}
	// buildLocalPodACLs is safe for concurrent use, see function comment for details
	acls, deletedACLs := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
	if err != nil {
		return err
	}
	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, np.portGroupName, acls...)
	if err != nil {
		return err
	}
	if len(deletedACLs) > 0 {
		deletedACLsWithUUID, err := libovsdbops.FindACLs(oc.nbClient, deletedACLs)
		if err != nil {
			return fmt.Errorf("failed to find deleted acls: %w", err)
		}

		ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, np.portGroupName, deletedACLsWithUUID...)
		if err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	return err
}

// addPeerNamespaceHandler starts a watcher for PeerNamespaceSelectorType.
// Sync function and Add event for every existing namespace will be executed sequentially first, and an error will be
// returned if something fails.
// PeerNamespaceSelectorType uses handlePeerNamespaceSelectorAdd on Add,
// and handlePeerNamespaceSelectorDel on Delete.
func (oc *DefaultNetworkController) addPeerNamespaceHandler(
	namespaceSelector *metav1.LabelSelector,
	gress *gressPolicy, np *networkPolicy) error {

	// NetworkPolicy is validated by the apiserver; this can't fail.
	sel, _ := metav1.LabelSelectorAsSelector(namespaceSelector)

	// start watching namespaces selected by the namespace selector
	syncFunc := func(objs []interface{}) error {
		// ignore returned error, since any namespace that wasn't properly handled will be retried individually.
		_ = oc.handlePeerNamespaceSelectorAdd(np, gress, objs...)
		return nil
	}
	retryPeerNamespaces := oc.newRetryFrameworkWithParameters(
		factory.PeerNamespaceSelectorType,
		syncFunc,
		&NetworkPolicyExtraParameters{gp: gress, np: np},
	)

	namespaceHandler, err := retryPeerNamespaces.WatchResourceFiltered("", sel)
	if err != nil {
		klog.Errorf("WatchResource failed for addPeerNamespaceHandler: %v", err)
		return err
	}

	np.nsHandlerList = append(np.nsHandlerList, namespaceHandler)
	return nil
}

func (oc *DefaultNetworkController) shutdownHandlers(np *networkPolicy) {
	if np.localPodHandler != nil {
		oc.watchFactory.RemovePodHandler(np.localPodHandler)
		np.localPodHandler = nil
	}
	for _, handler := range np.nsHandlerList {
		oc.watchFactory.RemoveNamespaceHandler(handler)
	}
	np.nsHandlerList = make([]*factory.Handler, 0)
}

// The following 2 functions should return the same key for network policy based on k8s on internal networkPolicy object
func getPolicyKey(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v", policy.Namespace, policy.Name)
}

func (np *networkPolicy) getKey() string {
	return fmt.Sprintf("%v/%v", np.namespace, np.name)
}

func (np *networkPolicy) getKeyWithKind() string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", np.namespace, np.name)
}

// PortGroupHasPorts returns true if a port group contains all given ports
func PortGroupHasPorts(nbClient libovsdbclient.Client, pgName string, portUUIDs []string) bool {
	pg := &nbdb.PortGroup{
		Name: pgName,
	}
	pg, err := libovsdbops.GetPortGroup(nbClient, pg)
	if err != nil {
		return false
	}

	return sets.NewString(pg.Ports...).HasAll(portUUIDs...)
}

// getStaleNetpolAddrSetDbIDs returns the ids for address sets that were owned by network policy before we
// switched to shared address sets with PodSelectorAddressSet. Should only be used for sync and testing.
func getStaleNetpolAddrSetDbIDs(policyNamespace, policyName, policyType, idx, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkPolicy, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: policyNamespace + "_" + policyName,
		// direction and idx uniquely identify address set (= gress policy rule)
		libovsdbops.PolicyDirectionKey: strings.ToLower(policyType),
		libovsdbops.GressIdxKey:        idx,
	})
}

func (oc *DefaultNetworkController) getNetpolDefaultACLDbIDs(direction string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolDefault, oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      allowHairpinningACLID,
			libovsdbops.PolicyDirectionKey: direction,
		})
}

func (oc *DefaultNetworkController) addHairpinAllowACL() error {
	var v4Match, v6Match, match string
	if config.IPv4Mode {
		v4Match = fmt.Sprintf("%s.src == %s", "ip4", types.V4OVNServiceHairpinMasqueradeIP)
		match = v4Match
	}
	if config.IPv6Mode {
		v6Match = fmt.Sprintf("%s.src == %s", "ip6", types.V6OVNServiceHairpinMasqueradeIP)
		match = v6Match
	}
	if config.IPv4Mode && config.IPv6Mode {
		match = fmt.Sprintf("(%s || %s)", v4Match, v6Match)
	}

	ingressACLIDs := oc.getNetpolDefaultACLDbIDs(string(knet.PolicyTypeIngress))
	ingressACL := BuildACL(ingressACLIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportIngress)

	egressACLIDs := oc.getNetpolDefaultACLDbIDs(string(knet.PolicyTypeEgress))
	egressACL := BuildACL(egressACLIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, lportEgressAfterLB)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, ingressACL, egressACL)
	if err != nil {
		return fmt.Errorf("failed to create or update hairpin allow ACL %v", err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, types.ClusterPortGroupName, ingressACL, egressACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL hairpin allow acl to port group: %v", err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}
