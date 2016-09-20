package sphinx

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcutil"
)

const (
	// So, 256-bit EC curve pubkeys, 160-bit keys symmetric encryption,
	// 160-bit keys for HMAC, etc. Represented in bytes.
	securityParameter = 20

	// Default message size in bytes. This is probably *much* too big atm?
	messageSize = 1024

	// Mix header over head. If we assume 5 hops (which seems sufficient for
	// LN, for now atleast), 32 byte group element to be re-randomized each
	// hop, and 32 byte symmetric key.
	// Overhead is: p + (2r + 2)s
	//  * p = pub key size (in bytes, for DH each hop)
	//  * r = max number of hops
	//  * s = summetric key size (in bytes)
	// It's: 32 + (2*5 + 2) * 20 = 273 bytes! But if we use secp256k1 instead of
	// Curve25519, then we've have an extra byte for the compressed keys.
	// 837 bytes for 20 hops.
	mixHeaderOverhead = 273

	// The maximum path length. This should be set to an
	// estiamate of the upper limit of the diameter of the node graph.
	numMaxHops = 5

	// Special destination to indicate we're at the end of the path.
	nullDestination = 0x00

	// (2r + 3)k = (2*5 + 3) * 32 = 260
	// The number of bytes produced by our CSPRG for the key stream
	// implementing our stream cipher to encrypt/decrypt the mix header. The
	// last 2 * securityParameter bytes are only used in order to generate/check
	// the MAC over the header.
	numStreamBytes = (2*numMaxHops + 3) * securityParameter

	sharedSecretSize = 32

	// node_id + mac + (2*5-1)*20
	// 20 + 20 + 180 = 220
	routingInfoSize = (securityParameter * 2) + (2*numMaxHops-1)*securityParameter
)

var defaultBitcoinNet = &chaincfg.TestNet3Params

//type LnAddr btcutil.Address
// TODO(roasbeef): ok, so we're back to k=20 then. Still using the truncated sha256 MAC.
type LightningAddress []byte

var zeroNode [securityParameter]byte
var nullDest byte

// MixHeader is the onion wrapped hop-to-hop routing information neccessary to
// propagate a message through the mix-net without intermediate nodes having
// knowledge of their position within the route, the source, the destination,
// and finally the identities of the past/future nodes in the route. At each hop
// the ephemeral key is used by the node to perform ECDH between itself and the
// source node. This derived secret key is used to check the MAC of the entire mix
// header, decrypt the next set of routing information, and re-randomize the
// ephemeral key for the next node in the path. This per-hop re-randomization
// allows us to only propgate a single group element through the onion route.
type MixHeader struct {
	EphemeralKey *btcec.PublicKey
	RoutingInfo  [routingInfoSize]byte
	HeaderMAC    [securityParameter]byte
}

// NewMixHeader creates a new mix header which is capable of obliviously
// routing a message through the mix-net path outline by 'paymentPath'
// to a final node indicated by 'identifier' housing a message addressed to
// 'dest'. This function returns the created mix header along with a derived
// shared secret for each node in the path.
func NewMixHeader(dest LightningAddress, identifier [securityParameter]byte,
	paymentPath []*btcec.PublicKey) (*MixHeader, [][sharedSecretSize]byte, error) {
	// Each hop performs ECDH with our ephemeral key pair to arrive at a
	// shared secret. Additionally, each hop randomizes the group element
	// for the next hop by multiplying it by the blinding factor. This way
	// we only need to transmit a single group element, and hops can't link
	// a session back to us if they have several nodes in the path.
	numHops := len(paymentPath)
	hopEphemeralPubKeys := make([]*btcec.PublicKey, numHops)
	hopSharedSecrets := make([][sha256.Size]byte, numHops)
	hopBlindingFactors := make([][sha256.Size]byte, numHops)

	// Generate a new ephemeral key to use for ECDH for this session.
	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	// Compute the triplet for the first hop outside of the main loop.
	// Within the loop each new triplet will be computed recursively based
	// off of the blinding factor of the last hop.
	hopEphemeralPubKeys[0] = sessionKey.PubKey()
	hopSharedSecrets[0] = sha256.Sum256(btcec.GenerateSharedSecret(sessionKey, paymentPath[0]))
	hopBlindingFactors[0] = computeBlindingFactor(hopEphemeralPubKeys[0], hopSharedSecrets[0][:])

	// Now recursively compute the ephemeral ECDH pub keys, the shared
	// secret, and blinding factor for each hop.
	for i := 1; i <= numHops-1; i++ {
		// a_{n} = a_{n-1} x c_{n-1} -> (Y_prev_pub_key x prevBlindingFactor)
		hopEphemeralPubKeys[i] = blindGroupElement(hopEphemeralPubKeys[i-1],
			hopBlindingFactors[i-1][:])

		// s_{n} = sha256( y_{n} x c_{n-1} ) ->
		// (Y_their_pub_key x x_our_priv) x all prev blinding factors
		yToX := blindGroupElement(paymentPath[i], sessionKey.D.Bytes())
		hopSharedSecrets[i] = sha256.Sum256(multiScalarMult(yToX, hopBlindingFactors[:i]).X.Bytes())

		// TODO(roasbeef): prob don't need to store all blinding factors, only the prev...
		// b_{n} = sha256(a_{n} || s_{n})
		hopBlindingFactors[i] = computeBlindingFactor(hopEphemeralPubKeys[i],
			hopSharedSecrets[i][:])

	}

	// Generate the padding, called "filler strings" in the paper.
	filler := generateHeaderPadding(numHops, hopSharedSecrets)
	// First we generate the routing info + MAC for the very last hop.
	mixHeader := make([]byte, 0, routingInfoSize)
	mixHeader = append(mixHeader, dest...)
	mixHeader = append(mixHeader, identifier[:]...)
	mixHeader = append(mixHeader,
		bytes.Repeat([]byte{0}, ((2*(numMaxHops-numHops)+2)*securityParameter-len(dest)))...)

	// Encrypt the header for the final hop with the shared secret the
	// destination will eventually derive, then pad the message out to full
	// size with the "random" filler bytes.
	streamBytes := generateCipherStream(generateKey("rho", hopSharedSecrets[numHops-1]), numStreamBytes)
	xor(mixHeader, mixHeader, streamBytes[:(2*(numMaxHops-numHops)+3)*securityParameter])
	mixHeader = append(mixHeader, filler...)

	// Calculate a MAC over the encrypted mix header for the last hop
	// (including the filler bytes), using the same shared secret key as
	// used for encryption above.
	headerMac := calcMac(generateKey("mu", hopSharedSecrets[numHops-1]), mixHeader)

	// Now we compute the routing information for each hop, along with a
	// MAC of the routing info using the shared key for that hop.
	for i := numHops - 2; i >= 0; i-- {
		// The next hop from the point of view of the current hop. Node
		// ID's are currently the hash160 of a node's pubKey serialized
		// in compressed format.
		nodeID := btcutil.Hash160(paymentPath[i+1].SerializeCompressed())

		var b bytes.Buffer
		b.Write(nodeID)
		// MAC for mix header.
		b.Write(headerMac[:])
		// Mix header itself.
		b.Write(mixHeader[:(2*numMaxHops-1)*securityParameter])

		streamBytes := generateCipherStream(generateKey("rho", hopSharedSecrets[i]), numStreamBytes)
		xor(mixHeader, b.Bytes(), streamBytes[:(2*numMaxHops+1)*securityParameter])
		headerMac = calcMac(generateKey("mu", hopSharedSecrets[i]), mixHeader)
	}

	var r [routingInfoSize]byte
	copy(r[:], mixHeader)
	header := &MixHeader{
		EphemeralKey: hopEphemeralPubKeys[0],
		RoutingInfo:  r,
		HeaderMAC:    headerMac,
	}

	return header, hopSharedSecrets, nil
}

// generateHeaderPadding derives the bytes for padding the mix header to ensure
// it remains fixed sized throughout route transit. At each step, we add
// 2*securityParameter padding of zeroes, concatenate it to the previous
// filler, then decrypt it (XOR) with the secret key of the current hop. When
// encrypting the mix header we essentially do the reverse of this operation:
// we "encrypt" the padding, and drop 2*k number of zeroes. As nodes process
// the mix header they add the padding (2*k) in order to check the MAC and
// decrypt the next routing information eventually leaving only the original
// "filler" bytes produced by this function at the last hop. Using this
// methodology, the size of the mix header stays constant at each hop.
func generateHeaderPadding(numHops int, sharedSecrets [][sharedSecretSize]byte) []byte {
	filler := make([]byte, 2*(numHops-1)*securityParameter)

	for i := 1; i < numHops; i++ {
		totalFillerSize := (2*(numMaxHops-i) + 3) * securityParameter
		padding := bytes.Repeat([]byte{0}, 2*securityParameter)

		var tempBuf bytes.Buffer
		tempBuf.Write(filler)
		tempBuf.Write(padding)

		streamBytes := generateCipherStream(generateKey("rho", sharedSecrets[i-1]),
			numStreamBytes)

		xor(filler, tempBuf.Bytes(), streamBytes[totalFillerSize:])
	}

	return filler
}

// OnionPacket represents a forwarding message containing onion wrapped
// hop-to-hop routing information along with an onion encrypted payload message
// addressed to the final destination.
type OnionPacket struct {
	Header *MixHeader
	Msg    [messageSize]byte
}

// NewOnionPaccket generates the a mix header containing the neccessary onion
// routing information required to propagate the message through the mixnet,
// eventually reaching the final node specified by 'identifier'. The onion
// encrypted message payload is then to be delivered to the specified 'dest'
// address.
func NewOnionPacket(route []*btcec.PublicKey, dest LightningAddress,
	message []byte) (*OnionPacket, error) {
	routeLength := len(route)

	// Compute the mix header, and shared secerts for each hop. We pass in
	// the null destination and zero identifier in order for the final node
	// in the route to be able to distinguish the payload as addressed to
	// itself.
	mixHeader, secrets, err := NewMixHeader([]byte{nullDest}, zeroNode, route)
	if err != nil {
		return nil, err
	}

	// Now for the body of the message. The next-node ID is set to all
	// zeroes in order to notify the final hop that the message is meant for
	// them. m = 0^k || dest || msg || padding.
	var body [messageSize]byte
	n := copy(body[:], bytes.Repeat([]byte{0}, securityParameter))
	// TODO(roasbeef): destination vs identifier (node id) format.
	n += copy(body[n:], []byte(dest))
	n += copy(body[n:], message)
	// TODO(roasbeef): make pad and unpad functions.
	n += copy(body[n:], []byte{0x7f})
	n += copy(body[n:], bytes.Repeat([]byte{0xff}, messageSize-len(body)))

	// Now we construct the onion. Walking backwards from the last hop, we
	// encrypt the message with the shared secret for each hop in the path.
	onion := lionessEncode(generateKey("pi", secrets[routeLength-1]), body)
	for i := routeLength - 2; i >= 0; i-- {
		onion = lionessEncode(generateKey("pi", secrets[i]), onion)
	}

	return &OnionPacket{Header: mixHeader, Msg: onion}, nil
}

// Encode serializes the raw bytes of the onoin packet into the passed
// io.Writer. The form encoded within the passed io.Writer is suitable for
// either storing on disk, or sending over the network.
func (f *OnionPacket) Encode(w io.Writer) error {
	ephemeral := f.Header.EphemeralKey.SerializeCompressed()
	if _, err := w.Write(ephemeral); err != nil {
		return err
	}

	if _, err := w.Write(f.Header.RoutingInfo[:]); err != nil {
		return err
	}

	if _, err := w.Write(f.Header.HeaderMAC[:]); err != nil {
		return err
	}

	if _, err := w.Write(f.Msg[:]); err != nil {
		return err
	}

	return nil
}

// Decode fully populates the target ForwardingMessage from the raw bytes
// encoded within the io.Reader. In the case of any decoding errors, an error
// will be returned. If the method successs, then the new ForwardingMessage is
// ready to be processed by an instance of SphinxNode.
func (f *OnionPacket) Decode(r io.Reader) error {
	var err error

	f.Header = &MixHeader{}

	var ephemeral [33]byte
	if _, err := io.ReadFull(r, ephemeral[:]); err != nil {
		return err
	}
	f.Header.EphemeralKey, err = btcec.ParsePubKey(ephemeral[:], btcec.S256())
	if err != nil {
		return err
	}

	if _, err := io.ReadFull(r, f.Header.RoutingInfo[:]); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, f.Header.HeaderMAC[:]); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, f.Msg[:]); err != nil {
		return err
	}

	return nil
}

// calcMac calculates HMAC-SHA-256 over the message using the passed secret key as
// input to the HMAC.
func calcMac(key [securityParameter]byte, msg []byte) [securityParameter]byte {
	hmac := hmac.New(sha256.New, key[:])
	hmac.Write(msg)
	h := hmac.Sum(nil)

	var mac [securityParameter]byte
	copy(mac[:], h[:securityParameter])

	return mac
}

// xor computes the byte wise XOR of a and b, storing the result in dst.
func xor(dst, a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dst[i] = a[i] ^ b[i]
	}
	return n
}

// generateKey generates a new key for usage in Sphinx packet
// construction/processing based off of the denoted keyType. Within Sphinx
// various keys are used within the same onion packet for padding generation,
// MAC generation, and encryption/decryption.
func generateKey(keyType string, sharedKey [sharedSecretSize]byte) [securityParameter]byte {
	mac := hmac.New(sha256.New, []byte(keyType))
	mac.Write(sharedKey[:])
	h := mac.Sum(nil)

	var key [securityParameter]byte
	copy(key[:], h[:securityParameter])

	return key
}

// generateCipherStream generates a stream of cryptographic psuedo-random bytes
// intened to be used to encrypt a message using a one-time-pad like
// construciton.
func generateCipherStream(key [securityParameter]byte, numBytes uint) []byte {
	// Key must be 16, 24, or 32 bytes.
	block, _ := aes.NewCipher(key[:16])

	// We use AES in CTR mode to generate a psuedo randmom stream of bytes
	// by encrypting a plaintext of all zeroes.
	cipherStream := make([]byte, numBytes)
	plainText := bytes.Repeat([]byte{0}, int(numBytes))

	// Our IV is just zero....
	iv := bytes.Repeat([]byte{0}, aes.BlockSize)

	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(cipherStream, plainText)

	return cipherStream
}

// computeBlindingFactor for the next hop given the ephemeral pubKey and
// sharedSecret for this hop. The blinding factor is computed as the
// sha-256(pubkey || sharedSecret).
func computeBlindingFactor(hopPubKey *btcec.PublicKey, hopSharedSecret []byte) [sha256.Size]byte {
	sha := sha256.New()
	sha.Write(hopPubKey.SerializeCompressed())
	sha.Write(hopSharedSecret)

	var hash [sha256.Size]byte
	copy(hash[:], sha.Sum(nil))
	return hash
}

// blindGroupElement blinds the group element by performing scalar
// multiplication of the group element by blindingFactor: G x blindingFactor.
func blindGroupElement(hopPubKey *btcec.PublicKey, blindingFactor []byte) *btcec.PublicKey {
	newX, newY := hopPubKey.Curve.ScalarMult(hopPubKey.X, hopPubKey.Y, blindingFactor[:])
	return &btcec.PublicKey{hopPubKey.Curve, newX, newY}
}

// multiScalarMult computes the cumulative product of the blinding factors
// times the passed public key.
//
// TODO(roasbeef): optimize using totient?
func multiScalarMult(hopPubKey *btcec.PublicKey, blindingFactors [][sha256.Size]byte) *btcec.PublicKey {
	finalPubKey := hopPubKey

	for _, blindingFactor := range blindingFactors {
		finalPubKey = blindGroupElement(finalPubKey, blindingFactor[:])
	}

	return finalPubKey
}

// ProcessCode is an enum-like type which describes to the high-level package
// user which action shuold be taken after processing a Sphinx packet.
type ProcessCode int

const (
	// ExitNode indicates that the node which processed the Sphinx packet
	// is the destination hop in the route.
	ExitNode = iota

	// MoreHops indicates that there are additional hops left within the
	// route. Therefore the caller should forward the packet to the node
	// denoted as the "NextHop".
	MoreHops

	// Failure indicates that a failure occured during packet processing.
	Failure
)

// String returns a human readable string for each of the ProcessCodes.
func (p ProcessCode) String() string {
	switch p {
	case ExitNode:
		return "ExitNode"
	case MoreHops:
		return "MoreHops"
	case Failure:
		return "Failure"
	default:
		return "Unknown"
	}
}

// ProcessedPacket encapsulates the resulting state generated after processing
// an OnionPacket. A processed packet communicates to the caller what action
// shuold be taken after processing.
type ProcessedPacket struct {
	// Action represents the action the caller should take after processing
	// the packet.
	Action ProcessCode

	// NextHop is the next hop in the route the caller should forward the
	// onion packet to.
	//
	// NOTE: This field will only be populated iff the above Action is
	// MoreHops.
	NextHop [securityParameter]byte

	// Packet is the resulting packet uncovered afer processing the
	// original onion packet by stripping off a layer from mix-header and
	// message.
	//
	// NOTE: This field will only be populated iff the above Action is
	// MoreHops.
	Packet *OnionPacket

	// DestAddr is the destination address of the final intended receiver.
	//
	// NOTE: This field will only be populated iff the above Action is
	// ExitNode.
	DestAddr LightningAddress

	// DestMsg is the final e2e message addressed to the final destination.
	//
	// NOTE: This field will only be populated iff the above Action is
	// ExitNode.
	DestMsg []byte
}

// Router is an onion router within the Sphinx network. The router is capable
// of processing incoming Sphinx onion packets thereby "peeling" a layer off
// the onion encryption which the packet is wrapped with.
type Router struct {
	nodeID [securityParameter]byte

	// TODO(roasbeef): swap out with btcutil.AddressLightningKey maybe?
	nodeAddr *btcutil.AddressPubKeyHash
	onionKey *btcec.PrivateKey

	sync.RWMutex
	seenSecrets map[[sharedSecretSize]byte]struct{}
}

// NewRouter creates a new instance of a Sphinx onion Router given the node's
// currently advertised onion private key, and the target Bitcoin network.
func NewRouter(nodeKey *btcec.PrivateKey, net *chaincfg.Params) *Router {
	var nodeID [securityParameter]byte
	copy(nodeID[:], btcutil.Hash160(nodeKey.PubKey().SerializeCompressed()))

	// Safe to ignore the error here, nodeID is 20 bytes.
	nodeAddr, _ := btcutil.NewAddressPubKeyHash(nodeID[:], net)

	return &Router{
		nodeID:   nodeID,
		nodeAddr: nodeAddr,
		onionKey: nodeKey,
		// TODO(roasbeef): replace instead with bloom filter?
		// * https://moderncrypto.org/mail-archive/messaging/2015/001911.html
		seenSecrets: make(map[[sharedSecretSize]byte]struct{}),
	}
}

// ProcessOnionPacket processes an incoming onion packet which has been forward
// to the target Sphinx router. If the encoded ephemeral key isn't on the
// target Elliptic Curve, then the packet is rejected. Similarly, if the
// derived shared secret has been seen before the packet is rejected.  Finally
// if the MAC doesn't check the packet is again rejected.
//
// In the case of a successful packet processing, and ProcessedPacket struct is
// returned which houses the newly parsed packet, along with instructions on
// what to do next.
func (r *Router) ProcessOnionPacket(onionPkt *OnionPacket) (*ProcessedPacket, error) {
	mixHeader := onionPkt.Header
	onionMsg := onionPkt.Msg

	dhKey := mixHeader.EphemeralKey
	routeInfo := mixHeader.RoutingInfo
	headerMac := mixHeader.HeaderMAC

	// Ensure that the public key is on our curve.
	if !r.onionKey.Curve.IsOnCurve(dhKey.X, dhKey.Y) {
		return nil, fmt.Errorf("pubkey isn't on secp256k1 curve")
	}

	// Compute our shared secret.
	sharedSecret := sha256.Sum256(btcec.GenerateSharedSecret(r.onionKey, dhKey))

	// In order to mitigate replay attacks, if we've seen this particular
	// shared secret before, cease processing and just drop this forwarding
	// message.
	r.RLock()
	if _, ok := r.seenSecrets[sharedSecret]; ok {
		r.RUnlock()
		return nil, ErrReplayedPacket
	}
	r.RUnlock()

	// Using the derived shared secret, ensure the integrity of the routing
	// information by checking the attached MAC without leaking timing
	// information.
	calculatedMac := calcMac(generateKey("mu", sharedSecret), routeInfo[:])
	if !hmac.Equal(headerMac[:], calculatedMac[:]) {
		return nil, fmt.Errorf("MAC mismatch, rejecting forwarding message")
	}

	// The MAC checks out, mark this current shared secret as processed in
	// order to mitigate future replay attacks.
	r.Lock()
	r.seenSecrets[sharedSecret] = struct{}{}
	r.Unlock()

	// Attach the padding zeroes in order to properly strip an encryption
	// layer off the routing info revealing the routing information for the
	// next hop.
	var hopInfo [numStreamBytes]byte
	streamBytes := generateCipherStream(generateKey("rho", sharedSecret), numStreamBytes)
	headerWithPadding := append(routeInfo[:], bytes.Repeat([]byte{0}, 2*securityParameter)...)
	xor(hopInfo[:], headerWithPadding, streamBytes)

	// Are we the final hop? Or should the message be forwarded further?
	switch hopInfo[0] {
	case nullDest: // We're the exit node for a forwarding message.
		onionCore := lionessDecode(generateKey("pi", sharedSecret), onionMsg)
		// TODO(roasbeef): check ver and reject if not our net.
		/*destAddr, _, _ := base58.CheckDecode(string(onionCore[securityParameter : securityParameter*2]))
		if err != nil {
			return nil, err
		}*/
		destAddr := onionCore[securityParameter : securityParameter*2]
		msg := onionCore[securityParameter*2:]
		return &ProcessedPacket{
			Action:   ExitNode,
			DestAddr: destAddr,
			DestMsg:  msg,
		}, nil

	default: // The message is destined for another mix-net node.
		// TODO(roasbeef): prob extract to func

		// Randomize the DH group element for the next hop using the
		// deterministic blinding factor.
		blindingFactor := computeBlindingFactor(dhKey, sharedSecret[:])
		nextDHKey := blindGroupElement(dhKey, blindingFactor[:])

		// Parse out the ID of the next node in the route.
		var nextHop [securityParameter]byte
		copy(nextHop[:], hopInfo[:securityParameter])

		// MAC and MixHeader for the next hop.
		var nextMac [securityParameter]byte
		copy(nextMac[:], hopInfo[securityParameter:securityParameter*2])
		var nextMixHeader [routingInfoSize]byte
		copy(nextMixHeader[:], hopInfo[securityParameter*2:])

		// Strip a single layer of encryption from the onion for the
		// next hop to also process.
		nextOnion := lionessDecode(generateKey("pi", sharedSecret), onionMsg)

		nextFwdMsg := &OnionPacket{
			Header: &MixHeader{
				EphemeralKey: nextDHKey,
				RoutingInfo:  nextMixHeader,
				HeaderMAC:    nextMac,
			},
			Msg: nextOnion,
		}

		return &ProcessedPacket{
			Action:  MoreHops,
			NextHop: nextHop,
			Packet:  nextFwdMsg,
		}, nil
	}
}
