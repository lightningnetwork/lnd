package lndc

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/roasbeef/btcd/btcec"
)

func TestConnectionCorrectness(t *testing.T) {
	// First, generate the long-term private keys both ends of the connection
	// within our test.
	localPriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate local priv key: %v", err)
	}
	remotePriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate remote priv key: %v", err)
	}

	// Having a port of "0" means a random port will be chosen for our
	// listener.
	addr := "127.0.0.1:0"

	// Our listener will be local, and the connection remote.
	listener, err := NewListener(localPriv, addr)
	if err != nil {
		t.Fatalf("unable to create listener: %v", err)
	}
	conn := NewConn(nil)

	var wg sync.WaitGroup
	var dialErr error

	// Initiate a connection with a separate goroutine, and listen with our
	// main one. If both errors are nil, then encryption+auth was succesful.
	wg.Add(1)
	go func() {
		dialErr = conn.Dial(remotePriv, listener.Addr().String(),
			localPriv.PubKey().SerializeCompressed())
		wg.Done()
	}()

	localConn, listenErr := listener.Accept()
	if listenErr != nil {
		t.Fatalf("unable to accept connection: %v", listenErr)
	}

	wg.Wait()

	if dialErr != nil {
		t.Fatalf("unable to establish connection: %v", dialErr)
	}

	// Test out some message full-message reads.
	for i := 0; i < 10; i++ {
		msg := []byte("hello" + string(i))

		if _, err := conn.Write(msg); err != nil {
			t.Fatalf("remote conn failed to write: %v", err)
		}

		readBuf := make([]byte, len(msg))
		if _, err := localConn.Read(readBuf); err != nil {
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
	fmt.Println("write")
	if _, err := conn.Write(outMsg); err != nil {
		t.Fatalf("remote conn failed to write: %v", err)
	}

	readBuf := make([]byte, len(outMsg))
	if _, err := localConn.Read(readBuf[:len(outMsg)/2]); err != nil {
		t.Fatalf("local conn failed to read: %v", err)
	}
	if _, err := localConn.Read(readBuf[len(outMsg)/2:]); err != nil {
		t.Fatalf("local conn failed to read: %v", err)
	}

	if !bytes.Equal(outMsg, readBuf) {
		t.Fatalf("messages don't match, %v vs %v",
			string(readBuf), string(outMsg))
	}
}
