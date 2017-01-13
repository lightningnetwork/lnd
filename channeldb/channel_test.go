package channeldb

import (
	"bytes"
	"io/ioutil"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	_ "github.com/roasbeef/btcwallet/walletdb/bdb"
)

var (
	netParams = &chaincfg.TestNet3Params

	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	id = &wire.OutPoint{
		Hash: [chainhash.HashSize]byte{
			0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
			0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
			0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
		},
		Index: 9,
	}
	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{0x04, 0x31, 0xdc, 0x00, 0x1b, 0x01, 0x62},
				Sequence:        0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			&wire.TxOut{
				Value: 5000000000,
				PkScript: []byte{
					0x41, // OP_DATA_65
					0x04, 0xd6, 0x4b, 0xdf, 0xd0, 0x9e, 0xb1, 0xc5,
					0xfe, 0x29, 0x5a, 0xbd, 0xeb, 0x1d, 0xca, 0x42,
					0x81, 0xbe, 0x98, 0x8e, 0x2d, 0xa0, 0xb6, 0xc1,
					0xc6, 0xa5, 0x9d, 0xc2, 0x26, 0xc2, 0x86, 0x24,
					0xe1, 0x81, 0x75, 0xe8, 0x51, 0xc9, 0x6b, 0x97,
					0x3d, 0x81, 0xb0, 0x1c, 0xc3, 0x1f, 0x04, 0x78,
					0x34, 0xbc, 0x06, 0xd6, 0xd6, 0xed, 0xf6, 0x20,
					0xd1, 0x84, 0x24, 0x1a, 0x6a, 0xed, 0x8b, 0x63,
					0xa6, // 65-byte signature
					0xac, // OP_CHECKSIG
				},
			},
		},
		LockTime: 5,
	}
	testOutpoint = &wire.OutPoint{
		Hash:  key,
		Index: 0,
	}
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), key[:])
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func makeTestDB() (*DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

func createTestChannelState(cdb *DB) (*OpenChannel, error) {
	addr, err := btcutil.NewAddressPubKey(pubKey.SerializeCompressed(), netParams)
	if err != nil {
		return nil, err
	}

	script, err := txscript.MultiSigScript([]*btcutil.AddressPubKey{addr, addr}, 2)
	if err != nil {
		return nil, err
	}

	// Simulate 1000 channel updates via progression of the elkrem
	// revocation trees.
	sender := elkrem.NewElkremSender(key)
	receiver := &elkrem.ElkremReceiver{}
	for i := 0; i < 1000; i++ {
		preImage, err := sender.AtIndex(uint64(i))
		if err != nil {
			return nil, err
		}

		if receiver.AddNext(preImage); err != nil {
			return nil, err
		}
	}

	var obsfucator [4]byte
	copy(obsfucator[:], key[:])

	return &OpenChannel{
		IsInitiator:                true,
		ChanType:                   SingleFunder,
		IdentityPub:                pubKey,
		ChanID:                     id,
		MinFeePerKb:                btcutil.Amount(5000),
		TheirDustLimit:             btcutil.Amount(200),
		OurDustLimit:               btcutil.Amount(200),
		OurCommitKey:               privKey.PubKey(),
		TheirCommitKey:             pubKey,
		Capacity:                   btcutil.Amount(10000),
		OurBalance:                 btcutil.Amount(3000),
		TheirBalance:               btcutil.Amount(9000),
		OurCommitTx:                testTx,
		OurCommitSig:               bytes.Repeat([]byte{1}, 71),
		LocalElkrem:                sender,
		RemoteElkrem:               receiver,
		StateHintObsfucator:        obsfucator,
		FundingOutpoint:            testOutpoint,
		OurMultiSigKey:             privKey.PubKey(),
		TheirMultiSigKey:           privKey.PubKey(),
		FundingWitnessScript:       script,
		TheirCurrentRevocation:     privKey.PubKey(),
		TheirCurrentRevocationHash: key,
		OurDeliveryScript:          script,
		TheirDeliveryScript:        script,
		LocalCsvDelay:              5,
		RemoteCsvDelay:             9,
		NumUpdates:                 0,
		TotalSatoshisSent:          8,
		TotalSatoshisReceived:      2,
		CreationTime:               time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
		Db:                         cdb,
	}, nil
}

func TestOpenChannelPutGetDelete(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	// Create the test channel state, then add an additional fake HTLC
	// before syncing to disk.
	state, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}
	state.Htlcs = []*HTLC{
		{
			Incoming:        true,
			Amt:             10,
			RHash:           key,
			RefundTimeout:   1,
			RevocationDelay: 2,
		},
	}
	if err := state.FullSync(); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	openChannels, err := cdb.FetchOpenChannels(state.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch open channel: %v", err)
	}

	newState := openChannels[0]

	// The decoded channel state should be identical to what we stored
	// above.
	if !state.IdentityPub.IsEqual(newState.IdentityPub) {
		t.Fatalf("their id doesn't match")
	}
	if !reflect.DeepEqual(state.ChanID, newState.ChanID) {
		t.Fatalf("chan id's don't match")
	}
	if state.MinFeePerKb != newState.MinFeePerKb {
		t.Fatalf("fee/kb doesn't match")
	}
	if state.TheirDustLimit != newState.TheirDustLimit {
		t.Fatalf("their dust limit doesn't match")
	}
	if state.OurDustLimit != newState.OurDustLimit {
		t.Fatalf("our dust limit doesn't match")
	}
	if state.IsInitiator != newState.IsInitiator {
		t.Fatalf("initiator status doesn't match")
	}
	if state.ChanType != newState.ChanType {
		t.Fatalf("channel type doesn't match")
	}

	if !bytes.Equal(state.OurCommitKey.SerializeCompressed(),
		newState.OurCommitKey.SerializeCompressed()) {
		t.Fatalf("our commit key doesn't match")
	}
	if !bytes.Equal(state.TheirCommitKey.SerializeCompressed(),
		newState.TheirCommitKey.SerializeCompressed()) {
		t.Fatalf("their commit key doesn't match")
	}

	if state.Capacity != newState.Capacity {
		t.Fatalf("capacity doesn't match: %v vs %v", state.Capacity,
			newState.Capacity)
	}
	if state.OurBalance != newState.OurBalance {
		t.Fatalf("our balance doesn't match")
	}
	if state.TheirBalance != newState.TheirBalance {
		t.Fatalf("their balance doesn't match")
	}

	var b1, b2 bytes.Buffer
	if err := state.OurCommitTx.Serialize(&b1); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if err := newState.OurCommitTx.Serialize(&b2); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatalf("ourCommitTx doesn't match")
	}
	if !bytes.Equal(newState.OurCommitSig, state.OurCommitSig) {
		t.Fatalf("commit sigs don't match")
	}

	// TODO(roasbeef): replace with a single equal?
	if !reflect.DeepEqual(state.FundingOutpoint, newState.FundingOutpoint) {
		t.Fatalf("funding outpoint doesn't match")
	}

	if !bytes.Equal(state.OurMultiSigKey.SerializeCompressed(),
		newState.OurMultiSigKey.SerializeCompressed()) {
		t.Fatalf("our multisig key doesn't match")
	}
	if !bytes.Equal(state.TheirMultiSigKey.SerializeCompressed(),
		newState.TheirMultiSigKey.SerializeCompressed()) {
		t.Fatalf("their multisig key doesn't match")
	}
	if !bytes.Equal(state.FundingWitnessScript, newState.FundingWitnessScript) {
		t.Fatalf("redeem script doesn't match")
	}

	if !bytes.Equal(state.OurDeliveryScript, newState.OurDeliveryScript) {
		t.Fatalf("our delivery address doesn't match")
	}
	if !bytes.Equal(state.TheirDeliveryScript, newState.TheirDeliveryScript) {
		t.Fatalf("their delivery address doesn't match")
	}

	if state.NumUpdates != newState.NumUpdates {
		t.Fatalf("num updates doesn't match: %v vs %v",
			state.NumUpdates, newState.NumUpdates)
	}
	if state.RemoteCsvDelay != newState.RemoteCsvDelay {
		t.Fatalf("csv delay doesn't match: %v vs %v",
			state.RemoteCsvDelay, newState.RemoteCsvDelay)
	}
	if state.LocalCsvDelay != newState.LocalCsvDelay {
		t.Fatalf("csv delay doesn't match: %v vs %v",
			state.LocalCsvDelay, newState.LocalCsvDelay)
	}
	if state.TotalSatoshisSent != newState.TotalSatoshisSent {
		t.Fatalf("satoshis sent doesn't match: %v vs %v",
			state.TotalSatoshisSent, newState.TotalSatoshisSent)
	}
	if state.TotalSatoshisReceived != newState.TotalSatoshisReceived {
		t.Fatalf("satoshis received doesn't match")
	}

	if state.CreationTime.Unix() != newState.CreationTime.Unix() {
		t.Fatalf("creation time doesn't match")
	}

	// The local and remote elkrems should be identical.
	if !bytes.Equal(state.LocalElkrem.ToBytes(), newState.LocalElkrem.ToBytes()) {
		t.Fatalf("local elkrems don't match")
	}
	oldRemoteElkrem, err := state.RemoteElkrem.ToBytes()
	if err != nil {
		t.Fatalf("unable to serialize old remote elkrem: %v", err)
	}
	newRemoteElkrem, err := newState.RemoteElkrem.ToBytes()
	if err != nil {
		t.Fatalf("unable to serialize new remote elkrem: %v", err)
	}
	if !bytes.Equal(oldRemoteElkrem, newRemoteElkrem) {
		t.Fatalf("remote elkrems don't match")
	}
	if !newState.TheirCurrentRevocation.IsEqual(state.TheirCurrentRevocation) {
		t.Fatalf("revocation keys don't match")
	}
	if !bytes.Equal(newState.TheirCurrentRevocationHash[:], state.TheirCurrentRevocationHash[:]) {
		t.Fatalf("revocation hashes don't match")
	}
	if !reflect.DeepEqual(state.Htlcs[0], newState.Htlcs[0]) {
		t.Fatalf("htlcs don't match: %v vs %v", spew.Sdump(state.Htlcs[0]),
			spew.Sdump(newState.Htlcs[0]))
	}
	if !bytes.Equal(state.StateHintObsfucator[:],
		newState.StateHintObsfucator[:]) {
		t.Fatalf("obsfuctators don't match")
	}

	// Finally to wrap up the test, delete the state of the channel within
	// the database. This involves "closing" the channel which removes all
	// written state, and creates a small "summary" elsewhere within the
	// database.
	if err := state.CloseChannel(); err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// As the channel is now closed, attempting to fetch all open channels
	// for our fake node ID should return an empty slice.
	openChans, err := cdb.FetchOpenChannels(state.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch open channels: %v", err)
	}

	// TODO(roasbeef): need to assert much more
	if len(openChans) != 0 {
		t.Fatalf("all channels not deleted, found %v", len(openChans))
	}
}

func TestChannelStateTransition(t *testing.T) {
	cdb, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	// First create a minimal channel, then perform a full sync in order to
	// persist the data.
	channel, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}
	if err := channel.FullSync(); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	// Add some HTLCs which were added during this new state transition.
	// Half of the HTLCs are incoming, while the other half are outgoing.
	var htlcs []*HTLC
	for i := uint32(0); i < 10; i++ {
		var incoming bool
		if i > 5 {
			incoming = true
		}
		htlc := &HTLC{
			Incoming:        incoming,
			Amt:             50000,
			RHash:           key,
			RefundTimeout:   i,
			RevocationDelay: i + 2,
			OutputIndex:     uint16(i * 3),
		}
		htlcs = append(htlcs, htlc)
	}

	// Create a new channel delta which includes the above HTLCs, some
	// balance updates, and an increment of the current commitment height.
	// Additionally, modify the signature and commitment transaction.
	newSequence := uint32(129498)
	newSig := bytes.Repeat([]byte{3}, 71)
	newTx := channel.OurCommitTx.Copy()
	newTx.TxIn[0].Sequence = newSequence
	delta := &ChannelDelta{
		LocalBalance:  btcutil.Amount(1e8),
		RemoteBalance: btcutil.Amount(1e8),
		Htlcs:         htlcs,
		UpdateNum:     1,
	}

	// First update the local node's broadcastable state.
	if err := channel.UpdateCommitment(newTx, newSig, delta); err != nil {
		t.Fatalf("unable to update commitment: %v", err)
	}

	// The balances, new update, the HTLCs and the changes to the fake
	// commitment transaction along with the modified signature should all
	// have been updated.
	updatedChannel, err := cdb.FetchOpenChannels(channel.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch updated channel: %v", err)
	}
	if !bytes.Equal(updatedChannel[0].OurCommitSig, newSig) {
		t.Fatalf("sigs don't match %x vs %x",
			updatedChannel[0].OurCommitSig, newSig)
	}
	if updatedChannel[0].OurCommitTx.TxIn[0].Sequence != newSequence {
		t.Fatalf("sequence numbers don't match: %v vs %v",
			updatedChannel[0].OurCommitTx.TxIn[0].Sequence, newSequence)
	}
	if updatedChannel[0].OurBalance != delta.LocalBalance {
		t.Fatalf("local balances don't match: %v vs %v",
			updatedChannel[0].OurBalance, delta.LocalBalance)
	}
	if updatedChannel[0].TheirBalance != delta.RemoteBalance {
		t.Fatalf("remote balances don't match: %v vs %v",
			updatedChannel[0].TheirBalance, delta.RemoteBalance)
	}
	if updatedChannel[0].NumUpdates != uint64(delta.UpdateNum) {
		t.Fatalf("update # doesn't match: %v vs %v",
			updatedChannel[0].NumUpdates, delta.UpdateNum)
	}
	numDiskUpdates, err := updatedChannel[0].CommitmentHeight()
	if err != nil {
		t.Fatalf("unable to read commitment height from disk: %v", err)
	}
	if numDiskUpdates != uint64(delta.UpdateNum) {
		t.Fatalf("num disk updates doesn't match: %v vs %v",
			numDiskUpdates, delta.UpdateNum)
	}
	for i := 0; i < len(updatedChannel[0].Htlcs); i++ {
		originalHTLC := updatedChannel[0].Htlcs[i]
		diskHTLC := channel.Htlcs[i]
		if !reflect.DeepEqual(originalHTLC, diskHTLC) {
			t.Fatalf("htlc's dont match: %v vs %v",
				spew.Sdump(originalHTLC),
				spew.Sdump(diskHTLC))
		}
	}

	// Next, write to the log which tracks the necessary revocation state
	// needed to rectify any fishy behavior by the remote party. Modify the
	// current uncollapsed revocation state to simulate a state transition
	// by the remote party.
	newRevocation := bytes.Repeat([]byte{9}, 32)
	copy(channel.TheirCurrentRevocationHash[:], newRevocation)
	if err := channel.AppendToRevocationLog(delta); err != nil {
		t.Fatalf("unable to append to revocation log: %v", err)
	}

	// We should be able to fetch the channel delta created above by it's
	// update number with all the state properly reconstructed.
	diskDelta, err := channel.FindPreviousState(uint64(delta.UpdateNum))
	if err != nil {
		t.Fatalf("unable to fetch past delta: %v", err)
	}

	// The two deltas (the original vs the on-disk version) should
	// identical, and all HTLC data should properly be retained.
	if delta.LocalBalance != diskDelta.LocalBalance {
		t.Fatalf("local balances don't match")
	}
	if delta.RemoteBalance != diskDelta.RemoteBalance {
		t.Fatalf("remote balances don't match")
	}
	if delta.UpdateNum != diskDelta.UpdateNum {
		t.Fatalf("update number doesn't match")
	}
	for i := 0; i < len(delta.Htlcs); i++ {
		originalHTLC := delta.Htlcs[i]
		diskHTLC := diskDelta.Htlcs[i]
		if !reflect.DeepEqual(originalHTLC, diskHTLC) {
			t.Fatalf("htlc's dont match: %v vs %v",
				spew.Sdump(originalHTLC),
				spew.Sdump(diskHTLC))
		}
	}
	// The revocation state stored on-disk should now also be identical.
	updatedChannel, err = cdb.FetchOpenChannels(channel.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch updated channel: %v", err)
	}
	if !bytes.Equal(updatedChannel[0].TheirCurrentRevocationHash[:],
		newRevocation) {
		t.Fatalf("revocation state wasn't synced!")
	}
}
