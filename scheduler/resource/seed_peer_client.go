/*
 *     Copyright 2022 The Dragonfly Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

//go:generate mockgen -destination seed_peer_client_mock.go -source seed_peer_client.go -package resource

package resource

import (
	"fmt"
	reflect "reflect"

	"google.golang.org/grpc"

	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/manager/model"
	"d7y.io/dragonfly/v2/pkg/dfnet"
	"d7y.io/dragonfly/v2/pkg/idgen"
	client "d7y.io/dragonfly/v2/pkg/rpc/cdnsystem/client"
	rpcscheduler "d7y.io/dragonfly/v2/pkg/rpc/scheduler"
	"d7y.io/dragonfly/v2/scheduler/config"
)

type SeedPeerClient interface {
	// client is seed peer grpc client interface.
	client.CdnClient

	// Observer is dynconfig observer interface.
	config.Observer
}

type seedPeerClient struct {
	// hostManager is host manager.
	hostManager HostManager

	// client is sedd peer grpc client instance.
	client.CdnClient

	// data is dynconfig data.
	data *config.DynconfigData
}

// New seed peer client interface.
func newSeedPeerClient(dynconfig config.DynconfigInterface, hostManager HostManager, opts ...grpc.DialOption) (SeedPeerClient, error) {
	config, err := dynconfig.Get()
	if err != nil {
		return nil, err
	}

	// Initialize seed peer grpc client.
	netAddrs := append(seedPeersToNetAddrs(config.SeedPeers), cdnsToNetAddrs(config.CDNs)...)
	client, err := client.GetClientByAddr(netAddrs, opts...)
	if err != nil {
		return nil, err
	}

	// Initialize seed hosts.
	for _, host := range seedPeersToHosts(config.SeedPeers) {
		hostManager.Store(host)
	}

	// Initialize cdn hosts.
	for _, host := range cdnsToHosts(config.CDNs) {
		hostManager.Store(host)
	}

	dc := &seedPeerClient{
		hostManager: hostManager,
		CdnClient:   client,
		data:        config,
	}

	dynconfig.Register(dc)
	return dc, nil
}

// Dynamic config notify function.
func (sc *seedPeerClient) OnNotify(data *config.DynconfigData) {
	var seedPeers []config.SeedPeer
	for _, seedPeer := range data.SeedPeers {
		seedPeers = append(seedPeers, *seedPeer)
	}

	var cdns []config.CDN
	for _, cdn := range data.CDNs {
		cdns = append(cdns, *cdn)
	}

	if reflect.DeepEqual(sc.data, data) {
		logger.Infof("addresses deep equal: %#v %#v", seedPeers, cdns)
		return
	}

	// If only the ip of the seed peer is changed,
	// the seed peer needs to be cleared.
	diffSeedPeers := diffSeedPeers(sc.data.SeedPeers, data.SeedPeers)
	for _, seedPeer := range diffSeedPeers {
		id := idgen.SeedHostID(seedPeer.Hostname, seedPeer.Port)
		if host, ok := sc.hostManager.Load(id); ok {
			host.LeavePeers()
			sc.hostManager.Delete(id)
		}
	}

	// Update seed host in host manager.
	for _, host := range seedPeersToHosts(data.SeedPeers) {
		sc.hostManager.Store(host)
	}

	// If only the ip of the cdn host is changed,
	// the cdn peer needs to be cleared.
	diffCDNs := diffCDNs(sc.data.CDNs, data.CDNs)
	for _, cdn := range diffCDNs {
		id := idgen.CDNHostID(cdn.Hostname, cdn.Port)
		if host, ok := sc.hostManager.Load(id); ok {
			host.LeavePeers()
			sc.hostManager.Delete(id)
		}
	}

	// Update cdn in host manager.
	for _, host := range cdnsToHosts(data.CDNs) {
		sc.hostManager.Store(host)
	}

	// Update dynamic data.
	sc.data = data

	// Update grpc seed peer addresses.
	netAddrs := append(seedPeersToNetAddrs(data.SeedPeers), cdnsToNetAddrs(data.CDNs)...)
	sc.UpdateState(netAddrs)
	logger.Infof("addresses have been updated: %#v %#v", seedPeers, cdns)
}

// seedPeersToHosts coverts []*config.SeedPeer to map[string]*Host.
func seedPeersToHosts(seedPeers []*config.SeedPeer) map[string]*Host {
	hosts := map[string]*Host{}
	for _, seedPeer := range seedPeers {
		options := []HostOption{WithHostType(seedPeerTypeToHostType(seedPeer.Type))}
		if config, ok := seedPeer.GetSeedPeerClusterConfig(); ok && config.LoadLimit > 0 {
			options = append(options, WithUploadLoadLimit(int32(config.LoadLimit)))
		}

		id := idgen.SeedHostID(seedPeer.Hostname, seedPeer.Port)
		hosts[id] = NewHost(&rpcscheduler.PeerHost{
			Uuid:        id,
			Ip:          seedPeer.IP,
			RpcPort:     seedPeer.Port,
			DownPort:    seedPeer.DownloadPort,
			HostName:    seedPeer.Hostname,
			Idc:         seedPeer.IDC,
			Location:    seedPeer.Location,
			NetTopology: seedPeer.NetTopology,
		}, options...)
	}

	return hosts
}

// seedPeerTypeToHostType coverts seed peer type to HostType.
func seedPeerTypeToHostType(seedPeerType string) HostType {
	switch seedPeerType {
	case model.SeedPeerTypeSuperSeed:
		return HostTypeSuperSeed
	case model.SeedPeerTypeStrongSeed:
		return HostTypeStrongSeed
	case model.SeedPeerTypeWeakSeed:
		return HostTypeWeakSeed
	}

	return HostTypeWeakSeed
}

// seedPeersToNetAddrs coverts []*config.SeedPeer to []dfnet.NetAddr.
func seedPeersToNetAddrs(seedPeers []*config.SeedPeer) []dfnet.NetAddr {
	netAddrs := make([]dfnet.NetAddr, 0, len(seedPeers))
	for _, seedPeer := range seedPeers {
		netAddrs = append(netAddrs, dfnet.NetAddr{
			Type: dfnet.TCP,
			Addr: fmt.Sprintf("%s:%d", seedPeer.IP, seedPeer.Port),
		})
	}

	return netAddrs
}

// diffSeedPeers find out different seed peers.
func diffSeedPeers(sx []*config.SeedPeer, sy []*config.SeedPeer) []*config.SeedPeer {
	// Get seedPeers with the same HostID but different IP.
	var diff []*config.SeedPeer
	for _, x := range sx {
		for _, y := range sy {
			if x.Hostname != y.Hostname {
				continue
			}

			if x.Port != y.Port {
				continue
			}

			if x.IP == y.IP {
				continue
			}

			diff = append(diff, x)
		}
	}

	// Get the removed seed peers.
	for _, x := range sx {
		found := false
		for _, y := range sy {
			if idgen.SeedHostID(x.Hostname, x.Port) == idgen.SeedHostID(y.Hostname, y.Port) {
				found = true
				break
			}
		}

		if !found {
			diff = append(diff, x)
		}
	}

	return diff
}

// cdnsToHosts coverts []*config.CDN to map[string]*Host.
func cdnsToHosts(cdns []*config.CDN) map[string]*Host {
	hosts := map[string]*Host{}
	for _, cdn := range cdns {
		var netTopology string
		options := []HostOption{WithHostType(HostTypeSuperSeed), WithIsCDN(true)}
		if config, ok := cdn.GetCDNClusterConfig(); ok && config.LoadLimit > 0 {
			options = append(options, WithUploadLoadLimit(int32(config.LoadLimit)))
			netTopology = config.NetTopology
		}

		id := idgen.CDNHostID(cdn.Hostname, cdn.Port)
		hosts[id] = NewHost(&rpcscheduler.PeerHost{
			Uuid:        id,
			Ip:          cdn.IP,
			RpcPort:     cdn.Port,
			DownPort:    cdn.DownloadPort,
			HostName:    cdn.Hostname,
			Idc:         cdn.IDC,
			Location:    cdn.Location,
			NetTopology: netTopology,
		}, options...)
	}
	return hosts
}

// cdnsToNetAddrs coverts []*config.CDN to []dfnet.NetAddr.
func cdnsToNetAddrs(cdns []*config.CDN) []dfnet.NetAddr {
	netAddrs := make([]dfnet.NetAddr, 0, len(cdns))
	for _, cdn := range cdns {
		netAddrs = append(netAddrs, dfnet.NetAddr{
			Type: dfnet.TCP,
			Addr: fmt.Sprintf("%s:%d", cdn.IP, cdn.Port),
		})
	}

	return netAddrs
}

// diffCDNs find out different cdns.
func diffCDNs(cx []*config.CDN, cy []*config.CDN) []*config.CDN {
	// Get cdns with the same HostID but different IP.
	var diff []*config.CDN
	for _, x := range cx {
		for _, y := range cy {
			if x.Hostname != y.Hostname {
				continue
			}

			if x.Port != y.Port {
				continue
			}

			if x.IP == y.IP {
				continue
			}

			diff = append(diff, x)
		}
	}

	// Get the removed cdns.
	for _, x := range cx {
		found := false
		for _, y := range cy {
			if idgen.CDNHostID(x.Hostname, x.Port) == idgen.CDNHostID(y.Hostname, y.Port) {
				found = true
				break
			}
		}

		if !found {
			diff = append(diff, x)
		}
	}

	return diff
}
