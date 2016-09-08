package lnwallet

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	privPass = []byte("private-test")

	// For simplicity a single priv key controls all of our test outputs.
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	// We're alice :)
	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed.
	testHdSeed = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	// The number of confirmations required to consider any created channel
	// open.
	numReqConfs = uint16(1)
)

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx, signDesc *SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	redeemScript := signDesc.RedeemScript
	privKey := m.key

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, redeemScript, txscript.SigHashAll, privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

// ComputeInputScript...
func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *SignDescriptor) (*InputScript, error) {
	return nil, nil
}

// createTestChannels creates two test channels funded with 10 BTC, with 5 BTC
// allocated to each side.
func createTestChannels() (*LightningChannel, *LightningChannel, func(), error) {
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)

	channelCapacity := btcutil.Amount(10 * 1e8)
	channelBal := channelCapacity / 2
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	redeemScript, _, err := GenFundingPkScript(aliceKeyPub.SerializeCompressed(),
		bobKeyPub.SerializeCompressed(), int64(channelCapacity))
	if err != nil {
		return nil, nil, nil, err
	}

	prevOut := &wire.OutPoint{
		Hash:  wire.ShaHash(testHdSeed),
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	bobElkrem := elkrem.NewElkremSender(deriveElkremRoot(bobKeyPriv, bobKeyPub, aliceKeyPub))
	bobFirstRevoke, err := bobElkrem.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobRevokeKey := DeriveRevocationPubkey(aliceKeyPub, bobFirstRevoke[:])

	aliceElkrem := elkrem.NewElkremSender(deriveElkremRoot(aliceKeyPriv, aliceKeyPub, bobKeyPub))
	aliceFirstRevoke, err := aliceElkrem.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceRevokeKey := DeriveRevocationPubkey(bobKeyPub, aliceFirstRevoke[:])

	aliceCommitTx, err := CreateCommitTx(fundingTxIn, aliceKeyPub,
		bobKeyPub, aliceRevokeKey, csvTimeoutAlice, channelBal, channelBal)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitTx, err := CreateCommitTx(fundingTxIn, bobKeyPub,
		aliceKeyPub, bobRevokeKey, csvTimeoutBob, channelBal, channelBal)
	if err != nil {
		return nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	dbAlice, err := channeldb.Open(alicePath, &chaincfg.TestNet3Params)
	if err != nil {
		return nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	dbBob, err := channeldb.Open(bobPath, &chaincfg.TestNet3Params)
	if err != nil {
		return nil, nil, nil, err
	}

	aliceChannelState := &channeldb.OpenChannel{
		TheirLNID:              testHdSeed,
		ChanID:                 prevOut,
		OurCommitKey:           aliceKeyPub,
		TheirCommitKey:         bobKeyPub,
		Capacity:               channelCapacity,
		OurBalance:             channelBal,
		TheirBalance:           channelBal,
		OurCommitTx:            aliceCommitTx,
		FundingOutpoint:        prevOut,
		OurMultiSigKey:         aliceKeyPub,
		TheirMultiSigKey:       bobKeyPub,
		FundingRedeemScript:    redeemScript,
		LocalCsvDelay:          csvTimeoutAlice,
		RemoteCsvDelay:         csvTimeoutBob,
		TheirCurrentRevocation: bobRevokeKey,
		LocalElkrem:            aliceElkrem,
		RemoteElkrem:           &elkrem.ElkremReceiver{},
		Db:                     dbAlice,
	}
	bobChannelState := &channeldb.OpenChannel{
		TheirLNID:              testHdSeed,
		ChanID:                 prevOut,
		OurCommitKey:           bobKeyPub,
		TheirCommitKey:         aliceKeyPub,
		Capacity:               channelCapacity,
		OurBalance:             channelBal,
		TheirBalance:           channelBal,
		OurCommitTx:            bobCommitTx,
		FundingOutpoint:        prevOut,
		OurMultiSigKey:         bobKeyPub,
		TheirMultiSigKey:       aliceKeyPub,
		FundingRedeemScript:    redeemScript,
		LocalCsvDelay:          csvTimeoutBob,
		RemoteCsvDelay:         csvTimeoutAlice,
		TheirCurrentRevocation: aliceRevokeKey,
		LocalElkrem:            bobElkrem,
		RemoteElkrem:           &elkrem.ElkremReceiver{},
		Db:                     dbBob,
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}

	channelAlice, err := NewLightningChannel(aliceSigner, nil, nil, aliceChannelState)
	if err != nil {
		return nil, nil, nil, err
	}
	channelBob, err := NewLightningChannel(bobSigner, nil, nil, bobChannelState)
	if err != nil {
		return nil, nil, nil, err
	}

	return channelAlice, channelBob, cleanUpFunc, nil
}

// TestSimpleAddSettleWorkflow tests a simple channel scenario wherein the
// local node (Alice in this case) creates a new outgoing HTLC to bob, commits
// this change, then bob immediately commits a settlement of the HTLC after the
// initial add is fully commited in both commit chains.
// TODO(roasbeef): write higher level framework to excercise various states of
// the state machine
//  * DSL language perhaps?
//  * constructed via input/output files
func TestSimpleAddSettleWorkflow(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels()
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	// For testing purposes we'll use a revocation window of size 3.
	for i := 1; i < 4; i++ {
		aliceNextRevoke, err := aliceChannel.ExtendRevocationWindow()
		if err != nil {
			t.Fatalf("unable to create new alice revoke")
		}
		if htlcs, err := bobChannel.ReceiveRevocation(aliceNextRevoke); err != nil {
			t.Fatalf("bob unable to process alice revocation increment: %v", err)
		} else if htlcs != nil {
			t.Fatalf("revocation window extend should not trigger htlc "+
				"forward, instead %v marked for forwarding", spew.Sdump(htlcs))
		}

		bobNextRevoke, err := bobChannel.ExtendRevocationWindow()
		if err != nil {
			t.Fatalf("unable to create new bob revoke")
		}
		if htlcs, err := aliceChannel.ReceiveRevocation(bobNextRevoke); err != nil {
			t.Fatalf("bob unable to process alice revocation increment: %v", err)
		} else if htlcs != nil {
			t.Fatalf("revocation window extend should not trigger htlc "+
				"forward, instead %v marked for forwarding", spew.Sdump(htlcs))
		}
	}

	// The edge of the revocation window for both sides should be 3 at this
	// point.
	if aliceChannel.revocationWindowEdge != 3 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			aliceChannel.revocationWindowEdge, 3)
	}
	if bobChannel.revocationWindowEdge != 3 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			bobChannel.revocationWindowEdge, 3)
	}

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := fastsha256.Sum256(paymentPreimage)
	htlc := &lnwire.HTLCAddRequest{
		RedemptionHashes: [][32]byte{paymentHash},
		// TODO(roasbeef): properly switch to credits: (1 msat)
		Amount: lnwire.CreditsAmount(1e8),
		Expiry: uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who also
	// adds this htlc to his local state update log.
	aliceChannel.AddHTLC(htlc)
	bobChannel.ReceiveHTLC(htlc)

	// Next alice commits this change by sending a signature message.
	aliceSig, bobLogIndex, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Bob recieves this signature message, then generates a signature for
	// Alice's commitment transaction, and the revocation to his prior
	// commitment transaction.
	if err := bobChannel.ReceiveNewCommitment(aliceSig, bobLogIndex); err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}
	bobSig, aliceLogIndex, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}
	bobRevocation, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	// Alice then proceses bob's signature, and generates a revocation for
	// bob.
	if err := aliceChannel.ReceiveNewCommitment(bobSig, aliceLogIndex); err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}
	// Alice then processes this revocation, sending her own revovation for
	// her prior commitment transaction. Alice shouldn't have any HTLC's to
	// forward since she's sending anoutgoing HTLC.
	if htlcs, err := aliceChannel.ReceiveRevocation(bobRevocation); err != nil {
		t.Fatalf("alice unable to rocess bob's revocation: %v", err)
	} else if len(htlcs) != 0 {
		t.Fatalf("alice forwards %v htlcs, should forward none: ", len(htlcs))
	}
	aliceRevocation, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke alice channel: %v", err)
	}

	// Finally Bob processes Alice's revocation, at this point the new HTLC
	// is fully locked in within both commitment transactions. Bob should
	// also be able to forward an HTLC now that the HTLC has been locked
	// into both commitment transactions.
	if htlcs, err := bobChannel.ReceiveRevocation(aliceRevocation); err != nil {
		t.Fatalf("bob unable to process alive's revocation: %v", err)
	} else if len(htlcs) != 1 {
		t.Fatalf("bob should be able to forward an HTLC, instead can "+
			"forward %v", len(htlcs))
	}

	// At this point, both sides should have the proper balance, and
	// commitment height updated within their local channel state.
	aliceBalance := btcutil.Amount(4 * 1e8)
	bobBalance := btcutil.Amount(5 * 1e8)
	if aliceChannel.channelState.OurBalance != aliceBalance {
		t.Fatalf("alice has incorrect local balance %v vs %v",
			aliceChannel.channelState.OurBalance, aliceBalance)
	}
	if aliceChannel.channelState.TheirBalance != bobBalance {
		t.Fatalf("alice has incorrect remote balance %v vs %v",
			aliceChannel.channelState.TheirBalance, bobBalance)
	}
	if bobChannel.channelState.OurBalance != bobBalance {
		t.Fatalf("bob has incorrect local balance %v vs %v",
			bobChannel.channelState.OurBalance, bobBalance)
	}
	if bobChannel.channelState.TheirBalance != aliceBalance {
		t.Fatalf("bob has incorrect remote balance %v vs %v",
			bobChannel.channelState.TheirBalance, aliceBalance)
	}
	if bobChannel.currentHeight != 1 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 1)
	}
	if aliceChannel.currentHeight != 1 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 1)
	}

	// Alice's revocation window should now be one beyond the size of the
	// intial window. Same goes for Bob.
	if aliceChannel.revocationWindowEdge != 4 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			aliceChannel.revocationWindowEdge, 4)
	}
	if bobChannel.revocationWindowEdge != 4 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			bobChannel.revocationWindowEdge, 4)
	}

	// Now we'll repeat a similar exchange, this time with Bob settling the
	// HTLC once he learns of the preimage.
	var preimage [32]byte
	copy(preimage[:], paymentPreimage)
	settleIndex, err := bobChannel.SettleHTLC(preimage)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	if err := aliceChannel.ReceiveHTLCSettle(preimage, settleIndex); err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	bobSig2, aliceIndex2, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign settle commitment: %v", err)
	}
	if err := aliceChannel.ReceiveNewCommitment(bobSig2, aliceIndex2); err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}
	aliceSig2, bobLogIndex2, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign new commitment: %v", err)
	}
	aliceRevocation2, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to generate revoation: %v", err)
	}
	if err := bobChannel.ReceiveNewCommitment(aliceSig2, bobLogIndex2); err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}
	bobRevocation2, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("bob unable to revoke commitment: %v", err)
	}
	if htlcs, err := bobChannel.ReceiveRevocation(aliceRevocation2); err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	} else if len(htlcs) != 0 {
		t.Fatalf("bob shouldn't forward any HTLC's after outgoing settle, "+
			"instead can forward: %v", spew.Sdump(htlcs))
	}
	if htlcs, err := aliceChannel.ReceiveRevocation(bobRevocation2); err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	} else if len(htlcs) != 1 {
		// Alice should now be able to forward the settlement HTLC to
		// any down stream peers.
		t.Fatalf("alice should be able to forward a single HTLC, "+
			"instead can forward %v: %v", len(htlcs), spew.Sdump(htlcs))
	}

	// At this point, bob should have 6BTC settled, with Alice still having
	// 4 BTC. They should also be at a commitment height at two, with the
	// revocation window extended by by 1 (5).
	aliceSettleBalance := btcutil.Amount(4 * 1e8)
	bobSettleBalance := btcutil.Amount(6 * 1e8)
	if aliceChannel.channelState.OurBalance != aliceSettleBalance {
		t.Fatalf("alice has incorrect local balance %v vs %v",
			aliceChannel.channelState.OurBalance, aliceSettleBalance)
	}
	if aliceChannel.channelState.TheirBalance != bobSettleBalance {
		t.Fatalf("alice has incorrect remote balance %v vs %v",
			aliceChannel.channelState.TheirBalance, bobSettleBalance)
	}
	if bobChannel.channelState.OurBalance != bobSettleBalance {
		t.Fatalf("bob has incorrect local balance %v vs %v",
			bobChannel.channelState.OurBalance, bobSettleBalance)
	}
	if bobChannel.channelState.TheirBalance != aliceSettleBalance {
		t.Fatalf("bob has incorrect remote balance %v vs %v",
			bobChannel.channelState.TheirBalance, aliceSettleBalance)
	}
	if bobChannel.currentHeight != 2 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 2)
	}
	if aliceChannel.currentHeight != 2 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 2)
	}
	if aliceChannel.revocationWindowEdge != 5 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			aliceChannel.revocationWindowEdge, 5)
	}
	if bobChannel.revocationWindowEdge != 5 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			bobChannel.revocationWindowEdge, 5)
	}

	// The logs of both sides should now be cleared since the entry adding
	// the HTLC should have been removed once both sides recieve the
	// revocation.
	if aliceChannel.ourUpdateLog.Len() != 0 {
		t.Fatalf("alice's local not updated, should be empty, has %v entries "+
			"instead", aliceChannel.ourUpdateLog.Len())
	}
	if aliceChannel.theirUpdateLog.Len() != 0 {
		t.Fatalf("alice's remote not updated, should be empty, has %v entries "+
			"instead", aliceChannel.theirUpdateLog.Len())
	}
	if len(aliceChannel.ourLogIndex) != 0 {
		t.Fatalf("alice's local log index not cleared, should be empty but "+
			"has %v entries", len(aliceChannel.ourLogIndex))
	}
	if len(aliceChannel.theirLogIndex) != 0 {
		t.Fatalf("alice's remote log index not cleared, should be empty but "+
			"has %v entries", len(aliceChannel.theirLogIndex))
	}
	if bobChannel.ourUpdateLog.Len() != 0 {
		t.Fatalf("bob's local log not updated, should be empty, has %v entries "+
			"instead", bobChannel.ourUpdateLog.Len())
	}
	if bobChannel.theirUpdateLog.Len() != 0 {
		t.Fatalf("bob's remote log not updated, should be empty, has %v entries "+
			"instead", bobChannel.theirUpdateLog.Len())
	}
	if len(bobChannel.ourLogIndex) != 0 {
		t.Fatalf("bob's local log index not cleared, should be empty but "+
			"has %v entries", len(bobChannel.ourLogIndex))
	}
	if len(bobChannel.theirLogIndex) != 0 {
		t.Fatalf("bob's remote log index not cleared, should be empty but "+
			"has %v entries", len(bobChannel.theirLogIndex))
	}
}

func TestCooperativeChannelClosure(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels()
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First we test the channel initiator requesting a cooperative close.
	sig, txid, err := aliceChannel.InitCooperativeClose()
	if err != nil {
		t.Fatalf("unable to initiate alice cooperative close: %v", err)
	}
	finalSig := append(sig, byte(txscript.SigHashAll))
	closeTx, err := bobChannel.CompleteCooperativeClose(finalSig)
	if err != nil {
		t.Fatalf("unable to complete alice cooperative close: %v", err)
	}
	bobCloseSha := closeTx.TxSha()
	if !bobCloseSha.IsEqual(txid) {
		t.Fatalf("alice's transactions doesn't match: %x vs %x",
			bobCloseSha[:], txid[:])
	}

	aliceChannel.status = channelOpen
	bobChannel.status = channelOpen

	// Next we test the channel recipient requesting a cooperative closure.
	// First we test the channel initiator requesting a cooperative close.
	sig, txid, err = bobChannel.InitCooperativeClose()
	if err != nil {
		t.Fatalf("unable to initiate bob cooperative close: %v", err)
	}
	finalSig = append(sig, byte(txscript.SigHashAll))
	closeTx, err = aliceChannel.CompleteCooperativeClose(finalSig)
	if err != nil {
		t.Fatalf("unable to complete bob cooperative close: %v", err)
	}
	aliceCloseSha := closeTx.TxSha()
	if !aliceCloseSha.IsEqual(txid) {
		t.Fatalf("bob's closure transactions don't match: %x vs %x",
			aliceCloseSha[:], txid[:])
	}
}
