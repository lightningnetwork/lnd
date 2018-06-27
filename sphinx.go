package sphinx

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math/big"

	"github.com/aead/chacha20"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
)

const (
	// hmacSize is the length of the HMACs used to verify the integrity of
	// the onion. Any value lower than 32 will truncate the HMAC both
	// during onion creation as well as during the verification.
	hmacSize = 32

	// addressSize is the length of the serialized address used to uniquely
	// identify the next hop to forward the onion to. BOLT 04 defines this
	// as 8 byte channel_id.
	addressSize = 8

	// NumMaxHops is the the maximum path length. This should be set to an
	// estiamate of the upper limit of the diameter of the node graph.
	NumMaxHops = 20

	// padSize is the number of padding bytes in the hopData. These bytes
	// are currently unused within the protocol, and are reserved for
	// future use.
	padSize = 12

	// hopDataSize is the fixed size of hop_data. BOLT 04 currently
	// specifies this to be 1 byte realm, 8 byte channel_id, 8 byte amount
	// to forward, 4 byte outgoing CLTV value, 12 bytes padding and 32
	// bytes HMAC for a total of 65 bytes per hop.
	hopDataSize = (1 + addressSize + 8 + 4 + padSize + hmacSize)

	// sharedSecretSize is the size in bytes of the shared secrets.
	sharedSecretSize = 32

	// routingInfoSize is the fixed size of the the routing info. This
	// consists of a addressSize byte address and a hmacSize byte HMAC for
	// each hop of the route, the first pair in cleartext and the following
	// pairs increasingly obfuscated. In case fewer than numMaxHops are
	// used, then the remainder is padded with null-bytes, also obfuscated.
	routingInfoSize = NumMaxHops * hopDataSize

	// numStreamBytes is the number of bytes produced by our CSPRG for the
	// key stream implementing our stream cipher to encrypt/decrypt the mix
	// header. The last hopDataSize bytes are only used in order to
	// generate/check the MAC over the header.
	numStreamBytes = routingInfoSize + hopDataSize

	// keyLen is the length of the keys used to generate cipher streams and
	// encrypt payloads. Since we use SHA256 to generate the keys, the
	// maximum length currently is 32 bytes.
	keyLen = 32

	// baseVersion represent the current supported version of onion packet.
	baseVersion = 0
)

// Hash256 is a statically sized, 32-byte array, typically containing
// the output of a SHA256 hash.
type Hash256 [sha256.Size]byte

// zeroHMAC is the special HMAC value that allows the final node to determine
// if it is the payment destination or not.
var zeroHMAC [hmacSize]byte

// OnionPacket is the onion wrapped hop-to-hop routing information necessary to
// propagate a message through the mix-net without intermediate nodes having
// knowledge of their position within the route, the source, the destination,
// and finally the identities of the past/future nodes in the route. At each
// hop the ephemeral key is used by the node to perform ECDH between itself and
// the source node. This derived secret key is used to check the MAC of the
// entire mix header, decrypt the next set of routing information, and
// re-randomize the ephemeral key for the next node in the path. This per-hop
// re-randomization allows us to only propagate a single group element through
// the onion route.
type OnionPacket struct {
	// Version denotes the version of this onion packet. The version
	// indicates how a receiver of the packet should interpret the bytes
	// following this version byte. Currently, a version of 0x00 is the
	// only defined version type.
	Version byte

	// EphemeralKey is the public key that each hop will used in
	// combination with the private key in an ECDH to derive the shared
	// secret used to check the HMAC on the packet and also decrypted the
	// routing information.
	EphemeralKey *btcec.PublicKey

	// RoutingInfo is the full routing information for this onion packet.
	// This encodes all the forwarding instructions for this current hop
	// and all the hops in the route.
	RoutingInfo [routingInfoSize]byte

	// HeaderMAC is an HMAC computed with the shared secret of the routing
	// data and the associated data for this route. Including the
	// associated data lets each hop authenticate higher-level data that is
	// critical for the forwarding of this HTLC.
	HeaderMAC [hmacSize]byte
}

// HopData is the information destined for individual hops. It is a fixed size
// 64 bytes, prefixed with a 1 byte realm that indicates how to interpret it.
// For now we simply assume it's the bitcoin realm (0x00) and hence the format
// is fixed. The last 32 bytes are always the HMAC to be passed to the next
// hop, or zero if this is the packet is not to be forwarded, since this is the
// last hop.
type HopData struct {
	// Realm denotes the "real" of target chain of the next hop. For
	// bitcoin, this value will be 0x00.
	Realm byte

	// NextAddress is the address of the next hop that this packet should
	// be forward to.
	NextAddress [addressSize]byte

	// ForwardAmount is the HTLC amount that the next hop should forward.
	// This value should take into account the fee require by this
	// particular hop, and the cumulative fee for the entire route.
	ForwardAmount uint64

	// OutgoingCltv is the value of the outgoing absolute time-lock that
	// should be included in the HTLC forwarded.
	OutgoingCltv uint32

	// ExtraBytes is the set of unused bytes within the onion payload. This
	// extra set of bytes can be utilized by higher level applications to
	// package additional data within the per-hop payload, or signal that a
	// portion of the remaining set of hops are to be consumed as Extra
	// Onion Blobs.
	ExtraBytes [padSize]byte

	// HMAC is an HMAC computed over the entire per-hop payload that also
	// includes the higher-level (optional) associated data bytes.
	HMAC [hmacSize]byte
}

// Encode writes the serialized version of the target HopData into the passed
// io.Writer.
func (hd *HopData) Encode(w io.Writer) error {
	if _, err := w.Write([]byte{hd.Realm}); err != nil {
		return err
	}

	if _, err := w.Write(hd.NextAddress[:]); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, hd.ForwardAmount); err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, hd.OutgoingCltv); err != nil {
		return err
	}

	if _, err := w.Write(hd.ExtraBytes[:]); err != nil {
		return err
	}

	if _, err := w.Write(hd.HMAC[:]); err != nil {
		return err
	}

	return nil
}

// Decode deserializes the encoded HopData contained int he passed io.Reader
// instance to the target empty HopData instance.
func (hd *HopData) Decode(r io.Reader) error {
	if _, err := io.ReadFull(r, []byte{hd.Realm}); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, hd.NextAddress[:]); err != nil {
		return err
	}

	if err := binary.Read(r, binary.BigEndian, &hd.ForwardAmount); err != nil {
		return err
	}

	if err := binary.Read(r, binary.BigEndian, &hd.OutgoingCltv); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, hd.ExtraBytes[:]); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, hd.HMAC[:]); err != nil {
		return err
	}

	return nil
}

// generateSharedSecrets by the given nodes pubkeys, generates the shared
// secrets.
func generateSharedSecrets(paymentPath []*btcec.PublicKey,
	sessionKey *btcec.PrivateKey) []Hash256 {
	// Each hop performs ECDH with our ephemeral key pair to arrive at a
	// shared secret. Additionally, each hop randomizes the group element
	// for the next hop by multiplying it by the blinding factor. This way
	// we only need to transmit a single group element, and hops can't link
	// a session back to us if they have several nodes in the path.
	numHops := len(paymentPath)
	hopSharedSecrets := make([]Hash256, numHops)

	// Compute the triplet for the first hop outside of the main loop.
	// Within the loop each new triplet will be computed recursively based
	// off of the blinding factor of the last hop.
	lastEphemeralPubKey := sessionKey.PubKey()
	hopSharedSecrets[0] = generateSharedSecret(paymentPath[0], sessionKey)
	lastBlindingFactor := computeBlindingFactor(lastEphemeralPubKey, hopSharedSecrets[0][:])

	// The cached blinding factor will contain the running product of the
	// session private key x and blinding factors b_i, computed as
	//   c_0 = x
	//   c_i = c_{i-1} * b_{i-1} 		 (mod |F(G)|).
	//       = x * b_0 * b_1 * ... * b_{i-1} (mod |F(G)|).
	//
	// We begin with just the session private key x, so that base case
	// c_0 = x. At the beginning of each iteration, the previous blinding
	// factor is aggregated into the modular product, and used as the scalar
	// value in deriving the hop ephemeral keys and shared secrets.
	var cachedBlindingFactor big.Int
	cachedBlindingFactor.SetBytes(sessionKey.D.Bytes())

	// Now recursively compute the cached blinding factor, ephemeral ECDH
	// pub keys, and shared secret for each hop.
	var nextBlindingFactor big.Int
	for i := 1; i <= numHops-1; i++ {
		// Update the cached blinding factor with b_{i-1}.
		nextBlindingFactor.SetBytes(lastBlindingFactor[:])
		cachedBlindingFactor.Mul(&cachedBlindingFactor, &nextBlindingFactor)
		cachedBlindingFactor.Mod(&cachedBlindingFactor, btcec.S256().Params().N)

		// a_i = g ^ c_i
		//     = g^( x * b_0 * ... * b_{i-1} )
		//     = X^( b_0 * ... * b_{i-1} )
		// X_our_session_pub_key x all prev blinding factors
		lastEphemeralPubKey = blindBaseElement(cachedBlindingFactor.Bytes())

		// e_i = Y_i ^ c_i
		//     = ( Y_i ^ x )^( b_0 * ... * b_{i-1} )
		// (Y_their_pub_key x x_our_priv) x all prev blinding factors
		hopBlindedPubKey := blindGroupElement(
			paymentPath[i], cachedBlindingFactor.Bytes())

		// s_i = sha256( e_i )
		//     = sha256( Y_i ^ (x * b_0 * ... * b_{i-1} )
		hopSharedSecrets[i] = sha256.Sum256(hopBlindedPubKey.SerializeCompressed())

		// Only need to evaluate up to the penultimate blinding factor.
		if i >= numHops-1 {
			break
		}

		// b_i = sha256( a_i || s_i )
		lastBlindingFactor = computeBlindingFactor(lastEphemeralPubKey,
			hopSharedSecrets[i][:])
	}

	return hopSharedSecrets
}

// NewOnionPacket creates a new onion packet which is capable of
// obliviously routing a message through the mix-net path outline by
// 'paymentPath'.
func NewOnionPacket(paymentPath []*btcec.PublicKey, sessionKey *btcec.PrivateKey,
	hopsData []HopData, assocData []byte) (*OnionPacket, error) {

	numHops := len(paymentPath)
	hopSharedSecrets := generateSharedSecrets(paymentPath, sessionKey)

	// Generate the padding, called "filler strings" in the paper.
	filler := generateHeaderPadding("rho", numHops, hopDataSize,
		hopSharedSecrets)

	// Allocate zero'd out byte slices to store the final mix header packet
	// and the hmac for each hop.
	var (
		mixHeader  [routingInfoSize]byte
		nextHmac   [hmacSize]byte
		hopDataBuf bytes.Buffer
	)

	// Now we compute the routing information for each hop, along with a
	// MAC of the routing info using the shared key for that hop.
	for i := numHops - 1; i >= 0; i-- {
		// We'll derive the two keys we need for each hop in order to:
		// generate our stream cipher bytes for the mixHeader, and
		// calculate the MAC over the entire constructed packet.
		rhoKey := generateKey("rho", &hopSharedSecrets[i])
		muKey := generateKey("mu", &hopSharedSecrets[i])

		// The HMAC for the final hop is simply zeroes. This allows the
		// last hop to recognize that it is the destination for a
		// particular payment.
		hopsData[i].HMAC = nextHmac

		// Next, using the key dedicated for our stream cipher, we'll
		// generate enough bytes to obfuscate this layer of the onion
		// packet.
		streamBytes := generateCipherStream(rhoKey, numStreamBytes)

		// Before we assemble the packet, we'll shift the current
		// mix-header to the write in order to make room for this next
		// per-hop data.
		rightShift(mixHeader[:], hopDataSize)

		// With the mix header right-shifted, we'll encode the current
		// hop data into a buffer we'll re-use during the packet
		// construction.
		if err := hopsData[i].Encode(&hopDataBuf); err != nil {
			return nil, err
		}
		copy(mixHeader[:], hopDataBuf.Bytes())

		// Once the packet for this hop has been assembled, we'll
		// re-encrypt the packet by XOR'ing with a stream of bytes
		// generated using our shared secret.
		xor(mixHeader[:], mixHeader[:], streamBytes[:routingInfoSize])

		// If this is the "last" hop, then we'll override the tail of
		// the hop data.
		if i == numHops-1 {
			copy(mixHeader[len(mixHeader)-len(filler):], filler)
		}

		// The packet for this hop consists of: mixHeader. When
		// calculating the MAC, we'll also include the optional
		// associated data which can allow higher level applications to
		// prevent replay attacks.
		packet := append(mixHeader[:], assocData...)
		nextHmac = calcMac(muKey, packet)

		hopDataBuf.Reset()
	}

	return &OnionPacket{
		Version:      baseVersion,
		EphemeralKey: sessionKey.PubKey(),
		RoutingInfo:  mixHeader,
		HeaderMAC:    nextHmac,
	}, nil
}

// Shift the byte-slice by the given number of bytes to the right and 0-fill
// the resulting gap.
func rightShift(slice []byte, num int) {
	for i := len(slice) - num - 1; i >= 0; i-- {
		slice[num+i] = slice[i]
	}

	for i := 0; i < num; i++ {
		slice[i] = 0
	}
}

// generateHeaderPadding derives the bytes for padding the mix header to ensure
// it remains fixed sized throughout route transit. At each step, we add
// 'hopSize' padding of zeroes, concatenate it to the previous filler, then
// decrypt it (XOR) with the secret key of the current hop. When encrypting the
// mix header we essentially do the reverse of this operation: we "encrypt" the
// padding, and drop 'hopSize' number of zeroes. As nodes process the mix
// header they add the padding ('hopSize') in order to check the MAC and
// decrypt the next routing information eventually leaving only the original
// "filler" bytes produced by this function at the last hop. Using this
// methodology, the size of the field stays constant at each hop.
func generateHeaderPadding(key string, numHops int, hopSize int,
	sharedSecrets []Hash256) []byte {

	filler := make([]byte, (numHops-1)*hopSize)
	for i := 1; i < numHops; i++ {
		totalFillerSize := ((NumMaxHops - i) + 1) * hopSize

		streamKey := generateKey(key, &sharedSecrets[i-1])
		streamBytes := generateCipherStream(streamKey, numStreamBytes)

		xor(filler, filler, streamBytes[totalFillerSize:totalFillerSize+i*hopSize])
	}
	return filler
}

// Encode serializes the raw bytes of the onion packet into the passed
// io.Writer. The form encoded within the passed io.Writer is suitable for
// either storing on disk, or sending over the network.
func (f *OnionPacket) Encode(w io.Writer) error {
	ephemeral := f.EphemeralKey.SerializeCompressed()

	if _, err := w.Write([]byte{f.Version}); err != nil {
		return err
	}

	if _, err := w.Write(ephemeral); err != nil {
		return err
	}

	if _, err := w.Write(f.RoutingInfo[:]); err != nil {
		return err
	}

	if _, err := w.Write(f.HeaderMAC[:]); err != nil {
		return err
	}

	return nil
}

// Decode fully populates the target ForwardingMessage from the raw bytes
// encoded within the io.Reader. In the case of any decoding errors, an error
// will be returned. If the method success, then the new OnionPacket is ready
// to be processed by an instance of SphinxNode.
func (f *OnionPacket) Decode(r io.Reader) error {
	var err error

	var buf [1]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err
	}
	f.Version = buf[0]

	// If version of the onion packet protocol unknown for us than in might
	// lead to improperly decoded data.
	if f.Version != baseVersion {
		return ErrInvalidOnionVersion
	}

	var ephemeral [33]byte
	if _, err := io.ReadFull(r, ephemeral[:]); err != nil {
		return err
	}
	f.EphemeralKey, err = btcec.ParsePubKey(ephemeral[:], btcec.S256())
	if err != nil {
		return ErrInvalidOnionKey
	}

	if _, err := io.ReadFull(r, f.RoutingInfo[:]); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, f.HeaderMAC[:]); err != nil {
		return err
	}

	return nil
}

// calcMac calculates HMAC-SHA-256 over the message using the passed secret key
// as input to the HMAC.
func calcMac(key [keyLen]byte, msg []byte) [hmacSize]byte {
	hmac := hmac.New(sha256.New, key[:])
	hmac.Write(msg)
	h := hmac.Sum(nil)

	var mac [hmacSize]byte
	copy(mac[:], h[:hmacSize])

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
func generateKey(keyType string, sharedKey *Hash256) [keyLen]byte {
	mac := hmac.New(sha256.New, []byte(keyType))
	mac.Write(sharedKey[:])
	h := mac.Sum(nil)

	var key [keyLen]byte
	copy(key[:], h[:keyLen])

	return key
}

// generateCipherStream generates a stream of cryptographic psuedo-random bytes
// intended to be used to encrypt a message using a one-time-pad like
// construction.
func generateCipherStream(key [keyLen]byte, numBytes uint) []byte {
	var (
		nonce [8]byte
	)
	cipher, err := chacha20.NewCipher(nonce[:], key[:])
	if err != nil {
		panic(err)
	}
	output := make([]byte, numBytes)
	cipher.XORKeyStream(output, output)

	return output
}

// computeBlindingFactor for the next hop given the ephemeral pubKey and
// sharedSecret for this hop. The blinding factor is computed as the
// sha-256(pubkey || sharedSecret).
func computeBlindingFactor(hopPubKey *btcec.PublicKey,
	hopSharedSecret []byte) Hash256 {

	sha := sha256.New()
	sha.Write(hopPubKey.SerializeCompressed())
	sha.Write(hopSharedSecret)

	var hash Hash256
	copy(hash[:], sha.Sum(nil))
	return hash
}

// blindGroupElement blinds the group element P by performing scalar
// multiplication of the group element by blindingFactor: blindingFactor * P.
func blindGroupElement(hopPubKey *btcec.PublicKey, blindingFactor []byte) *btcec.PublicKey {
	newX, newY := btcec.S256().ScalarMult(hopPubKey.X, hopPubKey.Y, blindingFactor[:])
	return &btcec.PublicKey{btcec.S256(), newX, newY}
}

// blindBaseElement blinds the groups's generator G by performing scalar base
// multiplication using the blindingFactor: blindingFactor * G.
func blindBaseElement(blindingFactor []byte) *btcec.PublicKey {
	newX, newY := btcec.S256().ScalarBaseMult(blindingFactor)
	return &btcec.PublicKey{btcec.S256(), newX, newY}
}

// generateSharedSecret generates the shared secret for a particular hop. The
// shared secret is generated by taking the group element contained in the
// mix-header, and performing an ECDH operation with the node's long term onion
// key. We then take the _entire_ point generated by the ECDH operation,
// serialize that using a compressed format, then feed the raw bytes through a
// single SHA256 invocation.  The resulting value is the shared secret.
func generateSharedSecret(pub *btcec.PublicKey, priv *btcec.PrivateKey) Hash256 {
	s := &btcec.PublicKey{}
	x, y := btcec.S256().ScalarMult(pub.X, pub.Y, priv.D.Bytes())
	s.X = x
	s.Y = y

	return sha256.Sum256(s.SerializeCompressed())
}

// ProcessCode is an enum-like type which describes to the high-level package
// user which action should be taken after processing a Sphinx packet.
type ProcessCode int

const (
	// ExitNode indicates that the node which processed the Sphinx packet
	// is the destination hop in the route.
	ExitNode = iota

	// MoreHops indicates that there are additional hops left within the
	// route. Therefore the caller should forward the packet to the node
	// denoted as the "NextHop".
	MoreHops

	// Failure indicates that a failure occurred during packet processing.
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
// should be taken after processing.
type ProcessedPacket struct {
	// Action represents the action the caller should take after processing
	// the packet.
	Action ProcessCode

	// ForwardingInstructions is the per-hop payload recovered from the
	// initial encrypted onion packet. It details how the packet should be
	// forwarded and also includes information that allows the processor of
	// the packet to authenticate the information passed within the HTLC.
	//
	// NOTE: This field will only be populated iff the above Action is
	// MoreHops.
	ForwardingInstructions HopData

	// NextPacket is the onion packet that should be forwarded to the next
	// hop as denoted by the ForwardingInstructions field.
	//
	// NOTE: This field will only be populated iff the above Action is
	// MoreHops.
	NextPacket *OnionPacket
}

// Router is an onion router within the Sphinx network. The router is capable
// of processing incoming Sphinx onion packets thereby "peeling" a layer off
// the onion encryption which the packet is wrapped with.
type Router struct {
	nodeID   [addressSize]byte
	nodeAddr *btcutil.AddressPubKeyHash

	onionKey *btcec.PrivateKey

	log ReplayLog
}

// NewRouter creates a new instance of a Sphinx onion Router given the node's
// currently advertised onion private key, and the target Bitcoin network.
func NewRouter(nodeKey *btcec.PrivateKey, net *chaincfg.Params, log ReplayLog,
) *Router {
	var nodeID [addressSize]byte
	copy(nodeID[:], btcutil.Hash160(nodeKey.PubKey().SerializeCompressed()))

	// Safe to ignore the error here, nodeID is 20 bytes.
	nodeAddr, _ := btcutil.NewAddressPubKeyHash(nodeID[:], net)

	return &Router{
		nodeID:   nodeID,
		nodeAddr: nodeAddr,
		onionKey: &btcec.PrivateKey{
			PublicKey: ecdsa.PublicKey{
				Curve: btcec.S256(),
				X:     nodeKey.X,
				Y:     nodeKey.Y,
			},
			D: nodeKey.D,
		},
		log: log,
	}
}

// Start starts / opens the ReplayLog's channeldb and its accompanying
// garbage collector goroutine.
func (r *Router) Start() error {
	return r.log.Start()
}

// Stop stops / closes the ReplayLog's channeldb and its accompanying
// garbage collector goroutine.
func (r *Router) Stop() {
	r.log.Stop()
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
func (r *Router) ProcessOnionPacket(onionPkt *OnionPacket,
	assocData []byte, incomingCltv uint32) (*ProcessedPacket, error) {

	// Compute the shared secret for this onion packet.
	sharedSecret, err := r.generateSharedSecret(onionPkt.EphemeralKey)
	if err != nil {
		return nil, err
	}

	// Additionally, compute the hash prefix of the shared secret, which
	// will serve as an identifier for detecting replayed packets.
	hashPrefix := hashSharedSecret(&sharedSecret)

	// Continue to optimistically process this packet, deferring replay
	// protection until the end to reduce the penalty of multiple IO
	// operations.
	packet, err := processOnionPacket(onionPkt, &sharedSecret, assocData)
	if err != nil {
		return nil, err
	}

	// Atomically compare this hash prefix with the contents of the on-disk
	// log, persisting it only if this entry was not detected as a replay.
	if err := r.log.Put(hashPrefix, incomingCltv); err != nil {
		return nil, err
	}

	return packet, nil
}

// ReconstructOnionPacket rederives the subsequent onion packet.
// NOTE: This method does not do any sort of replay protection, and should only
// be used to reconstruct packets that were successfully processed previously.
func (r *Router) ReconstructOnionPacket(onionPkt *OnionPacket,
	assocData []byte) (*ProcessedPacket, error) {

	// Compute the shared secret for this onion packet.
	sharedSecret, err := r.generateSharedSecret(onionPkt.EphemeralKey)
	if err != nil {
		return nil, err
	}

	return processOnionPacket(onionPkt, &sharedSecret, assocData)
}

// processOnionPacket performs the primary key derivation and handling of onion
// packets. The processed packets returned from this method should only be used
// if the packet was not flagged as a replayed packet.
func processOnionPacket(onionPkt *OnionPacket,
	sharedSecret *Hash256, assocData []byte) (*ProcessedPacket, error) {

	dhKey := onionPkt.EphemeralKey
	routeInfo := onionPkt.RoutingInfo
	headerMac := onionPkt.HeaderMAC

	// Using the derived shared secret, ensure the integrity of the routing
	// information by checking the attached MAC without leaking timing
	// information.
	message := append(routeInfo[:], assocData...)
	calculatedMac := calcMac(generateKey("mu", sharedSecret), message)
	if !hmac.Equal(headerMac[:], calculatedMac[:]) {
		return nil, ErrInvalidOnionHMAC
	}

	// Attach the padding zeroes in order to properly strip an encryption
	// layer off the routing info revealing the routing information for the
	// next hop.
	streamBytes := generateCipherStream(
		generateKey("rho", sharedSecret),
		numStreamBytes,
	)
	headerWithPadding := append(routeInfo[:],
		bytes.Repeat([]byte{0}, hopDataSize)...)

	var hopInfo [numStreamBytes]byte
	xor(hopInfo[:], headerWithPadding, streamBytes)

	// Randomize the DH group element for the next hop using the
	// deterministic blinding factor.
	blindingFactor := computeBlindingFactor(dhKey, sharedSecret[:])
	nextDHKey := blindGroupElement(dhKey, blindingFactor[:])

	// With the MAC checked, and the payload decrypted, we can now parse
	// out the per-hop data so we can derive the specified forwarding
	// instructions.
	var hopData HopData
	if err := hopData.Decode(bytes.NewReader(hopInfo[:])); err != nil {
		return nil, err
	}

	// With the necessary items extracted, we'll copy of the onion packet
	// for the next node, snipping off our per-hop data.
	var nextMixHeader [routingInfoSize]byte
	copy(nextMixHeader[:], hopInfo[hopDataSize:])
	nextFwdMsg := &OnionPacket{
		Version:      onionPkt.Version,
		EphemeralKey: nextDHKey,
		RoutingInfo:  nextMixHeader,
		HeaderMAC:    hopData.HMAC,
	}

	// By default we'll assume that there are additional hops in the route.
	// However if the uncovered 'nextMac' is all zeroes, then this
	// indicates that we're the final hop in the route.
	var action ProcessCode = MoreHops
	if bytes.Compare(zeroHMAC[:], hopData.HMAC[:]) == 0 {
		action = ExitNode
	}

	return &ProcessedPacket{
		Action:                 action,
		ForwardingInstructions: hopData,
		NextPacket:             nextFwdMsg,
	}, nil
}

// generateSharedSecret generates the shared secret by given ephemeral key.
func (r *Router) generateSharedSecret(dhKey *btcec.PublicKey) (Hash256,
	error) {
	var sharedSecret Hash256

	// Ensure that the public key is on our curve.
	if !btcec.S256().IsOnCurve(dhKey.X, dhKey.Y) {
		return sharedSecret, ErrInvalidOnionKey
	}

	// Compute our shared secret.
	sharedSecret = generateSharedSecret(dhKey, r.onionKey)
	return sharedSecret, nil
}

// Tx is a transaction consisting of a number of sphinx packets to be atomically
// written to the replay log. This structure helps to coordinate construction of
// the underlying Batch object, and to ensure that the result of the processing
// is idempotent.
type Tx struct {
	// batch is the set of packets to be incrementally processed and
	// ultimately committed in this transaction
	batch *Batch

	// router is a reference to the sphinx router that created this
	// transaction. Committing this transaction will utilize this router's
	// replay log.
	router *Router

	// packets contains a potentially sparse list of optimistically processed
	// packets for this batch. The contents of a particular index should
	// only be accessed if the index is *not* included in the replay set, or
	// otherwise failed any other stage of the processing.
	packets []ProcessedPacket
}

// BeginTxn creates a new transaction that can later be committed back to the
// sphinx router's replay log.
//
// NOTE: The nels parameter should represent the maximum number of that could be
// added to the batch, using sequence numbers that match or exceed this value
// could result in an out-of-bounds panic.
func (r *Router) BeginTxn(id []byte, nels int) *Tx {
	return &Tx{
		batch:   NewBatch(id),
		router:  r,
		packets: make([]ProcessedPacket, nels),
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
func (t *Tx) ProcessOnionPacket(seqNum uint16, onionPkt *OnionPacket,
	assocData []byte, incomingCltv uint32) error {

	// Compute the shared secret for this onion packet.
	sharedSecret, err := t.router.generateSharedSecret(
		onionPkt.EphemeralKey)
	if err != nil {
		return err
	}

	// Additionally, compute the hash prefix of the shared secret, which
	// will serve as an identifier for detecting replayed packets.
	hashPrefix := hashSharedSecret(&sharedSecret)

	// Continue to optimistically process this packet, deferring replay
	// protection until the end to reduce the penalty of multiple IO
	// operations.
	packet, err := processOnionPacket(onionPkt, &sharedSecret, assocData)
	if err != nil {
		return err
	}

	// Add the hash prefix to pending batch of shared secrets that will be
	// written later via Commit().
	err = t.batch.Put(seqNum, hashPrefix, incomingCltv)
	if err != nil {
		return err
	}

	// If we successfully added this packet to the batch, cache the processed
	// packet within the Tx which can be accessed after committing if this
	// sequence number does not appear in the replay set.
	t.packets[seqNum] = *packet

	return nil
}

// Commit writes this transaction's batch of sphinx packets to the replay log,
// performing a final check against the log for replays.
func (t *Tx) Commit() ([]ProcessedPacket, *ReplaySet, error) {
	if t.batch.IsCommitted {
		return t.packets, t.batch.ReplaySet, nil
	}

	rs, err := t.router.log.PutBatch(t.batch)

	return t.packets, rs, err
}
