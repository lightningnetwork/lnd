package discovery

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/miekg/dns"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil/bech32"
)

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
	SampleNodeAddrs(numAddrs uint32,
		ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error)

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
func MultiSourceBootstrap(ignore map[autopilot.NodeID]struct{}, numAddrs uint32,
	bootStrappers ...NetworkPeerBootstrapper) ([]*lnwire.NetAddress, error) {

	var addrs []*lnwire.NetAddress
	for _, bootStrapper := range bootStrappers {
		// If we already have enough addresses, then we can exit early
		// w/o querying the additional bootstrappers.
		if uint32(len(addrs)) >= numAddrs {
			break
		}

		log.Infof("Attempting to bootstrap with: %v", bootStrapper.Name())

		// If we still need additional addresses, then we'll compute
		// the number of address remaining that we need to fetch.
		numAddrsLeft := numAddrs - uint32(len(addrs))
		log.Tracef("Querying for %v addresses", numAddrsLeft)
		netAddrs, err := bootStrapper.SampleNodeAddrs(numAddrsLeft, ignore)
		if err != nil {
			// If we encounter an error with a bootstrapper, then
			// we'll continue on to the next available
			// bootstrapper.
			log.Errorf("Unable to query bootstrapper %v: %v",
				bootStrapper.Name(), err)
			continue
		}

		addrs = append(addrs, netAddrs...)
	}

	log.Infof("Obtained %v addrs to bootstrap network with", len(addrs))

	return addrs, nil
}

// ChannelGraphBootstrapper is an implementation of the NetworkPeerBootstrapper
// which attempts to retrieve advertised peers directly from the active channel
// graph. This instance requires a backing autopilot.ChannelGraph instance in
// order to operate properly.
type ChannelGraphBootstrapper struct {
	chanGraph autopilot.ChannelGraph

	// hashAccumulator is a set of 32 random bytes that are read upon the
	// creation of the channel graph bootstrapper. We use this value to
	// randomly select nodes within the known graph to connect to. After
	// each selection, we rotate the accumulator by hashing it with itself.
	hashAccumulator [32]byte

	tried map[autopilot.NodeID]struct{}
}

// A compile time assertion to ensure that ChannelGraphBootstrapper meets the
// NetworkPeerBootstrapper interface.
var _ NetworkPeerBootstrapper = (*ChannelGraphBootstrapper)(nil)

// NewGraphBootstrapper returns a new instance of a ChannelGraphBootstrapper
// backed by an active autopilot.ChannelGraph instance. This type of network
// peer bootstrapper will use the authenticated nodes within the known channel
// graph to bootstrap connections.
func NewGraphBootstrapper(cg autopilot.ChannelGraph) (NetworkPeerBootstrapper, error) {

	c := &ChannelGraphBootstrapper{
		chanGraph: cg,
		tried:     make(map[autopilot.NodeID]struct{}),
	}

	if _, err := rand.Read(c.hashAccumulator[:]); err != nil {
		return nil, err
	}

	return c, nil
}

// SampleNodeAddrs uniformly samples a set of specified address from the
// network peer bootstrapper source. The num addrs field passed in denotes how
// many valid peer addresses to return.
//
// NOTE: Part of the NetworkPeerBootstrapper interface.
func (c *ChannelGraphBootstrapper) SampleNodeAddrs(numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	// We'll merge the ignore map with our currently selected map in order
	// to ensure we don't return any duplicate nodes across inovocations.
	for n := range ignore {
		c.tried[n] = struct{}{}
	}

	// In order to bootstrap, we'll iterate all the nodes in the channel
	// graph and assign each one a lottery number as a sequence of bytes.
	// To maintain the random sampling criteria, the lottery number for each
	// node is calculated as the hash of the node's public key and our
	// accumulator. The lowest `numAddrs` lottery numbers are kept as winners.
	var (
		// candidate bootstrap nodes
		nodes []autopilot.Node
		// corresponding lottery numbers for each node above, maintained in
		// ascending sorted order
		hashes [][]byte
	)
	err := c.chanGraph.ForEachNode(func(node autopilot.Node) error {
		nID := autopilot.NewNodeID(node.PubKey())

		// pass by nodes which we should ignore
		if _, ok := c.tried[nID]; ok {
			return nil
		}

		// pass by nodes with no TCP addresses, since we only bootstrap with TCP
		hasTCPAddr := false
		for _, nodeAddr := range node.Addrs() {
			if _, ok := nodeAddr.(*net.TCPAddr); ok {
				hasTCPAddr = true
				break
			}
		}
		if !hasTCPAddr {
			return nil
		}

		// calculate a hash of this node's pubkey and our accumulator, which
		// acts as its lottery number into our sampling
		h := sha256.Sum256(
			append(c.hashAccumulator[:], node.PubKey().SerializeCompressed()...),
		)
		// convert array to a slice
		hash := h[:]

		// determine whether this node should be included
		if len(nodes) == 0 {
			// always include the first node
			nodes = append(nodes, node)
			hashes = append(hashes, hash[:])

		} else if uint32(len(nodes)) < numAddrs ||
			bytes.Compare(hash, hashes[len(hashes)-1]) < 0 {
			// Either this hash is smaller than the largest hash among the
			// current lottery winners, or we are still doing an initial fill.
			// So we should include this node as a winner. This is done by
			// inserting at the end and bubbling up, to maintain sorted order.
			nodes = append(nodes, node)
			hashes = append(hashes, hash)

			// bubble up by looping backwards until in sorted order
			for i := len(nodes) - 2; i >= 0 && bytes.Compare(hash, hashes[i]) < 0; i-- {
				// swap up
				nodes[i+1] = nodes[i]
				hashes[i+1] = hashes[i]
				nodes[i] = node
				hashes[i] = hash
			}

			// enforce only `numAddrs` winners of the lottery
			if uint32(len(nodes)) > numAddrs {
				nodes = nodes[:numAddrs]
				hashes = hashes[:numAddrs]
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// we now have our lottery winners, place them all into the ignore list
	for _, node := range nodes {
		nID := autopilot.NewNodeID(node.PubKey())
		c.tried[nID] = struct{}{}
	}

	// Select the first address from each node, plus any more to make up for
	// a shortage in available nodes
	shortage := numAddrs - uint32(len(nodes))
	var addrs []*lnwire.NetAddress
	for _, node := range nodes {
		foundOne := false
		for _, nodeAddr := range node.Addrs() {
			tcpAddr, ok := nodeAddr.(*net.TCPAddr)
			if ok {
				// If this isn't a valid TCP address,
				// then we'll ignore it as currently
				// we'll only attempt to connect out to
				// TCP peers.
				addrs = append(addrs, &lnwire.NetAddress{
					IdentityKey: node.PubKey(),
					Address:     tcpAddr,
				})

				if foundOne == false {
					// if this is the first address we've found, do not
					// count its contribution as filling the shortage
					foundOne = true
				} else {
					shortage--
				}
				// Only keep selecting addresses to make up for a shortage
				if shortage <= 0 {
					break
				}
			}
		}
	}

	// We'll now rotate our hash accumulator one value forwards.
	c.hashAccumulator = sha256.Sum256(c.hashAccumulator[:])

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
//     * https://github.com/lightningnetwork/lightning-rfc/blob/master/10-dns-bootstrap.md
type DNSSeedBootstrapper struct {
	// dnsSeeds is an array of two tuples we'll use for bootstrapping. The
	// first item in the tuple is the primary host we'll use to attempt the
	// SRV lookup we require. If we're unable to receive a response over
	// UDP, then we'll fall back to manual TCP resolution. The second item
	// in the tuple is a special A record that we'll query in order to
	// receive the IP address of the current authoritative DNS server for
	// the network seed.
	dnsSeeds   [][2]string
	lookupHost func(string) ([]string, error)
	lookupSRV  func(string, string, string) (string, []*net.SRV, error)
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
func NewDNSSeedBootstrapper(seeds [][2]string, lookupHost func(string) ([]string, error),
	lookupSRV func(string, string, string) (string, []*net.SRV, error)) (
	NetworkPeerBootstrapper, error) {
	return &DNSSeedBootstrapper{
		dnsSeeds:   seeds,
		lookupHost: lookupHost,
		lookupSRV:  lookupSRV,
	}, nil
}

// fallBackSRVLookup attempts to manually query for SRV records we need to
// properly bootstrap. We do this by querying the special record at the "soa."
// sub-domain of supporting DNS servers. The retuned IP address will be the IP
// address of the authoritative DNS server. Once we have this IP address, we'll
// connect manually over TCP to request the SRV record. This is necessary as
// the records we return are currently too large for a class of resolvers,
// causing them to be filtered out. The targetEndPoint is the original end
// point that was meant to be hit.
func fallBackSRVLookup(soaShim string, targetEndPoint string) ([]*net.SRV, error) {
	log.Tracef("Attempting to query fallback DNS seed")

	// First, we'll lookup the IP address of the server that will act as
	// our shim.
	addrs, err := net.LookupHost(soaShim)
	if err != nil {
		return nil, err
	}

	// Once we have the IP address, we'll establish a TCP connection using
	// port 53.
	dnsServer := net.JoinHostPort(addrs[0], "53")
	conn, err := net.Dial("tcp", dnsServer)
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
		return nil, fmt.Errorf("Unsuccessful SRV request, "+
			"received: %v", resp.Rcode)
	}

	// Retrieve the RR(s) of the Answer section, and covert to the format
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
func (d *DNSSeedBootstrapper) SampleNodeAddrs(numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	var netAddrs []*lnwire.NetAddress

	// We'll continue this loop until we reach our target address limit.
	// Each SRV query to the seed will return 25 random nodes, so we can
	// continue to query until we reach our target.
search:
	for uint32(len(netAddrs)) < numAddrs {
		for _, dnsSeedTuple := range d.dnsSeeds {
			// We'll first query the seed with an SRV record so we
			// can obtain a random sample of the encoded public
			// keys of nodes. We use the lndLookupSRV function for
			// this task.
			primarySeed := dnsSeedTuple[0]
			_, addrs, err := d.lookupSRV("nodes", "tcp", primarySeed)
			if err != nil {
				log.Tracef("Unable to lookup SRV records via " +
					"primary seed, falling back to secondary")

				// If the host of the secondary seed is blank,
				// then we'll bail here as we can't proceed.
				if dnsSeedTuple[1] == "" {
					return nil, fmt.Errorf("Secondary seed is blank")
				}

				// If we get an error when trying to query via
				// the primary seed, we'll fallback to the
				// secondary seed before concluding failure.
				soaShim := dnsSeedTuple[1]
				addrs, err = fallBackSRVLookup(
					soaShim, primarySeed,
				)
				if err != nil {
					return nil, err
				}
				log.Tracef("Successfully queried fallback DNS seed")
			}

			log.Tracef("Retrieved SRV records from dns seed: %v",
				spew.Sdump(addrs))

			// Next, we'll need to issue an A record request for
			// each of the nodes, skipping it if nothing comes
			// back.
			for _, nodeSrv := range addrs {
				if uint32(len(netAddrs)) >= numAddrs {
					break search
				}

				// With the SRV target obtained, we'll now
				// perform another query to obtain the IP
				// address for the matching bech32 encoded node
				// key. We use the lndLookup function for this
				// task.
				bechNodeHost := nodeSrv.Target
				addrs, err := d.lookupHost(bechNodeHost)
				if err != nil {
					return nil, err
				}

				if len(addrs) == 0 {
					log.Tracef("No addresses for %v, skipping",
						bechNodeHost)
					continue
				}

				log.Tracef("Attempting to convert: %v", bechNodeHost)

				// If we have a set of valid addresses, then
				// we'll need to parse the public key from the
				// original bech32 encoded string.
				bechNode := strings.Split(bechNodeHost, ".")
				_, nodeBytes5Bits, err := bech32.Decode(bechNode[0])
				if err != nil {
					return nil, err
				}

				// Once we have the bech32 decoded pubkey,
				// we'll need to convert the 5-bit word
				// grouping into our regular 8-bit word
				// grouping so we can convert it into a public
				// key.
				nodeBytes, err := bech32.ConvertBits(
					nodeBytes5Bits, 5, 8, false,
				)
				if err != nil {
					return nil, err
				}
				nodeKey, err := btcec.ParsePubKey(
					nodeBytes, btcec.S256(),
				)
				if err != nil {
					return nil, err
				}

				// If we have an ignore list, and this node is
				// in the ignore list, then we'll go to the
				// next candidate.
				if ignore != nil {
					nID := autopilot.NewNodeID(nodeKey)
					if _, ok := ignore[nID]; ok {
						continue
					}
				}

				// Finally we'll convert the host:port peer to
				// a proper TCP address to use within the
				// lnwire.NetAddress. We don't need to use
				// the lndResolveTCP function here because we
				// already have the host:port peer.
				addr := net.JoinHostPort(addrs[0],
					strconv.FormatUint(uint64(nodeSrv.Port), 10))
				tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
				if err != nil {
					return nil, err
				}

				// Finally, with all the information parsed,
				// we'll return this fully valid address as a
				// connection attempt.
				lnAddr := &lnwire.NetAddress{
					IdentityKey: nodeKey,
					Address:     tcpAddr,
				}

				log.Tracef("Obtained %v as valid reachable "+
					"node", lnAddr)

				netAddrs = append(netAddrs, lnAddr)
			}
		}
	}

	return netAddrs, nil
}

// Name returns a human readable string which names the concrete
// implementation of the NetworkPeerBootstrapper.
func (d *DNSSeedBootstrapper) Name() string {
	return fmt.Sprintf("BOLT-0010 DNS Seed: %v", d.dnsSeeds)
}
