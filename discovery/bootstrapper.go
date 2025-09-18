package discovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	prand "math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/miekg/dns"
)

func init() {
	prand.Seed(time.Now().Unix())
}

// NetworkPeerBootstrapper is an interface that represents an initial peer
// bootstrap mechanism. This interface is to be used to bootstrap a new peer to
// the connection by providing it with the pubkey+address of a set of existing
// peers on the network. Several bootstrap mechanisms can be implemented such
// as DNS, in channel graph, DHT's, etc.
type NetworkPeerBootstrapper interface {
	// SampleNodeAddrs uniformly samples a set of specified address from
	// the network peer bootstrapper source. The num addrs field passed in
	// denotes how many valid peer addresses to return. The passed set of
	// node nodes allows the caller to ignore a set of nodes perhaps
	// because they already have connections established.
	SampleNodeAddrs(ctx context.Context, numAddrs uint32,
		ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress,
		error)

	// Name returns a human readable string which names the concrete
	// implementation of the NetworkPeerBootstrapper.
	Name() string
}

// MultiSourceBootstrap attempts to utilize a set of NetworkPeerBootstrapper
// passed in to return the target (numAddrs) number of peer addresses that can
// be used to bootstrap a peer just joining the Lightning Network. Each
// bootstrapper will be queried successively until the target amount is met. If
// the ignore map is populated, then the bootstrappers will be instructed to
// skip those nodes.
func MultiSourceBootstrap(ctx context.Context,
	ignore map[autopilot.NodeID]struct{}, numAddrs uint32,
	bootstrappers ...NetworkPeerBootstrapper) ([]*lnwire.NetAddress, error) {

	// We'll randomly shuffle our bootstrappers before querying them in
	// order to avoid from querying the same bootstrapper method over and
	// over, as some of these might tend to provide better/worse results
	// than others.
	bootstrappers = shuffleBootstrappers(bootstrappers)

	var addrs []*lnwire.NetAddress
	for _, bootstrapper := range bootstrappers {
		// If we already have enough addresses, then we can exit early
		// w/o querying the additional bootstrappers.
		if uint32(len(addrs)) >= numAddrs {
			break
		}

		log.Infof("Attempting to bootstrap with: %v", bootstrapper.Name())

		// If we still need additional addresses, then we'll compute
		// the number of address remaining that we need to fetch.
		numAddrsLeft := numAddrs - uint32(len(addrs))
		log.Tracef("Querying for %v addresses", numAddrsLeft)
		netAddrs, err := bootstrapper.SampleNodeAddrs(
			ctx, numAddrsLeft, ignore,
		)
		if err != nil {
			// If we encounter an error with a bootstrapper, then
			// we'll continue on to the next available
			// bootstrapper.
			log.Errorf("Unable to query bootstrapper %v: %v",
				bootstrapper.Name(), err)
			continue
		}

		addrs = append(addrs, netAddrs...)
	}

	if len(addrs) == 0 {
		return nil, errors.New("no addresses found")
	}

	log.Infof("Obtained %v addrs to bootstrap network with", len(addrs))

	return addrs, nil
}

// shuffleBootstrappers shuffles the set of bootstrappers in order to avoid
// querying the same bootstrapper over and over. To shuffle the set of
// candidates, we use a version of the Fisher–Yates shuffle algorithm.
func shuffleBootstrappers(candidates []NetworkPeerBootstrapper) []NetworkPeerBootstrapper {
	shuffled := make([]NetworkPeerBootstrapper, len(candidates))
	perm := prand.Perm(len(candidates))

	for i, v := range perm {
		shuffled[v] = candidates[i]
	}

	return shuffled
}

// ChannelGraphBootstrapper is an implementation of the NetworkPeerBootstrapper
// which attempts to retrieve advertised peers directly from the active channel
// graph. This instance requires a backing autopilot.ChannelGraph instance in
// order to operate properly.
type ChannelGraphBootstrapper struct {
	chanGraph autopilot.ChannelGraph

	// hashAccumulator is used to determine which nodes to use for
	// bootstrapping. It allows us to potentially introduce some randomness
	// into the selection process.
	hashAccumulator hashAccumulator

	tried map[autopilot.NodeID]struct{}
}

// A compile time assertion to ensure that ChannelGraphBootstrapper meets the
// NetworkPeerBootstrapper interface.
var _ NetworkPeerBootstrapper = (*ChannelGraphBootstrapper)(nil)

// NewGraphBootstrapper returns a new instance of a ChannelGraphBootstrapper
// backed by an active autopilot.ChannelGraph instance. This type of network
// peer bootstrapper will use the authenticated nodes within the known channel
// graph to bootstrap connections.
func NewGraphBootstrapper(cg autopilot.ChannelGraph,
	deterministicSampling bool) (NetworkPeerBootstrapper, error) {

	var (
		hashAccumulator hashAccumulator
		err             error
	)
	if deterministicSampling {
		// If we're using deterministic sampling, then we'll use a
		// no-op hash accumulator that will always return false for
		// skipNode.
		hashAccumulator = newNoOpHashAccumulator()
	} else {
		// Otherwise, we'll use a random hash accumulator to sample
		// nodes from the channel graph.
		hashAccumulator, err = newRandomHashAccumulator()
		if err != nil {
			return nil, fmt.Errorf("unable to create hash "+
				"accumulator: %w", err)
		}
	}

	return &ChannelGraphBootstrapper{
		chanGraph:       cg,
		tried:           make(map[autopilot.NodeID]struct{}),
		hashAccumulator: hashAccumulator,
	}, nil
}

// SampleNodeAddrs uniformly samples a set of specified address from the
// network peer bootstrapper source. The num addrs field passed in denotes how
// many valid peer addresses to return.
//
// NOTE: Part of the NetworkPeerBootstrapper interface.
func (c *ChannelGraphBootstrapper) SampleNodeAddrs(_ context.Context,
	numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	ctx := context.TODO()

	// We'll merge the ignore map with our currently selected map in order
	// to ensure we don't return any duplicate nodes.
	for n := range ignore {
		log.Tracef("Ignored node %x for bootstrapping", n)
		c.tried[n] = struct{}{}
	}

	// In order to bootstrap, we'll iterate all the nodes in the channel
	// graph, accumulating nodes until either we go through all active
	// nodes, or we reach our limit. We ensure that we meet the randomly
	// sample constraint as we maintain an xor accumulator to ensure we
	// randomly sample nodes independent of the iteration of the channel
	// graph.
	sampleAddrs := func() ([]*lnwire.NetAddress, error) {
		var (
			a []*lnwire.NetAddress

			// We'll create a special error so we can return early
			// and abort the transaction once we find a match.
			errFound = fmt.Errorf("found node")
		)

		err := c.chanGraph.ForEachNode(ctx, func(_ context.Context,
			node autopilot.Node) error {

			nID := autopilot.NodeID(node.PubKey())
			if _, ok := c.tried[nID]; ok {
				return nil
			}

			// We'll select the first node we come across who's
			// public key is less than our current accumulator
			// value. When comparing, we skip the first byte as
			// it's 50/50. If it isn't less, than then we'll
			// continue forward.
			nodePubKeyBytes := node.PubKey()
			if c.hashAccumulator.skipNode(nodePubKeyBytes) {
				return nil
			}

			for _, nodeAddr := range node.Addrs() {
				// If we haven't yet reached our limit, then
				// we'll copy over the details of this node
				// into the set of addresses to be returned.
				switch nodeAddr.(type) {
				case *net.TCPAddr, *tor.OnionAddr:
				default:
					// If this isn't a valid address
					// supported by the protocol, then we'll
					// skip this node.
					return nil
				}

				nodePub, err := btcec.ParsePubKey(
					nodePubKeyBytes[:],
				)
				if err != nil {
					return err
				}

				// At this point, we've found an eligible node,
				// so we'll return early with our shibboleth
				// error.
				a = append(a, &lnwire.NetAddress{
					IdentityKey: nodePub,
					Address:     nodeAddr,
				})
			}

			return errFound
		}, func() {
			clear(a)
		})
		if err != nil && !errors.Is(err, errFound) {
			return nil, err
		}

		return a, nil
	}

	// We'll loop and sample new addresses from the graph source until
	// we've reached our target number of outbound connections or we hit 50
	// attempts, which ever comes first.
	var (
		addrs []*lnwire.NetAddress
		tries uint32
	)
	for tries < 30 && uint32(len(addrs)) < numAddrs {
		sampleAddrs, err := sampleAddrs()
		if err != nil {
			return nil, err
		}

		tries++

		// We'll now rotate our hash accumulator one value forwards.
		c.hashAccumulator.rotate()

		// If this attempt didn't yield any addresses, then we'll exit
		// early.
		if len(sampleAddrs) == 0 {
			continue
		}

		for _, addr := range sampleAddrs {
			nID := autopilot.NodeID(
				addr.IdentityKey.SerializeCompressed(),
			)

			c.tried[nID] = struct{}{}
		}

		addrs = append(addrs, sampleAddrs...)
	}

	log.Tracef("Ending hash accumulator state: %x", c.hashAccumulator)

	return addrs, nil
}

// Name returns a human readable string which names the concrete implementation
// of the NetworkPeerBootstrapper.
//
// NOTE: Part of the NetworkPeerBootstrapper interface.
func (c *ChannelGraphBootstrapper) Name() string {
	return "Authenticated Channel Graph"
}

// DNSSeedBootstrapper as an implementation of the NetworkPeerBootstrapper
// interface which implements peer bootstrapping via a special DNS seed as
// defined in BOLT-0010. For further details concerning Lightning's current DNS
// boot strapping protocol, see this link:
//   - https://github.com/lightningnetwork/lightning-rfc/blob/master/10-dns-bootstrap.md
type DNSSeedBootstrapper struct {
	// dnsSeeds is an array of two tuples we'll use for bootstrapping. The
	// first item in the tuple is the primary host we'll use to attempt the
	// SRV lookup we require. If we're unable to receive a response over
	// UDP, then we'll fall back to manual TCP resolution. The second item
	// in the tuple is a special A record that we'll query in order to
	// receive the IP address of the current authoritative DNS server for
	// the network seed.
	dnsSeeds [][2]string
	net      tor.Net

	// timeout is the maximum amount of time a dial will wait for a connect to
	// complete.
	timeout time.Duration
}

// A compile time assertion to ensure that DNSSeedBootstrapper meets the
// NetworkPeerjBootstrapper interface.
var _ NetworkPeerBootstrapper = (*ChannelGraphBootstrapper)(nil)

// NewDNSSeedBootstrapper returns a new instance of the DNSSeedBootstrapper.
// The set of passed seeds should point to DNS servers that properly implement
// Lightning's DNS peer bootstrapping protocol as defined in BOLT-0010. The set
// of passed DNS seeds should come in pairs, with the second host name to be
// used as a fallback for manual TCP resolution in the case of an error
// receiving the UDP response. The second host should return a single A record
// with the IP address of the authoritative name server.
func NewDNSSeedBootstrapper(
	seeds [][2]string, net tor.Net,
	timeout time.Duration) NetworkPeerBootstrapper {

	return &DNSSeedBootstrapper{dnsSeeds: seeds, net: net, timeout: timeout}
}

// fallBackSRVLookup attempts to manually query for SRV records we need to
// properly bootstrap. We do this by querying the special record at the "soa."
// sub-domain of supporting DNS servers. The returned IP address will be the IP
// address of the authoritative DNS server. Once we have this IP address, we'll
// connect manually over TCP to request the SRV record. This is necessary as
// the records we return are currently too large for a class of resolvers,
// causing them to be filtered out. The targetEndPoint is the original end
// point that was meant to be hit.
func (d *DNSSeedBootstrapper) fallBackSRVLookup(soaShim string,
	targetEndPoint string) ([]*net.SRV, error) {

	log.Tracef("Attempting to query fallback DNS seed")

	// First, we'll lookup the IP address of the server that will act as
	// our shim.
	addrs, err := d.net.LookupHost(soaShim)
	if err != nil {
		return nil, err
	}

	// Once we have the IP address, we'll establish a TCP connection using
	// port 53.
	dnsServer := net.JoinHostPort(addrs[0], "53")
	conn, err := d.net.Dial("tcp", dnsServer, d.timeout)
	if err != nil {
		return nil, err
	}

	dnsHost := fmt.Sprintf("_nodes._tcp.%v.", targetEndPoint)
	dnsConn := &dns.Conn{Conn: conn}
	defer dnsConn.Close()

	// With the connection established, we'll craft our SRV query, write
	// toe request, then wait for the server to give our response.
	msg := new(dns.Msg)
	msg.SetQuestion(dnsHost, dns.TypeSRV)
	if err := dnsConn.WriteMsg(msg); err != nil {
		return nil, err
	}
	resp, err := dnsConn.ReadMsg()
	if err != nil {
		return nil, err
	}

	// If the message response code was not the success code, fail.
	if resp.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("unsuccessful SRV request, "+
			"received: %v", resp.Rcode)
	}

	// Retrieve the RR(s) of the Answer section, and convert to the format
	// that net.LookupSRV would normally return.
	var rrs []*net.SRV
	for _, rr := range resp.Answer {
		srv := rr.(*dns.SRV)
		rrs = append(rrs, &net.SRV{
			Target:   srv.Target,
			Port:     srv.Port,
			Priority: srv.Priority,
			Weight:   srv.Weight,
		})
	}

	return rrs, nil
}

// SampleNodeAddrs uniformly samples a set of specified address from the
// network peer bootstrapper source. The num addrs field passed in denotes how
// many valid peer addresses to return. The set of DNS seeds are used
// successively to retrieve eligible target nodes.
func (d *DNSSeedBootstrapper) SampleNodeAddrs(_ context.Context,
	numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	var netAddrs []*lnwire.NetAddress

	// We'll try all the registered DNS seeds, exiting early if one of them
	// gives us all the peers we need.
	//
	// TODO(roasbeef): should combine results from both
search:
	for _, dnsSeedTuple := range d.dnsSeeds {
		// We'll first query the seed with an SRV record so we can
		// obtain a random sample of the encoded public keys of nodes.
		// We use the lndLookupSRV function for this task.
		primarySeed := dnsSeedTuple[0]
		_, addrs, err := d.net.LookupSRV(
			"nodes", "tcp", primarySeed, d.timeout,
		)
		if err != nil {
			log.Tracef("Unable to lookup SRV records via "+
				"primary seed (%v): %v", primarySeed, err)

			log.Trace("Falling back to secondary")

			// If the host of the secondary seed is blank, then
			// we'll bail here as we can't proceed.
			if dnsSeedTuple[1] == "" {
				log.Tracef("DNS seed %v has no secondary, "+
					"skipping fallback", primarySeed)
				continue
			}

			// If we get an error when trying to query via the
			// primary seed, we'll fallback to the secondary seed
			// before concluding failure.
			soaShim := dnsSeedTuple[1]
			addrs, err = d.fallBackSRVLookup(
				soaShim, primarySeed,
			)
			if err != nil {
				log.Tracef("Unable to query fall "+
					"back dns seed (%v): %v", soaShim, err)
				continue
			}

			log.Tracef("Successfully queried fallback DNS seed")
		}

		log.Tracef("Retrieved SRV records from dns seed: %v",
			lnutils.SpewLogClosure(addrs))

		// Next, we'll need to issue an A record request for each of
		// the nodes, skipping it if nothing comes back.
		for _, nodeSrv := range addrs {
			if uint32(len(netAddrs)) >= numAddrs {
				break search
			}

			// With the SRV target obtained, we'll now perform
			// another query to obtain the IP address for the
			// matching bech32 encoded node key. We use the
			// lndLookup function for this task.
			bechNodeHost := nodeSrv.Target
			addrs, err := d.net.LookupHost(bechNodeHost)
			if err != nil {
				return nil, err
			}

			if len(addrs) == 0 {
				log.Tracef("No addresses for %v, skipping",
					bechNodeHost)
				continue
			}

			log.Tracef("Attempting to convert: %v", bechNodeHost)

			// If the host isn't correctly formatted, then we'll
			// skip it.
			if len(bechNodeHost) == 0 ||
				!strings.Contains(bechNodeHost, ".") {

				continue
			}

			// If we have a set of valid addresses, then we'll need
			// to parse the public key from the original bech32
			// encoded string.
			bechNode := strings.Split(bechNodeHost, ".")
			_, nodeBytes5Bits, err := bech32.Decode(bechNode[0])
			if err != nil {
				return nil, err
			}

			// Once we have the bech32 decoded pubkey, we'll need
			// to convert the 5-bit word grouping into our regular
			// 8-bit word grouping so we can convert it into a
			// public key.
			nodeBytes, err := bech32.ConvertBits(
				nodeBytes5Bits, 5, 8, false,
			)
			if err != nil {
				return nil, err
			}
			nodeKey, err := btcec.ParsePubKey(nodeBytes)
			if err != nil {
				return nil, err
			}

			// If we have an ignore list, and this node is in the
			// ignore list, then we'll go to the next candidate.
			if ignore != nil {
				nID := autopilot.NewNodeID(nodeKey)
				if _, ok := ignore[nID]; ok {
					continue
				}
			}

			// Finally we'll convert the host:port peer to a proper
			// TCP address to use within the lnwire.NetAddress. We
			// don't need to use the lndResolveTCP function here
			// because we already have the host:port peer.
			addr := net.JoinHostPort(
				addrs[0],
				strconv.FormatUint(uint64(nodeSrv.Port), 10),
			)
			tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				return nil, err
			}

			// Finally, with all the information parsed, we'll
			// return this fully valid address as a connection
			// attempt.
			lnAddr := &lnwire.NetAddress{
				IdentityKey: nodeKey,
				Address:     tcpAddr,
			}

			log.Tracef("Obtained %v as valid reachable "+
				"node", lnAddr)

			netAddrs = append(netAddrs, lnAddr)
		}
	}

	return netAddrs, nil
}

// Name returns a human readable string which names the concrete
// implementation of the NetworkPeerBootstrapper.
func (d *DNSSeedBootstrapper) Name() string {
	return fmt.Sprintf("BOLT-0010 DNS Seed: %v", d.dnsSeeds)
}

// hashAccumulator is an interface that defines the methods required for
// a hash accumulator used to sample nodes from the channel graph.
type hashAccumulator interface {
	// rotate rotates the hash accumulator value.
	rotate()

	// skipNode returns true if the node with the given public key
	// should be skipped based on the current hash accumulator state.
	skipNode(pubKey route.Vertex) bool
}

// randomHashAccumulator is an implementation of the hashAccumulator
// interface that uses a random hash to sample nodes from the channel graph.
type randomHashAccumulator struct {
	hash [32]byte
}

// A compile time assertion to ensure that randomHashAccumulator meets the
// hashAccumulator interface.
var _ hashAccumulator = (*randomHashAccumulator)(nil)

// newRandomHashAccumulator returns a new instance of a randomHashAccumulator.
// This accumulator is used to randomly sample nodes from the channel graph.
func newRandomHashAccumulator() (*randomHashAccumulator, error) {
	var r randomHashAccumulator

	if _, err := rand.Read(r.hash[:]); err != nil {
		return nil, fmt.Errorf("unable to read random bytes: %w", err)
	}

	return &r, nil
}

// rotate rotates the hash accumulator by hashing the current value
// with itself. This ensures that we have a new random value to compare
// against when we sample nodes from the channel graph.
//
// NOTE: this is part of the hashAccumulator interface.
func (r *randomHashAccumulator) rotate() {
	r.hash = sha256.Sum256(r.hash[:])
}

// skipNode returns true if the node with the given public key should be skipped
// based on the current hash accumulator state. It will return false for the
// pub key if it is lexicographically less than our current accumulator value.
// It does so by comparing the current hash accumulator value with the passed
// byte slice. When comparing, we skip the first byte as it's 50/50 between 02
// and 03 for compressed pub keys.
//
// NOTE: this is part of the hashAccumulator interface.
func (r *randomHashAccumulator) skipNode(pub route.Vertex) bool {
	return bytes.Compare(r.hash[:], pub[1:]) > 0
}

// noOpHashAccumulator is a no-op implementation of the hashAccumulator
// interface. This is used when we want deterministic behavior and don't
// want to sample nodes randomly from the channel graph.
type noOpHashAccumulator struct{}

// newNoOpHashAccumulator returns a new instance of a noOpHashAccumulator.
func newNoOpHashAccumulator() *noOpHashAccumulator {
	return &noOpHashAccumulator{}
}

// rotate is a no-op for the noOpHashAccumulator.
//
// NOTE: this is part of the hashAccumulator interface.
func (*noOpHashAccumulator) rotate() {}

// skipNode always returns false, meaning that no nodes will be skipped.
//
// NOTE: this is part of the hashAccumulator interface.
func (*noOpHashAccumulator) skipNode(route.Vertex) bool {
	return false
}

// A compile-time assertion to ensure that noOpHashAccumulator meets the
// hashAccumulator interface.
var _ hashAccumulator = (*noOpHashAccumulator)(nil)
