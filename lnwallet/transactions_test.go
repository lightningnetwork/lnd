package lnwallet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

/**
* This file implements that different types of transactions used in the
* lightning protocol are created correctly. To do so, the tests use the test
* vectors defined in Appendix B & C of BOLT 03.
 */

// testContext contains the test parameters defined in Appendix B & C of the
// BOLT 03 spec.
type testContext struct {
	netParams *chaincfg.Params
	block1    *btcutil.Block

	fundingInputPrivKey *btcec.PrivateKey
	localFundingPrivKey *btcec.PrivateKey
	localPaymentPrivKey *btcec.PrivateKey

	remoteFundingPubKey    *btcec.PublicKey
	localFundingPubKey     *btcec.PublicKey
	localRevocationPubKey  *btcec.PublicKey
	localPaymentPubKey     *btcec.PublicKey
	remotePaymentPubKey    *btcec.PublicKey
	localDelayPubKey       *btcec.PublicKey
	commitmentPoint        *btcec.PublicKey
	localPaymentBasePoint  *btcec.PublicKey
	remotePaymentBasePoint *btcec.PublicKey

	fundingChangeAddress btcutil.Address
	fundingInputUtxo     *Utxo
	fundingInputTxOut    *wire.TxOut
	fundingTx            *btcutil.Tx
	fundingOutpoint      wire.OutPoint
	shortChanID          lnwire.ShortChannelID

	htlcs []channeldb.HTLC

	localCsvDelay uint16
	fundingAmount btcutil.Amount
	dustLimit     btcutil.Amount
	feePerKW      btcutil.Amount
}

// htlcDesc is a description used to construct each HTLC in each test case.
type htlcDesc struct {
	index           int
	remoteSigHex    string
	resolutionTxHex string
}

// getHTLC constructs an HTLC based on a configured HTLC with auxiliary data
// such as the remote signature from the htlcDesc. The partially defined HTLCs
// originate from the BOLT 03 spec and are contained in the test context.
func (tc *testContext) getHTLC(index int, desc *htlcDesc) (channeldb.HTLC, error) {
	signature, err := hex.DecodeString(desc.remoteSigHex)
	if err != nil {
		return channeldb.HTLC{}, fmt.Errorf(
			"Failed to parse serialized signature: %v", err)
	}

	htlc := tc.htlcs[desc.index]
	return channeldb.HTLC{
		Signature:     signature,
		RHash:         htlc.RHash,
		RefundTimeout: htlc.RefundTimeout,
		Amt:           htlc.Amt,
		OutputIndex:   int32(index),
		Incoming:      htlc.Incoming,
	}, nil
}

// newTestContext populates a new testContext struct with the constant
// parameters defined in the BOLT 03 spec. This may return an error if any of
// the serialized parameters cannot be parsed.
func newTestContext() (tc *testContext, err error) {
	tc = new(testContext)

	const genesisHash = "0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206"
	if tc.netParams, err = tc.createNetParams(genesisHash); err != nil {
		return
	}

	const block1Hex = "0000002006226e46111a0b59caaf126043eb5bbf28c34f3a5e332a1fc7b2b73cf188910fadbb20ea41a8423ea937e76e8151636bf6093b70eaff942930d20576600521fdc30f9858ffff7f20000000000101000000010000000000000000000000000000000000000000000000000000000000000000ffffffff03510101ffffffff0100f2052a010000001976a9143ca33c2e4446f4a305f23c80df8ad1afdcf652f988ac00000000"
	if tc.block1, err = blockFromHex(block1Hex); err != nil {
		err = fmt.Errorf("Failed to parse serialized block: %v", err)
		return
	}

	const fundingInputPrivKeyHex = "6bd078650fcee8444e4e09825227b801a1ca928debb750eb36e6d56124bb20e8"
	tc.fundingInputPrivKey, err = privkeyFromHex(fundingInputPrivKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized privkey: %v", err)
		return
	}

	const localFundingPrivKeyHex = "30ff4956bbdd3222d44cc5e8a1261dab1e07957bdac5ae88fe3261ef321f3749"
	tc.localFundingPrivKey, err = privkeyFromHex(localFundingPrivKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized privkey: %v", err)
		return
	}

	const localPaymentPrivKeyHex = "bb13b121cdc357cd2e608b0aea294afca36e2b34cf958e2e6451a2f274694491"
	tc.localPaymentPrivKey, err = privkeyFromHex(localPaymentPrivKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized privkey: %v", err)
		return
	}

	const localFundingPubKeyHex = "023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb"
	tc.localFundingPubKey, err = pubkeyFromHex(localFundingPubKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const remoteFundingPubKeyHex = "030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c1"
	tc.remoteFundingPubKey, err = pubkeyFromHex(remoteFundingPubKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const localRevocationPubKeyHex = "0212a140cd0c6539d07cd08dfe09984dec3251ea808b892efeac3ede9402bf2b19"
	tc.localRevocationPubKey, err = pubkeyFromHex(localRevocationPubKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const localPaymentPubKeyHex = "030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e7"
	tc.localPaymentPubKey, err = pubkeyFromHex(localPaymentPubKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const remotePaymentPubKeyHex = "0394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b"
	tc.remotePaymentPubKey, err = pubkeyFromHex(remotePaymentPubKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const localDelayPubKeyHex = "03fd5960528dc152014952efdb702a88f71e3c1653b2314431701ec77e57fde83c"
	tc.localDelayPubKey, err = pubkeyFromHex(localDelayPubKeyHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const commitmentPointHex = "025f7117a78150fe2ef97db7cfc83bd57b2e2c0d0dd25eaf467a4a1c2a45ce1486"
	tc.commitmentPoint, err = pubkeyFromHex(commitmentPointHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const localPaymentBasePointHex = "034f355bdcb7cc0af728ef3cceb9615d90684bb5b2ca5f859ab0f0b704075871aa"
	tc.localPaymentBasePoint, err = pubkeyFromHex(localPaymentBasePointHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const remotePaymentBasePointHex = "032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991"
	tc.remotePaymentBasePoint, err = pubkeyFromHex(remotePaymentBasePointHex)
	if err != nil {
		err = fmt.Errorf("Failed to parse serialized pubkey: %v", err)
		return
	}

	const fundingChangeAddressStr = "bcrt1qgyeqfmptyh780dsk32qawsvdffc2g5q5sxamg0"
	tc.fundingChangeAddress, err = btcutil.DecodeAddress(
		fundingChangeAddressStr, tc.netParams)
	if err != nil {
		err = fmt.Errorf("Failed to parse address: %v", err)
		return
	}

	tc.fundingInputUtxo, tc.fundingInputTxOut, err = tc.extractFundingInput()
	if err != nil {
		return
	}

	const fundingTxHex = "0200000001adbb20ea41a8423ea937e76e8151636bf6093b70eaff942930d20576600521fd000000006b48304502210090587b6201e166ad6af0227d3036a9454223d49a1f11839c1a362184340ef0240220577f7cd5cca78719405cbf1de7414ac027f0239ef6e214c90fcaab0454d84b3b012103535b32d5eb0a6ed0982a0479bbadc9868d9836f6ba94dd5a63be16d875069184ffffffff028096980000000000220020c015c4a6be010e21657068fc2e6a9d02b27ebe4d490a25846f7237f104d1a3cd20256d29010000001600143ca33c2e4446f4a305f23c80df8ad1afdcf652f900000000"
	if tc.fundingTx, err = txFromHex(fundingTxHex); err != nil {
		err = fmt.Errorf("Failed to parse serialized tx: %v", err)
		return
	}

	tc.fundingOutpoint = wire.OutPoint{
		Hash:  *tc.fundingTx.Hash(),
		Index: 0,
	}

	tc.shortChanID = lnwire.ShortChannelID{
		BlockHeight: 1,
		TxIndex:     0,
		TxPosition:  0,
	}

	htlcData := []struct {
		incoming bool
		amount   lnwire.MilliSatoshi
		expiry   uint32
		preimage string
	}{
		{
			incoming: true,
			amount:   1000000,
			expiry:   500,
			preimage: "0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			incoming: true,
			amount:   2000000,
			expiry:   501,
			preimage: "0101010101010101010101010101010101010101010101010101010101010101",
		},
		{
			incoming: false,
			amount:   2000000,
			expiry:   502,
			preimage: "0202020202020202020202020202020202020202020202020202020202020202",
		},
		{
			incoming: false,
			amount:   3000000,
			expiry:   503,
			preimage: "0303030303030303030303030303030303030303030303030303030303030303",
		},
		{
			incoming: true,
			amount:   4000000,
			expiry:   504,
			preimage: "0404040404040404040404040404040404040404040404040404040404040404",
		},
	}

	tc.htlcs = make([]channeldb.HTLC, len(htlcData))
	for i, htlc := range htlcData {
		preimage, decodeErr := hex.DecodeString(htlc.preimage)
		if decodeErr != nil {
			err = fmt.Errorf("Failed to decode HTLC preimage: %v", decodeErr)
			return
		}

		tc.htlcs[i].RHash = sha256.Sum256(preimage)
		tc.htlcs[i].Amt = htlc.amount
		tc.htlcs[i].RefundTimeout = htlc.expiry
		tc.htlcs[i].Incoming = htlc.incoming
	}

	tc.localCsvDelay = 144
	tc.fundingAmount = 10000000
	tc.dustLimit = 546
	tc.feePerKW = 15000

	return
}

// createNetParams is used by newTestContext to construct new chain parameters
// as required by the BOLT 03 spec.
func (tc *testContext) createNetParams(genesisHashStr string) (*chaincfg.Params, error) {
	params := chaincfg.RegressionNetParams

	// Ensure regression net genesis block matches the one listed in BOLT spec.
	expectedGenesisHash, err := chainhash.NewHashFromStr(genesisHashStr)
	if err != nil {
		return nil, err
	}
	if !params.GenesisHash.IsEqual(expectedGenesisHash) {
		err = fmt.Errorf("Expected regression net genesis hash to be %s, "+
			"got %s", expectedGenesisHash, params.GenesisHash)
		return nil, err
	}

	return &params, nil
}

// extractFundingInput returns references to the transaction output of the
// coinbase transaction which is used to fund the channel in the test vectors.
func (tc *testContext) extractFundingInput() (*Utxo, *wire.TxOut, error) {
	expectedTxHashHex := "fd2105607605d2302994ffea703b09f66b6351816ee737a93e42a841ea20bbad"
	expectedTxHash, err := chainhash.NewHashFromStr(expectedTxHashHex)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to parse transaction hash: %v", err)
	}

	tx, err := tc.block1.Tx(0)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to get coinbase transaction from "+
			"block 1: %v", err)
	}
	txout := tx.MsgTx().TxOut[0]

	var expectedAmount int64 = 5000000000
	if txout.Value != expectedAmount {
		return nil, nil, fmt.Errorf("Coinbase transaction output amount from "+
			"block 1 does not match expected output amount: "+
			"expected %v, got %v", expectedAmount, txout.Value)
	}
	if !tx.Hash().IsEqual(expectedTxHash) {
		return nil, nil, fmt.Errorf("Coinbase transaction hash from block 1 "+
			"does not match expected hash: expected %v, got %v", expectedTxHash,
			tx.Hash())
	}

	block1Utxo := Utxo{
		AddressType: WitnessPubKey,
		Value:       btcutil.Amount(txout.Value),
		OutPoint: wire.OutPoint{
			Hash:  *tx.Hash(),
			Index: 0,
		},
		PkScript: txout.PkScript,
	}
	return &block1Utxo, txout, nil
}

// TestCommitmentAndHTLCTransactions checks the test vectors specified in
// BOLT 03, Appendix C. This deterministically generates commitment and second
// level HTLC transactions and checks that they match the expected values.
func TestCommitmentAndHTLCTransactions(t *testing.T) {
	t.Parallel()

	tc, err := newTestContext()
	if err != nil {
		t.Fatal(err)
	}

	// Generate random some keys that don't actually matter but need to be set.
	var (
		identityKey         *btcec.PublicKey
		localDelayBasePoint *btcec.PublicKey
	)
	generateKeys := []**btcec.PublicKey{
		&identityKey,
		&localDelayBasePoint,
	}
	for _, keyRef := range generateKeys {
		privkey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			t.Fatalf("Failed to generate new key: %v", err)
		}
		*keyRef = privkey.PubKey()
	}

	// Manually construct a new LightningChannel.
	channelState := channeldb.OpenChannel{
		ChanType:        channeldb.SingleFunderTweakless,
		ChainHash:       *tc.netParams.GenesisHash,
		FundingOutpoint: tc.fundingOutpoint,
		ShortChannelID:  tc.shortChanID,
		IsInitiator:     true,
		IdentityPub:     identityKey,
		LocalChanCfg: channeldb.ChannelConfig{
			ChannelConstraints: channeldb.ChannelConstraints{
				DustLimit:        tc.dustLimit,
				MaxPendingAmount: lnwire.NewMSatFromSatoshis(tc.fundingAmount),
				MaxAcceptedHtlcs: input.MaxHTLCNumber,
				CsvDelay:         tc.localCsvDelay,
			},
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: tc.localFundingPubKey,
			},
			PaymentBasePoint: keychain.KeyDescriptor{
				PubKey: tc.localPaymentBasePoint,
			},
			HtlcBasePoint: keychain.KeyDescriptor{
				PubKey: tc.localPaymentBasePoint,
			},
			DelayBasePoint: keychain.KeyDescriptor{
				PubKey: localDelayBasePoint,
			},
		},
		RemoteChanCfg: channeldb.ChannelConfig{
			MultiSigKey: keychain.KeyDescriptor{
				PubKey: tc.remoteFundingPubKey,
			},
			PaymentBasePoint: keychain.KeyDescriptor{
				PubKey: tc.remotePaymentBasePoint,
			},
			HtlcBasePoint: keychain.KeyDescriptor{
				PubKey: tc.remotePaymentBasePoint,
			},
		},
		Capacity:           tc.fundingAmount,
		RevocationProducer: shachain.NewRevocationProducer(zeroHash),
	}
	signer := &input.MockSigner{
		Privkeys: []*btcec.PrivateKey{
			tc.localFundingPrivKey, tc.localPaymentPrivKey,
		},
		NetParams: tc.netParams,
	}

	// Construct a LightningChannel manually because we don't have nor need all
	// of the dependencies.
	channel := LightningChannel{
		channelState:  &channelState,
		Signer:        signer,
		localChanCfg:  &channelState.LocalChanCfg,
		remoteChanCfg: &channelState.RemoteChanCfg,
	}
	err = channel.createSignDesc()
	if err != nil {
		t.Fatalf("Failed to generate channel sign descriptor: %v", err)
	}
	channel.createStateHintObfuscator()

	// The commitmentPoint is technically hidden in the spec, but we need it to
	// generate the correct tweak.
	tweak := input.SingleTweakBytes(tc.commitmentPoint, tc.localPaymentBasePoint)
	keys := &CommitmentKeyRing{
		CommitPoint:         tc.commitmentPoint,
		LocalCommitKeyTweak: tweak,
		LocalHtlcKeyTweak:   tweak,
		LocalHtlcKey:        tc.localPaymentPubKey,
		RemoteHtlcKey:       tc.remotePaymentPubKey,
		DelayKey:            tc.localDelayPubKey,
		NoDelayKey:          tc.remotePaymentPubKey,
		RevocationKey:       tc.localRevocationPubKey,
	}

	// testCases encode the raw test vectors specified in Appendix C of BOLT 03.
	testCases := []struct {
		commitment              channeldb.ChannelCommitment
		htlcDescs               []htlcDesc
		expectedCommitmentTxHex string
		remoteSigHex            string
	}{
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  7000000000,
				RemoteBalance: 3000000000,
				FeePerKw:      15000,
			},
			htlcDescs:               []htlcDesc{},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8002c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de84311054a56a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0400473044022051b75c73198c6deee1a875871c3961832909acd297c6b908d59e3319e5185a46022055c419379c5051a78d00dbbce11b5b664a0c22815fbcc6fcef6b1937c383693901483045022100f51d2e566a70ba740fc5d8c0f07b9b93d2ed741c3c0860c613173de7d39e7968022041376d520e9c0e1ad52248ddf4b22e12be8763007df977253ef45a4ca3bdb7c001475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3045022100f51d2e566a70ba740fc5d8c0f07b9b93d2ed741c3c0860c613173de7d39e7968022041376d520e9c0e1ad52248ddf4b22e12be8763007df977253ef45a4ca3bdb7c0",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      0,
			},
			htlcDescs: []htlcDesc{
				{
					index:           0,
					remoteSigHex:    "304402206a6e59f18764a5bf8d4fa45eebc591566689441229c918b480fb2af8cc6a4aeb02205248f273be447684b33e3c8d1d85a8e0ca9fa0bae9ae33f0527ada9c162919a6",
					resolutionTxHex: "020000000001018154ecccf11a5fb56c39654c4deb4d2296f83c69268280b94d021370c94e219700000000000000000001e8030000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e050047304402206a6e59f18764a5bf8d4fa45eebc591566689441229c918b480fb2af8cc6a4aeb02205248f273be447684b33e3c8d1d85a8e0ca9fa0bae9ae33f0527ada9c162919a60147304402207cb324fa0de88f452ffa9389678127ebcf4cabe1dd848b8e076c1a1962bf34720220116ed922b12311bd602d67e60d2529917f21c5b82f25ff6506c0f87886b4dfd5012000000000000000000000000000000000000000000000000000000000000000008a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a914b8bcb07f6344b42ab04250c86a6e8b75d3fdbbc688527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f401b175ac686800000000",
				},
				{
					index:           2,
					remoteSigHex:    "3045022100d5275b3619953cb0c3b5aa577f04bc512380e60fa551762ce3d7a1bb7401cff9022037237ab0dac3fe100cde094e82e2bed9ba0ed1bb40154b48e56aa70f259e608b",
					resolutionTxHex: "020000000001018154ecccf11a5fb56c39654c4deb4d2296f83c69268280b94d021370c94e219701000000000000000001d0070000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100d5275b3619953cb0c3b5aa577f04bc512380e60fa551762ce3d7a1bb7401cff9022037237ab0dac3fe100cde094e82e2bed9ba0ed1bb40154b48e56aa70f259e608b01483045022100c89172099507ff50f4c925e6c5150e871fb6e83dd73ff9fbb72f6ce829a9633f02203a63821d9162e99f9be712a68f9e589483994feae2661e4546cd5b6cec007be501008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a914b43e1b38138a41b37f7cd9a1d274bc63e3a9b5d188ac6868f6010000",
				},
				{
					index:           1,
					remoteSigHex:    "304402201b63ec807771baf4fdff523c644080de17f1da478989308ad13a58b51db91d360220568939d38c9ce295adba15665fa68f51d967e8ed14a007b751540a80b325f202",
					resolutionTxHex: "020000000001018154ecccf11a5fb56c39654c4deb4d2296f83c69268280b94d021370c94e219702000000000000000001d0070000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e050047304402201b63ec807771baf4fdff523c644080de17f1da478989308ad13a58b51db91d360220568939d38c9ce295adba15665fa68f51d967e8ed14a007b751540a80b325f20201483045022100def389deab09cee69eaa1ec14d9428770e45bcbe9feb46468ecf481371165c2f022015d2e3c46600b2ebba8dcc899768874cc6851fd1ecb3fffd15db1cc3de7e10da012001010101010101010101010101010101010101010101010101010101010101018a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a9144b6b2e5444c2639cc0fb7bcea5afba3f3cdce23988527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f501b175ac686800000000",
				},
				{
					index:           3,
					remoteSigHex:    "3045022100daee1808f9861b6c3ecd14f7b707eca02dd6bdfc714ba2f33bc8cdba507bb182022026654bf8863af77d74f51f4e0b62d461a019561bb12acb120d3f7195d148a554",
					resolutionTxHex: "020000000001018154ecccf11a5fb56c39654c4deb4d2296f83c69268280b94d021370c94e219703000000000000000001b80b0000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100daee1808f9861b6c3ecd14f7b707eca02dd6bdfc714ba2f33bc8cdba507bb182022026654bf8863af77d74f51f4e0b62d461a019561bb12acb120d3f7195d148a554014730440220643aacb19bbb72bd2b635bc3f7375481f5981bace78cdd8319b2988ffcc6704202203d27784ec8ad51ed3bd517a05525a5139bb0b755dd719e0054332d186ac0872701008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "304402207e0410e45454b0978a623f36a10626ef17b27d9ad44e2760f98cfa3efb37924f0220220bd8acd43ecaa916a80bd4f919c495a2c58982ce7c8625153f8596692a801d",
					resolutionTxHex: "020000000001018154ecccf11a5fb56c39654c4deb4d2296f83c69268280b94d021370c94e219704000000000000000001a00f0000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e050047304402207e0410e45454b0978a623f36a10626ef17b27d9ad44e2760f98cfa3efb37924f0220220bd8acd43ecaa916a80bd4f919c495a2c58982ce7c8625153f8596692a801d014730440220549e80b4496803cbc4a1d09d46df50109f546d43fbbf86cd90b174b1484acd5402205f12a4f995cb9bded597eabfee195a285986aa6d93ae5bb72507ebc6a4e2349e012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8007e80300000000000022002052bfef0479d7b293c27e0f1eb294bea154c63a3294ef092c19af51409bce0e2ad007000000000000220020403d394747cae42e98ff01734ad5c08f82ba123d3d9a620abda88989651e2ab5d007000000000000220020748eba944fedc8827f6b06bc44678f93c0f9e6078b35c6331ed31e75f8ce0c2db80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110e0a06a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e04004730440220275b0c325a5e9355650dc30c0eccfbc7efb23987c24b556b9dfdd40effca18d202206caceb2c067836c51f296740c7ae807ffcbfbf1dd3a0d56b6de9a5b247985f060147304402204fd4928835db1ccdfc40f5c78ce9bd65249b16348df81f0c44328dcdefc97d630220194d3869c38bc732dd87d13d2958015e2fc16829e74cd4377f84d215c0b7060601475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "304402204fd4928835db1ccdfc40f5c78ce9bd65249b16348df81f0c44328dcdefc97d630220194d3869c38bc732dd87d13d2958015e2fc16829e74cd4377f84d215c0b70606",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      647,
			},
			htlcDescs: []htlcDesc{
				{
					index:           0,
					remoteSigHex:    "30440220385a5afe75632f50128cbb029ee95c80156b5b4744beddc729ad339c9ca432c802202ba5f48550cad3379ac75b9b4fedb86a35baa6947f16ba5037fb8b11ab343740",
					resolutionTxHex: "020000000001018323148ce2419f21ca3d6780053747715832e18ac780931a514b187768882bb60000000000000000000122020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e05004730440220385a5afe75632f50128cbb029ee95c80156b5b4744beddc729ad339c9ca432c802202ba5f48550cad3379ac75b9b4fedb86a35baa6947f16ba5037fb8b11ab3437400147304402205999590b8a79fa346e003a68fd40366397119b2b0cdf37b149968d6bc6fbcc4702202b1e1fb5ab7864931caed4e732c359e0fe3d86a548b557be2246efb1708d579a012000000000000000000000000000000000000000000000000000000000000000008a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a914b8bcb07f6344b42ab04250c86a6e8b75d3fdbbc688527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f401b175ac686800000000",
				},
				{
					index:           2,
					remoteSigHex:    "304402207ceb6678d4db33d2401fdc409959e57c16a6cb97a30261d9c61f29b8c58d34b90220084b4a17b4ca0e86f2d798b3698ca52de5621f2ce86f80bed79afa66874511b0",
					resolutionTxHex: "020000000001018323148ce2419f21ca3d6780053747715832e18ac780931a514b187768882bb60100000000000000000124060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e050047304402207ceb6678d4db33d2401fdc409959e57c16a6cb97a30261d9c61f29b8c58d34b90220084b4a17b4ca0e86f2d798b3698ca52de5621f2ce86f80bed79afa66874511b00147304402207ff03eb0127fc7c6cae49cc29e2a586b98d1e8969cf4a17dfa50b9c2647720b902205e2ecfda2252956c0ca32f175080e75e4e390e433feb1f8ce9f2ba55648a1dac01008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a914b43e1b38138a41b37f7cd9a1d274bc63e3a9b5d188ac6868f6010000",
				},
				{
					index:           1,
					remoteSigHex:    "304402206a401b29a0dff0d18ec903502c13d83e7ec019450113f4a7655a4ce40d1f65ba0220217723a084e727b6ca0cc8b6c69c014a7e4a01fcdcba3e3993f462a3c574d833",
					resolutionTxHex: "020000000001018323148ce2419f21ca3d6780053747715832e18ac780931a514b187768882bb6020000000000000000010a060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e050047304402206a401b29a0dff0d18ec903502c13d83e7ec019450113f4a7655a4ce40d1f65ba0220217723a084e727b6ca0cc8b6c69c014a7e4a01fcdcba3e3993f462a3c574d83301483045022100d50d067ca625d54e62df533a8f9291736678d0b86c28a61bb2a80cf42e702d6e02202373dde7e00218eacdafb9415fe0e1071beec1857d1af3c6a201a44cbc47c877012001010101010101010101010101010101010101010101010101010101010101018a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a9144b6b2e5444c2639cc0fb7bcea5afba3f3cdce23988527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f501b175ac686800000000",
				},
				{
					index:           3,
					remoteSigHex:    "30450221009b1c987ba599ee3bde1dbca776b85481d70a78b681a8d84206723e2795c7cac002207aac84ad910f8598c4d1c0ea2e3399cf6627a4e3e90131315bc9f038451ce39d",
					resolutionTxHex: "020000000001018323148ce2419f21ca3d6780053747715832e18ac780931a514b187768882bb6030000000000000000010c0a0000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e05004830450221009b1c987ba599ee3bde1dbca776b85481d70a78b681a8d84206723e2795c7cac002207aac84ad910f8598c4d1c0ea2e3399cf6627a4e3e90131315bc9f038451ce39d01483045022100db9dc65291077a52728c622987e9895b7241d4394d6dcb916d7600a3e8728c22022036ee3ee717ba0bb5c45ee84bc7bbf85c0f90f26ae4e4a25a6b4241afa8a3f1cb01008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "3045022100cc28030b59f0914f45b84caa983b6f8effa900c952310708c2b5b00781117022022027ba2ccdf94d03c6d48b327f183f6e28c8a214d089b9227f94ac4f85315274f0",
					resolutionTxHex: "020000000001018323148ce2419f21ca3d6780053747715832e18ac780931a514b187768882bb604000000000000000001da0d0000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100cc28030b59f0914f45b84caa983b6f8effa900c952310708c2b5b00781117022022027ba2ccdf94d03c6d48b327f183f6e28c8a214d089b9227f94ac4f85315274f00147304402202d1a3c0d31200265d2a2def2753ead4959ae20b4083e19553acfffa5dfab60bf022020ede134149504e15b88ab261a066de49848411e15e70f9e6a5462aec2949f8f012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8007e80300000000000022002052bfef0479d7b293c27e0f1eb294bea154c63a3294ef092c19af51409bce0e2ad007000000000000220020403d394747cae42e98ff01734ad5c08f82ba123d3d9a620abda88989651e2ab5d007000000000000220020748eba944fedc8827f6b06bc44678f93c0f9e6078b35c6331ed31e75f8ce0c2db80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110e09c6a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e040048304502210094bfd8f5572ac0157ec76a9551b6c5216a4538c07cd13a51af4a54cb26fa14320220768efce8ce6f4a5efac875142ff19237c011343670adf9c7ac69704a120d116301483045022100a5c01383d3ec646d97e40f44318d49def817fcd61a0ef18008a665b3e151785502203e648efddd5838981ef55ec954be69c4a652d021e6081a100d034de366815e9b01475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3045022100a5c01383d3ec646d97e40f44318d49def817fcd61a0ef18008a665b3e151785502203e648efddd5838981ef55ec954be69c4a652d021e6081a100d034de366815e9b",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      648,
			},
			htlcDescs: []htlcDesc{
				{
					index:           2,
					remoteSigHex:    "3044022062ef2e77591409d60d7817d9bb1e71d3c4a2931d1a6c7c8307422c84f001a251022022dad9726b0ae3fe92bda745a06f2c00f92342a186d84518588cf65f4dfaada8",
					resolutionTxHex: "02000000000101579c183eca9e8236a5d7f5dcd79cfec32c497fdc0ec61533cde99ecd436cadd10000000000000000000123060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500473044022062ef2e77591409d60d7817d9bb1e71d3c4a2931d1a6c7c8307422c84f001a251022022dad9726b0ae3fe92bda745a06f2c00f92342a186d84518588cf65f4dfaada801483045022100a4c574f00411dd2f978ca5cdc1b848c311cd7849c087ad2f21a5bce5e8cc5ae90220090ae39a9bce2fb8bc879d7e9f9022df249f41e25e51f1a9bf6447a9eeffc09801008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a914b43e1b38138a41b37f7cd9a1d274bc63e3a9b5d188ac6868f6010000",
				},
				{
					index:           1,
					remoteSigHex:    "3045022100e968cbbb5f402ed389fdc7f6cd2a80ed650bb42c79aeb2a5678444af94f6c78502204b47a1cb24ab5b0b6fe69fe9cfc7dba07b9dd0d8b95f372c1d9435146a88f8d4",
					resolutionTxHex: "02000000000101579c183eca9e8236a5d7f5dcd79cfec32c497fdc0ec61533cde99ecd436cadd10100000000000000000109060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100e968cbbb5f402ed389fdc7f6cd2a80ed650bb42c79aeb2a5678444af94f6c78502204b47a1cb24ab5b0b6fe69fe9cfc7dba07b9dd0d8b95f372c1d9435146a88f8d40147304402207679cf19790bea76a733d2fa0672bd43ab455687a068f815a3d237581f57139a0220683a1a799e102071c206b207735ca80f627ab83d6616b4bcd017c5d79ef3e7d0012001010101010101010101010101010101010101010101010101010101010101018a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a9144b6b2e5444c2639cc0fb7bcea5afba3f3cdce23988527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f501b175ac686800000000",
				},
				{
					index:           3,
					remoteSigHex:    "3045022100aa91932e305292cf9969cc23502bbf6cef83a5df39c95ad04a707c4f4fed5c7702207099fc0f3a9bfe1e7683c0e9aa5e76c5432eb20693bf4cb182f04d383dc9c8c2",
					resolutionTxHex: "02000000000101579c183eca9e8236a5d7f5dcd79cfec32c497fdc0ec61533cde99ecd436cadd1020000000000000000010b0a0000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100aa91932e305292cf9969cc23502bbf6cef83a5df39c95ad04a707c4f4fed5c7702207099fc0f3a9bfe1e7683c0e9aa5e76c5432eb20693bf4cb182f04d383dc9c8c20147304402200df76fea718745f3c529bac7fd37923e7309ce38b25c0781e4cf514dd9ef8dc802204172295739dbae9fe0474dcee3608e3433b4b2af3a2e6787108b02f894dcdda301008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "3044022035cac88040a5bba420b1c4257235d5015309113460bc33f2853cd81ca36e632402202fc94fd3e81e9d34a9d01782a0284f3044370d03d60f3fc041e2da088d2de58f",
					resolutionTxHex: "02000000000101579c183eca9e8236a5d7f5dcd79cfec32c497fdc0ec61533cde99ecd436cadd103000000000000000001d90d0000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500473044022035cac88040a5bba420b1c4257235d5015309113460bc33f2853cd81ca36e632402202fc94fd3e81e9d34a9d01782a0284f3044370d03d60f3fc041e2da088d2de58f0147304402200daf2eb7afd355b4caf6fb08387b5f031940ea29d1a9f35071288a839c9039e4022067201b562456e7948616c13acb876b386b511599b58ac1d94d127f91c50463a6012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8006d007000000000000220020403d394747cae42e98ff01734ad5c08f82ba123d3d9a620abda88989651e2ab5d007000000000000220020748eba944fedc8827f6b06bc44678f93c0f9e6078b35c6331ed31e75f8ce0c2db80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de8431104e9d6a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0400483045022100a2270d5950c89ae0841233f6efea9c951898b301b2e89e0adbd2c687b9f32efa02207943d90f95b9610458e7c65a576e149750ff3accaacad004cd85e70b235e27de01473044022072714e2fbb93cdd1c42eb0828b4f2eff143f717d8f26e79d6ada4f0dcb681bbe02200911be4e5161dd6ebe59ff1c58e1997c4aea804f81db6b698821db6093d7b05701475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3044022072714e2fbb93cdd1c42eb0828b4f2eff143f717d8f26e79d6ada4f0dcb681bbe02200911be4e5161dd6ebe59ff1c58e1997c4aea804f81db6b698821db6093d7b057",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      2069,
			},
			htlcDescs: []htlcDesc{
				{
					index:           2,
					remoteSigHex:    "3045022100d1cf354de41c1369336cf85b225ed033f1f8982a01be503668df756a7e668b66022001254144fb4d0eecc61908fccc3388891ba17c5d7a1a8c62bdd307e5a513f992",
					resolutionTxHex: "02000000000101ca94a9ad516ebc0c4bdd7b6254871babfa978d5accafb554214137d398bfcf6a0000000000000000000175020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100d1cf354de41c1369336cf85b225ed033f1f8982a01be503668df756a7e668b66022001254144fb4d0eecc61908fccc3388891ba17c5d7a1a8c62bdd307e5a513f99201473044022056eb1af429660e45a1b0b66568cb8c4a3aa7e4c9c292d5d6c47f86ebf2c8838f022065c3ac4ebe980ca7a41148569be4ad8751b0a724a41405697ec55035dae6640201008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a914b43e1b38138a41b37f7cd9a1d274bc63e3a9b5d188ac6868f6010000",
				},
				{
					index:           1,
					remoteSigHex:    "3045022100d065569dcb94f090345402736385efeb8ea265131804beac06dd84d15dd2d6880220664feb0b4b2eb985fadb6ec7dc58c9334ea88ce599a9be760554a2d4b3b5d9f4",
					resolutionTxHex: "02000000000101ca94a9ad516ebc0c4bdd7b6254871babfa978d5accafb554214137d398bfcf6a0100000000000000000122020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100d065569dcb94f090345402736385efeb8ea265131804beac06dd84d15dd2d6880220664feb0b4b2eb985fadb6ec7dc58c9334ea88ce599a9be760554a2d4b3b5d9f401483045022100914bb232cd4b2690ee3d6cb8c3713c4ac9c4fb925323068d8b07f67c8541f8d9022057152f5f1615b793d2d45aac7518989ae4fe970f28b9b5c77504799d25433f7f012001010101010101010101010101010101010101010101010101010101010101018a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a9144b6b2e5444c2639cc0fb7bcea5afba3f3cdce23988527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f501b175ac686800000000",
				},
				{
					index:           3,
					remoteSigHex:    "3045022100d4e69d363de993684eae7b37853c40722a4c1b4a7b588ad7b5d8a9b5006137a102207a069c628170ee34be5612747051bdcc087466dbaa68d5756ea81c10155aef18",
					resolutionTxHex: "02000000000101ca94a9ad516ebc0c4bdd7b6254871babfa978d5accafb554214137d398bfcf6a020000000000000000015d060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100d4e69d363de993684eae7b37853c40722a4c1b4a7b588ad7b5d8a9b5006137a102207a069c628170ee34be5612747051bdcc087466dbaa68d5756ea81c10155aef180147304402200e362443f7af830b419771e8e1614fc391db3a4eb799989abfc5ab26d6fcd032022039ab0cad1c14dfbe9446bf847965e56fe016e0cbcf719fd18c1bfbf53ecbd9f901008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "30450221008ec888e36e4a4b3dc2ed6b823319855b2ae03006ca6ae0d9aa7e24bfc1d6f07102203b0f78885472a67ff4fe5916c0bb669487d659527509516fc3a08e87a2cc0a7c",
					resolutionTxHex: "02000000000101ca94a9ad516ebc0c4bdd7b6254871babfa978d5accafb554214137d398bfcf6a03000000000000000001f2090000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e05004830450221008ec888e36e4a4b3dc2ed6b823319855b2ae03006ca6ae0d9aa7e24bfc1d6f07102203b0f78885472a67ff4fe5916c0bb669487d659527509516fc3a08e87a2cc0a7c0147304402202c3e14282b84b02705dfd00a6da396c9fe8a8bcb1d3fdb4b20a4feba09440e8b02202b058b39aa9b0c865b22095edcd9ff1f71bbfe20aa4993755e54d042755ed0d5012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8006d007000000000000220020403d394747cae42e98ff01734ad5c08f82ba123d3d9a620abda88989651e2ab5d007000000000000220020748eba944fedc8827f6b06bc44678f93c0f9e6078b35c6331ed31e75f8ce0c2db80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de84311077956a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e040047304402203ca8f31c6a47519f83255dc69f1894d9a6d7476a19f498d31eaf0cd3a85eeb63022026fd92dc752b33905c4c838c528b692a8ad4ced959990b5d5ee2ff940fa90eea01473044022001d55e488b8b035b2dd29d50b65b530923a416d47f377284145bc8767b1b6a75022019bb53ddfe1cefaf156f924777eaaf8fdca1810695a7d0a247ad2afba8232eb401475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3044022001d55e488b8b035b2dd29d50b65b530923a416d47f377284145bc8767b1b6a75022019bb53ddfe1cefaf156f924777eaaf8fdca1810695a7d0a247ad2afba8232eb4",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      2070,
			},
			htlcDescs: []htlcDesc{
				{
					index:           2,
					remoteSigHex:    "3045022100eed143b1ee4bed5dc3cde40afa5db3e7354cbf9c44054b5f713f729356f08cf7022077161d171c2bbd9badf3c9934de65a4918de03bbac1450f715275f75b103f891",
					resolutionTxHex: "0200000000010140a83ce364747ff277f4d7595d8d15f708418798922c40bc2b056aca5485a2180000000000000000000174020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100eed143b1ee4bed5dc3cde40afa5db3e7354cbf9c44054b5f713f729356f08cf7022077161d171c2bbd9badf3c9934de65a4918de03bbac1450f715275f75b103f89101483045022100a0d043ed533e7fb1911e0553d31a8e2f3e6de19dbc035257f29d747c5e02f1f5022030cd38d8e84282175d49c1ebe0470db3ebd59768cf40780a784e248a43904fb801008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a914b43e1b38138a41b37f7cd9a1d274bc63e3a9b5d188ac6868f6010000",
				},
				{
					index:           3,
					remoteSigHex:    "3044022071e9357619fd8d29a411dc053b326a5224c5d11268070e88ecb981b174747c7a02202b763ae29a9d0732fa8836dd8597439460b50472183f420021b768981b4f7cf6",
					resolutionTxHex: "0200000000010140a83ce364747ff277f4d7595d8d15f708418798922c40bc2b056aca5485a218010000000000000000015c060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500473044022071e9357619fd8d29a411dc053b326a5224c5d11268070e88ecb981b174747c7a02202b763ae29a9d0732fa8836dd8597439460b50472183f420021b768981b4f7cf601483045022100adb1d679f65f96178b59f23ed37d3b70443118f345224a07ecb043eee2acc157022034d24524fe857144a3bcfff3065a9994d0a6ec5f11c681e49431d573e242612d01008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "3045022100c9458a4d2cbb741705577deb0a890e5cb90ee141be0400d3162e533727c9cb2102206edcf765c5dc5e5f9b976ea8149bf8607b5a0efb30691138e1231302b640d2a4",
					resolutionTxHex: "0200000000010140a83ce364747ff277f4d7595d8d15f708418798922c40bc2b056aca5485a21802000000000000000001f1090000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100c9458a4d2cbb741705577deb0a890e5cb90ee141be0400d3162e533727c9cb2102206edcf765c5dc5e5f9b976ea8149bf8607b5a0efb30691138e1231302b640d2a40147304402200831422aa4e1ee6d55e0b894201770a8f8817a189356f2d70be76633ffa6a6f602200dd1b84a4855dc6727dd46c98daae43dfc70889d1ba7ef0087529a57c06e5e04012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8005d007000000000000220020403d394747cae42e98ff01734ad5c08f82ba123d3d9a620abda88989651e2ab5b80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110da966a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e04004730440220443cb07f650aebbba14b8bc8d81e096712590f524c5991ac0ed3bbc8fd3bd0c7022028a635f548e3ca64b19b69b1ea00f05b22752f91daf0b6dab78e62ba52eb7fd001483045022100f2377f7a67b7fc7f4e2c0c9e3a7de935c32417f5668eda31ea1db401b7dc53030220415fdbc8e91d0f735e70c21952342742e25249b0d062d43efbfc564499f3752601475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3045022100f2377f7a67b7fc7f4e2c0c9e3a7de935c32417f5668eda31ea1db401b7dc53030220415fdbc8e91d0f735e70c21952342742e25249b0d062d43efbfc564499f37526",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      2194,
			},
			htlcDescs: []htlcDesc{
				{
					index:           2,
					remoteSigHex:    "30450221009ed2f0a67f99e29c3c8cf45c08207b765980697781bb727fe0b1416de0e7622902206052684229bc171419ed290f4b615c943f819c0262414e43c5b91dcf72ddcf44",
					resolutionTxHex: "02000000000101fb824d4e4dafc0f567789dee3a6bce8d411fe80f5563d8cdfdcc7d7e4447d43a0000000000000000000122020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e05004830450221009ed2f0a67f99e29c3c8cf45c08207b765980697781bb727fe0b1416de0e7622902206052684229bc171419ed290f4b615c943f819c0262414e43c5b91dcf72ddcf4401473044022004ad5f04ae69c71b3b141d4db9d0d4c38d84009fb3cfeeae6efdad414487a9a0022042d3fe1388c1ff517d1da7fb4025663d372c14728ed52dc88608363450ff6a2f01008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a914b43e1b38138a41b37f7cd9a1d274bc63e3a9b5d188ac6868f6010000",
				},
				{
					index:           3,
					remoteSigHex:    "30440220155d3b90c67c33a8321996a9be5b82431b0c126613be751d400669da9d5c696702204318448bcd48824439d2c6a70be6e5747446be47ff45977cf41672bdc9b6b12d",
					resolutionTxHex: "02000000000101fb824d4e4dafc0f567789dee3a6bce8d411fe80f5563d8cdfdcc7d7e4447d43a010000000000000000010a060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e05004730440220155d3b90c67c33a8321996a9be5b82431b0c126613be751d400669da9d5c696702204318448bcd48824439d2c6a70be6e5747446be47ff45977cf41672bdc9b6b12d0147304402201707050c870c1f77cc3ed58d6d71bf281de239e9eabd8ef0955bad0d7fe38dcc02204d36d80d0019b3a71e646a08fa4a5607761d341ae8be371946ebe437c289c91501008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "3045022100a12a9a473ece548584aabdd051779025a5ed4077c4b7aa376ec7a0b1645e5a48022039490b333f53b5b3e2ddde1d809e492cba2b3e5fc3a436cd3ffb4cd3d500fa5a",
					resolutionTxHex: "02000000000101fb824d4e4dafc0f567789dee3a6bce8d411fe80f5563d8cdfdcc7d7e4447d43a020000000000000000019a090000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100a12a9a473ece548584aabdd051779025a5ed4077c4b7aa376ec7a0b1645e5a48022039490b333f53b5b3e2ddde1d809e492cba2b3e5fc3a436cd3ffb4cd3d500fa5a01483045022100ff200bc934ab26ce9a559e998ceb0aee53bc40368e114ab9d3054d9960546e2802202496856ca163ac12c143110b6b3ac9d598df7254f2e17b3b94c3ab5301f4c3b0012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8005d007000000000000220020403d394747cae42e98ff01734ad5c08f82ba123d3d9a620abda88989651e2ab5b80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de84311040966a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e040047304402203b1b010c109c2ecbe7feb2d259b9c4126bd5dc99ee693c422ec0a5781fe161ba0220571fe4e2c649dea9c7aaf7e49b382962f6a3494963c97d80fef9a430ca3f706101483045022100d33c4e541aa1d255d41ea9a3b443b3b822ad8f7f86862638aac1f69f8f760577022007e2a18e6931ce3d3a804b1c78eda1de17dbe1fb7a95488c9a4ec8620395334801475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3045022100d33c4e541aa1d255d41ea9a3b443b3b822ad8f7f86862638aac1f69f8f760577022007e2a18e6931ce3d3a804b1c78eda1de17dbe1fb7a95488c9a4ec86203953348",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      2195,
			},
			htlcDescs: []htlcDesc{
				{
					index:           3,
					remoteSigHex:    "3045022100a8a78fa1016a5c5c3704f2e8908715a3cef66723fb95f3132ec4d2d05cd84fb4022025ac49287b0861ec21932405f5600cbce94313dbde0e6c5d5af1b3366d8afbfc",
					resolutionTxHex: "020000000001014e16c488fa158431c1a82e8f661240ec0a71ba0ce92f2721a6538c510226ad5c0000000000000000000109060000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100a8a78fa1016a5c5c3704f2e8908715a3cef66723fb95f3132ec4d2d05cd84fb4022025ac49287b0861ec21932405f5600cbce94313dbde0e6c5d5af1b3366d8afbfc01483045022100be6ae1977fd7b630a53623f3f25c542317ccfc2b971782802a4f1ef538eb22b402207edc4d0408f8f38fd3c7365d1cfc26511b7cd2d4fecd8b005fba3cd5bc70439001008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "3045022100e769cb156aa2f7515d126cef7a69968629620ce82afcaa9e210969de6850df4602200b16b3f3486a229a48aadde520dbee31ae340dbadaffae74fbb56681fef27b92",
					resolutionTxHex: "020000000001014e16c488fa158431c1a82e8f661240ec0a71ba0ce92f2721a6538c510226ad5c0100000000000000000199090000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100e769cb156aa2f7515d126cef7a69968629620ce82afcaa9e210969de6850df4602200b16b3f3486a229a48aadde520dbee31ae340dbadaffae74fbb56681fef27b92014730440220665b9cb4a978c09d1ca8977a534999bc8a49da624d0c5439451dd69cde1a003d022070eae0620f01f3c1bd029cc1488da13fb40fdab76f396ccd335479a11c5276d8012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8004b80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110b8976a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e040047304402203b12d44254244b8ff3bb4129b0920fd45120ab42f553d9976394b099d500c99e02205e95bb7a3164852ef0c48f9e0eaf145218f8e2c41251b231f03cbdc4f29a54290147304402205e2f76d4657fb732c0dfc820a18a7301e368f5799e06b7828007633741bda6df0220458009ae59d0c6246065c419359e05eb2a4b4ef4a1b310cc912db44eb792429801475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "304402205e2f76d4657fb732c0dfc820a18a7301e368f5799e06b7828007633741bda6df0220458009ae59d0c6246065c419359e05eb2a4b4ef4a1b310cc912db44eb7924298",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      3702,
			},
			htlcDescs: []htlcDesc{
				{
					index:           3,
					remoteSigHex:    "3045022100dfb73b4fe961b31a859b2bb1f4f15cabab9265016dd0272323dc6a9e85885c54022059a7b87c02861ee70662907f25ce11597d7b68d3399443a831ae40e777b76bdb",
					resolutionTxHex: "02000000000101b8de11eb51c22498fe39722c7227b6e55ff1a94146cf638458cb9bc6a060d3a30000000000000000000122020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100dfb73b4fe961b31a859b2bb1f4f15cabab9265016dd0272323dc6a9e85885c54022059a7b87c02861ee70662907f25ce11597d7b68d3399443a831ae40e777b76bdb0147304402202765b9c9ece4f127fa5407faf66da4c5ce2719cdbe47cd3175fc7d48b482e43d02205605125925e07bad1e41c618a4b434d72c88a164981c4b8af5eaf4ee9142ec3a01008576a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c820120876475527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae67a9148a486ff2e31d6158bf39e2608864d63fefd09d5b88ac6868f7010000",
				},
				{
					index:           4,
					remoteSigHex:    "3045022100ea9dc2a7c3c3640334dab733bb4e036e32a3106dc707b24227874fa4f7da746802204d672f7ac0fe765931a8df10b81e53a3242dd32bd9dc9331eb4a596da87954e9",
					resolutionTxHex: "02000000000101b8de11eb51c22498fe39722c7227b6e55ff1a94146cf638458cb9bc6a060d3a30100000000000000000176050000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100ea9dc2a7c3c3640334dab733bb4e036e32a3106dc707b24227874fa4f7da746802204d672f7ac0fe765931a8df10b81e53a3242dd32bd9dc9331eb4a596da87954e9014730440220048a41c660c4841693de037d00a407810389f4574b3286afb7bc392a438fa3f802200401d71fa87c64fe621b49ac07e3bf85157ac680acb977124da28652cc7f1a5c012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8004b80b000000000000220020c20b5d1f8584fd90443e7b7b720136174fa4b9333c261d04dbbd012635c0f419a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de8431106f916a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e040047304402200e930a43c7951162dc15a2b7344f48091c74c70f7024e7116e900d8bcfba861c022066fa6cbda3929e21daa2e7e16a4b948db7e8919ef978402360d1095ffdaff7b001483045022100c1a3b0b60ca092ed5080121f26a74a20cec6bdee3f8e47bae973fcdceb3eda5502207d467a9873c939bf3aa758014ae67295fedbca52412633f7e5b2670fc7c381c101475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3045022100c1a3b0b60ca092ed5080121f26a74a20cec6bdee3f8e47bae973fcdceb3eda5502207d467a9873c939bf3aa758014ae67295fedbca52412633f7e5b2670fc7c381c1",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      3703,
			},
			htlcDescs: []htlcDesc{
				{
					index:           4,
					remoteSigHex:    "3044022044f65cf833afdcb9d18795ca93f7230005777662539815b8a601eeb3e57129a902206a4bf3e53392affbba52640627defa8dc8af61c958c9e827b2798ab45828abdd",
					resolutionTxHex: "020000000001011c076aa7fb3d7460d10df69432c904227ea84bbf3134d4ceee5fb0f135ef206d0000000000000000000175050000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500473044022044f65cf833afdcb9d18795ca93f7230005777662539815b8a601eeb3e57129a902206a4bf3e53392affbba52640627defa8dc8af61c958c9e827b2798ab45828abdd01483045022100b94d931a811b32eeb885c28ddcf999ae1981893b21dd1329929543fe87ce793002206370107fdd151c5f2384f9ceb71b3107c69c74c8ed5a28a94a4ab2d27d3b0724012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8003a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110eb936a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0400473044022047305531dd44391dce03ae20f8735005c615eb077a974edb0059ea1a311857d602202e0ed6972fbdd1e8cb542b06e0929bc41b2ddf236e04cb75edd56151f4197506014830450221008b7c191dd46893b67b628e618d2dc8e81169d38bade310181ab77d7c94c6675e02203b4dd131fd7c9deb299560983dcdc485545c98f989f7ae8180c28289f9e6bdb001475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "30450221008b7c191dd46893b67b628e618d2dc8e81169d38bade310181ab77d7c94c6675e02203b4dd131fd7c9deb299560983dcdc485545c98f989f7ae8180c28289f9e6bdb0",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      4914,
			},
			htlcDescs: []htlcDesc{
				{
					index:           4,
					remoteSigHex:    "3045022100fcb38506bfa11c02874092a843d0cc0a8613c23b639832564a5f69020cb0f6ba02206508b9e91eaa001425c190c68ee5f887e1ad5b1b314002e74db9dbd9e42dbecf",
					resolutionTxHex: "0200000000010110a3fdcbcd5db477cd3ad465e7f501ffa8c437e8301f00a6061138590add757f0000000000000000000122020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0500483045022100fcb38506bfa11c02874092a843d0cc0a8613c23b639832564a5f69020cb0f6ba02206508b9e91eaa001425c190c68ee5f887e1ad5b1b314002e74db9dbd9e42dbecf0148304502210086e76b460ddd3cea10525fba298405d3fe11383e56966a5091811368362f689a02200f72ee75657915e0ede89c28709acd113ede9e1b7be520e3bc5cda425ecd6e68012004040404040404040404040404040404040404040404040404040404040404048a76a91414011f7254d96b819c76986c277d115efce6f7b58763ac67210394854aa6eab5b2a8122cc726e9dded053a2184d88256816826d6231c068d4a5b7c8201208763a91418bc1a114ccf9c052d3d23e28d3b0a9d1227434288527c21030d417a46946384f88d5f3337267c5e579765875dc4daca813e21734b140639e752ae677502f801b175ac686800000000",
				},
			},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8003a00f0000000000002200208c48d15160397c9731df9bc3b236656efb6665fbfe92b4a6878e88a499f741c4c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110ae8f6a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e040047304402206a2679efa3c7aaffd2a447fd0df7aba8792858b589750f6a1203f9259173198a022008d52a0e77a99ab533c36206cb15ad7aeb2aa72b93d4b571e728cb5ec2f6fe260147304402206d6cb93969d39177a09d5d45b583f34966195b77c7e585cf47ac5cce0c90cefb022031d71ae4e33a4e80df7f981d696fbdee517337806a3c7138b7491e2cbb077a0e01475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "304402206d6cb93969d39177a09d5d45b583f34966195b77c7e585cf47ac5cce0c90cefb022031d71ae4e33a4e80df7f981d696fbdee517337806a3c7138b7491e2cbb077a0e",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      4915,
			},
			htlcDescs:               []htlcDesc{},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8002c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de843110fa926a00000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80e0400483045022100a012691ba6cea2f73fa8bac37750477e66363c6d28813b0bb6da77c8eb3fb0270220365e99c51304b0b1a6ab9ea1c8500db186693e39ec1ad5743ee231b0138384b90147304402200769ba89c7330dfa4feba447b6e322305f12ac7dac70ec6ba997ed7c1b598d0802204fe8d337e7fee781f9b7b1a06e580b22f4f79d740059560191d7db53f876555201475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "304402200769ba89c7330dfa4feba447b6e322305f12ac7dac70ec6ba997ed7c1b598d0802204fe8d337e7fee781f9b7b1a06e580b22f4f79d740059560191d7db53f8765552",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      9651180,
			},
			htlcDescs:               []htlcDesc{},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b800222020000000000002200204adb4e2f00643db396dd120d4e7dc17625f5f2c11a40d857accc862d6b7dd80ec0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de84311004004730440220514f977bf7edc442de8ce43ace9686e5ebdc0f893033f13e40fb46c8b8c6e1f90220188006227d175f5c35da0b092c57bea82537aed89f7778204dc5bacf4f29f2b901473044022037f83ff00c8e5fb18ae1f918ffc24e54581775a20ff1ae719297ef066c71caa9022039c529cccd89ff6c5ed1db799614533844bd6d101da503761c45c713996e3bbd01475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3044022037f83ff00c8e5fb18ae1f918ffc24e54581775a20ff1ae719297ef066c71caa9022039c529cccd89ff6c5ed1db799614533844bd6d101da503761c45c713996e3bbd",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      9651181,
			},
			htlcDescs:               []htlcDesc{},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8001c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de8431100400473044022031a82b51bd014915fe68928d1abf4b9885353fb896cac10c3fdd88d7f9c7f2e00220716bda819641d2c63e65d3549b6120112e1aeaf1742eed94a471488e79e206b101473044022064901950be922e62cbe3f2ab93de2b99f37cff9fc473e73e394b27f88ef0731d02206d1dfa227527b4df44a07599289e207d6fd9cca60c0365682dcd3deaf739567e01475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3044022064901950be922e62cbe3f2ab93de2b99f37cff9fc473e73e394b27f88ef0731d02206d1dfa227527b4df44a07599289e207d6fd9cca60c0365682dcd3deaf739567e",
		},
		{
			commitment: channeldb.ChannelCommitment{
				CommitHeight:  42,
				LocalBalance:  6988000000,
				RemoteBalance: 3000000000,
				FeePerKw:      9651936,
			},
			htlcDescs:               []htlcDesc{},
			expectedCommitmentTxHex: "02000000000101bef67e4e2fb9ddeeb3461973cd4c62abb35050b1add772995b820b584a488489000000000038b02b8001c0c62d0000000000160014ccf1af2f2aabee14bb40fa3851ab2301de8431100400473044022031a82b51bd014915fe68928d1abf4b9885353fb896cac10c3fdd88d7f9c7f2e00220716bda819641d2c63e65d3549b6120112e1aeaf1742eed94a471488e79e206b101473044022064901950be922e62cbe3f2ab93de2b99f37cff9fc473e73e394b27f88ef0731d02206d1dfa227527b4df44a07599289e207d6fd9cca60c0365682dcd3deaf739567e01475221023da092f6980e58d2c037173180e9a465476026ee50f96695963e8efe436f54eb21030e9f7b623d2ccc7c9bd44d66d5ce21ce504c0acf6385a132cec6d3c39fa711c152ae3e195220",
			remoteSigHex:            "3044022064901950be922e62cbe3f2ab93de2b99f37cff9fc473e73e394b27f88ef0731d02206d1dfa227527b4df44a07599289e207d6fd9cca60c0365682dcd3deaf739567e",
		},
	}

	for i, test := range testCases {
		expectedCommitmentTx, err := txFromHex(test.expectedCommitmentTxHex)
		if err != nil {
			t.Fatalf("Case %d: Failed to parse serialized tx: %v", i, err)
		}

		// Build required HTLC structs from raw test vector data.
		htlcs := make([]channeldb.HTLC, len(test.htlcDescs), len(test.htlcDescs))
		for i, htlcDesc := range test.htlcDescs {
			htlcs[i], err = tc.getHTLC(i, &htlcDesc)
			if err != nil {
				t.Fatal(err)
			}
		}
		theHTLCView := htlcViewFromHTLCs(htlcs)

		// Create unsigned commitment transaction.
		commitmentView := &commitment{
			height:       test.commitment.CommitHeight,
			ourBalance:   test.commitment.LocalBalance,
			theirBalance: test.commitment.RemoteBalance,
			feePerKw:     SatPerKWeight(test.commitment.FeePerKw),
			dustLimit:    tc.dustLimit,
			isOurs:       true,
		}
		err = channel.createCommitmentTx(
			commitmentView, theHTLCView, keys,
		)
		if err != nil {
			t.Errorf("Case %d: Failed to create new commitment tx: %v", i, err)
			continue
		}

		// Initialize LocalCommit, which is used in getSignedCommitTx.
		channelState.LocalCommitment = test.commitment
		channelState.LocalCommitment.Htlcs = htlcs
		channelState.LocalCommitment.CommitTx = commitmentView.txn

		// This is the remote party's signature over the commitment
		// transaction which is included in the commitment tx's witness
		// data.
		channelState.LocalCommitment.CommitSig, err = hex.DecodeString(test.remoteSigHex)
		if err != nil {
			t.Fatalf("Case %d: Failed to parse serialized signature: %v",
				i, err)
		}

		commitTx, err := channel.getSignedCommitTx()
		if err != nil {
			t.Errorf("Case %d: Failed to sign commitment tx: %v", i, err)
			continue
		}

		// Check that commitment transaction was created correctly.
		if commitTx.WitnessHash() != *expectedCommitmentTx.WitnessHash() {
			t.Errorf("Case %d: Generated unexpected commitment tx: "+
				"expected %s, got %s", i, spew.Sdump(expectedCommitmentTx),
				spew.Sdump(commitTx))
			continue
		}

		// Generate second-level HTLC transactions for HTLCs in
		// commitment tx.
		htlcResolutions, err := extractHtlcResolutions(
			SatPerKWeight(test.commitment.FeePerKw), true, signer,
			htlcs, keys, channel.localChanCfg, channel.remoteChanCfg,
			commitTx.TxHash(),
		)
		if err != nil {
			t.Errorf("Case %d: Failed to extract HTLC resolutions: %v", i, err)
			continue
		}

		resolutionIdx := 0
		for j, htlcDesc := range test.htlcDescs {
			// TODO: Check HTLC success transactions; currently not implemented.
			// resolutionIdx can be replaced by j when this is handled.
			if htlcs[j].Incoming {
				continue
			}

			expectedTx, err := txFromHex(htlcDesc.resolutionTxHex)
			if err != nil {
				t.Fatalf("Failed to parse serialized tx: %v", err)
			}

			htlcResolution := htlcResolutions.OutgoingHTLCs[resolutionIdx]
			resolutionIdx++

			actualTx := htlcResolution.SignedTimeoutTx
			if actualTx == nil {
				t.Errorf("Case %d: Failed to generate second level tx: "+
					"output %d, %v", i, j,
					htlcResolutions.OutgoingHTLCs[j])
				continue
			}

			// Check that second-level HTLC transaction was created correctly.
			if actualTx.WitnessHash() != *expectedTx.WitnessHash() {
				t.Errorf("Case %d: Generated unexpected second level tx: "+
					"output %d, expected %s, got %s", i, j,
					expectedTx.WitnessHash(), actualTx.WitnessHash())
				continue
			}
		}
	}
}

// htlcViewFromHTLCs constructs an htlcView of PaymentDescriptors from a slice
// of channeldb.HTLC structs.
func htlcViewFromHTLCs(htlcs []channeldb.HTLC) *htlcView {
	var theHTLCView htlcView
	for _, htlc := range htlcs {
		paymentDesc := &PaymentDescriptor{
			RHash:   htlc.RHash,
			Timeout: htlc.RefundTimeout,
			Amount:  htlc.Amt,
		}
		if htlc.Incoming {
			theHTLCView.theirUpdates =
				append(theHTLCView.theirUpdates, paymentDesc)
		} else {
			theHTLCView.ourUpdates =
				append(theHTLCView.ourUpdates, paymentDesc)
		}
	}
	return &theHTLCView
}

func TestCommitTxStateHint(t *testing.T) {
	t.Parallel()

	stateHintTests := []struct {
		name       string
		from       uint64
		to         uint64
		inputs     int
		shouldFail bool
	}{
		{
			name:       "states 0 to 1000",
			from:       0,
			to:         1000,
			inputs:     1,
			shouldFail: false,
		},
		{
			name:       "states 'maxStateHint-1000' to 'maxStateHint'",
			from:       maxStateHint - 1000,
			to:         maxStateHint,
			inputs:     1,
			shouldFail: false,
		},
		{
			name:       "state 'maxStateHint+1'",
			from:       maxStateHint + 1,
			to:         maxStateHint + 10,
			inputs:     1,
			shouldFail: true,
		},
		{
			name:       "commit transaction with two inputs",
			inputs:     2,
			shouldFail: true,
		},
	}

	var obfuscator [StateHintSize]byte
	copy(obfuscator[:], testHdSeed[:StateHintSize])
	timeYesterday := uint32(time.Now().Unix() - 24*60*60)

	for _, test := range stateHintTests {
		commitTx := wire.NewMsgTx(2)

		// Add supplied number of inputs to the commitment transaction.
		for i := 0; i < test.inputs; i++ {
			commitTx.AddTxIn(&wire.TxIn{})
		}

		for i := test.from; i <= test.to; i++ {
			stateNum := uint64(i)

			err := SetStateNumHint(commitTx, stateNum, obfuscator)
			if err != nil && !test.shouldFail {
				t.Fatalf("unable to set state num %v: %v", i, err)
			} else if err == nil && test.shouldFail {
				t.Fatalf("Failed(%v): test should fail but did not", test.name)
			}

			locktime := commitTx.LockTime
			sequence := commitTx.TxIn[0].Sequence

			// Locktime should not be less than 500,000,000 and not larger
			// than the time 24 hours ago. One day should provide a good
			// enough buffer for the tests.
			if locktime < 5e8 || locktime > timeYesterday {
				if !test.shouldFail {
					t.Fatalf("The value of locktime (%v) may cause the commitment "+
						"transaction to be unspendable", locktime)
				}
			}

			if sequence&wire.SequenceLockTimeDisabled == 0 {
				if !test.shouldFail {
					t.Fatalf("Sequence locktime is NOT disabled when it should be")
				}
			}

			extractedStateNum := GetStateNumHint(commitTx, obfuscator)
			if extractedStateNum != stateNum && !test.shouldFail {
				t.Fatalf("state number mismatched, expected %v, got %v",
					stateNum, extractedStateNum)
			} else if extractedStateNum == stateNum && test.shouldFail {
				t.Fatalf("Failed(%v): test should fail but did not", test.name)
			}
		}
		t.Logf("Passed: %v", test.name)
	}
}

// testSpendValidation ensures that we're able to spend all outputs in the
// commitment transaction that we create.
func testSpendValidation(t *testing.T, tweakless bool) {
	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	fundingOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	const channelBalance = btcutil.Amount(1 * 10e8)
	const csvTimeout = uint32(5)

	// We also set up set some resources for the commitment transaction.
	// Each side currently has 1 BTC within the channel, with a total
	// channel capacity of 2BTC.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(
		btcec.S256(), testWalletPrivKey,
	)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(
		btcec.S256(), bobsPrivKey,
	)

	revocationPreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(
		btcec.S256(), revocationPreimage,
	)
	revokePubKey := input.DeriveRevocationPubkey(bobKeyPub, commitPoint)

	aliceDelayKey := input.TweakPubKey(aliceKeyPub, commitPoint)

	// Bob will have the channel "force closed" on him, so for the sake of
	// our commitments, if it's tweakless, his key will just be his regular
	// pubkey.
	bobPayKey := input.TweakPubKey(bobKeyPub, commitPoint)
	if tweakless {
		bobPayKey = bobKeyPub
	}

	aliceCommitTweak := input.SingleTweakBytes(commitPoint, aliceKeyPub)
	bobCommitTweak := input.SingleTweakBytes(commitPoint, bobKeyPub)

	aliceSelfOutputSigner := &input.MockSigner{
		Privkeys: []*btcec.PrivateKey{aliceKeyPriv},
	}

	// With all the test data set up, we create the commitment transaction.
	// We only focus on a single party's transactions, as the scripts are
	// identical with the roles reversed.
	//
	// This is Alice's commitment transaction, so she must wait a CSV delay
	// of 5 blocks before sweeping the output, while bob can spend
	// immediately with either the revocation key, or his regular key.
	keyRing := &CommitmentKeyRing{
		DelayKey:      aliceDelayKey,
		RevocationKey: revokePubKey,
		NoDelayKey:    bobPayKey,
	}
	commitmentTx, err := CreateCommitTx(
		*fakeFundingTxIn, keyRing, csvTimeout, channelBalance,
		channelBalance, DefaultDustLimit(),
	)
	if err != nil {
		t.Fatalf("unable to create commitment transaction: %v", nil)
	}

	delayOutput := commitmentTx.TxOut[0]
	regularOutput := commitmentTx.TxOut[1]

	// We're testing an uncooperative close, output sweep, so construct a
	// transaction which sweeps the funds to a random address.
	targetOutput, err := input.CommitScriptUnencumbered(aliceKeyPub)
	if err != nil {
		t.Fatalf("unable to create target output: %v", err)
	}
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{
		Hash:  commitmentTx.TxHash(),
		Index: 0,
	}, nil, nil))
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: targetOutput,
		Value:    0.5 * 10e8,
	})

	// First, we'll test spending with Alice's key after the timeout.
	delayScript, err := input.CommitScriptToSelf(
		csvTimeout, aliceDelayKey, revokePubKey,
	)
	if err != nil {
		t.Fatalf("unable to generate alice delay script: %v", err)
	}
	sweepTx.TxIn[0].Sequence = input.LockTimeToSequence(false, csvTimeout)
	signDesc := &input.SignDescriptor{
		WitnessScript: delayScript,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		SingleTweak: aliceCommitTweak,
		SigHashes:   txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value: int64(channelBalance),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	aliceWitnessSpend, err := input.CommitSpendTimeout(
		aliceSelfOutputSigner, signDesc, sweepTx,
	)
	if err != nil {
		t.Fatalf("unable to generate delay commit spend witness: %v", err)
	}
	sweepTx.TxIn[0].Witness = aliceWitnessSpend
	vm, err := txscript.NewEngine(delayOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("spend from delay output is invalid: %v", err)
	}

	bobSigner := &input.MockSigner{Privkeys: []*btcec.PrivateKey{bobKeyPriv}}

	// Next, we'll test bob spending with the derived revocation key to
	// simulate the scenario when Alice broadcasts this commitment
	// transaction after it's been revoked.
	signDesc = &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		DoubleTweak:   commitSecret,
		WitnessScript: delayScript,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value: int64(channelBalance),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	bobWitnessSpend, err := input.CommitSpendRevoke(bobSigner, signDesc,
		sweepTx)
	if err != nil {
		t.Fatalf("unable to generate revocation witness: %v", err)
	}
	sweepTx.TxIn[0].Witness = bobWitnessSpend
	vm, err = txscript.NewEngine(delayOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("revocation spend is invalid: %v", err)
	}

	// In order to test the final scenario, we modify the TxIn of the sweep
	// transaction to instead point to the regular output (non delay)
	// within the commitment transaction.
	sweepTx.TxIn[0] = &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  commitmentTx.TxHash(),
			Index: 1,
		},
	}

	// Finally, we test bob sweeping his output as normal in the case that
	// Alice broadcasts this commitment transaction.
	bobScriptP2WKH, err := input.CommitScriptUnencumbered(bobPayKey)
	if err != nil {
		t.Fatalf("unable to create bob p2wkh script: %v", err)
	}
	signDesc = &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		WitnessScript: bobScriptP2WKH,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value:    int64(channelBalance),
			PkScript: bobScriptP2WKH,
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	if !tweakless {
		signDesc.SingleTweak = bobCommitTweak
	}
	bobRegularSpend, err := input.CommitSpendNoDelay(
		bobSigner, signDesc, sweepTx, tweakless,
	)
	if err != nil {
		t.Fatalf("unable to create bob regular spend: %v", err)
	}
	sweepTx.TxIn[0].Witness = bobRegularSpend
	vm, err = txscript.NewEngine(
		regularOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance),
	)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("bob p2wkh spend is invalid: %v", err)
	}
}

// TestCommitmentSpendValidation test the spendability of both outputs within
// the commitment transaction.
//
// The following spending cases are covered by this test:
//   * Alice's spend from the delayed output on her commitment transaction.
//   * Bob's spend from Alice's delayed output when she broadcasts a revoked
//     commitment transaction.
//   * Bob's spend from his unencumbered output within Alice's commitment
//     transaction.
func TestCommitmentSpendValidation(t *testing.T) {
	t.Parallel()

	// In the modern network, all channels use the new tweakless format,
	// but we also need to support older nodes that want to open channels
	// with the legacy format, so we'll test spending in both scenarios.
	for _, tweakless := range []bool{true, false} {
		tweakless := tweakless
		t.Run(fmt.Sprintf("tweak=%v", tweakless), func(t *testing.T) {
			testSpendValidation(t, tweakless)
		})
	}
}
