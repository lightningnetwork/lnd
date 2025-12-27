package routerrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestUnmarshallRouteMPPValidation tests that UnmarshallRoute correctly
// enforces the MPP/AMP record validation on incoming routes.
func TestUnmarshallRouteMPPValidation(t *testing.T) {
	t.Parallel()
	backend := &RouterBackend{
		SelfNode: route.Vertex{1, 2, 3},
	}

	// Test case 1: Route with no MPP or AMP record should succeed with
	// a deprecation warning.
	t.Run("no_mpp_or_amp", func(t *testing.T) {
		rpcRoute := &lnrpc.Route{
			TotalTimeLock: 100,
			TotalAmtMsat:  1000,
			Hops: []*lnrpc.Hop{
				{
					PubKey:           "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
					ChanId:           12345,
					AmtToForwardMsat: 1000,
					Expiry:           100,
				},
			},
		}

		// Should succeed with deprecation warning logged.
		_, err := backend.UnmarshallRoute(rpcRoute)
		require.NoError(t, err)
	})

	// Test case 2: Route with MPP record should succeed.
	t.Run("with_mpp", func(t *testing.T) {
		paymentAddr := make([]byte, 32)
		paymentAddr[0] = 1

		rpcRoute := &lnrpc.Route{
			TotalTimeLock: 100,
			TotalAmtMsat:  1000,
			Hops: []*lnrpc.Hop{
				{
					PubKey:           "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
					ChanId:           12345,
					AmtToForwardMsat: 1000,
					Expiry:           100,
					MppRecord: &lnrpc.MPPRecord{
						PaymentAddr:  paymentAddr,
						TotalAmtMsat: 1000,
					},
				},
			},
		}

		_, err := backend.UnmarshallRoute(rpcRoute)
		require.NoError(t, err)
	})

	// Test case 3: Route with AMP record should succeed.
	t.Run("with_amp", func(t *testing.T) {
		rootShare := make([]byte, 32)
		setID := make([]byte, 32)
		rootShare[0] = 1
		setID[0] = 2

		rpcRoute := &lnrpc.Route{
			TotalTimeLock: 100,
			TotalAmtMsat:  1000,
			Hops: []*lnrpc.Hop{
				{
					PubKey:           "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
					ChanId:           12345,
					AmtToForwardMsat: 1000,
					Expiry:           100,
					AmpRecord: &lnrpc.AMPRecord{
						RootShare:  rootShare,
						SetId:      setID,
						ChildIndex: 0,
					},
				},
			},
		}

		_, err := backend.UnmarshallRoute(rpcRoute)
		require.NoError(t, err)
	})

	// Test case 4: Route with both MPP and AMP records should fail.
	t.Run("both_mpp_and_amp", func(t *testing.T) {
		paymentAddr := make([]byte, 32)
		rootShare := make([]byte, 32)
		setID := make([]byte, 32)
		paymentAddr[0] = 1
		rootShare[0] = 1
		setID[0] = 2

		rpcRoute := &lnrpc.Route{
			TotalTimeLock: 100,
			TotalAmtMsat:  1000,
			Hops: []*lnrpc.Hop{
				{
					PubKey:           "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
					ChanId:           12345,
					AmtToForwardMsat: 1000,
					Expiry:           100,
					MppRecord: &lnrpc.MPPRecord{
						PaymentAddr:  paymentAddr,
						TotalAmtMsat: 1000,
					},
					AmpRecord: &lnrpc.AMPRecord{
						RootShare:  rootShare,
						SetId:      setID,
						ChildIndex: 0,
					},
				},
			},
		}

		_, err := backend.UnmarshallRoute(rpcRoute)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot have both MPP and AMP")
	})

	// Test case 5: Blinded route should not require MPP/AMP.
	t.Run("blinded_no_mpp", func(t *testing.T) {
		blindingPoint := []byte{
			0x02, 0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb,
			0xac, 0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b,
			0x07, 0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28,
			0xd9, 0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17,
			0x98,
		}

		rpcRoute := &lnrpc.Route{
			TotalTimeLock: 100,
			TotalAmtMsat:  1000,
			Hops: []*lnrpc.Hop{
				{
					PubKey:           "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
					ChanId:           12345,
					AmtToForwardMsat: 1000,
					Expiry:           100,
					BlindingPoint:    blindingPoint,
					EncryptedData:    []byte{1, 2, 3},
				},
			},
		}

		_, err := backend.UnmarshallRoute(rpcRoute)
		require.NoError(t, err)
	})
}
