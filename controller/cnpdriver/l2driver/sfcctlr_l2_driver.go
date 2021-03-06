// Copyright (c) 2017 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The L2 VxLan Tunnel CNP driver takes care of wiring the inter/intra
// hosts and external entity connections. The "internal" resource management is
// taken care of by this module.  The state of the wiring is stored in the ETCD
// just as the config of the system is.  For example, there are 2 EEs, and 3 HEs.
// This means we need to wire vxLans between each HE to all EEs, and all other
// HEs.  Then we have to do this in the reverse direction. There is, in effect,
// a star vxlan from each EE to each HE, and HE has a vxLan back to its EE.
// Also, there is a bidirectional mesh of VxLans between each HE to every other
// HE.  This involves allocating indicies for the VxLans ... these will be
// allocated and tracked by this module and stored in the ETCD datastore so
// upon sfc ctlr restart, we dont lose track of any "tracked" resource.

//go:generate protoc --proto_path=model --gogo_out=model model/l2.proto

package l2driver

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ligato/cn-infra/db/keyval"
	"github.com/ligato/cn-infra/logging/logrus"
	"github.com/ligato/cn-infra/servicelabel"
	l2driver "github.com/ligato/sfc-controller/controller/cnpdriver/l2driver/model"
	"github.com/ligato/sfc-controller/controller/extentitydriver"
	"github.com/ligato/sfc-controller/controller/model/controller"
	"github.com/ligato/sfc-controller/controller/utils"
	"github.com/ligato/sfc-controller/controller/utils/ipam"
	"github.com/ligato/vpp-agent/clientv1/linux"
	"github.com/ligato/vpp-agent/clientv1/linux/remoteclient"
	"github.com/ligato/vpp-agent/plugins/defaultplugins/common/model/interfaces"
	"github.com/ligato/vpp-agent/plugins/defaultplugins/common/model/l2"
	"github.com/ligato/vpp-agent/plugins/defaultplugins/common/model/l3"
	linuxIntf "github.com/ligato/vpp-agent/plugins/linuxplugin/ifplugin/model/interfaces"
)

var (
	log = logrus.DefaultLogger()
)

type sfcCtlrL2CNPDriver struct {
	dbFactory           func(string) keyval.ProtoBroker
	db                  keyval.ProtoBroker
	name                string
	l2CNPEntityCache    l2CNPEntityCacheType
	l2CNPStateCache     l2CNPStateCacheType
	reconcileBefore     reconcileCacheType
	reconcileAfter      reconcileCacheType
	reconcileInProgress bool
	seq                 sequencer
}

// sequencer groups all sequences used by L2 driver.
// (instead of using global variables that caused
// problems while running automated tests)
type sequencer struct {
	VLanID        uint32
	MemIfID       uint32
	MacInstanceID uint32
	VethID        uint32
}

type sfcInterfaceAddressStateType struct {
	ipAddress  string
	macAddress string
}

type heToEEStateType struct {
	vlanIf  *interfaces.Interfaces_Interface
	bd      *l2.BridgeDomains_BridgeDomain
	l3Route *l3.StaticRoutes_Route
}

type heToHEStateType struct {
	vlanIf  *interfaces.Interfaces_Interface
	bd      *l2.BridgeDomains_BridgeDomain
	l3Route *l3.StaticRoutes_Route
}

type heStateType struct {
	ewBD      *l2.BridgeDomains_BridgeDomain
	ewBDL2Fib *l2.BridgeDomains_BridgeDomain
}

type l2CNPStateCacheType struct {
	HEToEEs   map[string]map[string]*heToEEStateType
	HEToHEs   map[string]map[string]*heToHEStateType
	SFCToHEs  map[string]map[string]*heStateType
	HE        map[string]*heStateType
	SFCIFAddr map[string]sfcInterfaceAddressStateType
}

type l2CNPEntityCacheType struct {
	EEs      map[string]controller.ExternalEntity
	HEs      map[string]controller.HostEntity
	SFCs     map[string]controller.SfcEntity
	SysParms controller.SystemParameters
}

// NewRemoteClientTxn new vpp-agent remote client instance on top of key-val DB (ETCD)
// <microserviceLabel> that identifies a specific vpp-agent that needs to be configured
// <dbFactory> returns new instance of DataBroker for accessing key-val DB (ETCD)
func NewRemoteClientTxn(microserviceLabel string, dbFactory func(string) keyval.ProtoBroker) linux.DataChangeDSL {
	prefix := servicelabel.GetDifferentAgentPrefix(microserviceLabel)
	broker := dbFactory(prefix)
	return remoteclient.DataChangeRequestDB(broker)
}

// NewSfcCtlrL2CNPDriver creates new driver/mode for Native SFC Controller L2 Container Networking Policy
// <name> of the driver/plugin
// <dbFactory> returns new instance of DataBroker for accessing key-val DB (ETCD)
func NewSfcCtlrL2CNPDriver(name string, dbFactory func(string) keyval.ProtoBroker) *sfcCtlrL2CNPDriver {

	cnpd := &sfcCtlrL2CNPDriver{}
	cnpd.name = "Sfc Controller L2 Plugin: " + name
	cnpd.dbFactory = dbFactory
	cnpd.db = dbFactory(keyval.Root)

	cnpd.initL2CNPCache()
	cnpd.initReconcileCache()

	return cnpd
}

func (cnpd *sfcCtlrL2CNPDriver) initL2CNPCache() {
	cnpd.l2CNPStateCache.HEToEEs = make(map[string]map[string]*heToEEStateType)
	cnpd.l2CNPStateCache.HEToHEs = make(map[string]map[string]*heToHEStateType)
	cnpd.l2CNPStateCache.SFCToHEs = make(map[string]map[string]*heStateType)
	cnpd.l2CNPStateCache.HE = make(map[string]*heStateType)
	cnpd.l2CNPStateCache.SFCIFAddr = make(map[string]sfcInterfaceAddressStateType)

	cnpd.l2CNPEntityCache.EEs = make(map[string]controller.ExternalEntity)
	cnpd.l2CNPEntityCache.HEs = make(map[string]controller.HostEntity)
	cnpd.l2CNPEntityCache.SFCs = make(map[string]controller.SfcEntity)
}

// Perform plugin specific initializations
func (cnpd *sfcCtlrL2CNPDriver) InitPlugin() error {
	return nil
}

// Cleanup anything as plugin is being de-reged
func (cnpd *sfcCtlrL2CNPDriver) DeinitPlugin() error {
	return nil
}

// Return user friendly name for this plugin
func (cnpd *sfcCtlrL2CNPDriver) GetName() string {
	return cnpd.name
}

// SetSystemParameters caches the current settings for the system
func (cnpd *sfcCtlrL2CNPDriver) SetSystemParameters(sp *controller.SystemParameters) error {
	cnpd.l2CNPEntityCache.SysParms = *sp
	if cnpd.seq.VLanID == 0 { // only init if this is the first time being set
		cnpd.seq.VLanID = cnpd.l2CNPEntityCache.SysParms.StartingVlanId - 1
		log.Infof("SetSystemParameters: setting starting valnId: ", cnpd.seq.VLanID)
	}
	log.Infof("SetSystemParameters: SP", sp)
	return nil
}

// Perform CNP specific wiring for "connecting" a source host to a dest host
func (cnpd *sfcCtlrL2CNPDriver) WireHostEntityToDestinationHostEntity(sh *controller.HostEntity,
	dh *controller.HostEntity) error {

	cnpd.l2CNPEntityCache.HEs[sh.Name] = *sh
	cnpd.l2CNPEntityCache.HEs[dh.Name] = *dh

	log.Infof("WireHostEntityToDestinationHostEntity: sr", sh)
	log.Infof("WireHostEntityToDestinationHostEntity: dh", dh)

	// this holds the relationship from the HE to the map of EEs to which this HE is wired
	heToHEMap, exists := cnpd.l2CNPStateCache.HEToHEs[sh.Name]
	if !exists {
		heToHEMap = make(map[string]*heToHEStateType)
		cnpd.l2CNPStateCache.HEToHEs[sh.Name] = heToHEMap
	}

	// now ensure this source HE has not yet been associated to the dest HE
	heToHEState, exists := heToHEMap[dh.Name]
	if exists {
		// maybe look at contents to see if they are programmed properly but for now just return
		return nil
	}

	// delay adding of vlan tunnel, [static_route] and bridge till an sfc entity specifies one
	heToHEState = &heToHEStateType{
	//vlanIf:  vlanIf,
	//bd:      bd,
	//l3Route: sr,
	}

	// now link the sh to the dh
	heToHEMap[dh.Name] = heToHEState

	log.Infof("WireHostEntityToDestinationHostEntity: sh: %s, dh: %s", sh.Name, dh.Name)

	return nil
}

// Perform CNP specific wiring for "connecting" an external router to a host server, called from
// WireHostEntityToExternalEntity after host is wired to ee
func (cnpd *sfcCtlrL2CNPDriver) wireExternalEntityToHostEntity(ee *controller.ExternalEntity,
	he *controller.HostEntity) error {

	log.Infof("wireExternalEntityToHostEntity: he", he)
	log.Infof("wireExternalEntityToHostEntity: ee", ee)

	// this holds the relationship from the HE to the map of EEs to which this HE is wired
	heToEEMap, exists := cnpd.l2CNPStateCache.HEToEEs[he.Name]
	if !exists {
		return nil
	}

	// now ensure this HE has not yet been wired to the EE, if it has then wire the EE to the HE
	heToEEState, exists := heToEEMap[ee.Name]
	if !exists {
		return nil
	}

	//bdName := "BD_E2H_" + ee.Name + "_" + he.Name

	// create the vxlan i'f before the BD
	//ifName := "IF_VXLAN_E2H_" + ee.Name + "_" + he.Name
	tmpVlanid := heToEEState.vlanIf.Vxlan.Vni // use the same id as the reverse direction
	//vlanIf, err := cnpd.vxLanCreate(ee.Name, ifName, vlanid, ee.LoopbackIpv4, he.LoopbackIpv4)
	//if err != nil {
	//	log.Error("wireExternalEntityToHostEntity: error creating vxlan: '%s'", ifName)
	//	return err
	//}
	//
	//ifs := make([]*l2.BridgeDomains_BridgeDomain_Interfaces, 1)
	//ifEntry := l2.BridgeDomains_BridgeDomain_Interfaces{
	//	Name: ifName,
	//}
	//ifs[0] = &ifEntry
	//
	//// now create the bridge
	//ewBD, err := cnpd.bridgedDomainCreateWithIfs(ee.Name, bdName, ifs)
	//if err != nil {
	//	log.Error("wireExternalEntityToHostEntity: error creating BD: '%s'", ewBD.Name)
	//	return err
	//}

	// configure static route from this external router to the host
	description := "IF_STATIC_ROUTE_E2H_" + he.Name
	sr, err := cnpd.createStaticRoute(0, ee.Name, description, he.VxlanTunnelIpv4, he.EthIpv4, ee.HostInterface.IfName,
		cnpd.l2CNPEntityCache.SysParms.DefaultStaticRouteWeight, cnpd.l2CNPEntityCache.SysParms.DefaultStaticRoutePreference)
	if err != nil {
		log.Errorf("wireExternalEntityToHostEntity: error creating static route i/f: '%s'", description)
		return err
	}

	log.Infof("wireExternalEntityToHostEntity: ee: %s, he: %s, vlanid: %d, static route: %s",
		ee.Name, he.Name, tmpVlanid, sr.String())

	// call the external entity api to queue a msg so that the external router config will be sent to the router
	// this will be replace perhaps by a watcher in the ext-ent driver
	extentitydriver.SfcCtlrL2WireExternalEntityToHostEntity(*ee, *he, tmpVlanid, sr)
	return nil
}

// Perform CNP specific wiring for "connecting" a host server to an external router
func (cnpd *sfcCtlrL2CNPDriver) WireHostEntityToExternalEntity(he *controller.HostEntity,
	ee *controller.ExternalEntity) error {

	cnpd.l2CNPEntityCache.HEs[he.Name] = *he
	cnpd.l2CNPEntityCache.EEs[ee.Name] = *ee

	log.Infof("WireHostEntityToExternalEntity: he", he)
	log.Infof("WireHostEntityToExternalEntity: ee", ee)

	if ee.HostInterface == nil || ee.HostVxlan == nil {
		log.Error("WireHostEntityToExternalEntity: invalid external entity config")
		return errors.New("invalid external entity config")
	}

	// this holds the relationship from the HE to the map of EEs to which this HE is wired
	heToEEMap, exists := cnpd.l2CNPStateCache.HEToEEs[he.Name]
	if !exists {
		heToEEMap = make(map[string]*heToEEStateType)
		cnpd.l2CNPStateCache.HEToEEs[he.Name] = heToEEMap
	}

	// now ensure this HE has not yet been associated to the EE
	heToEEState, exists := heToEEMap[ee.Name]
	if exists {
		// maybe look at contents to see if they are programmed properly but for now just return
		return nil
	}

	// delay adding of vlan tunnel, [static_route] and bridge till an sfc entity specifies one
	heToEEState = &heToEEStateType{
	//vlanIf:  vlanIf,
	//bd:      bd,
	//l3Route: sr,
	}

	// now link the he to the ee
	heToEEMap[ee.Name] = heToEEState

	log.Infof("WireHostEntityToExternalEntity: he: %s, ee: %s", he.Name, ee.Name)

	return nil
}

// Perform CNP specific wiring for "preparing" a host server example: create an east-west bridge
func (cnpd *sfcCtlrL2CNPDriver) WireInternalsForHostEntity(he *controller.HostEntity) error {

	cnpd.l2CNPEntityCache.HEs[he.Name] = *he

	log.Infof("WireInternalsForHostEntity: caching host: ", he)

	// this holds the state for an HE
	heState, exists := cnpd.l2CNPStateCache.HE[he.Name]
	if exists {
		return nil // if it is being updated .... figure out what that means here
	}
	heState = &heStateType{}
	cnpd.l2CNPStateCache.HE[he.Name] = heState

	mtu := cnpd.getMtu(he.Mtu)

	// configure the nic/ethernet
	if he.EthIfName != "" {
		if err := cnpd.createEthernet(he.Name, he.EthIfName, he.EthIpv4, "", he.EthIpv6, mtu, he.RxMode); err != nil {
			log.Errorf("WireInternalsForHostEntity: error creating ethernet i/f: '%s'", he.EthIfName)
			return err
		}
	}

	var heID *l2driver.HEIDs
	var loopbackMacAddrID uint32

	if he.LoopbackIpv4 != "" || he.LoopbackIpv6 != "" { // if configured, then create a loop back address

		var loopbackMacAddress string

		if he.LoopbackMacAddr == "" { // if not supplied, generate one
			heID, _ = cnpd.DatastoreHEIDsRetrieve(he.Name)
			if heID == nil || heID.LoopbackMacAddrId == 0 {
				cnpd.seq.MacInstanceID++
				loopbackMacAddress = formatMacAddress(cnpd.seq.MacInstanceID)
				loopbackMacAddrID = cnpd.seq.MacInstanceID
			} else {
				loopbackMacAddress = formatMacAddress(heID.LoopbackMacAddrId)
				loopbackMacAddrID = heID.LoopbackMacAddrId
			}
		} else {
			loopbackMacAddress = he.LoopbackMacAddr
		}

		mtu := cnpd.getMtu(he.Mtu)

		// configure loopback interface
		loopIfName := "IF_LOOPBACK_H_" + he.Name
		if err := cnpd.createLoopback(he.Name, loopIfName, loopbackMacAddress, he.LoopbackIpv4, he.LoopbackIpv6, mtu,
			he.RxMode); err != nil {
			log.Errorf("WireInternalsForHostEntity: error creating loopback i/f: '%s'", loopIfName)
			return err
		}
	}

	// create a default flooding/learning/dynamic east-west bd, see controller/validate.go for defaults
	bdName := "BD_INTERNAL_EW_" + he.Name
	bd, err := cnpd.bridgedDomainCreateWithIfs(he.Name, bdName, nil, cnpd.l2CNPEntityCache.SysParms.DynamicBridgeParms)
	if err != nil {
		log.Errorf("WireInternalsForHostEntity: error creating BD: '%s'", bd.Name)
		return err
	}

	heState.ewBD = bd

	// create a default static east-west bd, see controller/validate.go for defaults
	bdName = "BD_INTERNAL_EW_L2FIB_" + he.Name
	bd, err = cnpd.bridgedDomainCreateWithIfs(he.Name, bdName, nil, cnpd.l2CNPEntityCache.SysParms.StaticBridgeParms)
	if err != nil {
		log.Errorf("WireInternalsForHostEntity: error creating BD: '%s'", bd.Name)
		return err
	}

	heState.ewBDL2Fib = bd

	key, heID, err := cnpd.DatastoreHEIDsCreate(he.Name, loopbackMacAddrID)
	if err == nil && cnpd.reconcileInProgress {
		cnpd.reconcileAfter.heIDs[key] = *heID
	}

	return err
}

// Perform CNP specific wiring for "preparing" an external entity
func (cnpd *sfcCtlrL2CNPDriver) WireInternalsForExternalEntity(ee *controller.ExternalEntity) error {

	extentitydriver.SfcCtlrL2WireExternalEntityInternals(*ee)

	return nil
}

// Perform CNP specific wiring for inter-container wiring, and container to external router wiring
func (cnpd *sfcCtlrL2CNPDriver) WireSfcEntity(sfc *controller.SfcEntity) error {

	var err error
	// the semantic difference between a north_south vs an east-west sfc entity, it what is the bridge that
	// the memIf/afPkt if's will be associated.
	switch sfc.Type {

	case controller.SfcType_SFC_NS_VXLAN:
		// north/south VXLAN type, memIfs/cntrs connect to vrouters/RASs bridge
		cnpd.l2CNPEntityCache.SFCs[sfc.Name] = *sfc
		err = cnpd.wireSfcNorthSouthVXLANElements(sfc)

	case controller.SfcType_SFC_NS_NIC_BD:
		fallthrough
	case controller.SfcType_SFC_NS_NIC_VRF:
		fallthrough
	case controller.SfcType_SFC_NS_NIC_L2XCONN:
		// north/south NIC type, memIfs/cntrs connect to physical NIC
		cnpd.l2CNPEntityCache.SFCs[sfc.Name] = *sfc
		err = cnpd.wireSfcNorthSouthNICElements(sfc)

	case controller.SfcType_SFC_EW_MEMIF:
		fallthrough
	case controller.SfcType_SFC_EW_BD:
		fallthrough
	case controller.SfcType_SFC_EW_BD_L2FIB:
		fallthrough
	case controller.SfcType_SFC_EW_L2XCONN:
		// east/west type, memIfs/cntrs connect to the hosts easet/west bridge
		cnpd.l2CNPEntityCache.SFCs[sfc.Name] = *sfc
		err = cnpd.wireSfcEastWestElements(sfc)

	default:
		err = fmt.Errorf("WireSfcEntity: unknown entity type: '%s'", sfc.Type)
		log.Error(err.Error())
	}

	return err
}

// for now, ensure there is only one ee ... as each container will be wirred to it
func (cnpd *sfcCtlrL2CNPDriver) wireSfcNorthSouthVXLANElements(sfc *controller.SfcEntity) error {

	var err error
	var bd *l2.BridgeDomains_BridgeDomain

	eeCount := 0
	eeName := ""
	var eeSfcElement *controller.SfcEntity_SfcElement

	dhCount := 0
	dhName := ""
	var dhSfcElement *controller.SfcEntity_SfcElement

	// find the external entity and ensure there is only one allowed
	for i, sfcEntityElement := range sfc.GetElements() {

		log.Infof("wireSfcNorthSouthVXLANElements: sfc entity element[%d]: ", i, sfcEntityElement)

		switch sfcEntityElement.Type {
		case controller.SfcElementType_EXTERNAL_ENTITY:
			eeCount++
			if eeCount > 1 {
				err := fmt.Errorf("wireSfcNorthSouthVXLANElements: only one ee allowed for n/s sfc: '%s'",
					sfc.Name)
				log.Error(err.Error())
				return err
			}

			eeName = sfcEntityElement.Container
			if _, exists := cnpd.l2CNPEntityCache.EEs[sfcEntityElement.Container]; !exists {
				err := fmt.Errorf("wireSfcNorthSouthVXLANElements: ee not found: '%s' for n/s sfc: '%s'",
					eeName, sfc.Name)
				log.Error(err.Error())
				return err
			}
			eeSfcElement = sfcEntityElement

		case controller.SfcElementType_HOST_ENTITY:
			dhCount++
			if dhCount > 1 {
				err := fmt.Errorf("wireSfcNorthSouthVXLANElements: only one dest host allowed for n/s sfc: '%s'",
					sfc.Name)
				log.Error(err.Error())
				return err
			}

			dhName = sfcEntityElement.Container
			if _, exists := cnpd.l2CNPEntityCache.HEs[sfcEntityElement.Container]; !exists {
				err := fmt.Errorf("wireSfcNorthSouthVXLANElements: dest host not found: '%s' for n/s sfc: '%s'",
					dhName, sfc.Name)
				log.Error(err.Error())
				return err
			}
			dhSfcElement = sfcEntityElement
		}
	}

	if eeCount == 0 && dhCount == 0 {
		err := fmt.Errorf("wireSfcNorthSouthVXLANElements: NO ee or dh specified for n/s sfc: '%s'", sfc.Name)
		log.Error(err.Error())
		return err
	}

	// now wire each container to the bridge wired from the host to the ee
	for i, sfcEntityElement := range sfc.GetElements() {

		log.Infof("wireSfcNorthSouthVXLANElements: sfc entity element[%d]: ", i, sfcEntityElement)

		switch sfcEntityElement.Type {

		case controller.SfcElementType_VPP_CONTAINER_AFP:
			fallthrough
		case controller.SfcElementType_NON_VPP_CONTAINER_AFP:

			if eeCount != 0 {
				bd, err = cnpd.createVxLANAndBridgeToExtEntity(sfc, sfcEntityElement.EtcdVppSwitchKey, eeName,
					eeSfcElement.VlanId)
			} else {
				bd, err = cnpd.createVxLANAndBridgeToDestHost(sfc, sfcEntityElement.EtcdVppSwitchKey, dhName,
					dhSfcElement.VlanId)
			}
			if err != nil {
				return err
			}

			if _, err := cnpd.createAFPacketVEthPairAndAddToBridge(sfc, bd, sfcEntityElement); err != nil {
				log.Errorf("wireSfcNorthSouthVXLANElements: error creating memIf pair: sfc: '%s', Container: '%s'",
					sfc.Name, sfcEntityElement.Container)
				return err
			}

		case controller.SfcElementType_VPP_CONTAINER_MEMIF:
			fallthrough
		case controller.SfcElementType_NON_VPP_CONTAINER_MEMIF:

			if eeCount != 0 {
				bd, err = cnpd.createVxLANAndBridgeToExtEntity(sfc, sfcEntityElement.EtcdVppSwitchKey, eeName,
					eeSfcElement.VlanId)
			} else {
				bd, err = cnpd.createVxLANAndBridgeToDestHost(sfc, sfcEntityElement.EtcdVppSwitchKey, dhName,
					dhSfcElement.VlanId)
			}
			if err != nil {
				return err
			}

			if _, err := cnpd.createMemIfPairAndAddToBridge(sfc, sfcEntityElement.EtcdVppSwitchKey, bd,
				sfcEntityElement, false); err != nil {
				log.Errorf("wireSfcNorthSouthVXLANElements: error creating memIf pair: sfc: '%s', Container: '%s'",
					sfc.Name, sfcEntityElement.Container)
				return err
			}
		}
	}

	return nil
}

// createVxLANAndBridgeToExtEntity and ensure vxlan and bridge are created if not already done yet
func (cnpd *sfcCtlrL2CNPDriver) createVxLANAndBridgeToExtEntity(sfc *controller.SfcEntity,
	hostName string, eeName string, vlanID uint32) (*l2.BridgeDomains_BridgeDomain, error) {

	// the container has which host it is assoc'ed with, get the ee bridge
	heToEEMap, exists := cnpd.l2CNPStateCache.HEToEEs[hostName]
	if !exists {
		err := fmt.Errorf("createVxLANAndBridgeToExtEntity: host not found: '%s' for this sfc: '%s'",
			hostName, sfc.Name)
		return nil, err
	}
	heToEEState, exists := heToEEMap[eeName]
	if !exists {
		err := fmt.Errorf("createVxLANAndBridgeToExtEntity: host '%s' not wired to this ee: '%s' for this sfc: '%s'",
			hostName, eeName, sfc.Name)
		return nil, err
	}

	if heToEEState.vlanIf == nil {

		// first time sfc is wired from this host to this external ee so create a vxlan tunnel

		he := cnpd.l2CNPEntityCache.HEs[hostName]
		ee := cnpd.l2CNPEntityCache.EEs[eeName]

		// create the vxlan i'f before the BD
		ifName := "IF_VXLAN_H2E_" + he.Name + "_" + ee.Name

		if vlanID == 0 {
			he2eeID, _ := cnpd.DatastoreHE2EEIDsRetrieve(he.Name, ee.Name)
			if he2eeID == nil || he2eeID.VlanId == 0 {
				cnpd.seq.VLanID++
				vlanID = cnpd.seq.VLanID
			} else {
				vlanID = he2eeID.VlanId
			}
		}
		vlanIf, err := cnpd.vxLanCreate(he.Name, ifName, vlanID, he.VxlanTunnelIpv4, ee.HostVxlan.SourceIpv4)
		if err != nil {
			log.Errorf("createVxLANAndBridgeToExtEntity: error creating vxlan: '%s'", ifName)
			return nil, err
		}

		heToEEState.vlanIf = vlanIf

		key, he2eeID, err := cnpd.DatastoreHE2EEIDsCreate(he.Name, ee.Name, vlanID)
		if err == nil && cnpd.reconcileInProgress {
			cnpd.reconcileAfter.he2eeIDs[key] = *he2eeID
		}
	}

	if heToEEState.l3Route == nil {

		he := cnpd.l2CNPEntityCache.HEs[hostName]
		ee := cnpd.l2CNPEntityCache.EEs[eeName]

		// configure static route from this host to the dest host
		if he.CreateVxlanStaticRoute {
			description := "IF_STATIC_ROUTE_H2E_" + ee.Name
			sr, err := cnpd.createStaticRoute(0, he.Name, description, ee.HostVxlan.SourceIpv4, ee.HostInterface.Ipv4Addr,
				he.EthIfName,
				cnpd.l2CNPEntityCache.SysParms.DefaultStaticRouteWeight,
				cnpd.l2CNPEntityCache.SysParms.DefaultStaticRoutePreference)
			if err != nil {
				log.Errorf("createVxLANAndBridgeToExtEntity: error creating static route i/f: '%s'", description)
				return nil, err
			}

			heToEEState.l3Route = sr
		}
	}

	if heToEEState.bd == nil {

		// first time sfc is wired from this host to this external ee so create a bridge

		he := cnpd.l2CNPEntityCache.HEs[hostName]
		ee := cnpd.l2CNPEntityCache.EEs[eeName]

		bdName := "BD_H2E_" + he.Name + "_" + ee.Name

		ifs := make([]*l2.BridgeDomains_BridgeDomain_Interfaces, 1)
		ifEntry := l2.BridgeDomains_BridgeDomain_Interfaces{
			Name: heToEEState.vlanIf.Name,
		}
		ifs[0] = &ifEntry

		// now create the bridge
		bd, err := cnpd.bridgedDomainCreateWithIfs(he.Name, bdName, ifs, cnpd.l2CNPEntityCache.SysParms.DynamicBridgeParms)
		if err != nil {
			log.Errorf("createVxLANAndBridgeToExtEntity: error creating BD: '%s'", bd.Name)
			return nil, err
		}

		heToEEState.bd = bd

		// now we can wire the external entity to this host
		cnpd.wireExternalEntityToHostEntity(&ee, &he)
	}

	return heToEEState.bd, nil
}

// createVxLANAndBridgeToDestHost and ensure vxlan and bridge are created if not already done yet
func (cnpd *sfcCtlrL2CNPDriver) createVxLANAndBridgeToDestHost(sfc *controller.SfcEntity,
	shName string, dhName string, vlanID uint32) (*l2.BridgeDomains_BridgeDomain, error) {

	// the container has which host it is assoc'ed with, get the dh bridge
	heToHEMap, exists := cnpd.l2CNPStateCache.HEToHEs[shName]
	if !exists {
		err := fmt.Errorf("createVxLANAndBridgeToDestHost: host not found: '%s' for this sfc: '%s'",
			shName, sfc.Name)
		return nil, err
	}
	heToHEState, exists := heToHEMap[dhName]
	if !exists {
		err := fmt.Errorf("createVxLANAndBridgeToDestHost: host '%s' not wired to this ee: '%s' for this sfc: '%s'",
			shName, dhName, sfc.Name)
		return nil, err
	}

	if heToHEState.vlanIf == nil {

		// first time sfc is wired from this host to this dest host so create a vxlan tunnel

		sh := cnpd.l2CNPEntityCache.HEs[shName]
		dh := cnpd.l2CNPEntityCache.HEs[dhName]

		// create the vxlan i'f before the BD
		ifName := "IF_VXLAN_H2H_" + sh.Name + "_" + dh.Name

		if vlanID == 0 {
			he2eeID, _ := cnpd.DatastoreHE2EEIDsRetrieve(sh.Name, dh.Name)
			if he2eeID == nil || he2eeID.VlanId == 0 {
				cnpd.seq.VLanID++
				vlanID = cnpd.seq.VLanID
			} else {
				vlanID = he2eeID.VlanId
			}
		}
		vlanIf, err := cnpd.vxLanCreate(sh.Name, ifName, vlanID, sh.VxlanTunnelIpv4, dh.VxlanTunnelIpv4)
		if err != nil {
			log.Errorf("createVxLANAndBridgeToDestHost: error creating vxlan: '%s'", ifName)
			return nil, err
		}

		heToHEState.vlanIf = vlanIf

		key, sh2dhID, err := cnpd.DatastoreHE2HEIDsCreate(sh.Name, dh.Name, vlanID)
		if err == nil && cnpd.reconcileInProgress {
			cnpd.reconcileAfter.he2heIDs[key] = *sh2dhID
		}
	}

	if heToHEState.l3Route == nil {

		sh := cnpd.l2CNPEntityCache.HEs[shName]
		dh := cnpd.l2CNPEntityCache.HEs[dhName]

		// configure static route from this host to the dest host
		if sh.CreateVxlanStaticRoute {
			description := "IF_STATIC_ROUTE_H2H_" + dh.Name
			sr, err := cnpd.createStaticRoute(0, sh.Name, description, dh.VxlanTunnelIpv4, dh.EthIpv4,
				sh.EthIfName,
				cnpd.l2CNPEntityCache.SysParms.DefaultStaticRouteWeight,
				cnpd.l2CNPEntityCache.SysParms.DefaultStaticRoutePreference)
			if err != nil {
				log.Errorf("createVxLANAndBridgeToDestHost: error creating static route i/f: '%s'", description)
				return nil, err
			}

			heToHEState.l3Route = sr
		}
	}

	if heToHEState.bd == nil {

		// first time sfc is wired from this host to this external ee so create a bridge

		sh := cnpd.l2CNPEntityCache.HEs[shName]
		dh := cnpd.l2CNPEntityCache.HEs[dhName]

		bdName := "BD_H2H_" + sh.Name + "_" + dh.Name

		ifs := make([]*l2.BridgeDomains_BridgeDomain_Interfaces, 1)
		ifEntry := l2.BridgeDomains_BridgeDomain_Interfaces{
			Name: heToHEState.vlanIf.Name,
		}
		ifs[0] = &ifEntry

		// now create the bridge
		bd, err := cnpd.bridgedDomainCreateWithIfs(sh.Name, bdName, ifs, cnpd.l2CNPEntityCache.SysParms.DynamicBridgeParms)
		if err != nil {
			log.Errorf("createVxLANAndBridgeToDestHost: error creating BD: '%s'", bd.Name)
			return nil, err
		}

		heToHEState.bd = bd
	}

	return heToHEState.bd, nil
}

// north/south NIC type, memIfs/cntrs connect to physical NIC
func (cnpd *sfcCtlrL2CNPDriver) wireSfcNorthSouthNICElements(sfc *controller.SfcEntity) error {

	heCount := 0
	var ifName string
	var he *controller.SfcEntity_SfcElement
	var bd *l2.BridgeDomains_BridgeDomain
	var err error

	// find the host entity and ensure there is only one allowed
	for i, sfcEntityElement := range sfc.GetElements() {

		log.Infof("wireSfcNorthSouthNICElements: sfc entity element[%d]: ", i, sfcEntityElement)

		switch sfcEntityElement.Type {
		case controller.SfcElementType_HOST_ENTITY:
			heCount++
			if heCount > 1 {
				err := fmt.Errorf("wireSfcNorthSouthNICElements: only one he allowed for n/s sfc: '%s'", sfc.Name)
				log.Error(err.Error())
				return err
			}
			he = sfcEntityElement
		}
	}

	if heCount == 0 {
		err := fmt.Errorf("wireSfcNorthSouthNICElements: NO he specified for n/s sfc: '%s'", sfc.Name)
		log.Error(err.Error())
		return err
	}

	// using the parameters for the host interface, create the eth i/f and a bridge for it if NIC_BD l2fib

	mtu := cnpd.getMtu(he.Mtu)
	// physical NIC
	if err := cnpd.createEthernet(he.Container, he.PortLabel, "", he.MacAddr, he.Ipv6Addr, mtu, he.RxMode); err != nil {
		log.Errorf("wireSfcNorthSouthNICElements: error creating ethernet i/f: '%s'", he.PortLabel)
		return err
	}

	if sfc.Type == controller.SfcType_SFC_NS_NIC_BD {

		// bridge domain -based wiring
		ifEntry := &l2.BridgeDomains_BridgeDomain_Interfaces{
			Name: he.PortLabel,
		}
		var bdParms *controller.BDParms
		if sfc.BdParms == nil {
			bdParms = cnpd.l2CNPEntityCache.SysParms.StaticBridgeParms
		} else {
			bdParms = sfc.BdParms
		}
		bdName := "BD_INTERNAL_NS_" + replaceSlashesWithUScores(he.PortLabel)
		bd, err = cnpd.bridgedDomainCreateWithIfs(he.Container, bdName,
			[]*l2.BridgeDomains_BridgeDomain_Interfaces{ifEntry}, bdParms)
		if err != nil {
			log.Errorf("wireSfcNorthSouthNICElements: error creating BD: '%s'", bd.Name)
			return err
		}

		// now create the l2fib entries
		if he.L2FibMacs != nil {
			for _, macAddr := range he.L2FibMacs {
				if _, err := cnpd.createL2FibEntry(he.Container, bd.Name, macAddr, he.PortLabel); err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating l2fib: ewBD: '%s', mac: '%s', i/f: '%s'",
						bd.Name, macAddr, he.PortLabel)
					return err
				}
			}
		}
	}

	if sfc.Type == controller.SfcType_SFC_NS_NIC_VRF {

		err := cnpd.createVRFEntries(he.Container, he, he.PortLabel, "VRF_"+sfc.Name+"_"+he.Container+"_"+he.PortLabel)
		if err != nil {
			log.Errorf("wireSfcNorthSouthNICElements: error creating processing vrf entries i/f: %s/'%s'", he.PortLabel, he)
			return err
		}
	}

	// now wire each container to the bridge on the he
	for i, sfcEntityElement := range sfc.GetElements() {

		log.Infof("wireSfcNorthSouthNICElements: sfc entity element[%d]: ", i, sfcEntityElement)

		switch sfcEntityElement.Type {

		case controller.SfcElementType_VPP_CONTAINER_AFP:
			fallthrough
		case controller.SfcElementType_NON_VPP_CONTAINER_AFP:

			if sfc.Type == controller.SfcType_SFC_NS_NIC_BD {
				// veth pair
				if ifName, err = cnpd.createAFPacketVEthPairAndAddToBridge(sfc, bd, sfcEntityElement); err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating veth pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}

				// now create the l2fib entries
				if sfcEntityElement.L2FibMacs != nil {
					for _, macAddr := range sfcEntityElement.L2FibMacs {
						if _, err := cnpd.createL2FibEntry(sfcEntityElement.EtcdVppSwitchKey, bd.Name, macAddr,
							ifName); err != nil {
							log.Errorf("wireSfcNorthSouthNICElements: error creating l2fib: ewBD: '%s', mac: '%s', i/f: '%s'",
								bd.Name, macAddr, ifName)
							return err
						}
					}
				}

			} else if sfc.Type == controller.SfcType_SFC_NS_NIC_VRF {
				// vrf
				afIfName, err := cnpd.createAFPacketVEthPair(sfc, sfcEntityElement)
				if err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating veth pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}

				err = cnpd.createVRFEntries(sfcEntityElement.EtcdVppSwitchKey, sfcEntityElement, afIfName,
					"VRF_"+sfc.Name+"_"+sfcEntityElement.Container+"_"+sfcEntityElement.PortLabel)
				if err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating processing vrf entries i/f: %s/'%s'", afIfName, sfcEntityElement)
					return err
				}

			} else {
				// l2xconnect -based wiring
				afIfName, err := cnpd.createAFPacketVEthPair(sfc, sfcEntityElement)
				if err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating veth pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}
				err = cnpd.createXConnectPair(sfcEntityElement.EtcdVppSwitchKey, he.PortLabel, afIfName)
				if err != nil {
					return err
				}
			}

		case controller.SfcElementType_VPP_CONTAINER_MEMIF:
			fallthrough
		case controller.SfcElementType_NON_VPP_CONTAINER_MEMIF:

			if sfc.Type == controller.SfcType_SFC_NS_NIC_BD {
				// memif
				if ifName, err = cnpd.createMemIfPairAndAddToBridge(sfc, sfcEntityElement.EtcdVppSwitchKey, bd,
					sfcEntityElement, false); err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating memIf pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}

				// now create the l2fib entries
				if sfcEntityElement.L2FibMacs != nil {
					for _, macAddr := range sfcEntityElement.L2FibMacs {
						if _, err := cnpd.createL2FibEntry(sfcEntityElement.EtcdVppSwitchKey, bd.Name, macAddr,
							ifName); err != nil {
							log.Errorf("wireSfcNorthSouthNICElements: error creating l2fib: ewBD: '%s', mac: '%s', i/f: '%s'",
								bd.Name, macAddr, ifName)
							return err
						}
					}
				}

			} else if sfc.Type == controller.SfcType_SFC_NS_NIC_VRF {
				// vrf
				afIfName, err := cnpd.createAFPacketVEthPair(sfc, sfcEntityElement)
				if err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating veth pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}

				err = cnpd.createVRFEntries(sfcEntityElement.EtcdVppSwitchKey, sfcEntityElement, afIfName,
					"VRF_"+sfc.Name+"_"+sfcEntityElement.Container+"_"+sfcEntityElement.PortLabel)
				if err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating processing vrf entries i/f: %s/'%s'", afIfName, sfcEntityElement)
					return err
				}

			} else {
				// l2xconnect-based wiring
				memIfName, err := cnpd.createMemIfPair(sfc, sfcEntityElement.EtcdVppSwitchKey, sfcEntityElement,
					false)
				if err != nil {
					log.Errorf("wireSfcNorthSouthNICElements: error creating memIf pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}
				err = cnpd.createXConnectPair(sfcEntityElement.EtcdVppSwitchKey, he.PortLabel, memIfName)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) createVRFEntries(etcdVppSwitchKey string, sfcEntityElement *controller.SfcEntity_SfcElement,
	ifaceName string, defaultDescription string) error {

	for i, l3VRFRoute := range sfcEntityElement.GetL3VrfRoutes() {

		weight := l3VRFRoute.Weight
		if weight == 0 {
			weight = cnpd.l2CNPEntityCache.SysParms.DefaultStaticRouteWeight
		}

		pref := l3VRFRoute.Preference
		if pref == 0 {
			pref = cnpd.l2CNPEntityCache.SysParms.DefaultStaticRoutePreference
		}

		vrfDescription := defaultDescription
		if l3VRFRoute.Description != "" {
			vrfDescription = l3VRFRoute.Description
		}

		sr, err := cnpd.createStaticRoute(l3VRFRoute.VrfId, etcdVppSwitchKey, vrfDescription, l3VRFRoute.DstIpAddr,
			l3VRFRoute.NextHopAddr, ifaceName, weight, pref)
		if err != nil {
			log.Errorf("createVRFEntries: error creating static route i/f: %d/'%s'", i, l3VRFRoute)
			return err
		}
		log.Info("createVRFEntries: creating vrf route: '%s'", sr)
	}

	for i, l3VRFArpEntry := range sfcEntityElement.GetL3ArpEntries() {

		ae, err := cnpd.createStaticArpEntry(etcdVppSwitchKey, l3VRFArpEntry.IpAddress, l3VRFArpEntry.PhysAddress,
			ifaceName)
		if err != nil {
			log.Errorf("createVRFEntries: error creating static arp entry i/f: %d/'%s'", i, l3VRFArpEntry)
			return err
		}

		log.Info("createVRFEntries: creating vrf arp entry: '%s'", ae)
	}

	return nil
}

// This is a group of containers that need to be wired to an e/w bridge.
func (cnpd *sfcCtlrL2CNPDriver) wireSfcEastWestElements(sfc *controller.SfcEntity) error {

	var ifName string
	var err error
	var bd *l2.BridgeDomains_BridgeDomain

	prevMemIfName := ""

	if sfc.Type == controller.SfcType_SFC_EW_MEMIF {
		if len(sfc.GetElements())%2 != 0 {
			err := fmt.Errorf("wireSfcEastWestElements: e-w memif sfc should have pairs of entries: '%s'", sfc.Name)
			log.Error(err.Error())
			return err
		}
	}

	for i, sfcEntityElement := range sfc.GetElements() {

		log.Infof("wireSfcEastWestElements: sfc entity element[%d]: ", i, sfcEntityElement)

		switch sfcEntityElement.Type {

		case controller.SfcElementType_EXTERNAL_ENTITY:
			err := fmt.Errorf("wireSfcEastWestElements: external entity not allowed in e-w sfc: '%s'", sfc.Name)
			log.Error(err.Error())
			return err

		case controller.SfcElementType_VPP_CONTAINER_AFP:
			fallthrough
		case controller.SfcElementType_NON_VPP_CONTAINER_AFP:

			if sfc.Type == controller.SfcType_SFC_EW_BD || sfc.Type == controller.SfcType_SFC_EW_BD_L2FIB {

				// TODO: need to revisit when sfc span hosts ...
				heState, exists := cnpd.l2CNPStateCache.HE[sfcEntityElement.EtcdVppSwitchKey]
				if !exists {
					err := fmt.Errorf("wireSfcEastWestElements: cannot find host/bridge: '%s' for this sfc: '%s'",
						sfcEntityElement.EtcdVppSwitchKey, sfc.Name)
					return err
				}

				if sfc.Type == controller.SfcType_SFC_EW_BD { // always use dynamic sys default for this sfc type
					bd = heState.ewBD
				} else if sfc.BdParms == nil { // if l2fib bridge, use static sys default
					bd = heState.ewBDL2Fib
				} else { // bd parms are provided so create bridge using these parms
					sfcToHEMap, exists := cnpd.l2CNPStateCache.SFCToHEs[sfc.Name]
					if !exists {
						cnpd.l2CNPStateCache.SFCToHEs[sfc.Name] = make(map[string]*heStateType, 0)
						sfcToHEMap = cnpd.l2CNPStateCache.SFCToHEs[sfc.Name]
					}
					heState, exists := sfcToHEMap[sfcEntityElement.EtcdVppSwitchKey]
					if !exists {
						bdName := "BD_INTERNAL_EW_" + sfc.Name + "_" + sfcEntityElement.EtcdVppSwitchKey
						bd, err = cnpd.bridgedDomainCreateWithIfs(sfcEntityElement.EtcdVppSwitchKey, bdName, nil, sfc.BdParms)
						if err != nil {
							log.Errorf("WireInternalsForHostEntity: error creating BD: '%s'", bd.Name)
							return err
						}
						heState = &heStateType{
							ewBDL2Fib: bd,
						}
						sfcToHEMap[sfcEntityElement.EtcdVppSwitchKey] = heState
					} else {
						bd = heState.ewBDL2Fib
					}
				}

				if ifName, err = cnpd.createAFPacketVEthPairAndAddToBridge(sfc, bd, sfcEntityElement); err != nil {
					log.Errorf("wireSfcEastWestElements: error creating memIf pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}

				// now create the l2fib entries
				if sfcEntityElement.L2FibMacs != nil {
					for _, macAddr := range sfcEntityElement.L2FibMacs {
						if _, err := cnpd.createL2FibEntry(sfcEntityElement.EtcdVppSwitchKey, bd.Name, macAddr,
							ifName); err != nil {
							log.Errorf("wireSfcNorthSouthNICElements: error creating l2fib: ewBD: '%s', mac: '%s', i/f: '%s'",
								bd.Name, macAddr, ifName)
							return err
						}
					}
				}

			} else {
				// l2xconnect -based wiring
				afIfName, err := cnpd.createAFPacketVEthPair(sfc, sfcEntityElement)
				if err != nil {
					log.Errorf("wireSfcEastWestElements: error creating veth pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}
				if prevMemIfName != "" {
					err = cnpd.createXConnectPair(sfcEntityElement.EtcdVppSwitchKey, afIfName, prevMemIfName)
					prevMemIfName = ""
					if err != nil {
						return err
					}
				} else {
					prevMemIfName = afIfName
				}
			}

		case controller.SfcElementType_VPP_CONTAINER_MEMIF:
			fallthrough
		case controller.SfcElementType_NON_VPP_CONTAINER_MEMIF:

			if sfc.Type == controller.SfcType_SFC_EW_MEMIF {
				if i%2 == 0 {
					// need to create an inter-container memif, use the left of the pair to create the pair
					if err := cnpd.createOneOrMoreInterContainerMemIfPairs(sfc.Name, sfc.Elements[i], sfc.Elements[i+1],
						sfc.VnfRepeatCount); err != nil {
						log.Errorf("wireSfcEastWestElements: error creating memIf pair: sfc: '%s', Container: '%s', i='%d'",
							sfc.Name, sfcEntityElement.Container, i)
						return err
					}
				}
			} else if sfc.Type == controller.SfcType_SFC_EW_BD || sfc.Type == controller.SfcType_SFC_EW_BD_L2FIB {

				// TODO: need to revisit when sfc span hosts ...
				heState, exists := cnpd.l2CNPStateCache.HE[sfcEntityElement.EtcdVppSwitchKey]
				if !exists {
					err := fmt.Errorf("wireSfcEastWestElements: cannot find host/bridge: '%s' for this sfc: '%s'",
						sfcEntityElement.EtcdVppSwitchKey, sfc.Name)
					return err
				}

				if sfc.Type == controller.SfcType_SFC_EW_BD { // always use dynamic sys default for this sfc type
					bd = heState.ewBD
				} else if sfc.BdParms == nil { // if l2fib bridge, use static sys default
					bd = heState.ewBDL2Fib
				} else { // bd parms are provided so create bridge using these parms
					sfcToHEMap, exists := cnpd.l2CNPStateCache.SFCToHEs[sfc.Name]
					if !exists {
						cnpd.l2CNPStateCache.SFCToHEs[sfc.Name] = make(map[string]*heStateType, 0)
						sfcToHEMap = cnpd.l2CNPStateCache.SFCToHEs[sfc.Name]
					}
					heState, exists := sfcToHEMap[sfcEntityElement.EtcdVppSwitchKey]
					if !exists {
						bdName := "BD_INTERNAL_EW_" + sfc.Name + "_" + sfcEntityElement.EtcdVppSwitchKey
						bd, err = cnpd.bridgedDomainCreateWithIfs(sfcEntityElement.EtcdVppSwitchKey, bdName, nil, sfc.BdParms)
						if err != nil {
							log.Errorf("WireInternalsForHostEntity: error creating BD: '%s'", bd.Name)
							return err
						}
						heState = &heStateType{
							ewBDL2Fib: bd,
						}
						sfcToHEMap[sfcEntityElement.EtcdVppSwitchKey] = heState
					} else {
						bd = heState.ewBDL2Fib
					}
				}

				if ifName, err = cnpd.createMemIfPairAndAddToBridge(sfc, sfcEntityElement.EtcdVppSwitchKey, bd,
					sfcEntityElement, true); err != nil {
					log.Errorf("wireSfcEastWestElements: error creating memIf pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}

				// now create the l2fib entries
				if sfcEntityElement.L2FibMacs != nil {
					for _, macAddr := range sfcEntityElement.L2FibMacs {
						if _, err := cnpd.createL2FibEntry(sfcEntityElement.EtcdVppSwitchKey, bd.Name, macAddr,
							ifName); err != nil {
							log.Errorf("wireSfcNorthSouthNICElements: error creating l2fib: ewBD: '%s', mac: '%s', i/f: '%s'",
								bd.Name, macAddr, ifName)
							return err
						}
					}
				}

			} else {
				// l2xconnect -based wiring
				memIfName, err := cnpd.createMemIfPair(sfc, sfcEntityElement.EtcdVppSwitchKey, sfcEntityElement,
					false)
				if err != nil {
					log.Errorf("wireSfcEastWestElements: error creating memIf pair: sfc: '%s', Container: '%s'",
						sfc.Name, sfcEntityElement.Container)
					return err
				}
				if prevMemIfName != "" {
					err = cnpd.createXConnectPair(sfcEntityElement.EtcdVppSwitchKey, memIfName, prevMemIfName)
					prevMemIfName = ""
					if err != nil {
						return err
					}
				} else {
					prevMemIfName = memIfName
				}
			}
		}
	}

	return nil
}

// createOneOrMoreInterContainerMemIfPairs creates memif pair and returns vswitch-end memif interface name
func (cnpd *sfcCtlrL2CNPDriver) createOneOrMoreInterContainerMemIfPairs(
	sfcName string,
	vnfElement1 *controller.SfcEntity_SfcElement,
	vnfElement2 *controller.SfcEntity_SfcElement,
	vnfRepeatCount uint32) error {

	log.Infof("createInterContainerMemIfPair: sfc: '%s', vnf1: '%s', vnf2: '%s', repeatCount: '%d'",
		sfcName, vnfElement1.Container, vnfElement2.Container, vnfRepeatCount)

	vnf1Port := ""
	vnf2Port := ""
	mtu := cnpd.getMtu(vnfElement1.Mtu)
	rxMode := vnfElement1.RxMode
	container1Name := ""
	container2Name := ""

	for repeatCount := uint32(0); repeatCount <= vnfRepeatCount; repeatCount++ {

		if repeatCount == 0 {
			container1Name = vnfElement1.Container
			vnf1Port = vnfElement1.PortLabel
		} else {
			container1Name = "vnfx-" + strconv.Itoa(int(repeatCount-1))
			vnf1Port = vnfElement1.PortLabel
		}
		if repeatCount == vnfRepeatCount {
			container2Name = vnfElement2.Container
			vnf2Port = vnfElement2.PortLabel
		} else {
			container2Name = "vnfx-" + strconv.Itoa(int(repeatCount))
			vnf2Port = vnfElement2.PortLabel
		}

		var memifID uint32

		sfcID, _ := cnpd.DatastoreSFCIDsRetrieve(sfcName, container1Name, vnf1Port)
		if sfcID == nil || sfcID.MemifId == 0 {
			cnpd.seq.MemIfID++
			memifID = cnpd.seq.MemIfID
		} else {
			memifID = sfcID.MemifId
		}

		// create a memif in the vnf container
		if err := cnpd.createInterContainerMemIfPair(
			sfcName,
			container1Name, vnf1Port,
			container2Name, vnf2Port,
			mtu,
			rxMode,
			memifID); err != nil {
			return err
		}

		key, sfcID, err := cnpd.DatastoreSFCIDsCreate(sfcName, container1Name, vnf1Port,
			0, 0, memifID, 0)
		if err == nil && cnpd.reconcileInProgress {
			cnpd.reconcileAfter.sfcIDs[key] = *sfcID
		}
	}

	return nil
}

// createInterContainerMemIfPair creates memif pair and returns vswitch-end memif interface name
func (cnpd *sfcCtlrL2CNPDriver) createInterContainerMemIfPair(
	sfcName string,
	vnf1Container string, vnf1Port string,
	vnf2Container string, vnf2Port string,
	mtu uint32,
	rxMode controller.RxModeType,
	memIFID uint32) error {

	log.Infof("createInterContainerMemIfPair: vnf1: '%s'/'%s', vnf2: '%s'/'%s', memIfID: '%d'",
		vnf1Container, vnf1Port, vnf2Container, vnf2Port, memIFID)

	// create a memif in the vnf container 1
	if _, err := cnpd.memIfCreate(vnf1Container, vnf1Port, memIFID, true, vnf1Container,
		"", "", "", mtu, rxMode); err != nil {
		log.Errorf("createInterContainerMemIfPair: error creating memIf for container: '%s'/'%s', memIF: '%d'",
			vnf1Container, vnf1Port, memIFID)
		return err
	}

	// create a memif in the vnf container 2
	if _, err := cnpd.memIfCreate(vnf2Container, vnf2Port, memIFID, false, vnf1Container,
		"", "", "", mtu, rxMode); err != nil {

		log.Errorf("createInterContainerMemIfPair: error creating memIf for container: '%s'/'%s', memIF: '%d'",
			vnf1Container, vnf1Port, memIFID)
		return err
	}

	return nil
}

// createMemIfPair creates memif pair and returns vswitch-end memif interface name
func (cnpd *sfcCtlrL2CNPDriver) createMemIfPair(sfc *controller.SfcEntity, hostName string,
	vnfChainElement *controller.SfcEntity_SfcElement, generateAddresses bool) (string, error) {

	log.Infof("createMemIfPair: vnf: '%s', host: '%s'", vnfChainElement.Container, hostName)

	var memifID uint32
	var macAddrID uint32
	var ipID uint32

	sfcID, err := cnpd.DatastoreSFCIDsRetrieve(sfc.Name, vnfChainElement.Container, vnfChainElement.PortLabel)
	if sfcID == nil || sfcID.MemifId == 0 {
		cnpd.seq.MemIfID++
		memifID = cnpd.seq.MemIfID
	} else {
		memifID = sfcID.MemifId
	}

	var macAddress string
	var ipv4Address string

	// the sfc controller can generate addresses if not provided
	if vnfChainElement.Ipv4Addr == "" {
		if generateAddresses {
			if sfc.SfcIpv4Prefix != "" {
				if sfcID == nil || sfcID.IpId == 0 {
					ipv4Address, ipID, err = ipam.AllocateFromSubnet(sfc.SfcIpv4Prefix)
					if err != nil {
						return "", err
					}
				} else {
					ipv4Address, err = ipam.SetIpIDInSubnet(sfc.SfcIpv4Prefix, sfcID.IpId)
					if err != nil {
						return "", err
					}
					ipID = sfcID.IpId
				}
			}
		}
	} else {
		strs := strings.Split(vnfChainElement.Ipv4Addr, "/")
		if len(strs) == 2 {
			ipv4Address = vnfChainElement.Ipv4Addr
		} else {
			ipv4Address = vnfChainElement.Ipv4Addr + "/24"
		}
		if sfc.SfcIpv4Prefix != "" {
			ipam.SetIpAddrIfInsideSubnet(sfc.SfcIpv4Prefix, strs[0])
		}
	}
	if sfc.SfcIpv4Prefix != "" {
		log.Info("createMemIfPair: ", ipam.DumpSubnet(sfc.SfcIpv4Prefix), ipv4Address)
	}

	if vnfChainElement.MacAddr == "" {
		if generateAddresses {
			if sfcID == nil || sfcID.MacAddrId == 0 {
				cnpd.seq.MacInstanceID++
				macAddress = formatMacAddress(cnpd.seq.MacInstanceID)
				macAddrID = cnpd.seq.MacInstanceID
			} else {
				macAddress = formatMacAddress(sfcID.MacAddrId)
				macAddrID = sfcID.MacAddrId
			}
		}
	} else {
		macAddress = vnfChainElement.MacAddr
	}

	mtu := cnpd.getMtu(vnfChainElement.Mtu)
	rxMode := vnfChainElement.RxMode

	// create a memif in the vnf container
	memIfName := vnfChainElement.PortLabel
	if _, err := cnpd.memIfCreate(vnfChainElement.Container, memIfName, memifID, false, vnfChainElement.EtcdVppSwitchKey,
		ipv4Address, macAddress, vnfChainElement.Ipv6Addr, mtu, rxMode); err != nil {
		log.Errorf("createMemIfPair: error creating memIf for container: '%s'", memIfName)
		return "", err
	}

	// now create a memif for the vpp switch
	memIfName = "IF_MEMIF_VSWITCH_" + vnfChainElement.Container + "_" + vnfChainElement.PortLabel
	memIf, err := cnpd.memIfCreate(vnfChainElement.EtcdVppSwitchKey, memIfName, memifID,
		true, vnfChainElement.EtcdVppSwitchKey, "", "", "", mtu, rxMode)
	if err != nil {
		log.Errorf("createMemIfPair: error creating memIf for vpp switch: '%s'", memIf.Name)
		return "", err
	}

	key, sfcID, err := cnpd.DatastoreSFCIDsCreate(sfc.Name, vnfChainElement.Container, vnfChainElement.PortLabel,
		ipID, macAddrID, memifID, 0)
	if err == nil && cnpd.reconcileInProgress {
		cnpd.reconcileAfter.sfcIDs[key] = *sfcID
	}

	cnpd.setSfcInterfaceIPAndMac(vnfChainElement.Container, vnfChainElement.PortLabel, ipv4Address, macAddress)

	return memIfName, err
}

// createMemIfPairAndAddToBridge creates a memif pair and adds the vswitch-end interface into the provided bridge domain
func (cnpd *sfcCtlrL2CNPDriver) createMemIfPairAndAddToBridge(sfc *controller.SfcEntity, hostName string,
	bd *l2.BridgeDomains_BridgeDomain, vnfChainElement *controller.SfcEntity_SfcElement,
	generateAddresses bool) (string, error) {

	memIfName, err := cnpd.createMemIfPair(sfc, hostName, vnfChainElement, generateAddresses)
	if err != nil {
		return "", err
	}

	ifEntry := l2.BridgeDomains_BridgeDomain_Interfaces{
		Name: memIfName,
	}
	ifs := make([]*l2.BridgeDomains_BridgeDomain_Interfaces, 1)
	ifs[0] = &ifEntry

	if err := cnpd.bridgedDomainAssociateWithIfs(vnfChainElement.EtcdVppSwitchKey, bd, ifs); err != nil {
		log.Errorf("createMemIfPairAndAddToBridge: error creating BD: '%s'", bd.Name)
		return "", err
	}

	return memIfName, nil
}

func (cnpd *sfcCtlrL2CNPDriver) createAFPacketVEthPair(sfc *controller.SfcEntity,
	vnfChainElement *controller.SfcEntity_SfcElement) (string, error) {

	log.Infof("createAFPacketVEthPair: vnf: '%s', host: '%s'", vnfChainElement.Container,
		vnfChainElement.EtcdVppSwitchKey)

	var macAddrID uint32
	var vethID uint32
	var ipID uint32
	var macAddress string
	var ipv4Address string

	ipv6Address := vnfChainElement.Ipv6Addr

	sfcID, err := cnpd.DatastoreSFCIDsRetrieve(sfc.Name, vnfChainElement.Container, vnfChainElement.PortLabel)

	if sfcID == nil || sfcID.VethId == 0 {
		cnpd.seq.VethID++
		vethID = cnpd.seq.VethID
	} else {
		vethID = sfcID.VethId
	}

	if vnfChainElement.Ipv4Addr == "" {
		if sfc.SfcIpv4Prefix != "" {
			if sfcID == nil || sfcID.IpId == 0 {
				ipv4Address, ipID, err = ipam.AllocateFromSubnet(sfc.SfcIpv4Prefix)
				if err != nil {
					return "", err
				}
			} else {
				ipv4Address, err = ipam.SetIpIDInSubnet(sfc.SfcIpv4Prefix, sfcID.IpId)
				if err != nil {
					return "", err
				}
				ipID = sfcID.IpId
			}
		}
	} else {
		strs := strings.Split(vnfChainElement.Ipv4Addr, "/")
		if len(strs) == 2 {
			ipv4Address = vnfChainElement.Ipv4Addr
		} else {
			ipv4Address = vnfChainElement.Ipv4Addr + "/24"
		}
		if sfc.SfcIpv4Prefix != "" {
			ipam.SetIpAddrIfInsideSubnet(sfc.SfcIpv4Prefix, strs[0])
		}
	}
	if sfc.SfcIpv4Prefix != "" {
		log.Info("createAFPacketVEthPair: ", ipam.DumpSubnet(sfc.SfcIpv4Prefix), ipv4Address)
	}

	if vnfChainElement.MacAddr == "" {
		if sfcID == nil || sfcID.MacAddrId == 0 {
			cnpd.seq.MacInstanceID++
			macAddress = formatMacAddress(cnpd.seq.MacInstanceID)
			macAddrID = cnpd.seq.MacInstanceID
		} else {
			macAddress = formatMacAddress(sfcID.MacAddrId)
			macAddrID = sfcID.MacAddrId
		}
	} else {
		macAddress = vnfChainElement.MacAddr
	}

	mtu := cnpd.getMtu(vnfChainElement.Mtu)
	rxMode := vnfChainElement.RxMode

	// Create a VETH if for the vnf container. VETH will get created by the agent from a more privileged vswitch.
	// Note: In Linux kernel the length of an interface name is limited by the constant IFNAMSIZ.
	//       In most distributions this is 16 characters including the terminating NULL character.
	//		 The hostname uses chars from the container, and port name plus a unique id base 36
	//       for a total of at most 15 chars. 3 chars for base36 given 36x36x36 = lots of interfaces

	veth1Name := "IF_VETH_VNF_" + vnfChainElement.Container + "_" + vnfChainElement.PortLabel
	veth2Name := "IF_VETH_VSWITCH_" + vnfChainElement.Container + "_" + vnfChainElement.PortLabel

	host1Name := vnfChainElement.PortLabel

	vethIDStr := strconv.FormatUint(uint64(vethID), 36)
	baseHostName := constructBaseHostName(vnfChainElement.Container, vnfChainElement.PortLabel, vethIDStr)
	host2Name := baseHostName + "_" + vethIDStr

	ipv4AddrForVEth := ipv4Address
	ipv4AddrForAFP := ipv4Address
	ipv6AddrForVEth := ipv6Address
	ipv6AddrForAFP := ipv6Address
	if vnfChainElement.Type == controller.SfcElementType_VPP_CONTAINER_AFP {
		ipv4AddrForVEth = ""
		ipv6AddrForVEth = ""
	}
	// Configure the VETH interface for the VNF end
	if err := cnpd.vEthIfCreate(vnfChainElement.EtcdVppSwitchKey, veth1Name, host1Name, veth2Name,
		vnfChainElement.Container, macAddress, ipv4AddrForVEth, ipv6AddrForVEth, mtu); err != nil {
		log.Errorf("createAFPacketVEthPair: error creating veth if '%s' for container: '%s'", veth1Name,
			vnfChainElement.Container)
		return "", err
	}
	// Configure the VETH interface for the VSWITCH end
	if err := cnpd.vEthIfCreate(vnfChainElement.EtcdVppSwitchKey, veth2Name, host2Name, veth1Name,
		vnfChainElement.EtcdVppSwitchKey, "", "", "", mtu); err != nil {
		log.Errorf("createAFPacketVEthPair: error creating veth if '%s' for container: '%s'", veth2Name,
			vnfChainElement.EtcdVppSwitchKey)
		return "", err
	}
	// create af_packet for the vnf -end of the veth
	if vnfChainElement.Type == controller.SfcElementType_VPP_CONTAINER_AFP {
		afPktIf1, err := cnpd.afPacketCreate(vnfChainElement.Container, vnfChainElement.PortLabel,
			host1Name, ipv4AddrForAFP, macAddress, ipv6AddrForAFP, mtu, rxMode)
		if err != nil {
			log.Errorf("createAFPacketVEthPair: error creating afpacket for vpp switch: '%s'", afPktIf1.Name)
			return "", err
		}
	}
	// create af_packet for the vswitch -end of the veth
	afPktName := "IF_AFPIF_VSWITCH_" + vnfChainElement.Container + "_" + vnfChainElement.PortLabel
	afPktIf2, err := cnpd.afPacketCreate(vnfChainElement.EtcdVppSwitchKey, afPktName, host2Name,
		"", "", "", mtu, rxMode)
	if err != nil {
		log.Errorf("createAFPacketVEthPair: error creating afpacket for vpp switch: '%s'", afPktIf2.Name)
		return "", err
	}

	key, sfcID, err := cnpd.DatastoreSFCIDsCreate(sfc.Name, vnfChainElement.Container, vnfChainElement.PortLabel,
		ipID, macAddrID, 0, vethID)
	if err == nil && cnpd.reconcileInProgress {
		cnpd.reconcileAfter.sfcIDs[key] = *sfcID
	}

	cnpd.setSfcInterfaceIPAndMac(vnfChainElement.Container, vnfChainElement.PortLabel, ipv4Address, macAddress)

	return afPktIf2.Name, nil
}

func (cnpd *sfcCtlrL2CNPDriver) createAFPacketVEthPairAndAddToBridge(sfc *controller.SfcEntity,
	bd *l2.BridgeDomains_BridgeDomain, vnfChainElement *controller.SfcEntity_SfcElement) (string, error) {

	log.Infof("createAFPacketVEthPairAndAddToBridge: vnf: '%s', host: '%s'", vnfChainElement.Container,
		vnfChainElement.EtcdVppSwitchKey)

	afPktIfName, err := cnpd.createAFPacketVEthPair(sfc, vnfChainElement)
	if err != nil {
		return "", err
	}

	ifEntry := l2.BridgeDomains_BridgeDomain_Interfaces{
		Name: afPktIfName,
	}
	ifs := make([]*l2.BridgeDomains_BridgeDomain_Interfaces, 1)
	ifs[0] = &ifEntry

	if err := cnpd.bridgedDomainAssociateWithIfs(vnfChainElement.EtcdVppSwitchKey, bd, ifs); err != nil {
		log.Errorf("createAFPacketVEthPairAndAddToBridge: error creating BD: '%s'", bd.Name)
		return "", err
	}

	return afPktIfName, nil
}

func (cnpd *sfcCtlrL2CNPDriver) bridgedDomainCreateWithIfs(etcdVppSwitchKey string, bdName string,
	ifs []*l2.BridgeDomains_BridgeDomain_Interfaces, bdParms *controller.BDParms) (*l2.BridgeDomains_BridgeDomain, error) {

	bd := &l2.BridgeDomains_BridgeDomain{
		Name:                bdName,
		Flood:               bdParms.Flood,
		UnknownUnicastFlood: bdParms.UnknownUnicastFlood,
		Forward:             bdParms.Forward,
		Learn:               bdParms.Learn,
		ArpTermination:      bdParms.ArpTermination,
		MacAge:              bdParms.MacAge,
		Interfaces:          ifs,
	}

	if cnpd.reconcileInProgress {
		cnpd.reconcileBridgeDomain(etcdVppSwitchKey, bd)
	} else {

		log.Println(bd)

		rc := NewRemoteClientTxn(etcdVppSwitchKey, cnpd.dbFactory)
		err := rc.Put().BD(bd).Send().ReceiveReply()

		if err != nil {
			log.Error("vxLanCreate: databroker.Store: ", err)
			return nil, err

		}
	}

	return bd, nil
}

// using the existing bridge, append the new if to the existing ifs in the bridge
func (cnpd *sfcCtlrL2CNPDriver) bridgedDomainAssociateWithIfs(etcdVppSwitchKey string,
	bd *l2.BridgeDomains_BridgeDomain,
	ifs []*l2.BridgeDomains_BridgeDomain_Interfaces) error {

	// only add the interface to ewBD array if it is not already in the bridge's interface array
	for _, iface := range ifs {
		found := false
		for _, bi := range bd.Interfaces {
			if bi.Name == iface.Name {
				found = true
				break
			}
		}
		if !found {
			bd.Interfaces = append(bd.Interfaces, iface)
		}
	}

	if cnpd.reconcileInProgress {
		cnpd.reconcileBridgeDomain(etcdVppSwitchKey, bd)
	} else {

		log.Println(bd)

		rc := NewRemoteClientTxn(etcdVppSwitchKey, cnpd.dbFactory)
		err := rc.Put().BD(bd).Send().ReceiveReply()

		if err != nil {
			log.Error("vxLanCreate: databroker.Store: ", err)
			return err

		}
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) vxLanCreate(etcdVppSwitchKey string, ifname string, vni uint32,
	srcStr string, dstStr string) (*interfaces.Interfaces_Interface, error) {

	src := stripSlashAndSubnetIpv4Address(srcStr)
	dst := stripSlashAndSubnetIpv4Address(dstStr)

	iface := &interfaces.Interfaces_Interface{
		Name:    ifname,
		Type:    interfaces.InterfaceType_VXLAN_TUNNEL,
		Enabled: true,
		Vxlan: &interfaces.Interfaces_Interface_Vxlan{
			SrcAddress: src,
			DstAddress: dst,
			Vni:        vni,
		},
	}

	if cnpd.reconcileInProgress {
		cnpd.reconcileInterface(etcdVppSwitchKey, iface)
	} else {

		log.Println(*iface)

		rc := NewRemoteClientTxn(etcdVppSwitchKey, cnpd.dbFactory)
		err := rc.Put().VppInterface(iface).Send().ReceiveReply()

		if err != nil {
			log.Error("vxLanCreate: databroker.Store: ", err)
			return nil, err

		}
	}

	return iface, nil
}

func constructIpv4AndV6AddressArray(ipv4 string, ipv6 string) []string {

	var ipAddrArray []string

	if ipv4 != "" || ipv6 != "" {
		if ipv4 != "" {
			ipAddrArray = make([]string, 1)
			ipAddrArray[0] = ipv4
		}
		if ipv6 != "" {
			if ipAddrArray == nil {
				ipAddrArray = make([]string, 1)
				ipAddrArray[0] = ipv6
			} else {
				ipAddrArray = append(ipAddrArray, ipv6)
			}
		}
	}
	return ipAddrArray
}

func (cnpd *sfcCtlrL2CNPDriver) memIfCreate(etcdPrefix string, memIfName string, memifID uint32, isMaster bool,
	masterContainer string, ipv4 string, macAddress string, ipv6 string, mtu uint32,
	rxMode controller.RxModeType) (*interfaces.Interfaces_Interface, error) {

	memIf := &interfaces.Interfaces_Interface{
		Name:        memIfName,
		Type:        interfaces.InterfaceType_MEMORY_INTERFACE,
		Enabled:     true,
		PhysAddress: macAddress,
		Mtu:         mtu,
		IpAddresses: constructIpv4AndV6AddressArray(ipv4, ipv6),
		Memif: &interfaces.Interfaces_Interface_Memif{
			Id:             memifID,
			Master:         isMaster,
			SocketFilename: "/tmp/memif_" + masterContainer + ".sock",
		},
	}

	memIf.RxModeSettings = rxModeControllerToInterface(rxMode)

	if cnpd.reconcileInProgress {
		cnpd.reconcileInterface(etcdPrefix, memIf)
	} else {

		log.Println(*memIf)

		rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
		err := rc.Put().VppInterface(memIf).Send().ReceiveReply()

		if err != nil {
			log.Error("memIfCreate: databroker.Store: ", err)
			return nil, err

		}
	}

	return memIf, nil
}

func rxModeControllerToInterface(contrtollerRxMode controller.RxModeType) *interfaces.Interfaces_Interface_RxModeSettings {

	rxSettings := &interfaces.Interfaces_Interface_RxModeSettings{}
	switch contrtollerRxMode {
	case controller.RxModeType_RX_MODE_INTERRUPT:
		rxSettings.RxMode = interfaces.RxModeType_INTERRUPT
		return rxSettings
	case controller.RxModeType_RX_MODE_POLLING:
		rxSettings.RxMode = interfaces.RxModeType_POLLING
		return rxSettings
	}
	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) createEthernet(etcdPrefix string, ifname string, ipv4 string, macAddr string,
	ipv6 string, mtu uint32, rxMode controller.RxModeType) error {

	iface := &interfaces.Interfaces_Interface{
		Name:        ifname,
		Type:        interfaces.InterfaceType_ETHERNET_CSMACD,
		Enabled:     true,
		PhysAddress: macAddr,
		IpAddresses: constructIpv4AndV6AddressArray(ipv4, ipv6),
		Mtu:         mtu,
	}

	iface.RxModeSettings = rxModeControllerToInterface(rxMode)

	if cnpd.reconcileInProgress {
		cnpd.reconcileInterface(etcdPrefix, iface)
	} else {

		log.Println(*iface)

		rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
		err := rc.Put().VppInterface(iface).Send().ReceiveReply()

		if err != nil {
			log.Error("createEthernet: databroker.Store: ", err)
			return err

		}
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) afPacketCreate(etcdPrefix string, ifName string, hostIfName string, ipv4 string,
	macAddress string, ipv6 string, mtu uint32, rxMode controller.RxModeType) (*interfaces.Interfaces_Interface, error) {

	afPacketIf := &interfaces.Interfaces_Interface{
		Name:        ifName,
		Type:        interfaces.InterfaceType_AF_PACKET_INTERFACE,
		Enabled:     true,
		PhysAddress: macAddress,
		IpAddresses: constructIpv4AndV6AddressArray(ipv4, ipv6),
		Mtu:         mtu,
		Afpacket: &interfaces.Interfaces_Interface_Afpacket{
			HostIfName: hostIfName,
		},
	}

	afPacketIf.RxModeSettings = rxModeControllerToInterface(rxMode)

	if cnpd.reconcileInProgress {
		cnpd.reconcileInterface(etcdPrefix, afPacketIf)
	} else {

		log.Println(*afPacketIf)

		rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
		err := rc.Put().VppInterface(afPacketIf).Send().ReceiveReply()

		if err != nil {
			log.Error("afPacketCreate: databroker.Store: ", err)
			return nil, err

		}
	}

	return afPacketIf, nil
}

func (cnpd *sfcCtlrL2CNPDriver) createLoopback(etcdPrefix string, ifname string, physAddr string, ipv4 string,
	ipv6 string, mtu uint32, rxMode controller.RxModeType) error {

	iface := &interfaces.Interfaces_Interface{
		Name:        ifname,
		Type:        interfaces.InterfaceType_SOFTWARE_LOOPBACK,
		Enabled:     true,
		PhysAddress: physAddr,
		Mtu:         mtu,
		IpAddresses: constructIpv4AndV6AddressArray(ipv4, ipv6),
	}

	iface.RxModeSettings = rxModeControllerToInterface(rxMode)

	if cnpd.reconcileInProgress {
		cnpd.reconcileInterface(etcdPrefix, iface)
	} else {

		log.Println(*iface)

		rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
		err := rc.Put().VppInterface(iface).Send().ReceiveReply()

		if err != nil {
			log.Error("createLoopback: databroker.Store: ", err)
			return err

		}
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) vEthIfCreate(etcdPrefix string, ifname string, hostIfName, peerIfName string, container string,
	physAddr string, ipv4 string, ipv6 string, mtu uint32) error {

	linuxif := &linuxIntf.LinuxInterfaces_Interface{
		Name:        ifname,
		Type:        linuxIntf.LinuxInterfaces_VETH,
		Enabled:     true,
		PhysAddress: physAddr,
		HostIfName:  hostIfName,
		IpAddresses: constructIpv4AndV6AddressArray(ipv4, ipv6),
		Mtu:         mtu,
		Namespace: &linuxIntf.LinuxInterfaces_Interface_Namespace{
			Type:         linuxIntf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS,
			Microservice: container,
		},
		Veth: &linuxIntf.LinuxInterfaces_Interface_Veth{
			PeerIfName: peerIfName,
		},
	}

	if cnpd.reconcileInProgress {
		cnpd.reconcileLinuxInterface(etcdPrefix, ifname, linuxif)
	} else {

		log.Println(linuxif)

		rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
		err := rc.Put().LinuxInterface(linuxif).Send().ReceiveReply()

		if err != nil {
			log.Error("createLoopback: databroker.Store: ", err)
			return err
		}
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) createStaticRoute(vrfID uint32, etcdPrefix string, description string, destIpv4AddrStr string,
	netHopIpv4Addr string, outGoingIf string, weight uint32, pref uint32) (*l3.StaticRoutes_Route, error) {

	sr := &l3.StaticRoutes_Route{
		VrfId:             vrfID,
		Description:       description,
		DstIpAddr:         destIpv4AddrStr,
		NextHopAddr:       stripSlashAndSubnetIpv4Address(netHopIpv4Addr),
		Weight:            weight,
		OutgoingInterface: outGoingIf,
		Preference:        pref,
	}

	if cnpd.reconcileInProgress {
		cnpd.reconcileStaticRoute(etcdPrefix, sr)
	} else {

		log.Println(sr)

		rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
		err := rc.Put().StaticRoute(sr).Send().ReceiveReply()

		if err != nil {
			log.Error("createStaticRoute: databroker.Store: ", err)
			return nil, err

		}
	}

	return sr, nil
}

func (cnpd *sfcCtlrL2CNPDriver) createStaticArpEntry(etcdPrefix string, destIPAddress string, physAddress string,
	outGoingIf string) (*l3.ArpTable_ArpTableEntry, error) {

	ae := &l3.ArpTable_ArpTableEntry{
		Interface:   outGoingIf,
		Static:      true,
		IpAddress:   destIPAddress,
		PhysAddress: physAddress,
	}

	//if cnpd.reconcileInProgress {
	//cnpd.reconcileStaticArpEntry(etcdPrefix, ae)
	//} else {

	key := utils.ArpEntryKey(etcdPrefix, outGoingIf, destIPAddress)

	log.Println(key)
	log.Println(ae)

	log.Info("createStaticArpEntry: arp entry: : ", key, ae)

	//rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
	err := cnpd.db.Put(key, ae)
	//err := rc.Put(key, ae)

	if err != nil {
		log.Error("createStaticArpEntry: databroker.Store: ", err)
		return nil, err

	}
	//}

	return ae, nil
}

func (cnpd *sfcCtlrL2CNPDriver) createXConnectPair(etcdPrefix, if1, if2 string) error {

	err := cnpd.createXConnect(etcdPrefix, if1, if2)
	if err != nil {
		return err
	}

	err = cnpd.createXConnect(etcdPrefix, if2, if1)
	if err != nil {
		return err
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) createXConnect(etcdPrefix, rxIf, txIf string) error {

	xconn := &l2.XConnectPairs_XConnectPair{
		ReceiveInterface:  rxIf,
		TransmitInterface: txIf,
	}

	log.Debugf("Storing l2xconnect config: %s", xconn)

	rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
	err := rc.Put().XConnect(xconn).Send().ReceiveReply()
	if err != nil {
		log.Errorf("Error by storing l2xconnect: %s", err)
		return err
	}

	return nil
}

func (cnpd *sfcCtlrL2CNPDriver) createL2FibEntry(etcdPrefix string, bdName string, destMacAddr string,
	outGoingIf string) (*l2.FibTableEntries_FibTableEntry, error) {

	l2fib := &l2.FibTableEntries_FibTableEntry{
		BridgeDomain:      bdName,
		PhysAddress:       destMacAddr,
		Action:            l2.FibTableEntries_FibTableEntry_FORWARD,
		OutgoingInterface: outGoingIf,
		StaticConfig:      true,
	}

	//if cnpd.reconcileInProgress {
	//	cnpd.reconcileL2FibEntry(etcdPrefix, l2fib)
	//} else {

	log.Println(l2fib)

	rc := NewRemoteClientTxn(etcdPrefix, cnpd.dbFactory)
	err := rc.Put().BDFIB(l2fib).Send().ReceiveReply()

	if err != nil {
		log.Error("createL2Fib: databroker.Store: ", err)
		return nil, err

	}
	//}

	return l2fib, nil
}

// Debug dump routine
func (cnpd *sfcCtlrL2CNPDriver) Dump() {
	log.Println(cnpd.seq)
	log.Println(cnpd.l2CNPEntityCache)
	log.Println(cnpd.l2CNPStateCache)
}

func (cnpd *sfcCtlrL2CNPDriver) getHEToEEState(heName string, eeName string) *heToEEStateType {

	eeMap, exists := cnpd.l2CNPStateCache.HEToEEs[heName]
	if !exists {
		return nil
	}
	return eeMap[eeName]
}

func (cnpd *sfcCtlrL2CNPDriver) getMtu(mtu uint32) uint32 {

	log.Info("getMtu: ", mtu)
	if mtu == 0 {
		mtu = cnpd.l2CNPEntityCache.SysParms.Mtu
		log.Info("getMtu: replacing with system value: ", mtu)
	}
	return mtu
}

func replaceSlashesWithUScores(slashesString string) string {
	strs := strings.Split(slashesString, "/")
	UScoresString := strs[0]
	for i := 1; i < len(strs); i++ {
		UScoresString += "_" + strs[i]
	}
	return UScoresString
}

func formatMacAddress(macInstanceID32 uint32) string {

	// uint32 is 4Billion interfaces ... lets not worry about it

	var macOctets [6]uint64
	macInstanceID := uint64(macInstanceID32)

	macOctets[0] = 0x02
	macOctets[1] = 0xFF & (macInstanceID >> (8 * 4))
	macOctets[2] = 0xFF & (macInstanceID >> (8 * 3))
	macOctets[3] = 0xFF & (macInstanceID >> (8 * 2))
	macOctets[4] = 0xFF & (macInstanceID >> (8 * 1))
	macOctets[5] = 0xFF & (macInstanceID >> (8 * 0))

	macOctetString := ""
	for i := 0; i < 5; i++ {
		macOctetString += fmt.Sprintf("%02X:", macOctets[i])
	}
	macOctetString += fmt.Sprintf("%02X", macOctets[5])

	return macOctetString
}

// if the ip address has a /xx subnet attached, it is stripped off
func stripSlashAndSubnetIpv4Address(ipAndSubnetStr string) string {
	strs := strings.Split(ipAndSubnetStr, "/")
	return strs[0]
}

// ByIfName is used to sort i/f by name
type ByIfName []*l2.BridgeDomains_BridgeDomain_Interfaces

func (a ByIfName) Len() int           { return len(a) }
func (a ByIfName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByIfName) Less(i, j int) bool { return a[i].Name < a[j].Name }

func (cnpd *sfcCtlrL2CNPDriver) sortBridgedInterfaces(ifs []*l2.BridgeDomains_BridgeDomain_Interfaces) {
	sort.Sort(ByIfName(ifs))
}

func (cnpd *sfcCtlrL2CNPDriver) GetSfcInterfaceIPAndMac(container string, port string) (string, string, error) {
	if sfcIFAddr, exists := cnpd.l2CNPStateCache.SFCIFAddr[container+"/"+port]; exists {
		return stripSlashAndSubnetIpv4Address(sfcIFAddr.ipAddress), sfcIFAddr.macAddress, nil
	}
	return "", "", fmt.Errorf("GetSfcInterfaceAddresses: container/port addresses not found: '%s/%s'",
		container, port)
}

func (cnpd *sfcCtlrL2CNPDriver) setSfcInterfaceIPAndMac(container string, port string, ip string, mac string) {

	sfcIFAddr := sfcInterfaceAddressStateType{
		ipAddress:  ip,
		macAddress: mac,
	}
	cnpd.l2CNPStateCache.SFCIFAddr[container+"/"+port] = sfcIFAddr
}

func stringFirstNLastM(n int, m int, str string) string {
	if len(str) <= n+m {
		return str
	}
	outStr := ""
	for i := 0; i < n; i++ {
		outStr += fmt.Sprintf("%c", str[i])
	}
	for i := 0; i < m; i++ {
		outStr += fmt.Sprintf("%c", str[len(str)-m+i])
	}
	return outStr
}

func constructBaseHostName(container string, port string, v string) string {

	// Use at most 5 chrs from cntr name, and 5 from port, 3 for base 36 unique id plus some under scores
	// If cntr is less than 5 then can use more for port and visa versa.  Also, when cntr and port name
	// is more than 5 chars, use first couple of chars and last 3 chars from name ... brain dead scheme?
	// will it be readable?
	// Example: container: vnf1, port: port1 will be vnf1_port1_1, and container: vnfunc1, port: myport1
	// will be vnnc1_myrt1_2

	cb := 2 // 2 from beginning of container string
	ce := 3 // 3 from end of container string
	pb := 2 // 2 from beginning of port string
	pe := 3 // 3 from end of port string

	if len(container) < 5 {
		// increase char budget for port if container is less than max budget of 5
		switch len(container) {
		case 4:
			pb++
		case 3:
			pb++
			pe++
		case 2:
			pb += 2
			pe++
		case 1:
			pb += 2
			pe += 2
		}
	}

	if len(port) < 5 {
		// increase char budget for container if port is less than max budget of 5
		switch len(port) {
		case 4:
			cb++
		case 3:
			cb++
			ce++
		case 2:
			cb += 2
			ce++
		case 1:
			cb += 2
			ce += 2
		}
	}

	if len(v) < 3 {
		// increase char budget for container if vethid str less than max budget of 3
		switch len(v) {
		case 2:
			cb++
		case 1:
			cb++
			ce++
		}
	}

	return stringFirstNLastM(cb, ce, container) + "_" + stringFirstNLastM(pb, pe, port)
}
