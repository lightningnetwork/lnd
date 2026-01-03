package peer

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestRemoteCloseStartTaprootIntegration tests the full flow of
// RemoteCloseStart handling a ClosingComplete message with taproot signatures.
//
// This is a regression test for a bug where the remote nonce was not properly
// created before sending over our signature.
func TestRemoteCloseStartTaprootIntegration(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.SimpleTaprootFeatureBit

	aliceChan, bobChan, err := lnwallet.CreateTestChannels(t, chanType)
	require.NoError(t, err)

	// Create TWO SEPARATE MusigChanCloser instances. This is the key to
	// exposing the bug - in production where the issue existed, they share one.
	localSession := NewMusigChanCloser(aliceChan)
	remoteSession := NewMusigChanCloser(bobChan)

	// Initialize local session with nonces (simulating LocalCloseStart path
	// during shutdown exchange).
	_, err = localSession.ClosingNonce()
	require.NoError(t, err)

	// Give local session the remote's closee nonce.
	remoteCloseeNonce, err := musig2.GenNonces(
		musig2.WithPublicKey(
			bobChan.State().LocalChanCfg.MultiSigKey.PubKey,
		),
	)
	require.NoError(t, err)
	localSession.InitRemoteNonce(remoteCloseeNonce)

	// For remoteSession, generate the Local nonce (simulating what would
	// happen during the shutdown exchange when we act as closee).
	_, err = remoteSession.ClosingNonce()
	require.NoError(t, err)

	// NOTE: remoteSession has its local nonce but NO remote nonce yet.
	// This is the setup that exposes the bug. In production, both
	// sessions point to the same object, so nonces set via localSession
	// would also be visible to remoteSession. Here they are separate.
	//
	// The bug was that ProcessEvent calls ProposalClosingOpts() BEFORE
	// processRemoteTaprootSig() which would initialize the remote nonce.

	// Make some fake shutdown scripts for both sides.
	localDeliveryScript := bytes.Repeat([]byte{0x01}, 34)
	localDeliveryScript[0] = txscript.OP_1
	localDeliveryScript[1] = txscript.OP_DATA_32

	remoteDeliveryScript := bytes.Repeat([]byte{0x02}, 34)
	remoteDeliveryScript[0] = txscript.OP_1
	remoteDeliveryScript[1] = txscript.OP_DATA_32

	// Create mocks and other set up configs.
	closeSigner := &mockCloseSigner{}
	feeEstimator := &mockCoopFeeEstimator{
		absoluteFee: btcutil.Amount(1000),
	}
	chanObserver := &mockChanObserver{}
	peerPub := bobChan.State().IdentityPub
	env := chancloser.Environment{
		ChainParams: chaincfg.RegressionNetParams,
		ChanPeer:    *peerPub,
		ChanPoint:   aliceChan.ChannelPoint(),
		ChanID: lnwire.NewChanIDFromOutPoint(
			aliceChan.ChannelPoint(),
		),
		ChanType:           chanType,
		DefaultFeeRate:     chainfee.SatPerVByte(10),
		FeeEstimator:       feeEstimator,
		ChanObserver:       chanObserver,
		CloseSigner:        closeSigner,
		LocalMusigSession:  localSession,
		RemoteMusigSession: remoteSession,
	}
	localBalance := lnwire.NewMSatFromSatoshis(btcutil.Amount(500000000))
	remoteBalance := lnwire.NewMSatFromSatoshis(btcutil.Amount(500000000))

	// Create RemoteCloseStart state, this is where the state machine will
	// start from.
	state := &chancloser.RemoteCloseStart{
		CloseChannelTerms: &chancloser.CloseChannelTerms{
			ShutdownScripts: chancloser.ShutdownScripts{
				LocalDeliveryScript:  localDeliveryScript,
				RemoteDeliveryScript: remoteDeliveryScript,
			},
			ShutdownBalances: chancloser.ShutdownBalances{
				LocalBalance:  localBalance,
				RemoteBalance: remoteBalance,
			},
		},
	}

	// Generate a valid JIT nonce for the ClosingComplete message.
	// This simulates the remote party's closer nonce.
	jitNonce, err := musig2.GenNonces(
		musig2.WithPublicKey(
			aliceChan.State().LocalChanCfg.MultiSigKey.PubKey,
		),
	)
	require.NoError(t, err)

	var dummySig btcec.ModNScalar
	dummySig.SetInt(12345)
	partialSigWithNonce := lnwire.PartialSigWithNonce{
		PartialSig: lnwire.NewPartialSig(dummySig),
		Nonce:      jitNonce.PubNonce,
	}

	// Create OfferReceivedEvent with taproot sig (ClosingComplete). Since
	// both local and remote balances are above dust, we need the
	// CloserAndClosee variant.
	closingComplete := lnwire.ClosingComplete{
		ChannelID:    env.ChanID,
		CloserScript: remoteDeliveryScript,
		CloseeScript: localDeliveryScript,
		FeeSatoshis:  btcutil.Amount(1000),
		LockTime:     0,
		TaprootClosingSigs: lnwire.TaprootClosingSigs{
			CloserAndClosee: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType7](
					partialSigWithNonce,
				),
			),
		},
	}

	event := &chancloser.OfferReceivedEvent{
		SigMsg: closingComplete,
	}

	// Call ProcessEvent. Before the bug fix, this will fail with: "failed
	// to get musig closing opts: remote nonce not generated"
	//
	// This is because ProposalClosingOpts() is called on line 1932 BEFORE
	// processRemoteTaprootSig() initializes the nonce on line 1943.
	//
	// After the fix (swapping the order), this should succeed.
	_, err = state.ProcessEvent(event, &env)

	require.NoError(
		t, err, "ProcessEvent should not fail - if it fails "+
			"with 'remote nonce not generated', the bug still "+
			"exists",
	)
}

type mockCloseSigner struct{}

func (m *mockCloseSigner) CreateCloseProposal(
	proposedFee btcutil.Amount, localDeliveryScript,
	remoteDeliveryScript []byte, closeOpt ...lnwallet.ChanCloseOpt,
) (input.Signature, *wire.MsgTx, btcutil.Amount, error) {

	// Create a minimal MusigPartialSig for taproot channels.
	partialSig := musig2.NewPartialSignature(
		new(btcec.ModNScalar), new(btcec.PublicKey),
	)
	musigSig := lnwallet.NewMusigPartialSig(
		&partialSig,
		lnwire.Musig2Nonce{},
		lnwire.Musig2Nonce{},
		nil,
		fn.None[chainhash.Hash](),
	)

	return musigSig, wire.NewMsgTx(2), proposedFee, nil
}

func (m *mockCloseSigner) CompleteCooperativeClose(
	localSig, remoteSig input.Signature,
	localDeliveryScript, remoteDeliveryScript []byte,
	proposedFee btcutil.Amount, closeOpts ...lnwallet.ChanCloseOpt,
) (*wire.MsgTx, btcutil.Amount, error) {

	return wire.NewMsgTx(2), 0, nil
}

type mockCoopFeeEstimator struct {
	absoluteFee btcutil.Amount
}

func (m *mockCoopFeeEstimator) EstimateFee(
	chanType channeldb.ChannelType, localTxOut, remoteTxOut *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	return m.absoluteFee
}

type mockChanObserver struct{}

func (m *mockChanObserver) NoDanglingUpdates() bool {
	return true
}

func (m *mockChanObserver) DisableIncomingAdds() error {
	return nil
}

func (m *mockChanObserver) DisableOutgoingAdds() error {
	return nil
}

func (m *mockChanObserver) DisableChannel() error {
	return nil
}

func (m *mockChanObserver) MarkCoopBroadcasted(*wire.MsgTx, bool) error {
	return nil
}

func (m *mockChanObserver) MarkShutdownSent(deliveryAddr []byte,
	isInitiator bool) error {

	return nil
}

func (m *mockChanObserver) FinalBalances() fn.Option[chancloser.ShutdownBalances] {

	return fn.None[chancloser.ShutdownBalances]()
}
