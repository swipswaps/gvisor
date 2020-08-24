// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"math/rand"
	"reflect"

	"gvisor.dev/gvisor/pkg/sleep"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// NetworkInterface is an interface that can be used by a NetworkEndpoint
type NetworkInterface interface {
	// ID returns the NetworkInterface's ID.
	ID() tcpip.NICID

	// IsLoopback returns true if the NetworkInterface is a loopback interface.
	IsLoopback() bool
}

var _ NetworkInterface = (*NIC)(nil)

// NIC represents a "network interface card" to which the networking stack is
// attached.
type NIC struct {
	stack   *Stack
	id      tcpip.NICID
	name    string
	linkEP  LinkEndpoint
	context NICContext

	stats            NICStats
	neigh            *neighborCache
	networkEndpoints map[tcpip.NetworkProtocolNumber]NetworkEndpoint

	mu struct {
		sync.RWMutex
		enabled     bool
		spoofing    bool
		promiscuous bool
		// packetEPs is protected by mu, but the contained PacketEndpoint
		// values are not.
		packetEPs map[tcpip.NetworkProtocolNumber][]PacketEndpoint
		ndp       ndpState
	}
}

// NICStats includes transmitted and received stats.
type NICStats struct {
	Tx DirectionStats
	Rx DirectionStats

	DisabledRx DirectionStats
}

func makeNICStats() NICStats {
	var s NICStats
	tcpip.InitStatCounters(reflect.ValueOf(&s).Elem())
	return s
}

// DirectionStats includes packet and byte counts.
type DirectionStats struct {
	Packets *tcpip.StatCounter
	Bytes   *tcpip.StatCounter
}

// PrimaryEndpointBehavior is an enumeration of an endpoint's primacy behavior.
type PrimaryEndpointBehavior int

const (
	// CanBePrimaryEndpoint indicates the endpoint can be used as a primary
	// endpoint for new connections with no local address. This is the
	// default when calling NIC.AddAddress.
	CanBePrimaryEndpoint PrimaryEndpointBehavior = iota

	// FirstPrimaryEndpoint indicates the endpoint should be the first
	// primary endpoint considered. If there are multiple endpoints with
	// this behavior, the most recently-added one will be first.
	FirstPrimaryEndpoint

	// NeverPrimaryEndpoint indicates the endpoint should never be a
	// primary endpoint.
	NeverPrimaryEndpoint
)

// newNIC returns a new NIC using the default NDP configurations from stack.
func newNIC(stack *Stack, id tcpip.NICID, name string, ep LinkEndpoint, ctx NICContext) *NIC {
	// TODO(b/141011931): Validate a LinkEndpoint (ep) is valid. For
	// example, make sure that the link address it provides is a valid
	// unicast ethernet address.

	// TODO(b/143357959): RFC 8200 section 5 requires that IPv6 endpoints
	// observe an MTU of at least 1280 bytes. Ensure that this requirement
	// of IPv6 is supported on this endpoint's LinkEndpoint.

	nic := &NIC{
		stack:            stack,
		id:               id,
		name:             name,
		linkEP:           ep,
		context:          ctx,
		stats:            makeNICStats(),
		networkEndpoints: make(map[tcpip.NetworkProtocolNumber]NetworkEndpoint),
	}
	nic.mu.packetEPs = make(map[tcpip.NetworkProtocolNumber][]PacketEndpoint)
	nic.mu.ndp = ndpState{
		nic:            nic,
		configs:        stack.ndpConfigs,
		dad:            make(map[tcpip.Address]dadState),
		defaultRouters: make(map[tcpip.Address]defaultRouterState),
		onLinkPrefixes: make(map[tcpip.Subnet]onLinkPrefixState),
		slaacPrefixes:  make(map[tcpip.Subnet]slaacPrefixState),
	}
	nic.mu.ndp.initializeTempAddrState()

	// Check for Neighbor Unreachability Detection support.
	var nud NUDHandler
	if ep.Capabilities()&CapabilityResolutionRequired != 0 && len(stack.linkAddrResolvers) != 0 && stack.useNeighborCache {
		rng := rand.New(rand.NewSource(stack.clock.NowNanoseconds()))
		nic.neigh = &neighborCache{
			nic:   nic,
			state: NewNUDState(stack.nudConfigs, rng),
			cache: make(map[tcpip.Address]*neighborEntry, neighborCacheSize),
		}
		// An interface value that holds a nil concrete value is itself non-nil.
		// For this reason, n.neigh cannot be passed directly to NewEndpoint so
		// NetworkEndpoints don't confuse it for non-nil.
		//
		// See https://golang.org/doc/faq#nil_error for more information.
		nud = nic.neigh
	}

	// Register supported packet endpoint protocols.
	for _, netProto := range header.Ethertypes {
		nic.mu.packetEPs[netProto] = []PacketEndpoint{}
	}
	for _, netProto := range stack.networkProtocols {
		netNum := netProto.Number()
		nic.mu.packetEPs[netNum] = nil
		nic.networkEndpoints[netNum] = netProto.NewEndpoint(nic, stack, nud, nic, ep, stack)
	}

	nic.linkEP.Attach(nic)

	return nic
}

// enabled returns true if n is enabled.
func (n *NIC) enabled() bool {
	n.mu.RLock()
	enabled := n.mu.enabled
	n.mu.RUnlock()
	return enabled
}

// disable disables n.
//
// It undoes the work done by enable.
func (n *NIC) disable() *tcpip.Error {
	n.mu.RLock()
	enabled := n.mu.enabled
	n.mu.RUnlock()
	if !enabled {
		return nil
	}

	n.mu.Lock()
	err := n.disableLocked()
	n.mu.Unlock()
	return err
}

// disableLocked disables n.
//
// It undoes the work done by enable.
//
// n MUST be locked.
func (n *NIC) disableLocked() *tcpip.Error {
	if !n.mu.enabled {
		return nil
	}

	// TODO(gvisor.dev/issue/1491): Should Routes that are currently bound to n be
	// invalidated? Currently, Routes will continue to work when a NIC is enabled
	// again, and applications may not know that the underlying NIC was ever
	// disabled.

	if ep, ok := n.networkEndpoints[header.IPv6ProtocolNumber]; ok {
		n.mu.ndp.stopSolicitingRouters()
		n.mu.ndp.cleanupState(false /* hostOnly */)

		// Stop DAD for all the unicast IPv6 endpoints that are in the
		// permanentTentative state.
		for _, r := range ep.AllEndpoints() {
			if addr := r.AddressWithPrefix().Address; r.GetKind() == PermanentTentative && header.IsV6UnicastAddress(addr) {
				n.mu.ndp.stopDuplicateAddressDetection(addr)
			}
		}
	}

	for _, ep := range n.networkEndpoints {
		if err := ep.Disable(); err != nil {
			return err
		}
	}

	n.mu.enabled = false
	return nil
}

// enable enables n.
//
// If the stack has IPv6 enabled, enable will join the IPv6 All-Nodes Multicast
// address (ff02::1), start DAD for permanent addresses, and start soliciting
// routers if the stack is not operating as a router. If the stack is also
// configured to auto-generate a link-local address, one will be generated.
func (n *NIC) enable() *tcpip.Error {
	n.mu.RLock()
	enabled := n.mu.enabled
	n.mu.RUnlock()
	if enabled {
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.mu.enabled {
		return nil
	}

	n.mu.enabled = true

	for _, ep := range n.networkEndpoints {
		if err := ep.Enable(); err != nil {
			return err
		}
	}

	// Join the IPv6 All-Nodes Multicast group if the stack is configured to
	// use IPv6. This is required to ensure that this node properly receives
	// and responds to the various NDP messages that are destined to the
	// all-nodes multicast address. An example is the Neighbor Advertisement
	// when we perform Duplicate Address Detection, or Router Advertisement
	// when we do Router Discovery. See RFC 4862, section 5.4.2 and RFC 4861
	// section 4.2 for more information.
	//
	// Also auto-generate an IPv6 link-local address based on the NIC's
	// link address if it is configured to do so. Note, each interface is
	// required to have IPv6 link-local unicast address, as per RFC 4291
	// section 2.1.
	ep, ok := n.networkEndpoints[header.IPv6ProtocolNumber]
	if !ok {
		return nil
	}

	// Perform DAD on the all the unicast IPv6 endpoints that are in the permanent
	// state.
	//
	// Addresses may have aleady completed DAD but in the time since the NIC was
	// last enabled, other devices may have acquired the same addresses.
	for _, r := range ep.AllEndpoints() {
		addr := r.AddressWithPrefix().Address
		if k := r.GetKind(); (k != Permanent && k != PermanentTentative) || !header.IsV6UnicastAddress(addr) {
			continue
		}

		ref := n.nepToRef(header.IPv6ProtocolNumber, ep, r)
		ref.setKind(PermanentTentative)
		if err := n.mu.ndp.startDuplicateAddressDetection(addr, ref); err != nil {
			return err
		}
	}

	// Do not auto-generate an IPv6 link-local address for loopback devices.
	if n.stack.autoGenIPv6LinkLocal && !n.IsLoopback() {
		// The valid and preferred lifetime is infinite for the auto-generated
		// link-local address.
		n.mu.ndp.doSLAAC(header.IPv6LinkLocalPrefix.Subnet(), header.NDPInfiniteLifetime, header.NDPInfiniteLifetime)
	}

	// If we are operating as a router, then do not solicit routers since we
	// won't process the RAs anyways.
	//
	// Routers do not process Router Advertisements (RA) the same way a host
	// does. That is, routers do not learn from RAs (e.g. on-link prefixes
	// and default routers). Therefore, soliciting RAs from other routers on
	// a link is unnecessary for routers.
	if !n.stack.forwarding {
		n.mu.ndp.startSolicitingRouters()
	}

	return nil
}

// remove detaches NIC from the link endpoint, and marks existing referenced
// network endpoints expired. This guarantees no packets between this NIC and
// the network stack.
func (n *NIC) remove() *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.disableLocked()

	// TODO(b/151378115): come up with a better way to pick an error than the
	// first one.
	var err *tcpip.Error

	// Forcefully leave multicast groups.
	for _, ep := range n.networkEndpoints {
		gep, ok := ep.(GroupAddressableEndpoint)
		if ok {
			// Let ep.close handle this
			if tempErr := gep.LeaveAllGroups(); tempErr != nil && err == nil {
				err = tempErr
			}
		}

		// Remove permanent and permanentTentative addresses, so no packet goes out.
		// Release any resources the network endpoint may hold.
		// Let ep.Close handle this.
		if tempErr := ep.RemoveAllAddresses(); tempErr != nil && err == nil {
			err = tempErr
		}

		ep.Close()
	}

	// Detach from link endpoint, so no packet comes in.
	n.linkEP.Attach(nil)

	return err
}

// becomeIPv6Router transitions n into an IPv6 router.
//
// When transitioning into an IPv6 router, host-only state (NDP discovered
// routers, discovered on-link prefixes, and auto-generated addresses) will
// be cleaned up/invalidated and NDP router solicitations will be stopped.
func (n *NIC) becomeIPv6Router() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mu.ndp.cleanupState(true /* hostOnly */)
	n.mu.ndp.stopSolicitingRouters()
}

// becomeIPv6Host transitions n into an IPv6 host.
//
// When transitioning into an IPv6 host, NDP router solicitations will be
// started.
func (n *NIC) becomeIPv6Host() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mu.ndp.startSolicitingRouters()
}

// setPromiscuousMode enables or disables promiscuous mode.
func (n *NIC) setPromiscuousMode(enable bool) {
	n.mu.Lock()
	n.mu.promiscuous = enable
	n.mu.Unlock()
}

func (n *NIC) isPromiscuousMode() bool {
	n.mu.RLock()
	rv := n.mu.promiscuous
	n.mu.RUnlock()
	return rv
}

// IsLoopback implements NetworkInterface.
func (n *NIC) IsLoopback() bool {
	return n.linkEP.Capabilities()&CapabilityLoopback != 0
}

// setSpoofing enables or disables address spoofing.
func (n *NIC) setSpoofing(enable bool) {
	n.mu.Lock()
	n.mu.spoofing = enable
	n.mu.Unlock()
}

// primaryEndpoint will return the first non-deprecated endpoint if such an
// endpoint exists for the given protocol and remoteAddr. If no non-deprecated
// endpoint exists, the first deprecated endpoint will be returned.
//
// If an IPv6 primary endpoint is requested, Source Address Selection (as
// defined by RFC 6724 section 5) will be performed.
func (n *NIC) primaryEndpoint(protocol tcpip.NetworkProtocolNumber, remoteAddr tcpip.Address) *referencedNetworkEndpoint {
	n.mu.RLock()
	defer n.mu.RUnlock()

	return n.primaryEndpointRLocked(protocol, remoteAddr)
}

// primaryEndpointRLocked is like primaryEndpoint but without the locking
// requirements.
//
// n.mu MUST be rad locked.
func (n *NIC) primaryEndpointRLocked(protocol tcpip.NetworkProtocolNumber, remoteAddr tcpip.Address) *referencedNetworkEndpoint {
	ep, ok := n.networkEndpoints[protocol]
	if !ok {
		return nil
	}

	nep := ep.PrimaryEndpoint(remoteAddr, n.mu.spoofing)
	if nep == nil {
		return nil
	}

	return n.nepToRef(protocol, ep, nep)
}

// hasPermanentAddrLocked returns true if n has a permanent (including currently
// tentative) address, addr.
func (n *NIC) hasPermanentAddrLocked(addr tcpip.Address) bool {
	for _, ep := range n.networkEndpoints {
		if ep.HasAddress(addr) {
			return true
		}
	}
	return false
}

type getRefBehaviour int

const (
	none getRefBehaviour = iota

	// spoofing indicates that the NIC's spoofing flag should be observed when
	// getting a NIC's referenced network endpoint.
	spoofing

	// promiscuous indicates that the NIC's promiscuous flag should be observed
	// when getting a NIC's referenced network endpoint.
	promiscuous
)

func (n *NIC) nepToRef(p tcpip.NetworkProtocolNumber, ep NetworkEndpoint, nep AddressEndpoint) *referencedNetworkEndpoint {
	ref := &referencedNetworkEndpoint{
		ep:       ep,
		nep:      nep,
		protocol: p,
		nic:      n,
	}

	// Set up cache if link address resolution exists for this protocol.
	if n.linkEP.Capabilities()&CapabilityResolutionRequired != 0 {
		if linkRes, ok := n.stack.linkAddrResolvers[p]; ok {
			ref.linkCache = n.stack
			ref.linkRes = linkRes
		}
	}

	return ref
}

func (n *NIC) getRef(protocol tcpip.NetworkProtocolNumber, dst tcpip.Address) *referencedNetworkEndpoint {
	return n.getRefOrCreateTemp(protocol, dst, CanBePrimaryEndpoint, promiscuous)
}

// findEndpoint finds the endpoint, if any, with the given address.
func (n *NIC) findEndpoint(protocol tcpip.NetworkProtocolNumber, address tcpip.Address, peb PrimaryEndpointBehavior) *referencedNetworkEndpoint {
	return n.getRefOrCreateTemp(protocol, address, peb, spoofing)
}

// getRefEpOrCreateTemp returns the referenced network endpoint for the given
// protocol and address.
//
// If none exists a temporary one may be created if we are in promiscuous mode
// or spoofing. Promiscuous mode will only be checked if promiscuous is true.
// Similarly, spoofing will only be checked if spoofing is true.
//
// If the address is the IPv4 broadcast address for an endpoint's network, that
// endpoint will be returned.
func (n *NIC) getRefOrCreateTemp(protocol tcpip.NetworkProtocolNumber, address tcpip.Address, peb PrimaryEndpointBehavior, tempRef getRefBehaviour) *referencedNetworkEndpoint {
	n.mu.Lock()
	var spoofingOrPromiscuous bool
	switch tempRef {
	case spoofing:
		spoofingOrPromiscuous = n.mu.spoofing
	case promiscuous:
		spoofingOrPromiscuous = n.mu.promiscuous
	}

	ref := n.getRefOrCreateTempLocked(protocol, address, spoofingOrPromiscuous, peb)
	n.mu.Unlock()
	return ref
}

/// getRefOrCreateTempLocked returns an existing endpoint for address or creates
/// and returns a temporary endpoint.
//
// If the address is the IPv4 broadcast address for an endpoint's network, that
// endpoint will be returned.
//
// n.mu must be write locked.
func (n *NIC) getRefOrCreateTempLocked(protocol tcpip.NetworkProtocolNumber, address tcpip.Address, createTemp bool, peb PrimaryEndpointBehavior) *referencedNetworkEndpoint {
	if protocol == 0 {
		for p, ep := range n.networkEndpoints {
			if nep := ep.GetAssignedEndpoint(address, n.IsLoopback(), createTemp, peb); nep != nil {
				return n.nepToRef(p, ep, nep)
			}
		}
	} else {
		ep, ok := n.networkEndpoints[protocol]
		if ok {
			if nep := ep.GetAssignedEndpoint(address, n.IsLoopback(), createTemp, peb); nep != nil {
				return n.nepToRef(protocol, ep, nep)
			}
		}
	}

	return nil
}

// addAddressLocked adds a new protocolAddress to n.
//
// If n already has the address in a non-permanent state, and the kind given is
// permanent, that address will be promoted in place and its properties set to
// the properties provided. Otherwise, it returns tcpip.ErrDuplicateAddress.
func (n *NIC) addAddressLocked(protocolAddress tcpip.ProtocolAddress, peb PrimaryEndpointBehavior, kind AddressKind, configType AddressConfigType, deprecated bool) (*referencedNetworkEndpoint, *tcpip.Error) {
	ep, ok := n.networkEndpoints[protocolAddress.Protocol]
	if !ok {
		return nil, tcpip.ErrUnknownProtocol
	}

	// If the address is an IPv6 address and it is a permanent address,
	// mark it as tentative so it goes through the DAD process if the NIC is
	// enabled. If the NIC is not enabled, DAD will be started when the NIC is
	// enabled.

	nep, err := ep.AddAddress(protocolAddress.AddressWithPrefix, AddAddressOptions{
		Deprecated: deprecated,
		ConfigType: AddressConfigType(configType),
		Kind:       AddressKind(kind),
		PEB:        peb,
	})
	if err != nil {
		return nil, err
	}
	ref := n.nepToRef(protocolAddress.Protocol, ep, nep)

	isIPv6Unicast := protocolAddress.Protocol == header.IPv6ProtocolNumber && header.IsV6UnicastAddress(protocolAddress.AddressWithPrefix.Address)
	if isIPv6Unicast && kind == Permanent {
		kind = PermanentTentative
		ref.setKind(kind)
	}

	// If we are adding a tentative IPv6 address, start DAD if the NIC is enabled.
	if isIPv6Unicast && kind == PermanentTentative && n.mu.enabled {
		if err := n.mu.ndp.startDuplicateAddressDetection(protocolAddress.AddressWithPrefix.Address, ref); err != nil {
			return nil, err
		}
	}

	return ref, nil
}

// AddAddress adds a new address to n, so that it starts accepting packets
// targeted at the given address (and network protocol).
func (n *NIC) AddAddress(protocolAddress tcpip.ProtocolAddress, peb PrimaryEndpointBehavior) *tcpip.Error {
	// Add the endpoint.
	n.mu.Lock()
	_, err := n.addAddressLocked(protocolAddress, peb, Permanent, AddressConfigStatic, false /* deprecated */)
	n.mu.Unlock()

	return err
}

// AllAddresses returns all addresses (primary and non-primary) associated with
// this NIC.
func (n *NIC) AllAddresses() []tcpip.ProtocolAddress {
	n.mu.RLock()
	defer n.mu.RUnlock()

	var addrs []tcpip.ProtocolAddress
	for p, ep := range n.networkEndpoints {
		for _, a := range ep.AllAddresses() {
			addrs = append(addrs, tcpip.ProtocolAddress{Protocol: p, AddressWithPrefix: a})
		}
	}
	return addrs
}

// PrimaryAddresses returns the primary addresses associated with this NIC.
func (n *NIC) PrimaryAddresses() []tcpip.ProtocolAddress {
	n.mu.RLock()
	defer n.mu.RUnlock()

	var addrs []tcpip.ProtocolAddress
	for p, ep := range n.networkEndpoints {
		for _, a := range ep.PrimaryAddresses() {
			addrs = append(addrs, tcpip.ProtocolAddress{Protocol: p, AddressWithPrefix: a})
		}
	}
	return addrs
}

// primaryAddress returns the primary address associated with this NIC.
//
// primaryAddress will return the first non-deprecated address if such an
// address exists. If no non-deprecated address exists, the first deprecated
// address will be returned.
func (n *NIC) primaryAddress(proto tcpip.NetworkProtocolNumber) tcpip.AddressWithPrefix {
	ref := n.primaryEndpoint(proto, "")
	if ref == nil {
		return tcpip.AddressWithPrefix{}
	}
	addr := ref.addrWithPrefix()
	ref.decRef()
	return addr
}

func (n *NIC) removePermanentAddressLocked(addr tcpip.Address) *tcpip.Error {
	for p, ep := range n.networkEndpoints {
		nep := ep.GetEndpoint(addr)
		if nep == nil {
			continue
		}

		addrWithPrefix := nep.AddressWithPrefix()
		if addrWithPrefix.Address != addr {
			continue
		}

		kind := nep.GetKind()
		if kind != Permanent && kind != PermanentTentative {
			return tcpip.ErrBadLocalAddress
		}

		ref := n.nepToRef(p, ep, nep)
		switch p {
		case header.IPv6ProtocolNumber:
			return n.removePermanentIPv6EndpointLocked(ref, true /* allowSLAACInvalidation */)
		default:
			ref.expireLocked()
			return nil
		}
	}

	return tcpip.ErrBadLocalAddress
}

func (n *NIC) removePermanentIPv6EndpointLocked(r *referencedNetworkEndpoint, allowSLAACInvalidation bool) *tcpip.Error {
	addr := r.addrWithPrefix()
	isIPv6Unicast := header.IsV6UnicastAddress(addr.Address)

	if isIPv6Unicast {
		n.mu.ndp.stopDuplicateAddressDetection(addr.Address)

		// If we are removing an address generated via SLAAC, cleanup
		// its SLAAC resources and notify the integrator.
		switch r.configType() {
		case AddressConfigSlaac:
			n.mu.ndp.cleanupSLAACAddrResourcesAndNotify(addr, allowSLAACInvalidation)
		case AddressConfigSlaacTemp:
			n.mu.ndp.cleanupTempSLAACAddrResourcesAndNotify(addr, allowSLAACInvalidation)
		}
	}

	r.expireLocked()

	// At this point the endpoint is deleted.

	return nil
}

// RemoveAddress removes an address from n.
func (n *NIC) RemoveAddress(addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.removePermanentAddressLocked(addr)
}

func (n *NIC) neighbors() ([]NeighborEntry, *tcpip.Error) {
	if n.neigh == nil {
		return nil, tcpip.ErrNotSupported
	}

	return n.neigh.entries(), nil
}

func (n *NIC) removeWaker(addr tcpip.Address, w *sleep.Waker) {
	if n.neigh == nil {
		return
	}

	n.neigh.removeWaker(addr, w)
}

func (n *NIC) addStaticNeighbor(addr tcpip.Address, linkAddress tcpip.LinkAddress) *tcpip.Error {
	if n.neigh == nil {
		return tcpip.ErrNotSupported
	}

	n.neigh.addStaticEntry(addr, linkAddress)
	return nil
}

func (n *NIC) removeNeighbor(addr tcpip.Address) *tcpip.Error {
	if n.neigh == nil {
		return tcpip.ErrNotSupported
	}

	if !n.neigh.removeEntry(addr) {
		return tcpip.ErrBadAddress
	}
	return nil
}

func (n *NIC) clearNeighbors() *tcpip.Error {
	if n.neigh == nil {
		return tcpip.ErrNotSupported
	}

	n.neigh.clear()
	return nil
}

// joinGroup adds a new endpoint for the given multicast address, if none
// exists yet. Otherwise it just increments its count.
func (n *NIC) joinGroup(protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.joinGroupLocked(protocol, addr)
}

// joinGroupLocked adds a new endpoint for the given multicast address, if none
// exists yet. Otherwise it just increments its count. n MUST be locked before
// joinGroupLocked is called.
func (n *NIC) joinGroupLocked(protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) *tcpip.Error {
	// TODO(b/143102137): When implementing MLD, make sure MLD packets are
	// not sent unless a valid link-local address is available for use on n
	// as an MLD packet's source address must be a link-local address as
	// outlined in RFC 3810 section 5.

	ep, ok := n.networkEndpoints[protocol]
	if !ok {
		return tcpip.ErrNotSupported
	}

	gep, ok := ep.(GroupAddressableEndpoint)
	if !ok {
		return tcpip.ErrNotSupported
	}

	_, err := gep.JoinGroup(addr)
	return err
}

// leaveGroup decrements the count for the given multicast address, and when it
// reaches zero removes the endpoint for this address.
func (n *NIC) leaveGroup(protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.leaveGroupLocked(protocol, addr, false /* force */)
}

// leaveGroupLocked decrements the count for the given multicast address, and
// when it reaches zero removes the endpoint for this address. n MUST be locked
// before leaveGroupLocked is called.
//
// If force is true, then the count for the multicast addres is ignored and the
// endpoint will be removed immediately.
func (n *NIC) leaveGroupLocked(protocol tcpip.NetworkProtocolNumber, addr tcpip.Address, force bool) *tcpip.Error {
	ep, ok := n.networkEndpoints[protocol]
	if !ok {
		return tcpip.ErrNotSupported
	}

	gep, ok := ep.(GroupAddressableEndpoint)
	if !ok {
		return tcpip.ErrNotSupported
	}

	if _, err := gep.LeaveGroup(addr, force); err != nil {
		return err
	}

	return nil
}

// isInGroup returns true if n has joined the multicast group addr.
func (n *NIC) isInGroup(addr tcpip.Address) bool {
	for _, ep := range n.networkEndpoints {
		gep, ok := ep.(GroupAddressableEndpoint)
		if !ok {
			continue
		}

		if gep.IsInGroup(addr) {
			return true
		}
	}

	return false
}

func handlePacket(protocol tcpip.NetworkProtocolNumber, dst, src tcpip.Address, localLinkAddr, remotelinkAddr tcpip.LinkAddress, ref *referencedNetworkEndpoint, pkt *PacketBuffer) {
	r := makeRoute(protocol, dst, src, localLinkAddr, ref, false /* handleLocal */, false /* multicastLoop */)
	r.RemoteLinkAddress = remotelinkAddr

	ref.ep.HandlePacket(&r, pkt)
	ref.decRef()
}

// DeliverNetworkPacket finds the appropriate network protocol endpoint and
// hands the packet over for further processing. This function is called when
// the NIC receives a packet from the link endpoint.
// Note that the ownership of the slice backing vv is retained by the caller.
// This rule applies only to the slice itself, not to the items of the slice;
// the ownership of the items is not retained by the caller.
func (n *NIC) DeliverNetworkPacket(remote, local tcpip.LinkAddress, protocol tcpip.NetworkProtocolNumber, pkt *PacketBuffer) {
	n.mu.RLock()
	enabled := n.mu.enabled
	// If the NIC is not yet enabled, don't receive any packets.
	if !enabled {
		n.mu.RUnlock()

		n.stats.DisabledRx.Packets.Increment()
		n.stats.DisabledRx.Bytes.IncrementBy(uint64(pkt.Data.Size()))
		return
	}

	n.stats.Rx.Packets.Increment()
	n.stats.Rx.Bytes.IncrementBy(uint64(pkt.Data.Size()))

	netProto, ok := n.stack.networkProtocols[protocol]
	if !ok {
		n.mu.RUnlock()
		n.stack.stats.UnknownProtocolRcvdPackets.Increment()
		return
	}

	// If no local link layer address is provided, assume it was sent
	// directly to this NIC.
	if local == "" {
		local = n.linkEP.LinkAddress()
	}

	// Are any packet sockets listening for this network protocol?
	packetEPs := n.mu.packetEPs[protocol]
	// Add any other packet sockets that maybe listening for all protocols.
	packetEPs = append(packetEPs, n.mu.packetEPs[header.EthernetProtocolAll]...)
	n.mu.RUnlock()
	for _, ep := range packetEPs {
		p := pkt.Clone()
		p.PktType = tcpip.PacketHost
		ep.HandlePacket(n.id, local, protocol, p)
	}

	if netProto.Number() == header.IPv4ProtocolNumber || netProto.Number() == header.IPv6ProtocolNumber {
		n.stack.stats.IP.PacketsReceived.Increment()
	}

	// Parse headers.
	transProtoNum, hasTransportHdr, ok := netProto.Parse(pkt)
	if !ok {
		// The packet is too small to contain a network header.
		n.stack.stats.MalformedRcvdPackets.Increment()
		return
	}
	if hasTransportHdr {
		// Parse the transport header if present.
		if state, ok := n.stack.transportProtocols[transProtoNum]; ok {
			state.proto.Parse(pkt)
		}
	}

	src, dst := netProto.ParseAddresses(pkt.NetworkHeader().View())

	if n.stack.handleLocal && !n.IsLoopback() && n.getRef(protocol, src) != nil {
		// The source address is one of our own, so we never should have gotten a
		// packet like this unless handleLocal is false. Loopback also calls this
		// function even though the packets didn't come from the physical interface
		// so don't drop those.
		n.stack.stats.IP.InvalidSourceAddressesReceived.Increment()
		return
	}

	// TODO(gvisor.dev/issue/170): Not supporting iptables for IPv6 yet.
	// Loopback traffic skips the prerouting chain.
	if protocol == header.IPv4ProtocolNumber && !n.IsLoopback() {
		// iptables filtering.
		ipt := n.stack.IPTables()
		address := n.primaryAddress(protocol)
		if ok := ipt.Check(Prerouting, pkt, nil, nil, address.Address, ""); !ok {
			// iptables is telling us to drop the packet.
			return
		}
	}

	if ref := n.getRef(protocol, dst); ref != nil {
		handlePacket(protocol, dst, src, n.linkEP.LinkAddress(), remote, ref, pkt)
		return
	}

	// This NIC doesn't care about the packet. Find a NIC that cares about the
	// packet and forward it to the NIC.
	//
	// TODO: Should we be forwarding the packet even if promiscuous?
	if n.stack.Forwarding() {
		r, err := n.stack.FindRoute(0, "", dst, protocol, false /* multicastLoop */)
		if err != nil {
			n.stack.stats.IP.InvalidDestinationAddressesReceived.Increment()
			return
		}

		// Found a NIC.
		n := r.ref.nic
		n.mu.RLock()
		ref := n.getRefOrCreateTempLocked(protocol, dst, false, NeverPrimaryEndpoint)
		ok := ref != nil && ref.isValidForOutgoingRLocked()
		n.mu.RUnlock()
		if ok {
			r.LocalLinkAddress = n.linkEP.LinkAddress()
			r.RemoteLinkAddress = remote
			r.RemoteAddress = src
			// TODO(b/123449044): Update the source NIC as well.
			ref.ep.HandlePacket(&r, pkt)
			ref.decRef()
			r.Release()
			return
		}

		// n doesn't have a destination endpoint.
		// Send the packet out of n.
		// TODO(b/128629022): move this logic to route.WritePacket.
		if ch, err := r.Resolve(nil); err != nil {
			if err == tcpip.ErrWouldBlock {
				n.stack.forwarder.enqueue(ch, n, &r, protocol, pkt)
				// forwarder will release route.
				return
			}
			n.stack.stats.IP.InvalidDestinationAddressesReceived.Increment()
			r.Release()
			return
		}

		// The link-address resolution finished immediately.
		n.forwardPacket(&r, protocol, pkt)
		r.Release()
		return
	}

	// If a packet socket handled the packet, don't treat it as invalid.
	if len(packetEPs) == 0 {
		n.stack.stats.IP.InvalidDestinationAddressesReceived.Increment()
	}
}

// DeliverOutboundPacket implements NetworkDispatcher.DeliverOutboundPacket.
func (n *NIC) DeliverOutboundPacket(remote, local tcpip.LinkAddress, protocol tcpip.NetworkProtocolNumber, pkt *PacketBuffer) {
	n.mu.RLock()
	// We do not deliver to protocol specific packet endpoints as on Linux
	// only ETH_P_ALL endpoints get outbound packets.
	// Add any other packet sockets that maybe listening for all protocols.
	packetEPs := n.mu.packetEPs[header.EthernetProtocolAll]
	n.mu.RUnlock()
	for _, ep := range packetEPs {
		p := pkt.Clone()
		p.PktType = tcpip.PacketOutgoing
		// Add the link layer header as outgoing packets are intercepted
		// before the link layer header is created.
		n.linkEP.AddHeader(local, remote, protocol, p)
		ep.HandlePacket(n.id, local, protocol, p)
	}
}

func (n *NIC) forwardPacket(r *Route, protocol tcpip.NetworkProtocolNumber, pkt *PacketBuffer) {
	// TODO(b/143425874) Decrease the TTL field in forwarded packets.

	// pkt may have set its header and may not have enough headroom for link-layer
	// header for the other link to prepend. Here we create a new packet to
	// forward.
	fwdPkt := NewPacketBuffer(PacketBufferOptions{
		ReserveHeaderBytes: int(n.linkEP.MaxHeaderLength()),
		Data:               buffer.NewVectorisedView(pkt.Size(), pkt.Views()),
	})

	// WritePacket takes ownership of fwdPkt, calculate numBytes first.
	numBytes := fwdPkt.Size()

	if err := n.linkEP.WritePacket(r, nil /* gso */, protocol, fwdPkt); err != nil {
		r.Stats().IP.OutgoingPacketErrors.Increment()
		return
	}

	n.stats.Tx.Packets.Increment()
	n.stats.Tx.Bytes.IncrementBy(uint64(numBytes))
}

// DeliverTransportPacket delivers the packets to the appropriate transport
// protocol endpoint.
func (n *NIC) DeliverTransportPacket(r *Route, protocol tcpip.TransportProtocolNumber, pkt *PacketBuffer) {
	state, ok := n.stack.transportProtocols[protocol]
	if !ok {
		n.stack.stats.UnknownProtocolRcvdPackets.Increment()
		return
	}

	transProto := state.proto

	// Raw socket packets are delivered based solely on the transport
	// protocol number. We do not inspect the payload to ensure it's
	// validly formed.
	n.stack.demux.deliverRawPacket(r, protocol, pkt)

	// TransportHeader is empty only when pkt is an ICMP packet or was reassembled
	// from fragments.
	if pkt.TransportHeader().View().IsEmpty() {
		// TODO(gvisor.dev/issue/170): ICMP packets don't have their TransportHeader
		// fields set yet, parse it here. See icmp/protocol.go:protocol.Parse for a
		// full explanation.
		if protocol == header.ICMPv4ProtocolNumber || protocol == header.ICMPv6ProtocolNumber {
			// ICMP packets may be longer, but until icmp.Parse is implemented, here
			// we parse it using the minimum size.
			if _, ok := pkt.TransportHeader().Consume(transProto.MinimumPacketSize()); !ok {
				n.stack.stats.MalformedRcvdPackets.Increment()
				return
			}
		} else {
			// This is either a bad packet or was re-assembled from fragments.
			transProto.Parse(pkt)
		}
	}

	if pkt.TransportHeader().View().Size() < transProto.MinimumPacketSize() {
		n.stack.stats.MalformedRcvdPackets.Increment()
		return
	}

	srcPort, dstPort, err := transProto.ParsePorts(pkt.TransportHeader().View())
	if err != nil {
		n.stack.stats.MalformedRcvdPackets.Increment()
		return
	}

	id := TransportEndpointID{dstPort, r.LocalAddress, srcPort, r.RemoteAddress}
	if n.stack.demux.deliverPacket(r, protocol, pkt, id) {
		return
	}

	// Try to deliver to per-stack default handler.
	if state.defaultHandler != nil {
		if state.defaultHandler(r, id, pkt) {
			return
		}
	}

	// We could not find an appropriate destination for this packet, so
	// deliver it to the global handler.
	if !transProto.HandleUnknownDestinationPacket(r, id, pkt) {
		n.stack.stats.MalformedRcvdPackets.Increment()
	}
}

// DeliverTransportControlPacket delivers control packets to the appropriate
// transport protocol endpoint.
func (n *NIC) DeliverTransportControlPacket(local, remote tcpip.Address, net tcpip.NetworkProtocolNumber, trans tcpip.TransportProtocolNumber, typ ControlType, extra uint32, pkt *PacketBuffer) {
	state, ok := n.stack.transportProtocols[trans]
	if !ok {
		return
	}

	transProto := state.proto

	// ICMPv4 only guarantees that 8 bytes of the transport protocol will
	// be present in the payload. We know that the ports are within the
	// first 8 bytes for all known transport protocols.
	transHeader, ok := pkt.Data.PullUp(8)
	if !ok {
		return
	}

	srcPort, dstPort, err := transProto.ParsePorts(transHeader)
	if err != nil {
		return
	}

	id := TransportEndpointID{srcPort, local, dstPort, remote}
	if n.stack.demux.deliverControlPacket(n, net, trans, typ, extra, pkt, id) {
		return
	}
}

// ID implements NetworkInterface.
func (n *NIC) ID() tcpip.NICID {
	return n.id
}

// Name returns the name of n.
func (n *NIC) Name() string {
	return n.name
}

// Stack returns the instance of the Stack that owns this NIC.
func (n *NIC) Stack() *Stack {
	return n.stack
}

// LinkEndpoint returns the link endpoint of n.
func (n *NIC) LinkEndpoint() LinkEndpoint {
	return n.linkEP
}

// isAddrTentative returns true if addr is tentative on n.
//
// Note that if addr is not associated with n, then this function will return
// false. It will only return true if the address is associated with the NIC
// AND it is tentative.
func (n *NIC) isAddrTentative(addr tcpip.Address) bool {
	nep := n.networkEndpoints[header.IPv6ProtocolNumber].GetEndpoint(addr)
	if nep == nil {
		return false
	}
	kind := nep.GetKind()
	return kind == PermanentTentative
}

// dupTentativeAddrDetected attempts to inform n that a tentative addr is a
// duplicate on a link.
//
// dupTentativeAddrDetected will remove the tentative address if it exists. If
// the address was generated via SLAAC, an attempt will be made to generate a
// new address.
func (n *NIC) dupTentativeAddrDetected(addr tcpip.Address) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	ep := n.networkEndpoints[header.IPv6ProtocolNumber]
	nep := ep.GetEndpoint(addr)
	if nep == nil {
		return tcpip.ErrBadAddress
	}

	ref := n.nepToRef(header.IPv6ProtocolNumber, ep, nep)
	if ref.getKind() != PermanentTentative {
		return tcpip.ErrInvalidEndpointState
	}

	// If the address is a SLAAC address, do not invalidate its SLAAC prefix as a
	// new address will be generated for it.
	if err := n.removePermanentIPv6EndpointLocked(ref, false /* allowSLAACInvalidation */); err != nil {
		return err
	}

	prefix := ref.addrWithPrefix().Subnet()

	switch ref.configType() {
	case AddressConfigSlaac:
		n.mu.ndp.regenerateSLAACAddr(prefix)
	case AddressConfigSlaacTemp:
		// Do not reset the generation attempts counter for the prefix as the
		// temporary address is being regenerated in response to a DAD conflict.
		n.mu.ndp.regenerateTempSLAACAddr(prefix, false /* resetGenAttempts */)
	}

	return nil
}

// setNDPConfigs sets the NDP configurations for n.
//
// Note, if c contains invalid NDP configuration values, it will be fixed to
// use default values for the erroneous values.
func (n *NIC) setNDPConfigs(c NDPConfigurations) {
	c.validate()

	n.mu.Lock()
	n.mu.ndp.configs = c
	n.mu.Unlock()
}

// NUDConfigs gets the NUD configurations for n.
func (n *NIC) NUDConfigs() (NUDConfigurations, *tcpip.Error) {
	if n.neigh == nil {
		return NUDConfigurations{}, tcpip.ErrNotSupported
	}
	return n.neigh.config(), nil
}

// setNUDConfigs sets the NUD configurations for n.
//
// Note, if c contains invalid NUD configuration values, it will be fixed to
// use default values for the erroneous values.
func (n *NIC) setNUDConfigs(c NUDConfigurations) *tcpip.Error {
	if n.neigh == nil {
		return tcpip.ErrNotSupported
	}
	c.resetInvalidFields()
	n.neigh.setConfig(c)
	return nil
}

// handleNDPRA handles an NDP Router Advertisement message that arrived on n.
func (n *NIC) handleNDPRA(ip tcpip.Address, ra header.NDPRouterAdvert) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.mu.ndp.handleRA(ip, ra)
}

func (n *NIC) registerPacketEndpoint(netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) *tcpip.Error {
	n.mu.Lock()
	defer n.mu.Unlock()

	eps, ok := n.mu.packetEPs[netProto]
	if !ok {
		return tcpip.ErrNotSupported
	}
	n.mu.packetEPs[netProto] = append(eps, ep)

	return nil
}

func (n *NIC) unregisterPacketEndpoint(netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) {
	n.mu.Lock()
	defer n.mu.Unlock()

	eps, ok := n.mu.packetEPs[netProto]
	if !ok {
		return
	}

	for i, epOther := range eps {
		if epOther == ep {
			n.mu.packetEPs[netProto] = append(eps[:i], eps[i+1:]...)
			return
		}
	}
}

type referencedNetworkEndpoint struct {
	ep       NetworkEndpoint
	nep      AddressEndpoint
	nic      *NIC
	protocol tcpip.NetworkProtocolNumber

	// linkCache is set if link address resolution is enabled for this
	// protocol. Set to nil otherwise.
	linkCache LinkAddressCache

	// linkRes is set if link address resolution is enabled for this protocol.
	// Set to nil otherwise.
	linkRes LinkAddressResolver
}

func (r *referencedNetworkEndpoint) address() tcpip.Address {
	return r.nep.AddressWithPrefix().Address
}

func (r *referencedNetworkEndpoint) addrWithPrefix() tcpip.AddressWithPrefix {
	return r.nep.AddressWithPrefix()
}

func (r *referencedNetworkEndpoint) getKind() AddressKind {
	return r.nep.GetKind()
}

func (r *referencedNetworkEndpoint) setKind(kind AddressKind) {
	r.nep.SetKind(kind)
}

// isValidForOutgoing returns true if the endpoint can be used to send out a
// packet. It requires the endpoint to not be marked expired (i.e., its address)
// has been removed) unless the NIC is in spoofing mode, or temporary.
func (r *referencedNetworkEndpoint) isValidForOutgoing() bool {
	r.nic.mu.RLock()
	defer r.nic.mu.RUnlock()

	return r.isValidForOutgoingRLocked()
}

// isValidForOutgoingRLocked is the same as isValidForOutgoing but requires
// r.nic.mu to be read locked.
func (r *referencedNetworkEndpoint) isValidForOutgoingRLocked() bool {
	if !r.nic.mu.enabled {
		return false
	}

	return r.isAssignedRLocked(r.nic.mu.spoofing)
}

// isAssignedRLocked returns true if r is considered to be assigned to the NIC.
//
// r.nic.mu must be read locked.
func (r *referencedNetworkEndpoint) isAssignedRLocked(spoofingOrPromiscuous bool) bool {
	return r.nep.IsAssigned(spoofingOrPromiscuous)
}

// expireLocked decrements the reference count and marks the permanent endpoint
// as expired.
func (r *referencedNetworkEndpoint) expireLocked() {
	_ = r.ep.RemoveAddress(r.address())
}

// decRef decrements the ref count and cleans up the endpoint once it reaches
// zero.
func (r *referencedNetworkEndpoint) decRef() {
	_ = r.nep.DecRef()
}

// decRefLocked is the same as decRef but assumes that the NIC.mu mutex is
// locked.
func (r *referencedNetworkEndpoint) decRefLocked() {
	_ = r.nep.DecRef()
}

// incRef increments the ref count. It must only be called when the caller is
// known to be holding a reference to the endpoint, otherwise tryIncRef should
// be used.
func (r *referencedNetworkEndpoint) incRef() {
	_ = r.tryIncRef()
}

// tryIncRef attempts to increment the ref count from n to n+1, but only if n is
// not zero. That is, it will increment the count if the endpoint is still
// alive, and do nothing if it has already been clean up.
func (r *referencedNetworkEndpoint) tryIncRef() bool {
	return r.nep.IncRef()
}

func (r *referencedNetworkEndpoint) setDeprecated(d bool) {
	r.nep.SetDeprecated(d)
}

func (r *referencedNetworkEndpoint) deprecated() bool {
	return r.nep.Deprecated()
}

func (r *referencedNetworkEndpoint) configType() AddressConfigType {
	return r.nep.ConfigType()
}

// stack returns the Stack instance that owns the underlying endpoint.
func (r *referencedNetworkEndpoint) stack() *Stack {
	return r.nic.stack
}
