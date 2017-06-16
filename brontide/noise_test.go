package brontide

import (
	"bytes"
	"io"
	"math"
	"net"
	"sync"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
)

func establishTestConnection() (net.Conn, net.Conn, error) {
	// First, generate the long-term private keys both ends of the connection
	// within our test.
	localPriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}
	remotePriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	// Having a port of ":0" means a random port, and interface will be
	// chosen for our listener.
	addr := ":0"

	// Our listener will be local, and the connection remote.
	listener, err := NewListener(localPriv, addr)
	if err != nil {
		return nil, nil, err
	}
	defer listener.Close()

	netAddr := &lnwire.NetAddress{
		IdentityKey: localPriv.PubKey(),
		Address:     listener.Addr().(*net.TCPAddr),
	}

	// Initiate a connection with a separate goroutine, and listen with our
	// main one. If both errors are nil, then encryption+auth was succesful.
	errChan := make(chan error)
	connChan := make(chan net.Conn)
	go func() {
		conn, err := Dial(remotePriv, netAddr)

		errChan <- err
		connChan <- conn
	}()

	localConn, listenErr := listener.Accept()
	if listenErr != nil {
		return nil, nil, listenErr
	}

	if dialErr := <-errChan; dialErr != nil {
		return nil, nil, dialErr
	}
	remoteConn := <-connChan

	return localConn, remoteConn, nil
}

func TestConnectionCorrectness(t *testing.T) {
	t.Parallel()

	// Create a test connection, grabbing either side of the connection
	// into local variables. If the initial crypto handshake fails, then
	// we'll get a non-nil error here.
	localConn, remoteConn, err := establishTestConnection()
	if err != nil {
		t.Fatalf("unable to establish test connection: %v", err)
	}

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

func TestMaxPayloadLength(t *testing.T) {
	t.Parallel()

	b := Machine{}
	b.split()

	// Create a payload that's juust over the maximum alloted payload
	// length.
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

	// Generate a final payload which is juuust over the max payload length
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
	t.Parallel()

	// Create a test connection, grabbing either side of the connection
	// into local variables. If the initial crypto handshake fails, then
	// we'll get a non-nil error here.
	localConn, remoteConn, err := establishTestConnection()
	if err != nil {
		t.Fatalf("unable to establish test connection: %v", err)
	}

	// Attempt to write a message which is over 3x the max allowed payload
	// size.
	largeMessage := bytes.Repeat([]byte("kek"), math.MaxUint16*3)

	// Launch a new goroutine to write the large message generated above in
	// chunks. We spawn a new goroutine because otherwise, we may block as
	// the kernal waits for the buffer to flush.
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
