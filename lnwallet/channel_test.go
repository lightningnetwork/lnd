package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"io/ioutil"
	"os"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
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
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, txscript.SigHashAll, privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}
func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *SignDescriptor) (*InputScript, error) {

	witnessScript, err := txscript.WitnessScript(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		txscript.SigHashAll, m.key, true)
	if err != nil {
		return nil, err
	}

	return &InputScript{
		Witness: witnessScript,
	}, nil
}

type mockNotfier struct {
}

func (m *mockNotfier) RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return nil, nil
}
func (m *mockNotfier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	return nil, nil
}

func (m *mockNotfier) Start() error {
	return nil
}

func (m *mockNotfier) Stop() error {
	return nil
}
func (m *mockNotfier) RegisterSpendNtfn(outpoint *wire.OutPoint, heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend: make(chan *chainntnfs.SpendDetail),
	}, nil
}

// initRevocationWindows simulates a new channel being opened within the p2p
// network by populating the initial revocation windows of the passed
// commitment state machines.
func initRevocationWindows(chanA, chanB *LightningChannel, windowSize int) error {
	for i := 0; i < windowSize; i++ {
		aliceNextRevoke, err := chanA.ExtendRevocationWindow()
		if err != nil {
			return err
		}
		if htlcs, err := chanB.ReceiveRevocation(aliceNextRevoke); err != nil {
			return err
		} else if htlcs != nil {
			return err
		}

		bobNextRevoke, err := chanB.ExtendRevocationWindow()
		if err != nil {
			return err
		}
		if htlcs, err := chanA.ReceiveRevocation(bobNextRevoke); err != nil {
			return err
		} else if htlcs != nil {
			return err
		}
	}

	return nil
}

// forceStateTransition executes the necessary interaction between the two
// commitment state machines to transition to a new state locking in any
// pending updates.
func forceStateTransition(chanA, chanB *LightningChannel) error {
	aliceSig, err := chanA.SignNextCommitment()
	if err != nil {
		return err
	}
	if err := chanB.ReceiveNewCommitment(aliceSig); err != nil {
		return err
	}

	bobRevocation, err := chanB.RevokeCurrentCommitment()
	if err != nil {
		return err
	}
	bobSig, err := chanB.SignNextCommitment()
	if err != nil {
		return err
	}

	if _, err := chanA.ReceiveRevocation(bobRevocation); err != nil {
		return err
	}
	if err := chanA.ReceiveNewCommitment(bobSig); err != nil {
		return err
	}

	aliceRevocation, err := chanA.RevokeCurrentCommitment()
	if err != nil {
		return err
	}
	if _, err := chanB.ReceiveRevocation(aliceRevocation); err != nil {
		return err
	}

	return nil
}

// createTestChannels creates two test channels funded with 10 BTC, with 5 BTC
// allocated to each side. Within the channel, Alice is the initiator.
func createTestChannels(revocationWindow int) (*LightningChannel, *LightningChannel, func(), error) {
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)

	channelCapacity := btcutil.Amount(10 * 1e8)
	channelBal := channelCapacity / 2
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(800)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	witnessScript, _, err := GenFundingPkScript(aliceKeyPub.SerializeCompressed(),
		bobKeyPub.SerializeCompressed(), int64(channelCapacity))
	if err != nil {
		return nil, nil, nil, err
	}

	prevOut := &wire.OutPoint{
		Hash:  chainhash.Hash(testHdSeed),
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	bobRoot := deriveRevocationRoot(bobKeyPriv, bobKeyPub, aliceKeyPub)
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobRevokeKey := DeriveRevocationPubkey(aliceKeyPub, bobFirstRevoke[:])

	aliceRoot := deriveRevocationRoot(aliceKeyPriv, aliceKeyPub, bobKeyPub)
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceRevokeKey := DeriveRevocationPubkey(bobKeyPub, aliceFirstRevoke[:])

	aliceCommitTx, err := CreateCommitTx(fundingTxIn, aliceKeyPub,
		bobKeyPub, aliceRevokeKey, csvTimeoutAlice, channelBal, channelBal, aliceDustLimit)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitTx, err := CreateCommitTx(fundingTxIn, bobKeyPub,
		aliceKeyPub, bobRevokeKey, csvTimeoutBob, channelBal, channelBal, bobDustLimit)
	if err != nil {
		return nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	dbAlice, err := channeldb.Open(alicePath)
	if err != nil {
		return nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	dbBob, err := channeldb.Open(bobPath)
	if err != nil {
		return nil, nil, nil, err
	}

	var obsfucator [StateHintSize]byte
	copy(obsfucator[:], aliceFirstRevoke[:])

	aliceChannelState := &channeldb.OpenChannel{
		IdentityPub:            aliceKeyPub,
		ChanID:                 prevOut,
		ChanType:               channeldb.SingleFunder,
		IsInitiator:            true,
		StateHintObsfucator:    obsfucator,
		OurCommitKey:           aliceKeyPub,
		TheirCommitKey:         bobKeyPub,
		Capacity:               channelCapacity,
		OurBalance:             channelBal,
		TheirBalance:           channelBal,
		OurCommitTx:            aliceCommitTx,
		OurCommitSig:           bytes.Repeat([]byte{1}, 71),
		FundingOutpoint:        prevOut,
		OurMultiSigKey:         aliceKeyPub,
		TheirMultiSigKey:       bobKeyPub,
		FundingWitnessScript:   witnessScript,
		LocalCsvDelay:          csvTimeoutAlice,
		RemoteCsvDelay:         csvTimeoutBob,
		TheirCurrentRevocation: bobRevokeKey,
		RevocationProducer:     alicePreimageProducer,
		RevocationStore:        shachain.NewRevocationStore(),
		TheirDustLimit:         bobDustLimit,
		OurDustLimit:           aliceDustLimit,
		Db:                     dbAlice,
	}
	bobChannelState := &channeldb.OpenChannel{
		IdentityPub:            bobKeyPub,
		ChanID:                 prevOut,
		ChanType:               channeldb.SingleFunder,
		IsInitiator:            false,
		StateHintObsfucator:    obsfucator,
		OurCommitKey:           bobKeyPub,
		TheirCommitKey:         aliceKeyPub,
		Capacity:               channelCapacity,
		OurBalance:             channelBal,
		TheirBalance:           channelBal,
		OurCommitTx:            bobCommitTx,
		OurCommitSig:           bytes.Repeat([]byte{1}, 71),
		FundingOutpoint:        prevOut,
		OurMultiSigKey:         bobKeyPub,
		TheirMultiSigKey:       aliceKeyPub,
		FundingWitnessScript:   witnessScript,
		LocalCsvDelay:          csvTimeoutBob,
		RemoteCsvDelay:         csvTimeoutAlice,
		TheirCurrentRevocation: aliceRevokeKey,
		RevocationProducer:     bobPreimageProducer,
		RevocationStore:        shachain.NewRevocationStore(),
		TheirDustLimit:         aliceDustLimit,
		OurDustLimit:           bobDustLimit,
		Db:                     dbBob,
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mockSigner{aliceKeyPriv}
	bobSigner := &mockSigner{bobKeyPriv}

	notifier := &mockNotfier{}
	estimator := &StaticFeeEstimator{50, 6}

	channelAlice, err := NewLightningChannel(aliceSigner, notifier,
		estimator, aliceChannelState)
	if err != nil {
		return nil, nil, nil, err
	}
	channelBob, err := NewLightningChannel(bobSigner, notifier,
		estimator, bobChannelState)
	if err != nil {
		return nil, nil, nil, err
	}

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	err = initRevocationWindows(channelAlice, channelBob, revocationWindow)
	if err != nil {
		return nil, nil, nil, err
	}

	return channelAlice, channelBob, cleanUpFunc, nil
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
// TODO(bvu): Refactor when dynamic fee estimation is added.
func calcStaticFee(numHTLCs int) btcutil.Amount {
	const (
		commitWeight = btcutil.Amount(724)
		htlcWeight   = 172
		feePerByte   = btcutil.Amount(50 * 4)
	)
	return feePerByte * (commitWeight +
		btcutil.Amount(htlcWeight*numHTLCs)) / 1000
}

// TestSimpleAddSettleWorkflow tests a simple channel scenario wherein the
// local node (Alice in this case) creates a new outgoing HTLC to bob, commits
// this change, then bob immediately commits a settlement of the HTLC after the
// initial add is fully committed in both commit chains.
//
// TODO(roasbeef): write higher level framework to exercise various states of
// the state machine
//  * DSL language perhaps?
//  * constructed via input/output files
func TestSimpleAddSettleWorkflow(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(1)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// The edge of the revocation window for both sides should be 1 at this
	// point.
	if aliceChannel.revocationWindowEdge != 1 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			aliceChannel.revocationWindowEdge, 1)
	}
	if bobChannel.revocationWindowEdge != 1 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			bobChannel.revocationWindowEdge, 1)
	}

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      btcutil.SatoshiPerBitcoin,
		Expiry:      uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who also
	// adds this htlc to his local state update log.
	if _, err := aliceChannel.AddHTLC(htlc); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Next alice commits this change by sending a signature message.
	aliceSig, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Bob receives this signature message, revokes his prior commitment
	// given to him by Alice,a nd then finally send a signature for Alice's
	// commitment transaction.
	if err := bobChannel.ReceiveNewCommitment(aliceSig); err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}
	bobRevocation, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}
	bobSig, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// Alice then processes this revocation, sending her own recovation for
	// her prior commitment transaction. Alice shouldn't have any HTLCs to
	// forward since she's sending an outgoing HTLC.
	if htlcs, err := aliceChannel.ReceiveRevocation(bobRevocation); err != nil {
		t.Fatalf("alice unable to rocess bob's revocation: %v", err)
	} else if len(htlcs) != 0 {
		t.Fatalf("alice forwards %v htlcs, should forward none: ", len(htlcs))
	}

	// Alice then processes bob's signature, and generates a revocation for
	// bob.
	if err := aliceChannel.ReceiveNewCommitment(bobSig); err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
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

	// At this point, both sides should have the proper number of satoshis
	// sent, and commitment height updated within their local channel
	// state.
	aliceSent := uint64(0)
	bobSent := uint64(0)

	if aliceChannel.channelState.TotalSatoshisSent != aliceSent {
		t.Fatalf("alice has incorrect satoshis sent: %v vs %v",
			aliceChannel.channelState.TotalSatoshisSent, aliceSent)
	}
	if aliceChannel.channelState.TotalSatoshisReceived != bobSent {
		t.Fatalf("alice has incorrect satoshis received %v vs %v",
			aliceChannel.channelState.TotalSatoshisReceived, bobSent)
	}
	if bobChannel.channelState.TotalSatoshisSent != bobSent {
		t.Fatalf("bob has incorrect satoshis sent %v vs %v",
			bobChannel.channelState.TotalSatoshisSent, bobSent)
	}
	if bobChannel.channelState.TotalSatoshisReceived != aliceSent {
		t.Fatalf("bob has incorrect satoshis received %v vs %v",
			bobChannel.channelState.TotalSatoshisReceived, aliceSent)
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
	// initial window. Same goes for Bob.
	if aliceChannel.revocationWindowEdge != 2 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			aliceChannel.revocationWindowEdge, 2)
	}
	if bobChannel.revocationWindowEdge != 2 {
		t.Fatalf("alice revocation window not incremented, is %v should be %v",
			bobChannel.revocationWindowEdge, 2)
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

	bobSig2, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign settle commitment: %v", err)
	}
	if err := aliceChannel.ReceiveNewCommitment(bobSig2); err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	aliceRevocation2, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to generate revocation: %v", err)
	}
	aliceSig2, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign new commitment: %v", err)
	}

	if htlcs, err := bobChannel.ReceiveRevocation(aliceRevocation2); err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	} else if len(htlcs) != 0 {
		t.Fatalf("bob shouldn't forward any HTLCs after outgoing settle, "+
			"instead can forward: %v", spew.Sdump(htlcs))
	}
	if err := bobChannel.ReceiveNewCommitment(aliceSig2); err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	bobRevocation2, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("bob unable to revoke commitment: %v", err)
	}

	if htlcs, err := aliceChannel.ReceiveRevocation(bobRevocation2); err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	} else if len(htlcs) != 1 {
		// Alice should now be able to forward the settlement HTLC to
		// any down stream peers.
		t.Fatalf("alice should be able to forward a single HTLC, "+
			"instead can forward %v: %v", len(htlcs), spew.Sdump(htlcs))
	}

	// At this point, Bob should have 6 BTC settled, with Alice still having
	// 4 BTC. Alice's channel should show 1 BTC sent and Bob's channel should
	// show 1 BTC received. They should also be at commitment height two,
	// with the revocation window extended by by 1 (5).
	satoshisTransferred := uint64(100000000)
	if aliceChannel.channelState.TotalSatoshisSent != satoshisTransferred {
		t.Fatalf("alice satoshis sent incorrect %v vs %v expected",
			aliceChannel.channelState.TotalSatoshisSent,
			satoshisTransferred)
	}
	if aliceChannel.channelState.TotalSatoshisReceived != 0 {
		t.Fatalf("alice satoshis received incorrect %v vs %v expected",
			aliceChannel.channelState.TotalSatoshisSent, 0)
	}
	if bobChannel.channelState.TotalSatoshisReceived != satoshisTransferred {
		t.Fatalf("bob satoshis received incorrect %v vs %v expected",
			bobChannel.channelState.TotalSatoshisReceived,
			satoshisTransferred)
	}
	if bobChannel.channelState.TotalSatoshisSent != 0 {
		t.Fatalf("bob satoshis sent incorrect %v vs %v expected",
			bobChannel.channelState.TotalSatoshisReceived, 0)
	}
	if bobChannel.currentHeight != 2 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 2)
	}
	if aliceChannel.currentHeight != 2 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 2)
	}
	if aliceChannel.revocationWindowEdge != 3 {
		t.Fatalf("alice revocation window not incremented, is %v "+
			"should be %v", aliceChannel.revocationWindowEdge, 3)
	}
	if bobChannel.revocationWindowEdge != 3 {
		t.Fatalf("alice revocation window not incremented, is %v "+
			"should be %v", bobChannel.revocationWindowEdge, 3)
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

// TestCheckCommitTxSize checks that estimation size of commitment
// transaction with some degree of error corresponds to the actual size.
func TestCheckCommitTxSize(t *testing.T) {
	checkSize := func(channel *LightningChannel, count int) {
		// Due to variable size of the signatures (70-73) in
		// witness script actual size of commitment transaction might
		// be lower on 6 weight.
		BaseCommitmentTxSizeEstimationError := 6

		commitTx, err := channel.getSignedCommitTx()
		if err != nil {
			t.Fatalf("unable to initiate alice force close: %v", err)
		}

		actualCost := blockchain.GetTransactionWeight(btcutil.NewTx(commitTx))
		estimatedCost := estimateCommitTxCost(count, false)

		diff := int(estimatedCost - actualCost)
		if 0 > diff || BaseCommitmentTxSizeEstimationError < diff {
			t.Fatalf("estimation is wrong, diff: %v", diff)
		}

	}

	createHTLC := func(i int) (*lnwire.UpdateAddHTLC, [32]byte) {
		preimage := bytes.Repeat([]byte{byte(i)}, 32)
		paymentHash := sha256.Sum256(preimage)

		var returnPreimage [32]byte
		copy(returnPreimage[:], preimage)

		return &lnwire.UpdateAddHTLC{
			PaymentHash: paymentHash,
			Amount:      btcutil.Amount(1e7),
			Expiry:      uint32(5),
		}, returnPreimage
	}

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(1)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Check that weight estimation of the commitment transaction without
	// HTLCs is right.
	checkSize(aliceChannel, 0)
	checkSize(bobChannel, 0)

	// Adding HTLCs and check that size stays in allowable estimation
	// error window.
	for i := 1; i <= 10; i++ {
		htlc, _ := createHTLC(i)

		if _, err := aliceChannel.AddHTLC(htlc); err != nil {
			t.Fatalf("alice unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("bob unable to receive htlc: %v", err)
		}

		if err := forceStateTransition(aliceChannel, bobChannel); err != nil {
			t.Fatalf("unable to complete state update: %v", err)
		}
		checkSize(aliceChannel, i)
		checkSize(bobChannel, i)
	}

	// Settle HTLCs and check that estimation is counting cost of settle
	// HTLCs properly.
	for i := 10; i >= 1; i-- {
		_, preimage := createHTLC(i)

		settleIndex, err := bobChannel.SettleHTLC(preimage)
		if err != nil {
			t.Fatalf("bob unable to settle inbound htlc: %v", err)
		}
		err = aliceChannel.ReceiveHTLCSettle(preimage, settleIndex)
		if err != nil {
			t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
		}

		if err := forceStateTransition(bobChannel, aliceChannel); err != nil {
			t.Fatalf("unable to complete state update: %v", err)
		}
		checkSize(aliceChannel, i-1)
		checkSize(bobChannel, i-1)
	}
}

func TestCooperativeChannelClosure(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(1)
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
	bobCloseSha := closeTx.TxHash()
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
	aliceCloseSha := closeTx.TxHash()
	if !aliceCloseSha.IsEqual(txid) {
		t.Fatalf("bob's closure transactions don't match: %x vs %x",
			aliceCloseSha[:], txid[:])
	}
}

// TestCheckHTLCNumberConstraint checks that we can't add HTLC or receive
// HTLC if number of HTLCs exceed maximum available number, also this test
// checks that if for some reason max number of HTLCs was exceeded and not
// caught before, the creation of new commitment will not be possible because
// of validation error.
func TestCheckHTLCNumberConstraint(t *testing.T) {
	createHTLC := func(i int) *lnwire.UpdateAddHTLC {
		preimage := bytes.Repeat([]byte{byte(i)}, 32)
		paymentHash := sha256.Sum256(preimage)
		return &lnwire.UpdateAddHTLC{
			PaymentHash: paymentHash,
			Amount:      btcutil.Amount(1e7),
			Expiry:      uint32(5),
		}
	}

	checkError := func(err error) error {
		if err == nil {
			return errors.New("Exceed max htlc count error was " +
				"not received")
		} else if err != ErrMaxHTLCNumber {
			return errors.Errorf("Unexpected error occured: %v", err)
		}

		return nil
	}

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(1)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Add max available number of HTLCs.
	for i := 0; i < MaxHTLCNumber; i++ {
		htlc := createHTLC(i)
		if _, err := aliceChannel.AddHTLC(htlc); err != nil {
			t.Fatalf("alice unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("bob unable to receive htlc: %v", err)
		}
	}

	// Next addition should cause HTLC max number validation error.
	htlc := createHTLC(0)
	if _, err := aliceChannel.AddHTLC(htlc); err != nil {
		if err := checkError(err); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("Error was not received")
	}
	if _, err := bobChannel.AddHTLC(htlc); err != nil {
		if err := checkError(err); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("Error was not received")
	}
	if _, err := aliceChannel.ReceiveHTLC(htlc); err != nil {
		if err := checkError(err); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("Error was not received")
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		if err := checkError(err); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("Error was not received")
	}

	// Manually add HTLC to check SignNextCommitment validation error.
	aliceChannel.localUpdateLog.appendUpdate(
		&PaymentDescriptor{
			Index: aliceChannel.localUpdateLog.logIndex,
		},
	)

	_, err = aliceChannel.SignNextCommitment()
	if err := checkError(err); err != nil {
		t.Fatal(err)
	}

	// Manually add HTLC to check ReceiveNewCommitment validation error.
	bobChannel.remoteUpdateLog.appendUpdate(
		&PaymentDescriptor{
			Index: bobChannel.remoteUpdateLog.logIndex,
		},
	)

	// And on this stage we should receive the weight error.
	someSig := []byte("somesig")
	err = bobChannel.ReceiveNewCommitment(someSig)
	if err := checkError(err); err != nil {
		t.Fatal(err)
	}

}

// TestForceClose checks that the resulting ForceCloseSummary is correct when
// a peer is ForceClosing the channel. Will check outputs both above and below
// the dust limit.
// TODO(cjamthagen): Check HTLCs when implemented.
func TestForceClose(t *testing.T) {
	createHTLC := func(data, amount btcutil.Amount) (*lnwire.UpdateAddHTLC,
		[32]byte) {
		preimage := bytes.Repeat([]byte{byte(data)}, 32)
		paymentHash := sha256.Sum256(preimage)

		var returnPreimage [32]byte
		copy(returnPreimage[:], preimage)

		return &lnwire.UpdateAddHTLC{
			PaymentHash: paymentHash,
			Amount:      amount,
			Expiry:      uint32(5),
		}, returnPreimage
	}

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(3)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	htlcAmount := btcutil.Amount(500)

	aliceAmount := aliceChannel.channelState.OurBalance
	bobAmount := bobChannel.channelState.OurBalance

	closeSummary, err := aliceChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}

	// The SelfOutputSignDesc should be non-nil since the outout to-self is non-dust.
	if closeSummary.SelfOutputSignDesc == nil {
		t.Fatalf("alice fails to include to-self output in ForceCloseSummary")
	} else {
		if closeSummary.SelfOutputSignDesc.PubKey !=
			aliceChannel.channelState.OurCommitKey {
			t.Fatalf("alice incorrect pubkey in SelfOutputSignDesc")
		}
		if closeSummary.SelfOutputSignDesc.Output.Value != int64(aliceAmount) {
			t.Fatalf("alice incorrect output value in SelfOutputSignDesc, "+
				"expected %v, got %v",
				aliceChannel.channelState.OurBalance,
				closeSummary.SelfOutputSignDesc.Output.Value)
		}
	}

	if closeSummary.SelfOutputMaturity != aliceChannel.channelState.LocalCsvDelay {
		t.Fatalf("alice: incorrect local CSV delay in ForceCloseSummary, "+
			"expected %v, got %v", aliceChannel.channelState.LocalCsvDelay,
			closeSummary.SelfOutputMaturity)
	}

	closeTxHash := closeSummary.CloseTx.TxHash()
	commitTxHash := aliceChannel.channelState.OurCommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("alice: incorrect close transaction txid")
	}

	// Check the same for Bobs' ForceCloseSummary
	closeSummary, err = bobChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}
	if closeSummary.SelfOutputSignDesc == nil {
		t.Fatalf("bob fails to include to-self output in ForceCloseSummary")
	} else {
		if closeSummary.SelfOutputSignDesc.PubKey !=
			bobChannel.channelState.OurCommitKey {
			t.Fatalf("bob incorrect pubkey in SelfOutputSignDesc")
		}
		if closeSummary.SelfOutputSignDesc.Output.Value != int64(bobAmount) {
			t.Fatalf("bob incorrect output value in SelfOutputSignDesc, "+
				"expected %v, got %v",
				bobChannel.channelState.OurBalance,
				closeSummary.SelfOutputSignDesc.Output.Value)
		}
	}

	if closeSummary.SelfOutputMaturity != bobChannel.channelState.LocalCsvDelay {
		t.Fatalf("bob: incorrect local CSV delay in ForceCloseSummary, "+
			"expected %v, got %v", bobChannel.channelState.LocalCsvDelay,
			closeSummary.SelfOutputMaturity)
	}

	closeTxHash = closeSummary.CloseTx.TxHash()
	commitTxHash = bobChannel.channelState.OurCommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("bob: incorrect close transaction txid")
	}

	// "re-open" channels
	aliceChannel.status = channelOpen
	bobChannel.status = channelOpen
	aliceChannel.ForceCloseSignal = make(chan struct{})
	bobChannel.ForceCloseSignal = make(chan struct{})

	// Have Bobs' to-self output be below her dust limit and check
	// ForceCloseSummary again on both peers.
	htlc, preimage := createHTLC(0, bobAmount-htlcAmount)
	if _, err := bobChannel.AddHTLC(htlc); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := forceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// Settle HTLC and sign new commitment.
	settleIndex, err := aliceChannel.SettleHTLC(preimage)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	err = bobChannel.ReceiveHTLCSettle(preimage, settleIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := forceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	aliceAmount = aliceChannel.channelState.OurBalance
	bobAmount = bobChannel.channelState.OurBalance

	closeSummary, err = aliceChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}

	// Alices' to-self output should still be in the commitment transaction.
	if closeSummary.SelfOutputSignDesc == nil {
		t.Fatalf("alice fails to include to-self output in ForceCloseSummary")
	} else {
		if closeSummary.SelfOutputSignDesc.PubKey !=
			aliceChannel.channelState.OurCommitKey {
			t.Fatalf("alice incorrect pubkey in SelfOutputSignDesc")
		}
		if closeSummary.SelfOutputSignDesc.Output.Value !=
			int64(aliceAmount) {
			t.Fatalf("alice incorrect output value in SelfOutputSignDesc, "+
				"expected %v, got %v",
				aliceChannel.channelState.OurBalance,
				closeSummary.SelfOutputSignDesc.Output.Value)
		}
	}

	if closeSummary.SelfOutputMaturity != aliceChannel.channelState.LocalCsvDelay {
		t.Fatalf("alice: incorrect local CSV delay in ForceCloseSummary, "+
			"expected %v, got %v", aliceChannel.channelState.LocalCsvDelay,
			closeSummary.SelfOutputMaturity)
	}

	closeTxHash = closeSummary.CloseTx.TxHash()
	commitTxHash = aliceChannel.channelState.OurCommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("alice: incorrect close transaction txid")
	}

	closeSummary, err = bobChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}
	// Bob's to-self output is below Bob's dust value and should be
	// reflected in the ForceCloseSummary.
	if closeSummary.SelfOutputSignDesc != nil {
		t.Fatalf("bob incorrectly includes to-self output in ForceCloseSummary")
	}

	closeTxHash = closeSummary.CloseTx.TxHash()
	commitTxHash = bobChannel.channelState.OurCommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("bob: incorrect close transaction txid")
	}
}

// TestCheckDustLimit checks that unsettled HTLC with dust limit not included in
// commitment transaction as output, but sender balance is decreased (thereby all
// unsettled dust HTLCs will go to miners fee).
func TestCheckDustLimit(t *testing.T) {
	createHTLC := func(data, amount btcutil.Amount) (*lnwire.UpdateAddHTLC,
		[32]byte) {
		preimage := bytes.Repeat([]byte{byte(data)}, 32)
		paymentHash := sha256.Sum256(preimage)

		var returnPreimage [32]byte
		copy(returnPreimage[:], preimage)

		return &lnwire.UpdateAddHTLC{
			PaymentHash: paymentHash,
			Amount:      amount,
			Expiry:      uint32(5),
		}, returnPreimage
	}

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(3)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	assertChannelState := func(chanA, chanB *LightningChannel, numOutsA,
		numOutsB int, amountSentA, amountSentB uint64) {

		commitment := chanA.localCommitChain.tip()
		if len(commitment.txn.TxOut) != numOutsA {
			t.Fatalf("incorrect # of outputs: expected %v, got %v",
				numOutsA, len(commitment.txn.TxOut))
		}
		commitment = chanB.localCommitChain.tip()
		if len(commitment.txn.TxOut) != numOutsB {
			t.Fatalf("Incorrect # of outputs: expected %v, got %v",
				numOutsB, len(commitment.txn.TxOut))
		}

		if chanA.channelState.TotalSatoshisSent != amountSentA {
			t.Fatalf("alice satoshis sent incorrect: expected %v, "+
				"got %v", amountSentA,
				chanA.channelState.TotalSatoshisSent)
		}
		if chanA.channelState.TotalSatoshisReceived != amountSentB {
			t.Fatalf("alice satoshis received incorrect: expected"+
				"%v, got %v", amountSentB,
				chanA.channelState.TotalSatoshisReceived)
		}
		if chanB.channelState.TotalSatoshisSent != amountSentB {
			t.Fatalf("bob satoshis sent incorrect: expected %v, "+
				" got %v", amountSentB,
				chanB.channelState.TotalSatoshisSent)
		}
		if chanB.channelState.TotalSatoshisReceived != amountSentA {
			t.Fatalf("bob satoshis received incorrect: expected "+
				"%v, got %v", amountSentA,
				chanB.channelState.TotalSatoshisReceived)
		}
	}

	aliceDustLimit := aliceChannel.channelState.OurDustLimit
	bobDustLimit := bobChannel.channelState.OurDustLimit
	htlcAmount := btcutil.Amount(500)

	if !((htlcAmount > aliceDustLimit) && (bobDustLimit > htlcAmount)) {
		t.Fatalf("htlc amount (%v) should be above Alice's dust limit (%v),"+
			"and below Bob's dust limit (%v).", htlcAmount, aliceDustLimit,
			bobDustLimit)
	}

	htlc, preimage := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlc); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := forceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// First two outputs are payment to them and to us. If we encounter
	// third output it means that dust HTLC was included. Their channel
	// balance shouldn't change because, it will be changed only after HTLC
	// will be settled.

	// From Alice point of view HTLC's amount is bigger than dust limit.
	// From Bob point of view HTLC's amount is lower then dust limit.
	assertChannelState(aliceChannel, bobChannel, 3, 2, 0, 0)

	// Settle HTLC and sign new commitment.
	settleIndex, err := bobChannel.SettleHTLC(preimage)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(preimage, settleIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := forceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("state transition error: %v", err)
	}

	assertChannelState(aliceChannel, bobChannel, 2, 2, uint64(htlcAmount), 0)

	// Next we will test when a non-HTLC output in the commitment
	// transaction is below the dust limit.  We create an HTLC that will
	// only leave a small enough amount to Alice such that Bob will
	// consider it a dust output.

	aliceAmount := btcutil.Amount(5e8)
	htlcAmount2 := aliceAmount - btcutil.Amount(1000)
	htlc, preimage = createHTLC(0, htlcAmount2)
	if _, err := aliceChannel.AddHTLC(htlc); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := forceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("state transition error: %v", err)
	}

	// From Alice's point of view, her output is bigger than the dust limit
	// From Bob's point of view, Alice's output is lower than the dust limit
	assertChannelState(aliceChannel, bobChannel, 3, 2, uint64(htlcAmount), 0)

	// Settle HTLC and sign new commitment.
	settleIndex, err = bobChannel.SettleHTLC(preimage)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(preimage, settleIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := forceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("state transition error: %v", err)
	}

	assertChannelState(aliceChannel, bobChannel, 2, 1,
		uint64(htlcAmount+htlcAmount2), 0)
}

func TestStateUpdatePersistence(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(1)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	if err := aliceChannel.channelState.FullSync(); err != nil {
		t.Fatalf("unable to sync alice's channel: %v", err)
	}
	if err := bobChannel.channelState.FullSync(); err != nil {
		t.Fatalf("unable to sync bob's channel: %v", err)
	}

	const numHtlcs = 4

	// Alice adds 3 HTLCs to the update log, while Bob adds a single HTLC.
	var alicePreimage [32]byte
	copy(alicePreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xbb}, 32))
	for i := 0; i < 3; i++ {
		rHash := sha256.Sum256(alicePreimage[:])
		h := &lnwire.UpdateAddHTLC{
			PaymentHash: rHash,
			Amount:      btcutil.Amount(5000),
			Expiry:      uint32(10),
		}

		if _, err := aliceChannel.AddHTLC(h); err != nil {
			t.Fatalf("unable to add alice's htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(h); err != nil {
			t.Fatalf("unable to recv alice's htlc: %v", err)
		}
	}
	rHash := sha256.Sum256(bobPreimage[:])
	bobh := &lnwire.UpdateAddHTLC{
		PaymentHash: rHash,
		Amount:      btcutil.Amount(5000),
		Expiry:      uint32(10),
	}
	if _, err := bobChannel.AddHTLC(bobh); err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(bobh); err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}

	// Next, Alice initiates a state transition to include the HTLC's she
	// added above in a new commitment state.
	if err := forceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete alice's state transition: %v", err)
	}

	// Since the HTLC Bob sent wasn't included in Bob's version of the
	// commitment transaction (but it was in Alice's, as he ACK'd her
	// changes before creating a new state), Bob needs to trigger another
	// state update in order to re-sync their states.
	if err := forceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// The latest commitment from both sides should have all the HTLCs.
	numAliceOutgoing := aliceChannel.localCommitChain.tail().outgoingHTLCs
	numAliceIncoming := aliceChannel.localCommitChain.tail().incomingHTLCs
	if len(numAliceOutgoing) != 3 {
		t.Fatalf("expected %v htlcs, instead got %v", 3, numAliceOutgoing)
	}
	if len(numAliceIncoming) != 1 {
		t.Fatalf("expected %v htlcs, instead got %v", 1, numAliceIncoming)
	}
	numBobOutgoing := bobChannel.localCommitChain.tail().outgoingHTLCs
	numBobIncoming := bobChannel.localCommitChain.tail().incomingHTLCs
	if len(numBobOutgoing) != 1 {
		t.Fatalf("expected %v htlcs, instead got %v", 1, numBobOutgoing)
	}
	if len(numBobIncoming) != 3 {
		t.Fatalf("expected %v htlcs, instead got %v", 3, numBobIncoming)
	}

	// Now fetch both of the channels created above from disk to simulate a
	// node restart with persistence.
	alicePub := aliceChannel.channelState.IdentityPub
	aliceChannels, err := aliceChannel.channelState.Db.FetchOpenChannels(alicePub)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}
	bobPub := bobChannel.channelState.IdentityPub
	bobChannels, err := bobChannel.channelState.Db.FetchOpenChannels(bobPub)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}
	notifier := aliceChannel.channelEvents
	aliceChannelNew, err := NewLightningChannel(aliceChannel.signer,
		notifier, aliceChannel.feeEstimator, aliceChannels[0])
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}
	bobChannelNew, err := NewLightningChannel(bobChannel.signer, notifier,
		bobChannel.feeEstimator, bobChannels[0])
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}
	if err := initRevocationWindows(aliceChannelNew, bobChannelNew, 3); err != nil {
		t.Fatalf("unable to init revocation windows: %v", err)
	}

	// The state update logs of the new channels and the old channels
	// should now be identical other than the height the HTLCs were added.
	if aliceChannel.localUpdateLog.logIndex !=
		aliceChannelNew.localUpdateLog.logIndex {
		t.Fatalf("alice log counter: expected %v, got %v",
			aliceChannel.localUpdateLog.logIndex,
			aliceChannelNew.localUpdateLog.logIndex)
	}
	if aliceChannel.remoteUpdateLog.logIndex !=
		aliceChannelNew.remoteUpdateLog.logIndex {
		t.Fatalf("alice log counter: expected %v, got %v",
			aliceChannel.remoteUpdateLog.logIndex,
			aliceChannelNew.remoteUpdateLog.logIndex)
	}
	if aliceChannel.localUpdateLog.Len() !=
		aliceChannelNew.localUpdateLog.Len() {
		t.Fatalf("alice log len: expected %v, got %v",
			aliceChannel.localUpdateLog.Len(),
			aliceChannelNew.localUpdateLog.Len())
	}
	if aliceChannel.remoteUpdateLog.Len() !=
		aliceChannelNew.remoteUpdateLog.Len() {
		t.Fatalf("alice log len: expected %v, got %v",
			aliceChannel.remoteUpdateLog.Len(),
			aliceChannelNew.remoteUpdateLog.Len())
	}
	if bobChannel.localUpdateLog.logIndex !=
		bobChannelNew.localUpdateLog.logIndex {
		t.Fatalf("bob log counter: expected %v, got %v",
			bobChannel.localUpdateLog.logIndex,
			bobChannelNew.localUpdateLog.logIndex)
	}
	if bobChannel.remoteUpdateLog.logIndex !=
		bobChannelNew.remoteUpdateLog.logIndex {
		t.Fatalf("bob log counter: expected %v, got %v",
			bobChannel.remoteUpdateLog.logIndex,
			bobChannelNew.remoteUpdateLog.logIndex)
	}
	if bobChannel.localUpdateLog.Len() !=
		bobChannelNew.localUpdateLog.Len() {
		t.Fatalf("bob log len: expected %v, got %v",
			bobChannel.localUpdateLog.Len(),
			bobChannelNew.localUpdateLog.Len())
	}
	if bobChannel.remoteUpdateLog.Len() !=
		bobChannelNew.remoteUpdateLog.Len() {
		t.Fatalf("bob log len: expected %v, got %v",
			bobChannel.remoteUpdateLog.Len(),
			bobChannelNew.remoteUpdateLog.Len())
	}

	// Newly generated pkScripts for HTLCs should be the same as in the old channel.
	for _, entry := range aliceChannel.localUpdateLog.updateIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := aliceChannelNew.localUpdateLog.lookup(htlc.Index)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("alice ourPkScript in ourLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("alice theirPkScript in ourLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}
	for _, entry := range aliceChannel.remoteUpdateLog.updateIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := aliceChannelNew.remoteUpdateLog.lookup(htlc.Index)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("alice ourPkScript in theirLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("alice theirPkScript in theirLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}
	for _, entry := range bobChannel.localUpdateLog.updateIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := bobChannelNew.localUpdateLog.lookup(htlc.Index)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("bob ourPkScript in ourLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("bob theirPkScript in ourLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}
	for _, entry := range bobChannel.remoteUpdateLog.updateIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := bobChannelNew.remoteUpdateLog.lookup(htlc.Index)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("bob ourPkScript in theirLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("bob theirPkScript in theirLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}

	// Now settle all the HTLCs, then force a state update. The state
	// update should suceed as both sides have identical.
	for i := 0; i < 3; i++ {
		settleIndex, err := bobChannelNew.SettleHTLC(alicePreimage)
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
		err = aliceChannelNew.ReceiveHTLCSettle(alicePreimage, settleIndex)
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
	}
	settleIndex, err := aliceChannelNew.SettleHTLC(bobPreimage)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = bobChannelNew.ReceiveHTLCSettle(bobPreimage, settleIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}

	// Similar to the two transitions above, as both Bob and Alice added
	// entries to the update log before a state transition was initiated by
	// either side, both sides are required to trigger an update in order
	// to lock in their changes.
	if err := forceStateTransition(aliceChannelNew, bobChannelNew); err != nil {
		t.Fatalf("unable to update commitments: %v", err)
	}
	if err := forceStateTransition(bobChannelNew, aliceChannelNew); err != nil {
		t.Fatalf("unable to update commitments: %v", err)
	}

	// The amounts transferred should been updated as per the amounts in
	// the HTLCs
	if aliceChannelNew.channelState.TotalSatoshisSent != 15000 {
		t.Fatalf("expected %v alice satoshis sent, got %v",
			15000, aliceChannelNew.channelState.TotalSatoshisSent)
	}
	if aliceChannelNew.channelState.TotalSatoshisReceived != 5000 {
		t.Fatalf("expected %v alice satoshis received, got %v",
			5000, aliceChannelNew.channelState.TotalSatoshisReceived)
	}
	if bobChannelNew.channelState.TotalSatoshisSent != 5000 {
		t.Fatalf("expected %v bob satoshis sent, got %v",
			5000, bobChannel.channelState.TotalSatoshisSent)
	}
	if bobChannelNew.channelState.TotalSatoshisReceived != 15000 {
		t.Fatalf("expected %v bob satoshis sent, got %v",
			15000, bobChannel.channelState.TotalSatoshisSent)
	}
}

func TestCancelHTLC(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(5)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Add a new HTLC from Alice to Bob, then trigger a new state
	// transition in order to include it in the latest state.
	const htlcAmt = btcutil.SatoshiPerBitcoin

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{0xaa}, 32))
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: sha256.Sum256(preImage[:]),
		Amount:      htlcAmt,
		Expiry:      10,
	}
	paymentHash := htlc.PaymentHash

	if _, err := aliceChannel.AddHTLC(htlc); err != nil {
		t.Fatalf("unable to add alice htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to add bob htlc: %v", err)
	}
	if err := forceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to create new commitment state: %v", err)
	}

	// With the HTLC committed, Alice's balance should reflect the clearing
	// of the new HTLC.
	aliceExpectedBalance := btcutil.Amount(btcutil.SatoshiPerBitcoin*4) -
		calcStaticFee(1)
	if aliceChannel.channelState.OurBalance != aliceExpectedBalance {
		t.Fatalf("Alice's balance is wrong: expected %v, got %v",
			aliceExpectedBalance, aliceChannel.channelState.OurBalance)
	}

	// Now, with the HTLC committed on both sides, trigger a cancellation
	// from Bob to Alice, removing the HTLC.
	htlcCancelIndex, err := bobChannel.FailHTLC(paymentHash)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	if err := aliceChannel.ReceiveFailHTLC(htlcCancelIndex); err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// Now trigger another state transition, the HTLC should now be removed
	// from both sides, with balances reflected.
	if err := forceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to create new commitment: %v", err)
	}

	// Now HTLCs should be present on the commitment transaction for
	// either side.
	if len(aliceChannel.localCommitChain.tip().outgoingHTLCs) != 0 ||
		len(aliceChannel.remoteCommitChain.tip().outgoingHTLCs) != 0 {
		t.Fatalf("htlc's still active from alice's POV")
	}
	if len(aliceChannel.localCommitChain.tip().incomingHTLCs) != 0 ||
		len(aliceChannel.remoteCommitChain.tip().incomingHTLCs) != 0 {
		t.Fatalf("htlc's still active from alice's POV")
	}
	if len(bobChannel.localCommitChain.tip().outgoingHTLCs) != 0 ||
		len(bobChannel.remoteCommitChain.tip().outgoingHTLCs) != 0 {
		t.Fatalf("htlc's still active from bob's POV")
	}
	if len(bobChannel.localCommitChain.tip().incomingHTLCs) != 0 ||
		len(bobChannel.remoteCommitChain.tip().incomingHTLCs) != 0 {
		t.Fatalf("htlc's still active from bob's POV")
	}

	expectedBalance := btcutil.Amount(btcutil.SatoshiPerBitcoin * 5)
	if aliceChannel.channelState.OurBalance != expectedBalance-calcStaticFee(0) {
		t.Fatalf("balance is wrong: expected %v, got %v",
			aliceChannel.channelState.OurBalance, expectedBalance-
				calcStaticFee(0))
	}
	if aliceChannel.channelState.TheirBalance != expectedBalance {
		t.Fatalf("balance is wrong: expected %v, got %v",
			aliceChannel.channelState.TheirBalance, expectedBalance)
	}
	if bobChannel.channelState.OurBalance != expectedBalance {
		t.Fatalf("balance is wrong: expected %v, got %v",
			bobChannel.channelState.OurBalance, expectedBalance)
	}
	if bobChannel.channelState.TheirBalance != expectedBalance-calcStaticFee(0) {
		t.Fatalf("balance is wrong: expected %v, got %v",
			bobChannel.channelState.TheirBalance,
			expectedBalance-calcStaticFee(0))
	}
}

func TestCooperativeCloseDustAdherence(t *testing.T) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := createTestChannels(5)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	setDustLimit := func(dustVal btcutil.Amount) {
		aliceChannel.channelState.OurDustLimit = dustVal
		aliceChannel.channelState.TheirDustLimit = dustVal
		aliceChannel.channelState.OurDustLimit = dustVal
		aliceChannel.channelState.TheirDustLimit = dustVal
	}

	resetChannelState := func() {
		aliceChannel.status = channelOpen
		bobChannel.status = channelOpen
	}

	setBalances := func(aliceBalance, bobBalance btcutil.Amount) {
		aliceChannel.channelState.OurBalance = aliceBalance
		aliceChannel.channelState.TheirBalance = bobBalance
		bobChannel.channelState.OurBalance = bobBalance
		bobChannel.channelState.TheirBalance = aliceBalance
	}

	// We'll start be initializing the limit of both Alice and Bob to 10k
	// satoshis.
	dustLimit := btcutil.Amount(10000)
	setDustLimit(dustLimit)

	// Both sides currently have over 1 BTC settled as part of their
	// balances. As a result, performing a cooperative closure now result
	// in both sides having an output within the closure transaction.
	closeSig, _, err := aliceChannel.InitCooperativeClose()
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	closeSig = append(closeSig, byte(txscript.SigHashAll))
	closeTx, err := bobChannel.CompleteCooperativeClose(closeSig)
	if err != nil {
		t.Fatalf("unable to accept channel close: %v", err)
	}

	// The closure transaction should have exactly two outputs.
	if len(closeTx.TxOut) != 2 {
		t.Fatalf("close tx has wrong number of outputs: expected %v "+
			"got %v", 2, len(closeTx.TxOut))
	}

	// We'll reset the channel states before proceeding to our nest test.
	resetChannelState()

	// Next we'll modify the current balances and dust limits such that
	// Bob's current balance is above _below_ his dust limit.
	aliceBal := btcutil.Amount(btcutil.SatoshiPerBitcoin)
	bobBal := btcutil.Amount(250)
	setBalances(aliceBal, bobBal)

	// Attempt another cooperative channel closure. It should succeed
	// without any issues.
	closeSig, _, err = aliceChannel.InitCooperativeClose()
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	closeSig = append(closeSig, byte(txscript.SigHashAll))
	closeTx, err = bobChannel.CompleteCooperativeClose(closeSig)
	if err != nil {
		t.Fatalf("unable to accept channel close: %v", err)
	}

	// The closure transaction should only have a single output, and that
	// output should be Alice's balance.
	if len(closeTx.TxOut) != 1 {
		t.Fatalf("close tx has wrong number of outputs: expected %v "+
			"got %v", 1, len(closeTx.TxOut))
	}
	if closeTx.TxOut[0].Value != int64(aliceBal-calcStaticFee(0)) {
		t.Fatalf("alice's balance is incorrect: expected %v, got %v",
			aliceBal-calcStaticFee(0), closeTx.TxOut[0].Value)
	}

	// Finally, we'll modify the current balances and dust limits such that
	// Alice's current balance is _below_ his her limit.
	setBalances(bobBal, aliceBal)
	resetChannelState()

	// Our final attempt at another cooperative channel closure. It should
	// succeed without any issues.
	closeSig, _, err = aliceChannel.InitCooperativeClose()
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	closeSig = append(closeSig, byte(txscript.SigHashAll))
	closeTx, err = bobChannel.CompleteCooperativeClose(closeSig)
	if err != nil {
		t.Fatalf("unable to accept channel close: %v", err)
	}

	// The closure transaction should only have a single output, and that
	// output should be Bob's balance.
	if len(closeTx.TxOut) != 1 {
		t.Fatalf("close tx has wrong number of outputs: expected %v "+
			"got %v", 1, len(closeTx.TxOut))
	}
	if closeTx.TxOut[0].Value != int64(aliceBal) {
		t.Fatalf("bob's balance is incorrect: expected %v, got %v",
			aliceBal, closeTx.TxOut[0].Value)
	}
}
