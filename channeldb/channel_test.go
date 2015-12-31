package channeldb

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
)

var (
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

// createDbNamespace creates a new wallet database at the provided path and
// returns it along with the address manager namespace.
func createDbNamespace(dbPath string) (walletdb.DB, walletdb.Namespace, error) {
	db, err := walletdb.Create("bdb", dbPath)
	if err != nil {
		fmt.Println("fuk")
		return nil, nil, err
	}

	namespace, err := db.Namespace([]byte("waddr"))
	if err != nil {
		db.Close()
		return nil, nil, err
	}

	return db, namespace, nil
}

// setupManager creates a new address manager and returns a teardown function
// that should be invoked to ensure it is closed and removed upon completion.
func createTestManager(t *testing.T) (tearDownFunc func(), mgr *waddrmgr.Manager) {
	t.Parallel()

	// Create a new manager in a temp directory.
	dirName, err := ioutil.TempDir("", "mgrtest")
	if err != nil {
		t.Fatalf("Failed to create db temp dir: %v", err)
	}
	dbPath := filepath.Join(dirName, "mgrtest.db")
	db, namespace, err := createDbNamespace(dbPath)
	if err != nil {
		_ = os.RemoveAll(dirName)
		t.Fatalf("createDbNamespace: unexpected error: %v", err)
	}
	mgr, err = waddrmgr.Create(namespace, key[:], []byte("test"),
		[]byte("test"), ActiveNetParams, nil)
	if err != nil {
		db.Close()
		_ = os.RemoveAll(dirName)
		t.Fatalf("Failed to create Manager: %v", err)
	}
	tearDownFunc = func() {
		mgr.Close()
		db.Close()
		_ = os.RemoveAll(dirName)
	}
	if err := mgr.Unlock([]byte("test")); err != nil {
		t.Fatalf("unable to unlock mgr: %v", err)
	}
	return tearDownFunc, mgr
}

func TestOpenChannelEncodeDecode(t *testing.T) {
	teardown, manager := createTestManager(t)
	defer teardown()

	privKey, pubKey := btcec.PrivKeyFromBytes(btcec.S256(), key[:])
	addr, err := btcutil.NewAddressPubKey(pubKey.SerializeCompressed(), ActiveNetParams)
	if err != nil {
		t.Fatalf("unable to create delivery address")
	}

	script, err := txscript.MultiSigScript([]*btcutil.AddressPubKey{addr, addr}, 2)
	if err != nil {
		t.Fatalf("unable to create redeemScript")
	}

	state := OpenChannel{
		TheirLNID:              id,
		ChanID:                 id,
		MinFeePerKb:            btcutil.Amount(5000),
		OurCommitKey:           privKey,
		TheirCommitKey:         pubKey,
		Capacity:               btcutil.Amount(10000),
		OurBalance:             btcutil.Amount(3000),
		TheirBalance:           btcutil.Amount(7000),
		TheirCommitTx:          testTx,
		OurCommitTx:            testTx,
		FundingTx:              testTx,
		MultiSigKey:            privKey,
		FundingRedeemScript:    script,
		TheirCurrentRevocation: rev,
		OurDeliveryAddress:     addr,
		TheirDeliveryAddress:   addr,
		CsvDelay:               5,
		NumUpdates:             1,
		TotalSatoshisSent:      1,
		TotalSatoshisReceived:  2,
		CreationTime:           time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC),
	}

	var b bytes.Buffer
	if err := state.Encode(&b, manager); err != nil {
		t.Fatalf("unable to encode channel state: %v", err)
	}

	reader := bytes.NewReader(b.Bytes())
	newState := &OpenChannel{}
	if err := newState.Decode(reader, manager); err != nil {
		t.Fatalf("unable to decode channel state: %v", err)
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
		t.Fatalf("capacity doesn't match")
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

	if state.OurDeliveryAddress.EncodeAddress() != newState.OurDeliveryAddress.EncodeAddress() {
		t.Fatalf("our delivery address doesn't match")
	}
	if state.TheirDeliveryAddress.EncodeAddress() != newState.TheirDeliveryAddress.EncodeAddress() {
		t.Fatalf("their delivery address doesn't match")
	}

	if state.NumUpdates != newState.NumUpdates {
		t.Fatalf("num updates doesn't match: %v vs %v",
			state.NumUpdates, newState.NumUpdates)
	}
	if state.CsvDelay != newState.CsvDelay {
		t.Fatalf("csv delay doesn't match: %v vs %v",
			state.CsvDelay, newState.CsvDelay)
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
