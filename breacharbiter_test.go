// +build !rpctest

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	breachOutPoints = []wire.OutPoint{
		{
			Hash: [chainhash.HashSize]byte{
				0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
				0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
				0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
				0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
			},
			Index: 9,
		},
		{
			Hash: [chainhash.HashSize]byte{
				0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
				0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
				0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
				0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
			},
			Index: 49,
		},
		{
			Hash: [chainhash.HashSize]byte{
				0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
				0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
				0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
				0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
			},
			Index: 23,
		},
	}

	breachKeys = [][]byte{
		{0x04, 0x11, 0xdb, 0x93, 0xe1, 0xdc, 0xdb, 0x8a,
			0x01, 0x6b, 0x49, 0x84, 0x0f, 0x8c, 0x53, 0xbc, 0x1e,
			0xb6, 0x8a, 0x38, 0x2e, 0x97, 0xb1, 0x48, 0x2e, 0xca,
			0xd7, 0xb1, 0x48, 0xa6, 0x90, 0x9a, 0x5c, 0xb2, 0xe0,
			0xea, 0xdd, 0xfb, 0x84, 0xcc, 0xf9, 0x74, 0x44, 0x64,
			0xf8, 0x2e, 0x16, 0x0b, 0xfa, 0x9b, 0x8b, 0x64, 0xf9,
			0xd4, 0xc0, 0x3f, 0x99, 0x9b, 0x86, 0x43, 0xf6, 0x56,
			0xb4, 0x12, 0xa3,
		},
		{0x07, 0x11, 0xdb, 0x93, 0xe1, 0xdc, 0xdb, 0x8a,
			0x01, 0x6b, 0x49, 0x84, 0x0f, 0x8c, 0x53, 0xbc, 0x1e,
			0xb6, 0x8a, 0x38, 0x2e, 0x97, 0xb1, 0x48, 0x2e, 0xca,
			0xd7, 0xb1, 0x48, 0xa6, 0x90, 0x9a, 0x5c, 0xb2, 0xe0,
			0xea, 0xdd, 0xfb, 0x84, 0xcc, 0xf9, 0x74, 0x44, 0x64,
			0xf8, 0x2e, 0x16, 0x0b, 0xfa, 0x9b, 0x8b, 0x64, 0xf9,
			0xd4, 0xc0, 0x3f, 0x99, 0x9b, 0x86, 0x43, 0xf6, 0x56,
			0xb4, 0x12, 0xa3,
		},
		{0x02, 0xce, 0x0b, 0x14, 0xfb, 0x84, 0x2b, 0x1b,
			0xa5, 0x49, 0xfd, 0xd6, 0x75, 0xc9, 0x80, 0x75, 0xf1,
			0x2e, 0x9c, 0x51, 0x0f, 0x8e, 0xf5, 0x2b, 0xd0, 0x21,
			0xa9, 0xa1, 0xf4, 0x80, 0x9d, 0x3b, 0x4d,
		},
	}

	breachedOutputs = []breachedOutput{
		{
			amt:         btcutil.Amount(1e7),
			outpoint:    breachOutPoints[0],
			witnessType: lnwallet.CommitmentNoDelay,
			signDesc: lnwallet.SignDescriptor{
				SingleTweak: []byte{
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02,
				},
				WitnessScript: []byte{
					0x00, 0x14, 0xee, 0x91, 0x41, 0x7e,
					0x85, 0x6c, 0xde, 0x10, 0xa2, 0x91,
					0x1e, 0xdc, 0xbd, 0xbd, 0x69, 0xe2,
					0xef, 0xb5, 0x71, 0x48,
				},
				Output: &wire.TxOut{
					Value: 5000000000,
					PkScript: []byte{
						0x41, // OP_DATA_65
						0x04, 0xd6, 0x4b, 0xdf, 0xd0,
						0x9e, 0xb1, 0xc5, 0xfe, 0x29,
						0x5a, 0xbd, 0xeb, 0x1d, 0xca,
						0x42, 0x81, 0xbe, 0x98, 0x8e,
						0x2d, 0xa0, 0xb6, 0xc1, 0xc6,
						0xa5, 0x9d, 0xc2, 0x26, 0xc2,
						0x86, 0x24, 0xe1, 0x81, 0x75,
						0xe8, 0x51, 0xc9, 0x6b, 0x97,
						0x3d, 0x81, 0xb0, 0x1c, 0xc3,
						0x1f, 0x04, 0x78, 0x34, 0xbc,
						0x06, 0xd6, 0xd6, 0xed, 0xf6,
						0x20, 0xd1, 0x84, 0x24, 0x1a,
						0x6a, 0xed, 0x8b, 0x63,
						0xa6, // 65-byte signature
						0xac, // OP_CHECKSIG
					},
				},
				HashType: txscript.SigHashAll,
			},
		},
		{
			amt:         btcutil.Amount(2e9),
			outpoint:    breachOutPoints[1],
			witnessType: lnwallet.CommitmentRevoke,
			signDesc: lnwallet.SignDescriptor{
				SingleTweak: []byte{
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02,
				},
				WitnessScript: []byte{
					0x00, 0x14, 0xee, 0x91, 0x41, 0x7e,
					0x85, 0x6c, 0xde, 0x10, 0xa2, 0x91,
					0x1e, 0xdc, 0xbd, 0xbd, 0x69, 0xe2,
					0xef, 0xb5, 0x71, 0x48,
				},
				Output: &wire.TxOut{
					Value: 5000000000,
					PkScript: []byte{
						0x41, // OP_DATA_65
						0x04, 0xd6, 0x4b, 0xdf, 0xd0,
						0x9e, 0xb1, 0xc5, 0xfe, 0x29,
						0x5a, 0xbd, 0xeb, 0x1d, 0xca,
						0x42, 0x81, 0xbe, 0x98, 0x8e,
						0x2d, 0xa0, 0xb6, 0xc1, 0xc6,
						0xa5, 0x9d, 0xc2, 0x26, 0xc2,
						0x86, 0x24, 0xe1, 0x81, 0x75,
						0xe8, 0x51, 0xc9, 0x6b, 0x97,
						0x3d, 0x81, 0xb0, 0x1c, 0xc3,
						0x1f, 0x04, 0x78, 0x34, 0xbc,
						0x06, 0xd6, 0xd6, 0xed, 0xf6,
						0x20, 0xd1, 0x84, 0x24, 0x1a,
						0x6a, 0xed, 0x8b, 0x63,
						0xa6, // 65-byte signature
						0xac, // OP_CHECKSIG
					},
				},
				HashType: txscript.SigHashAll,
			},
		},
		{
			amt:         btcutil.Amount(3e4),
			outpoint:    breachOutPoints[2],
			witnessType: lnwallet.CommitmentDelayOutput,
			signDesc: lnwallet.SignDescriptor{
				SingleTweak: []byte{
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
					0x02, 0x02,
				},
				WitnessScript: []byte{
					0x00, 0x14, 0xee, 0x91, 0x41, 0x7e,
					0x85, 0x6c, 0xde, 0x10, 0xa2, 0x91,
					0x1e, 0xdc, 0xbd, 0xbd, 0x69, 0xe2,
					0xef, 0xb5, 0x71, 0x48,
				},
				Output: &wire.TxOut{
					Value: 5000000000,
					PkScript: []byte{
						0x41, // OP_DATA_65
						0x04, 0xd6, 0x4b, 0xdf, 0xd0,
						0x9e, 0xb1, 0xc5, 0xfe, 0x29,
						0x5a, 0xbd, 0xeb, 0x1d, 0xca,
						0x42, 0x81, 0xbe, 0x98, 0x8e,
						0x2d, 0xa0, 0xb6, 0xc1, 0xc6,
						0xa5, 0x9d, 0xc2, 0x26, 0xc2,
						0x86, 0x24, 0xe1, 0x81, 0x75,
						0xe8, 0x51, 0xc9, 0x6b, 0x97,
						0x3d, 0x81, 0xb0, 0x1c, 0xc3,
						0x1f, 0x04, 0x78, 0x34, 0xbc,
						0x06, 0xd6, 0xd6, 0xed, 0xf6,
						0x20, 0xd1, 0x84, 0x24, 0x1a,
						0x6a, 0xed, 0x8b, 0x63,
						0xa6, // 65-byte signature
						0xac, // OP_CHECKSIG
					},
				},
				HashType: txscript.SigHashAll,
			},
		},
	}

	retributionMap = make(map[wire.OutPoint]retributionInfo)
	retributions   = []retributionInfo{
		{
			commitHash: [chainhash.HashSize]byte{
				0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
				0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
				0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
				0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
			},
			chainHash: [chainhash.HashSize]byte{
				0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
				0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
				0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
				0x6b, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
			},
			chanPoint:      breachOutPoints[0],
			capacity:       btcutil.Amount(1e7),
			settledBalance: btcutil.Amount(1e7),
			// Set to breachedOutputs 0 and 1 in init()
			breachedOutputs: []breachedOutput{{}, {}},
		},
		{
			commitHash: [chainhash.HashSize]byte{
				0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
				0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
				0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
				0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
			},
			chainHash: [chainhash.HashSize]byte{
				0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
				0xb7, 0x94, 0x39, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
				0x6b, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
				0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
			},
			chanPoint:      breachOutPoints[1],
			capacity:       btcutil.Amount(1e7),
			settledBalance: btcutil.Amount(1e7),
			// Set to breachedOutputs 1 and 2 in init()
			breachedOutputs: []breachedOutput{{}, {}},
		},
	}
)

func init() {
	// Ensure that breached outputs are initialized before starting tests.
	if err := initBreachedOutputs(); err != nil {
		panic(err)
	}

	// Populate a retribution map to for convenience, to allow lookups by
	// channel point.
	for i := range retributions {
		retInfo := &retributions[i]
		retInfo.remoteIdentity = breachedOutputs[i].signDesc.PubKey
		retInfo.breachedOutputs[0] = breachedOutputs[i]
		retInfo.breachedOutputs[1] = breachedOutputs[i+1]

		retributionMap[retInfo.chanPoint] = *retInfo

	}
}

// FailingRetributionStore wraps a RetributionStore and supports controlled
// restarts of the persistent instance. This allows us to test (1) that no
// modifications to the entries are made between calls or through side effects,
// and (2) that the database is actually being persisted between actions.
type FailingRetributionStore interface {
	RetributionStore

	Restart()
}

// failingRetributionStore is a concrete implementation of a
// FailingRetributionStore. It wraps an underlying RetributionStore and is
// parameterized entirely by a restart function, which is intended to simulate a
// full stop/start of the store.
type failingRetributionStore struct {
	mu sync.Mutex

	rs RetributionStore

	restart func() RetributionStore
}

// newFailingRetributionStore creates a new failing retribution store. The given
// restart closure should ensure that it is reloading its contents from the
// persistent source.
func newFailingRetributionStore(
	restart func() RetributionStore) *failingRetributionStore {

	return &failingRetributionStore{
		mu:      sync.Mutex{},
		rs:      restart(),
		restart: restart,
	}
}

func (frs *failingRetributionStore) Restart() {
	frs.mu.Lock()
	frs.rs = frs.restart()
	frs.mu.Unlock()
}

func (frs *failingRetributionStore) Add(retInfo *retributionInfo) error {
	frs.mu.Lock()
	defer frs.mu.Unlock()

	return frs.rs.Add(retInfo)
}

func (frs *failingRetributionStore) Remove(key *wire.OutPoint) error {
	frs.mu.Lock()
	defer frs.mu.Unlock()

	return frs.rs.Remove(key)
}

func (frs *failingRetributionStore) ForAll(cb func(*retributionInfo) error) error {
	frs.mu.Lock()
	defer frs.mu.Unlock()

	return frs.rs.ForAll(cb)
}

// Parse the pubkeys in the breached outputs.
func initBreachedOutputs() error {
	for i := range breachedOutputs {
		bo := &breachedOutputs[i]

		// Parse the sign descriptor's pubkey.
		pubkey, err := btcec.ParsePubKey(breachKeys[i], btcec.S256())
		if err != nil {
			return fmt.Errorf("unable to parse pubkey: %v",
				breachKeys[i])
		}
		bo.signDesc.PubKey = pubkey
	}

	return nil
}

// Test that breachedOutput Encode/Decode works.
func TestBreachedOutputSerialization(t *testing.T) {
	for i := range breachedOutputs {
		bo := &breachedOutputs[i]

		var buf bytes.Buffer

		if err := bo.Encode(&buf); err != nil {
			t.Fatalf("unable to serialize breached output [%v]: %v",
				i, err)
		}

		desBo := &breachedOutput{}
		if err := desBo.Decode(&buf); err != nil {
			t.Fatalf("unable to deserialize "+
				"breached output [%v]: %v", i, err)
		}

		if !reflect.DeepEqual(bo, desBo) {
			t.Fatalf("original and deserialized "+
				"breached outputs not equal:\n"+
				"original     : %+v\n"+
				"deserialized : %+v\n",
				bo, desBo)
		}
	}
}

// Test that retribution Encode/Decode works.
func TestRetributionSerialization(t *testing.T) {
	for i := range retributions {
		ret := &retributions[i]

		var buf bytes.Buffer

		if err := ret.Encode(&buf); err != nil {
			t.Fatalf("unable to serialize retribution [%v]: %v",
				i, err)
		}

		desRet := &retributionInfo{}
		if err := desRet.Decode(&buf); err != nil {
			t.Fatalf("unable to deserialize retribution [%v]: %v",
				i, err)
		}

		if !reflect.DeepEqual(ret, desRet) {
			t.Fatalf("original and deserialized "+
				"retribution infos not equal:\n"+
				"original     : %+v\n"+
				"deserialized : %+v\n",
				ret, desRet)
		}
	}
}

// copyRetInfo creates a complete copy of the given retributionInfo.
func copyRetInfo(retInfo *retributionInfo) *retributionInfo {
	nOutputs := len(retInfo.breachedOutputs)

	ret := &retributionInfo{
		commitHash:      retInfo.commitHash,
		chainHash:       retInfo.chainHash,
		chanPoint:       retInfo.chanPoint,
		remoteIdentity:  retInfo.remoteIdentity,
		capacity:        retInfo.capacity,
		settledBalance:  retInfo.settledBalance,
		breachedOutputs: make([]breachedOutput, nOutputs),
	}

	for i := range retInfo.breachedOutputs {
		ret.breachedOutputs[i] = retInfo.breachedOutputs[i]
	}

	return ret
}

// mockRetributionStore implements the RetributionStore interface and is backed
// by an in-memory map. Access to the internal state is provided by a mutex.
// TODO(cfromknecht) extend to support and test controlled failures.
type mockRetributionStore struct {
	mu    sync.Mutex
	state map[wire.OutPoint]*retributionInfo
}

func newMockRetributionStore() *mockRetributionStore {
	return &mockRetributionStore{
		mu:    sync.Mutex{},
		state: make(map[wire.OutPoint]*retributionInfo),
	}
}

func (rs *mockRetributionStore) Add(retInfo *retributionInfo) error {
	rs.mu.Lock()
	rs.state[retInfo.chanPoint] = copyRetInfo(retInfo)
	rs.mu.Unlock()

	return nil
}

func (rs *mockRetributionStore) Remove(key *wire.OutPoint) error {
	rs.mu.Lock()
	delete(rs.state, *key)
	rs.mu.Unlock()

	return nil
}

func (rs *mockRetributionStore) ForAll(cb func(*retributionInfo) error) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	for _, retInfo := range rs.state {
		if err := cb(copyRetInfo(retInfo)); err != nil {
			return err
		}
	}

	return nil
}

var retributionStoreTestSuite = []struct {
	name string
	test func(FailingRetributionStore, *testing.T)
}{
	{
		"Initialization",
		testRetributionStoreInit,
	},
	{
		"Add/Remove",
		testRetributionStoreAddRemove,
	},
	{
		"Persistence",
		testRetributionStorePersistence,
	},
	{
		"Overwrite",
		testRetributionStoreOverwrite,
	},
	{
		"RemoveEmpty",
		testRetributionStoreRemoveEmpty,
	},
}

// TestMockRetributionStore instantiates a mockRetributionStore and tests its
// behavior using the general RetributionStore test suite.
func TestMockRetributionStore(t *testing.T) {
	for _, test := range retributionStoreTestSuite {
		t.Run(
			"mockRetributionStore."+test.name,
			func(tt *testing.T) {
				mrs := newMockRetributionStore()
				frs := newFailingRetributionStore(
					func() RetributionStore { return mrs },
				)
				test.test(frs, tt)
			},
		)
	}
}

// TestChannelDBRetributionStore instantiates a retributionStore backed by a
// channeldb.DB, and tests its behavior using the general RetributionStore test
// suite.
func TestChannelDBRetributionStore(t *testing.T) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to initialize temp "+
			"directory for channeldb: %v", err)
	}
	defer os.RemoveAll(tempDirName)

	// Disable logging to prevent panics bc. of global state
	channeldb.UseLogger(btclog.Disabled)

	// Next, create channeldb for the first time.
	db, err := channeldb.Open(tempDirName)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}
	defer db.Close()

	restartDb := func() RetributionStore {
		// Close and reopen channeldb
		if err = db.Close(); err != nil {
			t.Fatalf("unalbe to close channeldb during restart: %v",
				err)
		}
		db, err = channeldb.Open(tempDirName)
		if err != nil {
			t.Fatalf("unable to open channeldb: %v", err)
		}

		return newRetributionStore(db)
	}

	// Finally, instantiate retribution store and execute RetributionStore
	// test suite.
	for _, test := range retributionStoreTestSuite {
		t.Run(
			"channeldbDBRetributionStore."+test.name,
			func(tt *testing.T) {
				if err = db.Wipe(); err != nil {
					t.Fatalf("unable to wipe channeldb: %v",
						err)
				}

				frs := newFailingRetributionStore(restartDb)
				test.test(frs, tt)
			},
		)
	}
}

// countRetributions uses a retribution store's ForAll to count the number of
// elements emitted from the store.
func countRetributions(t *testing.T, rs RetributionStore) int {
	count := 0
	err := rs.ForAll(func(_ *retributionInfo) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("unable to list retributions in db: %v", err)
	}
	return count
}

// testRetributionStoreAddRemove executes a generic test suite for any concrete
// implementation of the RetributionStore interface. This test adds all
// retributions to the store, confirms that they are all present, and then
// removes each one individually.  Between each addition or removal, the number
// of elements in the store is checked to ensure that it only changes by one.
func testRetributionStoreAddRemove(frs FailingRetributionStore, t *testing.T) {
	// Make sure that a new retribution store is actually emtpy.
	if count := countRetributions(t, frs); count != 0 {
		t.Fatalf("expected 0 retributions, found %v", count)
	}

	// Add all retributions, check that ForAll returns the correct
	// information, and then remove all retributions.
	testRetributionStoreAdds(frs, t, false)
	testRetributionStoreForAll(frs, t, false)
	testRetributionStoreRemoves(frs, t, false)
}

// testRetributionStorePersistence executes the same general test as
// testRetributionStoreAddRemove, except that it also restarts the store between
// each operation to ensure that the results are properly persisted.
func testRetributionStorePersistence(frs FailingRetributionStore, t *testing.T) {
	// Make sure that a new retribution store is still emtpy after failing
	// right off the bat.
	frs.Restart()
	if count := countRetributions(t, frs); count != 0 {
		t.Fatalf("expected 1 retributions, found %v", count)
	}

	// Insert all retributions into the database, restarting and checking
	// between subsequent calls to test that each intermediate additions are
	// persisted.
	testRetributionStoreAdds(frs, t, true)

	// After all retributions have been inserted, verify that the store
	// emits a distinct set of retributions that are equivalent to the test
	// vector.
	testRetributionStoreForAll(frs, t, true)

	// Remove all retributions from the database, restarting and checking
	// between subsequent calls to test that each intermediate removals are
	// persisted.
	testRetributionStoreRemoves(frs, t, true)
}

// testRetributionStoreInit ensures that a retribution store is always
// initialized with no retributions.
func testRetributionStoreInit(frs FailingRetributionStore, t *testing.T) {
	// Make sure that a new retribution store starts empty.
	if count := countRetributions(t, frs); count != 0 {
		t.Fatalf("expected 0 retributions, found %v", count)
	}
}

// testRetributionStoreRemoveEmpty ensures that a retribution store will not
// fail or panic if it is instructed to remove an entry while empty.
func testRetributionStoreRemoveEmpty(frs FailingRetributionStore, t *testing.T) {
	testRetributionStoreRemoves(frs, t, false)
}

// testRetributionStoreOverwrite ensures that attempts to write retribution
// information regarding a channel point that already exists does not change the
// total number of entries held by the retribution store.
func testRetributionStoreOverwrite(frs FailingRetributionStore, t *testing.T) {
	// Initially, add all retributions to store.
	testRetributionStoreAdds(frs, t, false)

	// Overwrite the initial entries again.
	for i, retInfo := range retributions {
		if err := frs.Add(&retInfo); err != nil {
			t.Fatalf("unable to add to retribution %v to store: %v",
				i, err)
		}
	}

	// Check that retribution store still has 2 entries.
	if count := countRetributions(t, frs); count != 2 {
		t.Fatalf("expected 2 retributions, found %v", count)
	}
}

// testRetributionStoreAdds adds all of the test retributions to the database,
// ensuring that the total number of elements increases by exactly 1 after each
// operation.  If the `failing` flag is provide, the test will restart the
// database and confirm that the delta is still 1.
func testRetributionStoreAdds(
	frs FailingRetributionStore,
	t *testing.T,
	failing bool) {

	// Iterate over retributions, adding each from the store. If we are
	// testing the store under failures, we restart the store and verify
	// that the contents are the same.
	for i, retInfo := range retributions {
		// Snapshot number of entires before and after the addition.
		nbefore := countRetributions(t, frs)
		if err := frs.Add(&retInfo); err != nil {
			t.Fatalf("unable to add to retribution %v to store: %v",
				i, err)
		}
		nafter := countRetributions(t, frs)

		// Check that only one retribution was added.
		if nafter-nbefore != 1 {
			t.Fatalf("expected %v retributions, found %v",
				nbefore+1, nafter)
		}

		if failing {
			frs.Restart()

			// Check that retribution store has persisted addition
			// after restarting.
			nrestart := countRetributions(t, frs)
			if nrestart-nbefore != 1 {
				t.Fatalf("expected %v retributions, found %v",
					nbefore+1, nrestart)
			}
		}
	}
}

// testRetributionStoreRemoves removes all of the test retributions to the
// database, ensuring that the total number of elements decreases by exactly 1
// after each operation.  If the `failing` flag is provide, the test will
// restart the database and confirm that the delta is the same.
func testRetributionStoreRemoves(
	frs FailingRetributionStore,
	t *testing.T,
	failing bool) {

	// Iterate over retributions, removing each from the store. If we are
	// testing the store under failures, we restart the store and verify
	// that the contents are the same.
	for i, retInfo := range retributions {
		// Snapshot number of entires before and after the removal.
		nbefore := countRetributions(t, frs)
		if err := frs.Remove(&retInfo.chanPoint); err != nil {
			t.Fatalf("unable to remove to retribution %v "+
				"from store: %v", i, err)
		}
		nafter := countRetributions(t, frs)

		// If the store is empty, increment nbefore to simulate the
		// removal of one element.
		if nbefore == 0 {
			nbefore++
		}

		// Check that only one retribution was removed.
		if nbefore-nafter != 1 {
			t.Fatalf("expected %v retributions, found %v",
				nbefore-1, nafter)
		}

		if failing {
			frs.Restart()

			// Check that retribution store has persisted removal
			// after restarting.
			nrestart := countRetributions(t, frs)
			if nbefore-nrestart != 1 {
				t.Fatalf("expected %v retributions, found %v",
					nbefore-1, nrestart)
			}
		}
	}
}

// testRetributionStoreForAll iterates over the current entries in the
// retribution store, ensuring that each entry in the database is unique, and
// corresponds to exactly one of the entries in the test vector. If the
// `failing` flag is provide, the test will restart the database and confirm
// that the entries again validate against the test vectors.
func testRetributionStoreForAll(
	frs FailingRetributionStore,
	t *testing.T,
	failing bool) {

	// nrets is the number of retributions in the test vector
	nrets := len(retributions)

	// isRestart indicates whether or not the database has been restarted.
	// When testing for failures, this allows the test case to make a second
	// attempt without causing a subsequent restart on the second pass.
	var isRestart bool

restartCheck:
	// Construct a set of all channel points presented by the store. Entires
	// are only be added to the set if their corresponding retribution
	// infromation matches the test vector.
	var foundSet = make(map[wire.OutPoint]struct{})

	// Iterate through the stored retributions, checking to see if we have
	// an equivalent retribution in the test vector. This will return an
	// error unless all persisted retributions exist in the test vector.
	if err := frs.ForAll(func(ret *retributionInfo) error {
		// Fetch the retribution information from the test vector. If
		// the entry does not exist, the test returns an error.
		if exRetInfo, ok := retributionMap[ret.chanPoint]; ok {
			// Compare the presented retribution information with
			// the expected value, fail if they are inconsistent.
			if !reflect.DeepEqual(ret, &exRetInfo) {
				return fmt.Errorf("unexpected retribution "+
					"retrieved from db --\n"+
					"want: %#v\ngot: %#v", exRetInfo, ret,
				)
			}

			// Retribution information from database matches the
			// test vector, record the channel point in the found
			// map.
			foundSet[ret.chanPoint] = struct{}{}

		} else {
			return fmt.Errorf("unkwown retribution retrieved "+
				"from db: %v", ret)
		}

		return nil
	}); err != nil {
		t.Fatalf("failed to iterate over persistent retributions: %v",
			err)
	}

	// Check that retribution store emits nrets entires
	if count := countRetributions(t, frs); count != nrets {
		t.Fatalf("expected %v retributions, found %v", nrets, count)
	}

	// Confirm that all of the retributions emitted from the iteration
	// correspond to unique channel points.
	nunique := len(foundSet)
	if nunique != nrets {
		t.Fatalf("expected %v unique retributions, only found %v",
			nrets, nunique)
	}

	// If in failure mode on only on first pass, restart the database and
	// rexecute the test.
	if failing && !isRestart {
		frs.Restart()
		isRestart = true

		goto restartCheck
	}
}
