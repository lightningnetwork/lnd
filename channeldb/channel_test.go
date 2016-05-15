package channeldb

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	_ "github.com/roasbeef/btcwallet/walletdb/bdb"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/roasbeef/btcutil"
)

var (
	netParams = &chaincfg.SegNet4Params

	key = [wire.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	id = [wire.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	rev = [20]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  wire.ShaHash{},
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
)

type MockEncryptorDecryptor struct {
}

func (m *MockEncryptorDecryptor) Encrypt(n []byte) ([]byte, error) {
	return n, nil
}

func (m *MockEncryptorDecryptor) Decrypt(n []byte) ([]byte, error) {
	return n, nil
}

func (m *MockEncryptorDecryptor) OverheadSize() uint32 {
	return 0
}

var _ EncryptorDecryptor = (*MockEncryptorDecryptor)(nil)

func TestOpenChannelEncodeDecode(t *testing.T) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v")
	}
	defer os.RemoveAll(tempDirName)

	// Next, create channeldb for the first time, also setting a mock
	// EncryptorDecryptor implementation for testing purposes.
	cdb, err := Open(tempDirName, netParams)
	if err != nil {
		t.Fatalf("unable to create channeldb: %v", err)
	}
	cdb.RegisterCryptoSystem(&MockEncryptorDecryptor{})
	defer cdb.Close()

	privKey, pubKey := btcec.PrivKeyFromBytes(btcec.S256(), key[:])
	addr, err := btcutil.NewAddressPubKey(pubKey.SerializeCompressed(), netParams)
	if err != nil {
		t.Fatalf("unable to create delivery address")
	}

	script, err := txscript.MultiSigScript([]*btcutil.AddressPubKey{addr, addr}, 2)
	if err != nil {
		t.Fatalf("unable to create redeemScript")
	}

	// Simulate 1000 channel updates via progression of the elkrem
	// revocation trees.
	sender := elkrem.NewElkremSender(32, key)
	receiver := elkrem.NewElkremReceiver(32)
	for i := 0; i < 1000; i++ {
		preImage, err := sender.AtIndex(uint64(i))
		if err != nil {
			t.Fatalf("unable to progress elkrem sender: %v", err)
		}

		if receiver.AddNext(preImage); err != nil {
			t.Fatalf("unable to progress elkrem receiver: %v", err)
		}
	}

	state := OpenChannel{
		TheirLNID:              id,
		ChanID:                 id,
		MinFeePerKb:            btcutil.Amount(5000),
		OurCommitKey:           privKey,
		TheirCommitKey:         pubKey,
		Capacity:               btcutil.Amount(10000),
		OurBalance:             btcutil.Amount(3000),
		TheirBalance:           btcutil.Amount(9000),
		TheirCommitTx:          testTx,
		OurCommitTx:            testTx,
		LocalElkrem:            &sender,
		RemoteElkrem:           &receiver,
		FundingTx:              testTx,
		MultiSigKey:            privKey,
		FundingRedeemScript:    script,
		TheirCurrentRevocation: rev,
		OurDeliveryScript:      script,
		TheirDeliveryScript:    script,
		LocalCsvDelay:          5,
		RemoteCsvDelay:         9,
		NumUpdates:             1,
		TotalSatoshisSent:      8,
		TotalSatoshisReceived:  2,
		TotalNetFees:           9,
		CreationTime:           time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
		Db:                     cdb,
	}

	if err := state.FullSync(); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	newState, err := cdb.FetchOpenChannel(id)
	if err != nil {
		t.Fatalf("unable to fetch open channel: %v", err)
	}

	// The decoded channel state should be identical to what we stored
	// above.
	if !bytes.Equal(state.TheirLNID[:], newState.TheirLNID[:]) {
		t.Fatalf("their id doesn't match")
	}
	if !bytes.Equal(state.ChanID[:], newState.ChanID[:]) {
		t.Fatalf("chan id's don't match")
	}
	if state.MinFeePerKb != newState.MinFeePerKb {
		t.Fatalf("fee/kb doens't match")
	}

	if !bytes.Equal(state.OurCommitKey.Serialize(),
		newState.OurCommitKey.Serialize()) {
		t.Fatalf("our commit key dont't match")
	}
	if !bytes.Equal(state.TheirCommitKey.SerializeCompressed(),
		newState.TheirCommitKey.SerializeCompressed()) {
		t.Fatalf("their commit key dont't match")
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
	if err := state.TheirCommitTx.Serialize(&b1); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if err := newState.TheirCommitTx.Serialize(&b2); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatalf("theirCommitTx doesn't match")
	}

	b1.Reset()
	b2.Reset()

	if err := state.OurCommitTx.Serialize(&b1); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if err := newState.OurCommitTx.Serialize(&b2); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatalf("ourCommitTx doesn't match")
	}

	b1.Reset()
	b2.Reset()

	if err := state.FundingTx.Serialize(&b1); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if err := newState.FundingTx.Serialize(&b2); err != nil {
		t.Fatalf("unable to serialize transaction")
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatalf("funding tx doesn't match")
	}

	if !bytes.Equal(state.MultiSigKey.Serialize(),
		newState.MultiSigKey.Serialize()) {
		t.Fatalf("multisig key doesn't match")
	}
	if !bytes.Equal(state.FundingRedeemScript, newState.FundingRedeemScript) {
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
}

func TestOpenChannelEncodeDecodeCorruption(t *testing.T) {
}
