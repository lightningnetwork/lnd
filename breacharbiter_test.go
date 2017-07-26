package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"sync"
	"testing"

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

	breachSignDescs = []lnwallet.SignDescriptor{
		{
			SingleTweak: []byte{
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02,
			},
			WitnessScript: []byte{
				0x00, 0x14, 0xee, 0x91, 0x41, 0x7e, 0x85, 0x6c, 0xde,
				0x10, 0xa2, 0x91, 0x1e, 0xdc, 0xbd, 0xbd, 0x69, 0xe2,
				0xef, 0xb5, 0x71, 0x48,
			},
			Output: &wire.TxOut{
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
			HashType: txscript.SigHashAll,
		},
		{
			SingleTweak: []byte{
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02,
			},
			WitnessScript: []byte{
				0x00, 0x14, 0xee, 0x91, 0x41, 0x7e, 0x85, 0x6c, 0xde,
				0x10, 0xa2, 0x91, 0x1e, 0xdc, 0xbd, 0xbd, 0x69, 0xe2,
				0xef, 0xb5, 0x71, 0x48,
			},
			Output: &wire.TxOut{
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
			HashType: txscript.SigHashAll,
		},
		{
			SingleTweak: []byte{
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02,
				0x02, 0x02, 0x02, 0x02, 0x02,
			},
			WitnessScript: []byte{
				0x00, 0x14, 0xee, 0x91, 0x41, 0x7e, 0x85, 0x6c, 0xde,
				0x10, 0xa2, 0x91, 0x1e, 0xdc, 0xbd, 0xbd, 0x69, 0xe2,
				0xef, 0xb5, 0x71, 0x48,
			},
			Output: &wire.TxOut{
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
			HashType: txscript.SigHashAll,
		},
	}

	breachedOutputs = []breachedOutput{
		{
			amt:           btcutil.Amount(1e7),
			outpoint:      breachOutPoints[0],
			witnessType:   lnwallet.CommitmentNoDelay,
			twoStageClaim: true,
		},

		{
			amt:           btcutil.Amount(2e9),
			outpoint:      breachOutPoints[1],
			witnessType:   lnwallet.CommitmentRevoke,
			twoStageClaim: false,
		},

		{
			amt:           btcutil.Amount(3e4),
			outpoint:      breachOutPoints[2],
			witnessType:   lnwallet.CommitmentDelayOutput,
			twoStageClaim: false,
		},
	}

	retributions = []retributionInfo{
		{
			commitHash: [chainhash.HashSize]byte{
				0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
				0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
				0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
				0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
			},
			chanPoint:      breachOutPoints[0],
			capacity:       btcutil.Amount(1e7),
			settledBalance: btcutil.Amount(1e7),
			selfOutput:     &breachedOutputs[0],
			revokedOutput:  &breachedOutputs[1],
			htlcOutputs:    []*breachedOutput{},
		},
		{
			commitHash: [chainhash.HashSize]byte{
				0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
				0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
				0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
				0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
			},
			chanPoint:      breachOutPoints[1],
			capacity:       btcutil.Amount(1e7),
			settledBalance: btcutil.Amount(1e7),
			selfOutput:     &breachedOutputs[0],
			revokedOutput:  &breachedOutputs[1],
			htlcOutputs: []*breachedOutput{
				&breachedOutputs[1],
				&breachedOutputs[2],
			},
		},
	}
)

// Parse the pubkeys in the breached outputs.
func initBreachedOutputs() error {
	for i := range breachedOutputs {
		bo := &breachedOutputs[i]

		// Parse the sign descriptor's pubkey.
		sd := &breachSignDescs[i]
		pubkey, err := btcec.ParsePubKey(breachKeys[i], btcec.S256())
		if err != nil {
			return fmt.Errorf("unable to parse pubkey: %v", breachKeys[i])
		}
		sd.PubKey = pubkey
		bo.signDescriptor = *sd
	}

	return nil
}

// Test that breachedOutput Encode/Decode works.
func TestBreachedOutputSerialization(t *testing.T) {
	if err := initBreachedOutputs(); err != nil {
		t.Fatalf("unable to init breached outputs: %v", err)
	}

	for i := 0; i < len(breachedOutputs); i++ {
		bo := &breachedOutputs[i]

		var buf bytes.Buffer

		if err := bo.Encode(&buf); err != nil {
			t.Fatalf("unable to serialize breached output [%v]: %v", i, err)
		}

		desBo := &breachedOutput{}
		if err := desBo.Decode(&buf); err != nil {
			t.Fatalf("unable to deserialize breached output [%v]: %v", i, err)
		}

		if !reflect.DeepEqual(bo, desBo) {
			t.Fatalf("original and deserialized breached outputs not equal:\n"+
				"original     : %+v\n"+
				"deserialized : %+v\n",
				bo, desBo)
		}
	}
}

// Test that retribution Encode/Decode works.
func TestRetributionSerialization(t *testing.T) {
	if err := initBreachedOutputs(); err != nil {
		t.Fatalf("unable to init breached outputs: %v", err)
	}

	for i := 0; i < len(retributions); i++ {
		ret := &retributions[i]

		remoteIdentity, err := btcec.ParsePubKey(breachKeys[i], btcec.S256())
		if err != nil {
			t.Fatalf("unable to parse public key [%v]: %v", i, err)
		}
		ret.remoteIdentity = *remoteIdentity

		var buf bytes.Buffer

		if err := ret.Encode(&buf); err != nil {
			t.Fatalf("unable to serialize retribution [%v]: %v", i, err)
		}

		desRet := &retributionInfo{}
		if err := desRet.Decode(&buf); err != nil {
			t.Fatalf("unable to deserialize retribution [%v]: %v", i, err)
		}

		if !reflect.DeepEqual(ret, desRet) {
			t.Fatalf("original and deserialized retribution infos not equal:\n"+
				"original     : %+v\n"+
				"deserialized : %+v\n",
				ret, desRet)
		}
	}
}

// copyRetInfo creates a complete copy of the given retributionInfo.
func copyRetInfo(retInfo *retributionInfo) *retributionInfo {
	ret := &retributionInfo{
		commitHash:     retInfo.commitHash,
		chanPoint:      retInfo.chanPoint,
		remoteIdentity: retInfo.remoteIdentity,
		capacity:       retInfo.capacity,
		settledBalance: retInfo.settledBalance,
		selfOutput:     retInfo.selfOutput,
		revokedOutput:  retInfo.revokedOutput,
		htlcOutputs:    make([]*breachedOutput, len(retInfo.htlcOutputs)),
		doneChan:       make(chan struct{}),
	}

	for i, htlco := range retInfo.htlcOutputs {
		ret.htlcOutputs[i] = htlco
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

// TestMockRetributionStore instantiates a mockRetributionStore and tests its
// behavior using the general RetributionStore test suite.
func TestMockRetributionStore(t *testing.T) {
	mrs := newMockRetributionStore()
	testRetributionStore(mrs, t)
}

// TestChannelDBRetributionStore instantiates a retributionStore backed by a
// channeldb.DB, and tests its behavior using the general RetributionStore test
// suite.
func TestChannelDBRetributionStore(t *testing.T) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to initialize temp directory for channeldb: %v", err)
	}
	defer os.RemoveAll(tempDirName)

	// Next, create channeldb for the first time.
	db, err := channeldb.Open(tempDirName)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}
	defer db.Close()

	// Finally, instantiate retribution store and execute RetributionStore test
	// suite.
	rs := newRetributionStore(db)
	testRetributionStore(rs, t)
}

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

// Test that the retribution persistence layer works.
func testRetributionStore(rs RetributionStore, t *testing.T) {
	if err := initBreachedOutputs(); err != nil {
		t.Fatalf("unable to init breached outputs: %v", err)
	}

	// Make sure that a new retribution store is actually emtpy.
	if count := countRetributions(t, rs); count != 0 {
		t.Fatalf("expected 0 retributions, found %v", count)
	}

	// Add first retribution state to the store.
	if err := rs.Add(&retributions[0]); err != nil {
		t.Fatalf("unable to add to retribution store: %v", err)
	}
	// Ensure that the retribution store has one retribution.
	if count := countRetributions(t, rs); count != 1 {
		t.Fatalf("expected 1 retributions, found %v", count)
	}

	// Add second retribution state to the store.
	if err := rs.Add(&retributions[1]); err != nil {
		t.Fatalf("unable to add to retribution store: %v", err)
	}
	// There should be 2 retributions in the store.
	if count := countRetributions(t, rs); count != 2 {
		t.Fatalf("expected 2 retributions, found %v", count)
	}

	// Retrieving the retribution states from the store should yield the same
	// values as the originals.
	rs.ForAll(func(ret *retributionInfo) error {
		equal0 := reflect.DeepEqual(ret, &retributions[0])
		equal1 := reflect.DeepEqual(ret, &retributions[1])
		if !equal0 || !equal1 {
			return errors.New("unexpected retribution retrieved from db")
		}
		return nil
	})

	// Remove the retribution states.
	if err := rs.Remove(&retributions[0].chanPoint); err != nil {
		t.Fatalf("unable to remove from retribution store: %v", err)
	}
	// Ensure that the retribution store has one retribution.
	if count := countRetributions(t, rs); count != 1 {
		t.Fatalf("expected 1 retributions, found %v", count)
	}

	if err := rs.Remove(&retributions[1].chanPoint); err != nil {
		t.Fatalf("unable to remove from retribution store: %v", err)
	}

	// Ensure that the retribution store is empty again.
	if count := countRetributions(t, rs); count != 0 {
		t.Fatalf("expected 0 retributions, found %v", count)
	}
}
