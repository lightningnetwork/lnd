package brontide

import (
	"bytes"
	"encoding/hex"
	"io"
	"math"
	"net"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
)

type maybeNetConn struct {
	conn net.Conn
	err  error
}

func makeListener() (*Listener, *lnwire.NetAddress, error) {
	// First, generate the long-term private keys for the brontide listener.
	localPriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	// Having a port of ":0" means a random port, and interface will be
	// chosen for our listener.
	addr := "localhost:0"

	// Our listener will be local, and the connection remote.
	listener, err := NewListener(localPriv, addr)
	if err != nil {
		return nil, nil, err
	}

	netAddr := &lnwire.NetAddress{
		IdentityKey: localPriv.PubKey(),
		Address:     listener.Addr().(*net.TCPAddr),
	}

	return listener, netAddr, nil
}

func establishTestConnection() (net.Conn, net.Conn, func(), error) {
	listener, netAddr, err := makeListener()
	if err != nil {
		return nil, nil, nil, err
	}
	defer listener.Close()

	// Nos, generate the long-term private keys remote end of the connection
	// within our test.
	remotePriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, nil, err
	}

	// Initiate a connection with a separate goroutine, and listen with our
	// main one. If both errors are nil, then encryption+auth was
	// successful.
	remoteConnChan := make(chan maybeNetConn, 1)
	go func() {
		remoteConn, err := Dial(remotePriv, netAddr, net.Dial)
		remoteConnChan <- maybeNetConn{remoteConn, err}
	}()

	localConnChan := make(chan maybeNetConn, 1)
	go func() {
		localConn, err := listener.Accept()
		localConnChan <- maybeNetConn{localConn, err}
	}()

	remote := <-remoteConnChan
	if remote.err != nil {
		return nil, nil, nil, err
	}

	local := <-localConnChan
	if local.err != nil {
		return nil, nil, nil, err
	}

	cleanUp := func() {
		local.conn.Close()
		remote.conn.Close()
	}

	return local.conn, remote.conn, cleanUp, nil
}

func TestConnectionCorrectness(t *testing.T) {
	// Create a test connection, grabbing either side of the connection
	// into local variables. If the initial crypto handshake fails, then
	// we'll get a non-nil error here.
	localConn, remoteConn, cleanUp, err := establishTestConnection()
	if err != nil {
		t.Fatalf("unable to establish test connection: %v", err)
	}
	defer cleanUp()

	// Test out some message full-message reads.
	for i := 0; i < 10; i++ {
		msg := []byte("hello" + string(i))

		if _, err := localConn.Write(msg); err != nil {
			t.Fatalf("remote conn failed to write: %v", err)
		}

		readBuf := make([]byte, len(msg))
		if _, err := remoteConn.Read(readBuf); err != nil {
			t.Fatalf("local conn failed to read: %v", err)
		}

		if !bytes.Equal(readBuf, msg) {
			t.Fatalf("messages don't match, %v vs %v",
				string(readBuf), string(msg))
		}
	}

	// Now try incremental message reads. This simulates first writing a
	// message header, then a message body.
	outMsg := []byte("hello world")
	if _, err := localConn.Write(outMsg); err != nil {
		t.Fatalf("remote conn failed to write: %v", err)
	}

	readBuf := make([]byte, len(outMsg))
	if _, err := remoteConn.Read(readBuf[:len(outMsg)/2]); err != nil {
		t.Fatalf("local conn failed to read: %v", err)
	}
	if _, err := remoteConn.Read(readBuf[len(outMsg)/2:]); err != nil {
		t.Fatalf("local conn failed to read: %v", err)
	}

	if !bytes.Equal(outMsg, readBuf) {
		t.Fatalf("messages don't match, %v vs %v",
			string(readBuf), string(outMsg))
	}
}

// TestConecurrentHandshakes verifies the listener's ability to not be blocked
// by other pending handshakes. This is tested by opening multiple tcp
// connections with the listener, without completing any of the brontide acts.
// The test passes if real brontide dialer connects while the others are
// stalled.
func TestConcurrentHandshakes(t *testing.T) {
	listener, netAddr, err := makeListener()
	if err != nil {
		t.Fatalf("unable to create listener connection: %v", err)
	}
	defer listener.Close()

	const nblocking = 5

	// Open a handful of tcp connections, that do not complete any steps of
	// the brontide handshake.
	connChan := make(chan maybeNetConn)
	for i := 0; i < nblocking; i++ {
		go func() {
			conn, err := net.Dial("tcp", listener.Addr().String())
			connChan <- maybeNetConn{conn, err}
		}()
	}

	// Receive all connections/errors from our blocking tcp dials. We make a
	// pass to gather all connections and errors to make sure we defer the
	// calls to Close() on all successful connections.
	tcpErrs := make([]error, 0, nblocking)
	for i := 0; i < nblocking; i++ {
		result := <-connChan
		if result.conn != nil {
			defer result.conn.Close()
		}
		if result.err != nil {
			tcpErrs = append(tcpErrs, result.err)
		}
	}
	for _, tcpErr := range tcpErrs {
		if tcpErr != nil {
			t.Fatalf("unable to tcp dial listener: %v", tcpErr)
		}
	}

	// Now, construct a new private key and use the brontide dialer to
	// connect to the listener.
	remotePriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate private key: %v", err)
	}

	go func() {
		remoteConn, err := Dial(remotePriv, netAddr, net.Dial)
		connChan <- maybeNetConn{remoteConn, err}
	}()

	// This connection should be accepted without error, as the brontide
	// connection should bypass stalled tcp connections.
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("unable to accept dial: %v", err)
	}
	defer conn.Close()

	result := <-connChan
	if result.err != nil {
		t.Fatalf("unable to dial %v: %v", netAddr, result.err)
	}
	result.conn.Close()
}

func TestMaxPayloadLength(t *testing.T) {
	t.Parallel()

	b := Machine{}
	b.split()

	// Create a payload that's only *slightly* above the maximum allotted
	// payload length.
	payloadToReject := make([]byte, math.MaxUint16+1)

	var buf bytes.Buffer

	// A write of the payload generated above to the state machine should
	// be rejected as it's over the max payload length.
	err := b.WriteMessage(&buf, payloadToReject)
	if err != ErrMaxMessageLengthExceeded {
		t.Fatalf("payload is over the max allowed length, the write " +
			"should have been rejected")
	}

	// Generate another payload which should be accepted as a valid
	// payload.
	payloadToAccept := make([]byte, math.MaxUint16-1)
	if err := b.WriteMessage(&buf, payloadToAccept); err != nil {
		t.Fatalf("write for payload was rejected, should have been " +
			"accepted")
	}

	// Generate a final payload which is only *slightly* above the max payload length
	// when the MAC is accounted for.
	payloadToReject = make([]byte, math.MaxUint16+1)

	// This payload should be rejected.
	err = b.WriteMessage(&buf, payloadToReject)
	if err != ErrMaxMessageLengthExceeded {
		t.Fatalf("payload is over the max allowed length, the write " +
			"should have been rejected")
	}
}

func TestWriteMessageChunking(t *testing.T) {
	// Create a test connection, grabbing either side of the connection
	// into local variables. If the initial crypto handshake fails, then
	// we'll get a non-nil error here.
	localConn, remoteConn, cleanUp, err := establishTestConnection()
	if err != nil {
		t.Fatalf("unable to establish test connection: %v", err)
	}
	defer cleanUp()

	// Attempt to write a message which is over 3x the max allowed payload
	// size.
	largeMessage := bytes.Repeat([]byte("kek"), math.MaxUint16*3)

	// Launch a new goroutine to write the large message generated above in
	// chunks. We spawn a new goroutine because otherwise, we may block as
	// the kernel waits for the buffer to flush.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		bytesWritten, err := localConn.Write(largeMessage)
		if err != nil {
			t.Fatalf("unable to write message: %v", err)
		}

		// The entire message should have been written out to the remote
		// connection.
		if bytesWritten != len(largeMessage) {
			t.Fatalf("bytes not fully written!")
		}

		wg.Done()
	}()

	// Attempt to read the entirety of the message generated above.
	buf := make([]byte, len(largeMessage))
	if _, err := io.ReadFull(remoteConn, buf); err != nil {
		t.Fatalf("unable to read message: %v", err)
	}

	wg.Wait()

	// Finally, the message the remote end of the connection received
	// should be identical to what we sent from the local connection.
	if !bytes.Equal(buf, largeMessage) {
		t.Fatalf("bytes don't match")
	}
}

// TestBolt0008TestVectors ensures that our implementation of brontide exactly
// matches the test vectors within the specification.
func TestBolt0008TestVectors(t *testing.T) {
	t.Parallel()

	// First, we'll generate the state of the initiator from the test
	// vectors at the appendix of BOLT-0008
	initiatorKeyBytes, err := hex.DecodeString("1111111111111111111111" +
		"111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("unable to decode hex: %v", err)
	}
	initiatorPriv, _ := btcec.PrivKeyFromBytes(btcec.S256(),
		initiatorKeyBytes)

	// We'll then do the same for the responder.
	responderKeyBytes, err := hex.DecodeString("212121212121212121212121" +
		"2121212121212121212121212121212121212121")
	if err != nil {
		t.Fatalf("unable to decode hex: %v", err)
	}
	responderPriv, responderPub := btcec.PrivKeyFromBytes(btcec.S256(),
		responderKeyBytes)

	// With the initiator's key data parsed, we'll now define a custom
	// EphemeralGenerator function for the state machine to ensure that the
	// initiator and responder both generate the ephemeral public key
	// defined within the test vectors.
	initiatorEphemeral := EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		e := "121212121212121212121212121212121212121212121212121212" +
			"1212121212"
		eBytes, err := hex.DecodeString(e)
		if err != nil {
			return nil, err
		}

		priv, _ := btcec.PrivKeyFromBytes(btcec.S256(), eBytes)
		return priv, nil
	})
	responderEphemeral := EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		e := "222222222222222222222222222222222222222222222222222" +
			"2222222222222"
		eBytes, err := hex.DecodeString(e)
		if err != nil {
			return nil, err
		}

		priv, _ := btcec.PrivKeyFromBytes(btcec.S256(), eBytes)
		return priv, nil
	})

	// Finally, we'll create both brontide state machines, so we can begin
	// our test.
	initiator := NewBrontideMachine(true, initiatorPriv, responderPub,
		initiatorEphemeral)
	responder := NewBrontideMachine(false, responderPriv, nil,
		responderEphemeral)

	// We'll start with the initiator generating the initial payload for
	// act one. This should consist of exactly 50 bytes. We'll assert that
	// the payload return is _exactly_ the same as what's specified within
	// the test vectors.
	actOne, err := initiator.GenActOne()
	if err != nil {
		t.Fatalf("unable to generate act one: %v", err)
	}
	expectedActOne, err := hex.DecodeString("00036360e856310ce5d294e" +
		"8be33fc807077dc56ac80d95d9cd4ddbd21325eff73f70df608655115" +
		"1f58b8afe6c195782c6a")
	if err != nil {
		t.Fatalf("unable to parse expected act one: %v", err)
	}
	if !bytes.Equal(expectedActOne, actOne[:]) {
		t.Fatalf("act one mismatch: expected %x, got %x",
			expectedActOne, actOne)
	}

	// With the assertion above passed, we'll now process the act one
	// payload with the responder of the crypto handshake.
	if err := responder.RecvActOne(actOne); err != nil {
		t.Fatalf("responder unable to process act one: %v", err)
	}

	// Next, we'll start the second act by having the responder generate
	// its contribution to the crypto handshake. We'll also verify that we
	// produce the _exact_ same byte stream as advertised within the spec's
	// test vectors.
	actTwo, err := responder.GenActTwo()
	if err != nil {
		t.Fatalf("unable to generate act two: %v", err)
	}
	expectedActTwo, err := hex.DecodeString("0002466d7fcae563e5cb09a0" +
		"d1870bb580344804617879a14949cf22285f1bae3f276e2470b93aac58" +
		"3c9ef6eafca3f730ae")
	if err != nil {
		t.Fatalf("unable to parse expected act two: %v", err)
	}
	if !bytes.Equal(expectedActTwo, actTwo[:]) {
		t.Fatalf("act two mismatch: expected %x, got %x",
			expectedActTwo, actTwo)
	}

	// Moving the handshake along, we'll also ensure that the initiator
	// accepts the act two payload.
	if err := initiator.RecvActTwo(actTwo); err != nil {
		t.Fatalf("initiator unable to process act two: %v", err)
	}

	// At the final step, we'll generate the last act from the initiator
	// and once again verify that it properly matches the test vectors.
	actThree, err := initiator.GenActThree()
	if err != nil {
		t.Fatalf("unable to generate act three: %v", err)
	}
	expectedActThree, err := hex.DecodeString("00b9e3a702e93e3a9948c2e" +
		"d6e5fd7590a6e1c3a0344cfc9d5b57357049aa22355361aa02e55a8f" +
		"c28fef5bd6d71ad0c38228dc68b1c466263b47fdf31e560e139ba")
	if err != nil {
		t.Fatalf("unable to parse expected act three: %v", err)
	}
	if !bytes.Equal(expectedActThree, actThree[:]) {
		t.Fatalf("act three mismatch: expected %x, got %x",
			expectedActThree, actThree)
	}

	// Finally, we'll ensure that the responder itself also properly parses
	// the last payload in the crypto handshake.
	if err := responder.RecvActThree(actThree); err != nil {
		t.Fatalf("responder unable to process act three: %v", err)
	}

	// As a final assertion, we'll ensure that both sides have derived the
	// proper symmetric encryption keys.
	sendingKey, err := hex.DecodeString("969ab31b4d288cedf6218839b27a3e2" +
		"140827047f2c0f01bf5c04435d43511a9")
	if err != nil {
		t.Fatalf("unable to parse sending key: %v", err)
	}
	recvKey, err := hex.DecodeString("bb9020b8965f4df047e07f955f3c4b884" +
		"18984aadc5cdb35096b9ea8fa5c3442")
	if err != nil {
		t.Fatalf("unable to parse receiving key: %v", err)
	}

	chainKey, err := hex.DecodeString("919219dbb2920afa8db80f9a51787a840" +
		"bcf111ed8d588caf9ab4be716e42b01")
	if err != nil {
		t.Fatalf("unable to parse chaining key: %v", err)
	}

	if !bytes.Equal(initiator.sendCipher.secretKey[:], sendingKey) {
		t.Fatalf("sending key mismatch: expected %x, got %x",
			initiator.sendCipher.secretKey[:], sendingKey)
	}
	if !bytes.Equal(initiator.recvCipher.secretKey[:], recvKey) {
		t.Fatalf("receiving key mismatch: expected %x, got %x",
			initiator.recvCipher.secretKey[:], recvKey)
	}
	if !bytes.Equal(initiator.chainingKey[:], chainKey) {
		t.Fatalf("chaining key mismatch: expected %x, got %x",
			initiator.chainingKey[:], chainKey)
	}

	if !bytes.Equal(responder.sendCipher.secretKey[:], recvKey) {
		t.Fatalf("sending key mismatch: expected %x, got %x",
			responder.sendCipher.secretKey[:], recvKey)
	}
	if !bytes.Equal(responder.recvCipher.secretKey[:], sendingKey) {
		t.Fatalf("receiving key mismatch: expected %x, got %x",
			responder.recvCipher.secretKey[:], sendingKey)
	}
	if !bytes.Equal(responder.chainingKey[:], chainKey) {
		t.Fatalf("chaining key mismatch: expected %x, got %x",
			responder.chainingKey[:], chainKey)
	}

	// Now test as per section "transport-message test" in Test Vectors
	// (the transportMessageVectors ciphertexts are from this section of BOLT 8);
	// we do slightly greater than 1000 encryption/decryption operations
	// to ensure that the key rotation algorithm is operating as expected.
	// The starting point for enc/decr is already guaranteed correct from the
	// above tests of sendingKey, receivingKey, chainingKey.
	transportMessageVectors := map[int]string{
		0: "cf2b30ddf0cf3f80e7c35a6e6730b59fe802473180f396d88a8fb0db8cb" +
			"cf25d2f214cf9ea1d95",
		1: "72887022101f0b6753e0c7de21657d35a4cb2a1f5cde2650528bbc8f837" +
			"d0f0d7ad833b1a256a1",
		500: "178cb9d7387190fa34db9c2d50027d21793c9bc2d40b1e14dcf30ebeeeb2" +
			"20f48364f7a4c68bf8",
		501: "1b186c57d44eb6de4c057c49940d79bb838a145cb528d6e8fd26dbe50a6" +
			"0ca2c104b56b60e45bd",
		1000: "4a2f3cc3b5e78ddb83dcb426d9863d9d9a723b0337c89dd0b005d89f8d3" +
			"c05c52b76b29b740f09",
		1001: "2ecd8c8a5629d0d02ab457a0fdd0f7b90a192cd46be5ecb6ca570bfc5e2" +
			"68338b1a16cf4ef2d36",
	}

	// Payload for every message is the string "hello".
	payload := []byte("hello")

	var buf bytes.Buffer

	for i := 0; i < 1002; i++ {
		err = initiator.WriteMessage(&buf, payload)
		if err != nil {
			t.Fatalf("could not write message %s", payload)
		}
		if val, ok := transportMessageVectors[i]; ok {
			binaryVal, err := hex.DecodeString(val)
			if err != nil {
				t.Fatalf("Failed to decode hex string %s", val)
			}
			if !bytes.Equal(buf.Bytes(), binaryVal) {
				t.Fatalf("Ciphertext %x was not equal to expected %s",
					buf.String()[:], val)
			}
		}

		// Responder decrypts the bytes, in every iteration, and
		// should always be able to decrypt the same payload message.
		plaintext, err := responder.ReadMessage(&buf)
		if err != nil {
			t.Fatalf("failed to read message in responder: %v", err)
		}

		// Ensure decryption succeeded
		if !bytes.Equal(plaintext, payload) {
			t.Fatalf("Decryption failed to receive plaintext: %s, got %s",
				payload, plaintext)
		}

		// Clear out the buffer for the next iteration
		buf.Reset()
	}
}
