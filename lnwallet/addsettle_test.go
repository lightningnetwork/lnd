package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet/omnicore"
	"github.com/lightningnetwork/lnd/lnwire"
	"testing"
)


func TestAddSettle(t *testing.T){
	EnableTestLog();
	testAddSettleWorkflow(t,true)
}
// testAddSettleWorkflow tests a simple channel scenario where Alice and Bob
// add, the settle an HTLC between themselves.
func testAddSettleWorkflow(t *testing.T, tweakless bool) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	chanType := channeldb.SingleFunderTweaklessBit
	if !tweakless {
		chanType = channeldb.SingleFunderBit
	}

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(chanType)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	//htlcAmt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	//htlcAssetAmt,_ := omnicore.NewAmount(1.0)
	htlcAssetAmt:=lnwire.UnitPrec11(1)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      htlcAssetAmt,
		AssetID:      aliceChannel.channelState.AssetID,
		Expiry:      uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who adds
	// this htlc to his remote state update log.
	walletLog.Trace("AddHTLC")
	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	walletLog.Trace("ReceiveHTLC")
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Next alice commits this change by sending a signature message. Since
	// we expect the messages to be ordered, Bob will receive the HTLC we
	// just sent before he receives this signature, so the signature will
	// cover the HTLC.
	walletLog.Trace("aliceChannel.SignNextCommitment")
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Bob receives this signature message, and checks that this covers the
	// state he has in his remote log. This includes the HTLC just sent
	// from Alice.
	walletLog.Trace("bobChannel.ReceiveNewCommitment")
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	// Bob revokes his prior commitment given to him by Alice, since he now
	// has a valid signature for a newer commitment.
	walletLog.Trace("bobChannel.RevokeCurrentCommitment")
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	// Bob finally send a signature for Alice's commitment transaction.
	// This signature will cover the HTLC, since Bob will first send the
	// revocation just created. The revocation also acks every received
	// HTLC up to the point where Alice sent here signature.
	walletLog.Trace("bobChannel.SignNextCommitment")
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// Alice then processes this revocation, sending her own revocation for
	// her prior commitment transaction. Alice shouldn't have any HTLCs to
	// forward since she's sending an outgoing HTLC.
	walletLog.Trace("aliceChannel.ReceiveRevocation")
	fwdPkg, _, _, _, err := aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("alice forwards %v add htlcs, should forward none",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("alice forwards %v settle/fail htlcs, "+
			"should forward none", len(fwdPkg.SettleFails))
	}

	// Alice then processes bob's signature, and since she just received
	// the revocation, she expect this signature to cover everything up to
	// the point where she sent her signature, including the HTLC.
	walletLog.Trace("aliceChannel.ReceiveNewCommitment")
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	// Alice then generates a revocation for bob.
	walletLog.Trace("aliceChannel.RevokeCurrentCommitment")
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke alice channel: %v", err)
	}

	// Finally Bob processes Alice's revocation, at this point the new HTLC
	// is fully locked in within both commitment transactions. Bob should
	// also be able to forward an HTLC now that the HTLC has been locked
	// into both commitment transactions.

	walletLog.Trace("bobChannel.ReceiveRevocation")
	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 1 {
		t.Fatalf("bob forwards %v add htlcs, should only forward one",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("bob forwards %v settle/fail htlcs, "+
			"should forward none", len(fwdPkg.SettleFails))
	}

	// At this point, both sides should have the proper number of satoshis
	// sent, and commitment height updated within their local channel
	// state.
	aliceSent := lnwire.MilliSatoshi(0)
	bobSent := lnwire.MilliSatoshi(0)

	if aliceChannel.channelState.TotalMSatSent != aliceSent {
		t.Fatalf("alice has incorrect milli-satoshis sent: %v vs %v",
			aliceChannel.channelState.TotalMSatSent, aliceSent)
	}
	if aliceChannel.channelState.TotalMSatReceived != bobSent {
		t.Fatalf("alice has incorrect milli-satoshis received %v vs %v",
			aliceChannel.channelState.TotalMSatReceived, bobSent)
	}
	if bobChannel.channelState.TotalMSatSent != bobSent {
		t.Fatalf("bob has incorrect milli-satoshis sent %v vs %v",
			bobChannel.channelState.TotalMSatSent, bobSent)
	}
	if bobChannel.channelState.TotalMSatReceived != aliceSent {
		t.Fatalf("bob has incorrect milli-satoshis received %v vs %v",
			bobChannel.channelState.TotalMSatReceived, aliceSent)
	}
	if bobChannel.currentHeight != 1 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 1)
	}
	if aliceChannel.currentHeight != 1 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 1)
	}


	outputCounts:=3
	//if aliceChannel.channelState.LocalCommitment.CommitTx.TxOut[0].Value ==0{
	//	//tx with opreturn output will have 4 outputs
	//	outputCounts=4
	//}

	// Both commitment transactions should have three outputs, and one of
	// them should be exactly the amount of the HTLC.
	if len(aliceChannel.channelState.LocalCommitment.CommitTx.TxOut) != outputCounts {
		t.Fatalf("alice should have three commitment outputs, instead "+
			"have %v",
			len(aliceChannel.channelState.LocalCommitment.CommitTx.TxOut))
	}
	if len(bobChannel.channelState.LocalCommitment.CommitTx.TxOut) != outputCounts {
		t.Fatalf("bob should have three commitment outputs, instead "+
			"have %v",
			len(bobChannel.channelState.LocalCommitment.CommitTx.TxOut))
	}
	assertOutputExistsByValue(t,
		aliceChannel.channelState.LocalCommitment.CommitTx,
		omnicore.OmniGas*3)
	//assertOutputExistsByValue(t,
	//	bobChannel.channelState.LocalCommitment.CommitTx,
	//	htlcAmt.ToSatoshis())

	// Now we'll repeat a similar exchange, this time with Bob settling the
	// HTLC once he learns of the preimage.
	var preimage [32]byte
	copy(preimage[:], paymentPreimage)
	walletLog.Trace("bobChannel.SettleHTLC")
	err = bobChannel.SettleHTLC(preimage, bobHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}

	walletLog.Trace("aliceChannel.ReceiveHTLCSettle")
	err = aliceChannel.ReceiveHTLCSettle(preimage, aliceHtlcIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}

	walletLog.Trace("bobChannel.SignNextCommitment")
	bobSig2, bobHtlcSigs2, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign settle commitment: %v", err)
	}
	walletLog.Trace("aliceChannel.ReceiveNewCommitment")
	err = aliceChannel.ReceiveNewCommitment(bobSig2, bobHtlcSigs2)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	walletLog.Trace("aliceChannel.RevokeCurrentCommitment")
	aliceRevocation2, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to generate revocation: %v", err)
	}
	aliceSig2, aliceHtlcSigs2, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign new commitment: %v", err)
	}

	walletLog.Trace("bobChannel.ReceiveRevocation")
	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation2)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("bob forwards %v add htlcs, should forward none",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("bob forwards %v settle/fail htlcs, "+
			"should forward none", len(fwdPkg.SettleFails))
	}

	walletLog.Trace("bobChannel.ReceiveNewCommitment")
	err = bobChannel.ReceiveNewCommitment(aliceSig2, aliceHtlcSigs2)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	walletLog.Trace("bobChannel.RevokeCurrentCommitment")
	bobRevocation2, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("bob unable to revoke commitment: %v", err)
	}

	walletLog.Trace("aliceChannel.ReceiveRevocation")
	fwdPkg, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation2)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 0 {
		// Alice should now be able to forward the settlement HTLC to
		// any down stream peers.
		t.Fatalf("alice should be forwarding an add HTLC, "+
			"instead forwarding %v: %v", len(fwdPkg.Adds),
			spew.Sdump(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 1 {
		t.Fatalf("alice should be forwarding one settle/fails HTLC, "+
			"instead forwarding: %v", len(fwdPkg.SettleFails))
	}

	// At this point, Bob should have 6 BTC settled, with Alice still
	// having 4 BTC. Alice's channel should show 1 BTC sent and Bob's
	// channel should show 1 BTC received. They should also be at
	// commitment height two, with the revocation window extended by 1 (5).
	mSatTransferred := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	if aliceChannel.channelState.AssetID>1{
		mSatTransferred=lnwire.NewMSatFromSatoshis(omnicore.OmniGas*3)
	}
	if aliceChannel.channelState.TotalMSatSent != mSatTransferred {
		t.Fatalf("alice satoshis sent incorrect %v vs %v expected",
			aliceChannel.channelState.TotalMSatSent,
			mSatTransferred)
	}
	if aliceChannel.channelState.TotalMSatReceived != 0 {
		t.Fatalf("alice satoshis received incorrect %v vs %v expected",
			aliceChannel.channelState.TotalMSatReceived, 0)
	}
	if bobChannel.channelState.TotalMSatReceived != mSatTransferred {
		t.Fatalf("bob satoshis received incorrect %v vs %v expected",
			bobChannel.channelState.TotalMSatReceived,
			mSatTransferred)
	}
	if bobChannel.channelState.TotalMSatSent != 0 {
		t.Fatalf("bob satoshis sent incorrect %v vs %v expected",
			bobChannel.channelState.TotalMSatSent, 0)
	}
	if bobChannel.currentHeight != 2 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 2)
	}
	if aliceChannel.currentHeight != 2 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 2)
	}

	// The logs of both sides should now be cleared since the entry adding
	// the HTLC should have been removed once both sides receive the
	// revocation.
	if aliceChannel.localUpdateLog.Len() != 0 {
		t.Fatalf("alice's local not updated, should be empty, has %v "+
			"entries instead", aliceChannel.localUpdateLog.Len())
	}
	if aliceChannel.remoteUpdateLog.Len() != 0 {
		t.Fatalf("alice's remote not updated, should be empty, has %v "+
			"entries instead", aliceChannel.remoteUpdateLog.Len())
	}
	if len(aliceChannel.localUpdateLog.updateIndex) != 0 {
		t.Fatalf("alice's local log index not cleared, should be empty but "+
			"has %v entries", len(aliceChannel.localUpdateLog.updateIndex))
	}
	if len(aliceChannel.remoteUpdateLog.updateIndex) != 0 {
		t.Fatalf("alice's remote log index not cleared, should be empty but "+
			"has %v entries", len(aliceChannel.remoteUpdateLog.updateIndex))
	}
}

func assertOutputExistsByValue(t *testing.T, commitTx *wire.MsgTx,
	value btcutil.Amount) {

	for _, txOut := range commitTx.TxOut {
		if txOut.Value == int64(value) {
			return
		}
	}

	t.Fatalf("unable to find output of value %v within tx %v", value,
		spew.Sdump(commitTx))
}

