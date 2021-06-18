package itest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testNetworkConnectionTimeout checks that the connectiontimeout is taking
// effect. It creates a node with a small connection timeout value, and connects
// it to a non-routable IP address.
func testNetworkConnectionTimeout(net *lntest.NetworkHarness, t *harnessTest) {
	var (
		ctxt, _ = context.WithTimeout(
			context.Background(), defaultTimeout,
		)
		// testPub is a random public key for testing only.
		testPub = "0332bda7da70fefe4b6ab92f53b3c4f4ee7999" +
			"f312284a8e89c8670bb3f67dbee2"
		// testHost is a non-routable IP address. It's used to cause a
		// connection timeout.
		testHost = "10.255.255.255"
	)

	// First, test the global timeout settings.
	// Create Carol with a connection timeout of 1 millisecond.
	carol := net.NewNode(t.t, "Carol", []string{"--connectiontimeout=1ms"})
	defer shutdownAndAssert(net, t, carol)

	// Try to connect Carol to a non-routable IP address, which should give
	// us a timeout error.
	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: testPub,
			Host:   testHost,
		},
	}
	assertTimeoutError(ctxt, t, carol, req)

	// Second, test timeout on the connect peer request.
	// Create Dave with the default timeout setting.
	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// Try to connect Dave to a non-routable IP address, using a timeout
	// value of 1ms, which should give us a timeout error immediately.
	req = &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: testPub,
			Host:   testHost,
		},
		Timeout: 1,
	}
	assertTimeoutError(ctxt, t, dave, req)
}

// assertTimeoutError asserts that a connection timeout error is raised. A
// context with a default timeout is used to make the request. If our customized
// connection timeout is less than the default, we won't see the request context
// times out, instead a network connection timeout will be returned.
func assertTimeoutError(ctxt context.Context, t *harnessTest,
	node *lntest.HarnessNode, req *lnrpc.ConnectPeerRequest) {

	t.t.Helper()

	// Create a context with a timeout value.
	ctxt, cancel := context.WithTimeout(ctxt, defaultTimeout)
	defer cancel()

	err := connect(ctxt, node, req)

	// a DeadlineExceeded error will appear in the context if the above
	// ctxtTimeout value is reached.
	require.NoError(t.t, ctxt.Err(), "context time out")

	// Check that the network returns a timeout error.
	require.Containsf(
		t.t, err.Error(), "i/o timeout",
		"expected to get a timeout error, instead got: %v", err,
	)
}

func connect(ctxt context.Context, node *lntest.HarnessNode,
	req *lnrpc.ConnectPeerRequest) error {

	syncTimeout := time.After(15 * time.Second)
	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_, err := node.ConnectPeer(ctxt, req)
			// If there's no error, return nil
			if err == nil {
				return err
			}
			// If the error is no ErrServerNotActive, return it.
			// Otherwise, we will retry until timeout.
			if !strings.Contains(err.Error(),
				lnd.ErrServerNotActive.Error()) {

				return err
			}
		case <-syncTimeout:
			return fmt.Errorf("chain backend did not " +
				"finish syncing")
		}
	}
	return nil
}
