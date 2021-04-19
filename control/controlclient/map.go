// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package controlclient

import (
	"log"
	"sort"

	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/wgkey"
	"tailscale.com/wgengine/filter"
)

// mapSession holds the state over a long-polled "map" request to the
// control plane.
//
// It accepts incremental tailcfg.MapResponse values to
// netMapForResponse and returns fully inflated NetworkMaps, filling
// in the omitted data implicit from prior MapResponse values from
// within the same session (the same long-poll HTTP response to the
// one MapRequest).
type mapSession struct {
	// Immutable fields.
	privateNodeKey         wgkey.Private
	logf                   logger.Logf
	vlogf                  logger.Logf
	machinePubKey          tailcfg.MachineKey
	keepSharerAndUserSplit bool // see Options.KeepSharerAndUserSplit

	// Fields storing state over the the coards of multiple MapResponses.
	lastDNSConfig          *tailcfg.DNSConfig
	lastDERPMap            *tailcfg.DERPMap
	lastUserProfile        map[tailcfg.UserID]tailcfg.UserProfile
	lastParsedPacketFilter []filter.Match
	collectServices        bool
	previousPeers          []*tailcfg.Node // for delta-purposes

	// netMapBuilding is non-nil during a netmapForResponse call,
	// containing the value to be returned, once fully populated.
	netMapBuilding *netmap.NetworkMap
}

func newMapSession(privateNodeKey wgkey.Private) *mapSession {
	ms := &mapSession{
		privateNodeKey:  privateNodeKey,
		logf:            logger.Discard,
		vlogf:           logger.Discard,
		lastDNSConfig:   new(tailcfg.DNSConfig),
		lastUserProfile: map[tailcfg.UserID]tailcfg.UserProfile{},
	}
	return ms
}

func (ms *mapSession) addUserProfile(userID tailcfg.UserID) {
	nm := ms.netMapBuilding
	if _, dup := nm.UserProfiles[userID]; dup {
		// Already populated it from a previous peer.
		return
	}
	if up, ok := ms.lastUserProfile[userID]; ok {
		nm.UserProfiles[userID] = up
	}
}

// netmapForResponse returns a fully populated NetworkMap from a full
// or incremental MapResponse within the session, filling in omitted
// information from prior MapResponse values.
func (ms *mapSession) netmapForResponse(resp *tailcfg.MapResponse) *netmap.NetworkMap {
	undeltaPeers(resp, ms.previousPeers)

	ms.previousPeers = cloneNodes(resp.Peers) // defensive/lazy clone, since this escapes to who knows where
	for _, up := range resp.UserProfiles {
		ms.lastUserProfile[up.ID] = up
	}

	if resp.DERPMap != nil {
		ms.vlogf("netmap: new map contains DERP map")
		ms.lastDERPMap = resp.DERPMap
	}

	if pf := resp.PacketFilter; pf != nil {
		var err error
		ms.lastParsedPacketFilter, err = filter.MatchesFromFilterRules(pf)
		if err != nil {
			ms.logf("parsePacketFilter: %v", err)
		}
	}
	if c := resp.DNSConfig; c != nil {
		ms.lastDNSConfig = c
	}

	if v, ok := resp.CollectServices.Get(); ok {
		ms.collectServices = v
	}

	nm := &netmap.NetworkMap{
		SelfNode:        resp.Node,
		NodeKey:         tailcfg.NodeKey(ms.privateNodeKey.Public()),
		PrivateKey:      ms.privateNodeKey,
		MachineKey:      ms.machinePubKey,
		Expiry:          resp.Node.KeyExpiry,
		Name:            resp.Node.Name,
		Addresses:       resp.Node.Addresses,
		Peers:           resp.Peers,
		User:            resp.Node.User,
		UserProfiles:    make(map[tailcfg.UserID]tailcfg.UserProfile),
		Domain:          resp.Domain,
		DNS:             *ms.lastDNSConfig,
		Hostinfo:        resp.Node.Hostinfo,
		PacketFilter:    ms.lastParsedPacketFilter,
		CollectServices: ms.collectServices,
		DERPMap:         ms.lastDERPMap,
		Debug:           resp.Debug,
	}
	ms.netMapBuilding = nm

	ms.addUserProfile(nm.User)
	magicDNSSuffix := nm.MagicDNSSuffix()
	nm.SelfNode.InitDisplayNames(magicDNSSuffix)
	for _, peer := range resp.Peers {
		peer.InitDisplayNames(magicDNSSuffix)
		if !peer.Sharer.IsZero() {
			if ms.keepSharerAndUserSplit {
				ms.addUserProfile(peer.Sharer)
			} else {
				peer.User = peer.Sharer
			}
		}
		ms.addUserProfile(peer.User)
	}
	if resp.Node.MachineAuthorized {
		nm.MachineStatus = tailcfg.MachineAuthorized
	} else {
		nm.MachineStatus = tailcfg.MachineUnauthorized
	}
	if len(resp.DNS) > 0 {
		nm.DNS.Nameservers = resp.DNS
	}
	if len(resp.SearchPaths) > 0 {
		nm.DNS.Domains = resp.SearchPaths
	}
	if Debug.ProxyDNS {
		nm.DNS.Proxied = true
	}
	ms.netMapBuilding = nil
	return nm
}

// undeltaPeers updates mapRes.Peers to be complete based on the
// provided previous peer list and the PeersRemoved and PeersChanged
// fields in mapRes, as well as the PeerSeenChange and OnlineChange
// maps.
//
// It then also nils out the delta fields.
func undeltaPeers(mapRes *tailcfg.MapResponse, prev []*tailcfg.Node) {
	if len(mapRes.Peers) > 0 {
		// Not delta encoded.
		if !nodesSorted(mapRes.Peers) {
			log.Printf("netmap: undeltaPeers: MapResponse.Peers not sorted; sorting")
			sortNodes(mapRes.Peers)
		}
		return
	}

	var removed map[tailcfg.NodeID]bool
	if pr := mapRes.PeersRemoved; len(pr) > 0 {
		removed = make(map[tailcfg.NodeID]bool, len(pr))
		for _, id := range pr {
			removed[id] = true
		}
	}
	changed := mapRes.PeersChanged

	if !nodesSorted(changed) {
		log.Printf("netmap: undeltaPeers: MapResponse.PeersChanged not sorted; sorting")
		sortNodes(changed)
	}
	if !nodesSorted(prev) {
		// Internal error (unrelated to the network) if we get here.
		log.Printf("netmap: undeltaPeers: [unexpected] prev not sorted; sorting")
		sortNodes(prev)
	}

	newFull := prev
	if len(removed) > 0 || len(changed) > 0 {
		newFull = make([]*tailcfg.Node, 0, len(prev)-len(removed))
		for len(prev) > 0 && len(changed) > 0 {
			pID := prev[0].ID
			cID := changed[0].ID
			if removed[pID] {
				prev = prev[1:]
				continue
			}
			switch {
			case pID < cID:
				newFull = append(newFull, prev[0])
				prev = prev[1:]
			case pID == cID:
				newFull = append(newFull, changed[0])
				prev, changed = prev[1:], changed[1:]
			case cID < pID:
				newFull = append(newFull, changed[0])
				changed = changed[1:]
			}
		}
		newFull = append(newFull, changed...)
		for _, n := range prev {
			if !removed[n.ID] {
				newFull = append(newFull, n)
			}
		}
		sortNodes(newFull)
	}

	if len(mapRes.PeerSeenChange) != 0 || len(mapRes.OnlineChange) != 0 {
		peerByID := make(map[tailcfg.NodeID]*tailcfg.Node, len(newFull))
		for _, n := range newFull {
			peerByID[n.ID] = n
		}
		now := clockNow()
		for nodeID, seen := range mapRes.PeerSeenChange {
			if n, ok := peerByID[nodeID]; ok {
				if seen {
					n.LastSeen = &now
				} else {
					n.LastSeen = nil
				}
			}
		}
		for nodeID, online := range mapRes.OnlineChange {
			if n, ok := peerByID[nodeID]; ok {
				online := online
				n.Online = &online
			}
		}
	}

	mapRes.Peers = newFull
	mapRes.PeersChanged = nil
	mapRes.PeersRemoved = nil
}

func nodesSorted(v []*tailcfg.Node) bool {
	for i, n := range v {
		if i > 0 && n.ID <= v[i-1].ID {
			return false
		}
	}
	return true
}

func sortNodes(v []*tailcfg.Node) {
	sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
}

func cloneNodes(v1 []*tailcfg.Node) []*tailcfg.Node {
	if v1 == nil {
		return nil
	}
	v2 := make([]*tailcfg.Node, len(v1))
	for i, n := range v1 {
		v2[i] = n.Clone()
	}
	return v2
}
