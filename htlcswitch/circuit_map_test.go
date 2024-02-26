package htlcswitch_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("closed-chan-bucket")
)

// TestCircuitMapCleanClosedChannels checks that the circuits and keystones are
// deleted for closed channels upon restart.
func TestCircuitMapCleanClosedChannels(t *testing.T) {
	t.Parallel()

	var (
		// chanID0 is a zero value channel ID indicating a locally
		// initiated payment.
		chanID0 = lnwire.NewShortChanIDFromInt(uint64(0))
		chanID1 = lnwire.NewShortChanIDFromInt(uint64(1))
		chanID2 = lnwire.NewShortChanIDFromInt(uint64(2))

		inKey00  = htlcswitch.CircuitKey{ChanID: chanID0, HtlcID: 0}
		inKey10  = htlcswitch.CircuitKey{ChanID: chanID1, HtlcID: 0}
		inKey11  = htlcswitch.CircuitKey{ChanID: chanID1, HtlcID: 1}
		inKey20  = htlcswitch.CircuitKey{ChanID: chanID2, HtlcID: 0}
		inKey21  = htlcswitch.CircuitKey{ChanID: chanID2, HtlcID: 1}
		inKey22  = htlcswitch.CircuitKey{ChanID: chanID2, HtlcID: 2}
		outKey00 = htlcswitch.CircuitKey{ChanID: chanID0, HtlcID: 0}
		outKey10 = htlcswitch.CircuitKey{ChanID: chanID1, HtlcID: 0}
		outKey11 = htlcswitch.CircuitKey{ChanID: chanID1, HtlcID: 1}
		outKey20 = htlcswitch.CircuitKey{ChanID: chanID2, HtlcID: 0}
		outKey21 = htlcswitch.CircuitKey{ChanID: chanID2, HtlcID: 1}
		outKey22 = htlcswitch.CircuitKey{ChanID: chanID2, HtlcID: 2}
	)

	type closeChannelParams struct {
		chanID    lnwire.ShortChannelID
		isPending bool
	}

	testParams := []struct {
		name string

		// keystones is used to create and open circuits. A keystone is
		// a pair of circuit keys, inKey and outKey, with the outKey
		// optionally being empty. If a keystone with an outKey is used,
		// a circuit will be created and opened, thus creating a circuit
		// and a keystone in the DB. Otherwise, only the circuit is
		// created.
		keystones []htlcswitch.Keystone

		chanParams []closeChannelParams
		deleted    []htlcswitch.Keystone
		untouched  []htlcswitch.Keystone

		// If resMsg is true, then closed channels will not delete
		// circuits if the channel was the keystone / outgoing key in
		// the open circuit.
		resMsg bool
	}{
		{
			name: "no deletion if there are no closed channels",
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey10, OutKey: outKey10},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey10, OutKey: outKey10},
			},
		},
		{
			name: "no deletion if channel is pending close",
			chanParams: []closeChannelParams{
				// Creates a pending close channel.
				{chanID: chanID1, isPending: true},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey10, OutKey: outKey10},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey10, OutKey: outKey10},
			},
		},
		{
			name: "no deletion if the chanID is zero value",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID0.
				{chanID: chanID0, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey00, OutKey: outKey00},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey00, OutKey: outKey00},
			},
		},
		{
			name: "delete half circuits on inKey match",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID1.
				{chanID: chanID1, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit, no keystone created
				{InKey: inKey10},
				// Creates a circuit, no keystone created
				{InKey: inKey11},
				// Creates a circuit and a keystone
				{InKey: inKey20, OutKey: outKey20},
			},
			deleted: []htlcswitch.Keystone{
				{InKey: inKey10}, {InKey: inKey11},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey20, OutKey: outKey20},
			},
		},
		{
			name: "delete half circuits on outKey match",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID1.
				{chanID: chanID1, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey20, OutKey: outKey10},
				// Creates a circuit and a keystone
				{InKey: inKey21, OutKey: outKey11},
				// Creates a circuit and a keystone
				{InKey: inKey22, OutKey: outKey21},
			},
			deleted: []htlcswitch.Keystone{
				{InKey: inKey20, OutKey: outKey10},
				{InKey: inKey21, OutKey: outKey11},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey22, OutKey: outKey21},
			},
		},
		{
			name: "delete full circuits on inKey match",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID1.
				{chanID: chanID1, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey10, OutKey: outKey20},
				// Creates a circuit and a keystone
				{InKey: inKey11, OutKey: outKey21},
				// Creates a circuit and a keystone
				{InKey: inKey20, OutKey: outKey22},
			},
			deleted: []htlcswitch.Keystone{
				{InKey: inKey10, OutKey: outKey20},
				{InKey: inKey11, OutKey: outKey21},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey20, OutKey: outKey22},
			},
		},
		{
			name: "delete full circuits on outKey match",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID1.
				{chanID: chanID1, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey20, OutKey: outKey10},
				// Creates a circuit and a keystone
				{InKey: inKey21, OutKey: outKey11},
				// Creates a circuit and a keystone
				{InKey: inKey22, OutKey: outKey20},
			},
			deleted: []htlcswitch.Keystone{
				{InKey: inKey20, OutKey: outKey10},
				{InKey: inKey21, OutKey: outKey11},
			},
			untouched: []htlcswitch.Keystone{
				{InKey: inKey22, OutKey: outKey20},
			},
		},
		{
			name: "delete all circuits",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID1.
				{chanID: chanID1, isPending: false},
				// Creates a close channel with chanID2.
				{chanID: chanID2, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey20, OutKey: outKey10},
				// Creates a circuit and a keystone
				{InKey: inKey21, OutKey: outKey11},
				// Creates a circuit and a keystone
				{InKey: inKey22, OutKey: outKey20},
			},
			deleted: []htlcswitch.Keystone{
				{InKey: inKey20, OutKey: outKey10},
				{InKey: inKey21, OutKey: outKey11},
				{InKey: inKey22, OutKey: outKey20},
			},
		},
		{
			name: "don't delete circuits for outgoing",
			chanParams: []closeChannelParams{
				// Creates a close channel with chanID1.
				{chanID: chanID1, isPending: false},
			},
			keystones: []htlcswitch.Keystone{
				// Creates a circuit and a keystone
				{InKey: inKey10, OutKey: outKey10},
				// Creates a circuit and a keystone
				{InKey: inKey11, OutKey: outKey20},
				// Creates a circuit and a keystone
				{InKey: inKey00, OutKey: outKey11},
			},
			deleted: []htlcswitch.Keystone{
				{InKey: inKey10, OutKey: outKey10},
				{InKey: inKey11, OutKey: outKey20},
			},
			resMsg: true,
		},
	}

	for _, tt := range testParams {
		test := tt

		t.Run(test.name, func(t *testing.T) {
			cfg, circuitMap := newCircuitMap(t, test.resMsg)

			// create test circuits
			for _, ks := range test.keystones {
				err := createTestCircuit(ks, circuitMap)
				require.NoError(
					t, err,
					"failed to create test circuit",
				)
			}

			// create close channels
			err := kvdb.Update(cfg.DB, func(tx kvdb.RwTx) error {
				for _, channel := range test.chanParams {
					if err := createTestCloseChannelSummery(
						tx, channel.isPending,
						channel.chanID,
					); err != nil {
						return err
					}
				}
				return nil
			}, func() {})

			require.NoError(
				t, err,
				"failed to create close channel summery",
			)

			// Now, restart the circuit map, and check that the
			// circuits and keystones of closed channels are
			// deleted in DB.
			_, circuitMap = restartCircuitMap(t, cfg)

			// Check that items are deleted. LookupCircuit and
			// LookupOpenCircuit will check the cached circuits,
			// which are loaded on restart from the DB.
			for _, ks := range test.deleted {
				assertKeystoneDeleted(t, circuitMap, ks)
			}

			// We also check we are not deleting wanted circuits.
			for _, ks := range test.untouched {
				assertKeystoneNotDeleted(t, circuitMap, ks)
			}
		})
	}
}

// createTestCircuit creates a circuit for testing with its incoming key being
// the keystone's InKey. If the keystone has an OutKey, the circuit will be
// opened, which causes a Keystone to be created in DB.
func createTestCircuit(ks htlcswitch.Keystone, cm htlcswitch.CircuitMap) error {
	circuit := &htlcswitch.PaymentCircuit{
		Incoming:       ks.InKey,
		ErrorEncrypter: testExtracter,
	}

	// First we will try to add an new circuit to the circuit map, this
	// should succeed.
	_, err := cm.CommitCircuits(circuit)
	if err != nil {
		return fmt.Errorf("failed to commit circuits: %w", err)
	}

	// If the keystone has no outgoing key, we won't open it.
	if ks.OutKey == htlcswitch.EmptyCircuitKey {
		return nil
	}

	// Open the circuit, implicitly creates a keystone on disk.
	err = cm.OpenCircuits(ks)
	if err != nil {
		return fmt.Errorf("failed to open circuits: %w", err)
	}

	return nil
}

// assertKeystoneDeleted checks that a given keystone is deleted from the
// circuit map.
func assertKeystoneDeleted(t *testing.T,
	cm htlcswitch.CircuitLookup, ks htlcswitch.Keystone) {

	c := cm.LookupCircuit(ks.InKey)
	require.Nil(t, c, "no circuit should be found using InKey")

	if ks.OutKey != htlcswitch.EmptyCircuitKey {
		c = cm.LookupOpenCircuit(ks.OutKey)
		require.Nil(t, c, "no circuit should be found using OutKey")
	}
}

// assertKeystoneDeleted checks that a given keystone is not deleted from the
// circuit map.
func assertKeystoneNotDeleted(t *testing.T,
	cm htlcswitch.CircuitLookup, ks htlcswitch.Keystone) {

	c := cm.LookupCircuit(ks.InKey)
	require.NotNil(t, c, "expecting circuit found using InKey")

	if ks.OutKey != htlcswitch.EmptyCircuitKey {
		c = cm.LookupOpenCircuit(ks.OutKey)
		require.NotNil(t, c, "expecting circuit found using OutKey")
	}
}

// createTestCloseChannelSummery creates a CloseChannelSummery for testing.
func createTestCloseChannelSummery(tx kvdb.RwTx, isPending bool,
	chanID lnwire.ShortChannelID) error {

	closedChanBucket, err := tx.CreateTopLevelBucket(closedChannelBucket)
	if err != nil {
		return err
	}
	outputPoint := wire.OutPoint{Hash: hash1, Index: 1}

	ccs := &channeldb.ChannelCloseSummary{
		ChanPoint:      outputPoint,
		ShortChanID:    chanID,
		ChainHash:      hash1,
		ClosingTXID:    hash2,
		CloseHeight:    100,
		RemotePub:      testEphemeralKey,
		Capacity:       btcutil.Amount(10000),
		SettledBalance: btcutil.Amount(50000),
		CloseType:      channeldb.RemoteForceClose,
		IsPending:      isPending,
	}
	var b bytes.Buffer
	if err := serializeChannelCloseSummary(&b, ccs); err != nil {
		return err
	}

	var chanPointBuf bytes.Buffer
	if err := lnwire.WriteOutPoint(&chanPointBuf, outputPoint); err != nil {
		return err
	}

	return closedChanBucket.Put(chanPointBuf.Bytes(), b.Bytes())
}

func serializeChannelCloseSummary(
	w io.Writer,
	cs *channeldb.ChannelCloseSummary) error {

	err := channeldb.WriteElements(
		w,
		cs.ChanPoint, cs.ShortChanID, cs.ChainHash, cs.ClosingTXID,
		cs.CloseHeight, cs.RemotePub, cs.Capacity, cs.SettledBalance,
		cs.TimeLockedBalance, cs.CloseType, cs.IsPending,
	)
	if err != nil {
		return err
	}

	// If this is a close channel summary created before the addition of
	// the new fields, then we can exit here.
	if cs.RemoteCurrentRevocation == nil {
		return channeldb.WriteElements(w, false)
	}

	return nil
}
