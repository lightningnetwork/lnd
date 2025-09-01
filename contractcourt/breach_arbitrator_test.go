package contractcourt

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

var (
	defaultTimeout = 10 * time.Second

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
		{0x02, 0xce, 0x0b, 0x14, 0xfb, 0x84, 0x2b, 0x1b,
			0x2e, 0x9c, 0x51, 0x0f, 0x8e, 0xf5, 0x2b, 0xd0, 0x21,
			0xa5, 0x49, 0xfd, 0xd6, 0x75, 0xc9, 0x80, 0x75, 0xf1,
			0xa3, 0xa1, 0xf4, 0x80, 0x9d, 0x3b, 0x4d,
		},
	}

	breachedOutputs = []breachedOutput{
		{
			amt:         btcutil.Amount(1e7),
			outpoint:    breachOutPoints[0],
			witnessType: input.CommitmentNoDelay,
			signDesc: input.SignDescriptor{
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
			secondLevelWitnessScript: breachKeys[0],
		},
		{
			amt:         btcutil.Amount(1e7),
			outpoint:    breachOutPoints[0],
			witnessType: input.CommitSpendNoDelayTweakless,
			signDesc: input.SignDescriptor{
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
			secondLevelWitnessScript: breachKeys[0],
		},
		{
			amt:         btcutil.Amount(2e9),
			outpoint:    breachOutPoints[1],
			witnessType: input.CommitmentRevoke,
			signDesc: input.SignDescriptor{
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
			secondLevelWitnessScript: breachKeys[0],
		},
		{
			amt:         btcutil.Amount(3e4),
			outpoint:    breachOutPoints[2],
			witnessType: input.CommitmentDelayOutput,
			signDesc: input.SignDescriptor{
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
			secondLevelWitnessScript: breachKeys[0],
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
			chanPoint:    breachOutPoints[0],
			breachHeight: 337,
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
			chanPoint:    breachOutPoints[1],
			breachHeight: 420420,
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
	RetributionStorer

	Restart()
}

// failingRetributionStore is a concrete implementation of a
// FailingRetributionStore. It wraps an underlying RetributionStore and is
// parameterized entirely by a restart function, which is intended to simulate a
// full stop/start of the store.
type failingRetributionStore struct {
	mu sync.Mutex

	rs RetributionStorer

	nextAddErr error

	restart func() RetributionStorer
}

// newFailingRetributionStore creates a new failing retribution store. The given
// restart closure should ensure that it is reloading its contents from the
// persistent source.
func newFailingRetributionStore(
	restart func() RetributionStorer) *failingRetributionStore {

	return &failingRetributionStore{
		mu:      sync.Mutex{},
		rs:      restart(),
		restart: restart,
	}
}

// FailNextAdd instructs the retribution store to return the provided error. If
// the error is nil, a generic default will be used.
func (frs *failingRetributionStore) FailNextAdd(err error) {
	if err == nil {
		err = errors.New("retribution store failed")
	}

	frs.mu.Lock()
	frs.nextAddErr = err
	frs.mu.Unlock()
}

func (frs *failingRetributionStore) Restart() {
	frs.mu.Lock()
	frs.rs = frs.restart()
	frs.mu.Unlock()
}

// Add forwards the call to the underlying retribution store, unless this Add
// has been previously instructed to fail.
func (frs *failingRetributionStore) Add(retInfo *retributionInfo) error {
	frs.mu.Lock()
	defer frs.mu.Unlock()

	if frs.nextAddErr != nil {
		err := frs.nextAddErr
		frs.nextAddErr = nil
		return err
	}

	return frs.rs.Add(retInfo)
}

func (frs *failingRetributionStore) IsBreached(chanPoint *wire.OutPoint) (bool, error) {
	frs.mu.Lock()
	defer frs.mu.Unlock()

	return frs.rs.IsBreached(chanPoint)
}

func (frs *failingRetributionStore) Remove(key *wire.OutPoint) error {
	frs.mu.Lock()
	defer frs.mu.Unlock()

	return frs.rs.Remove(key)
}

func (frs *failingRetributionStore) ForAll(cb func(*retributionInfo) error,
	reset func()) error {

	frs.mu.Lock()
	defer frs.mu.Unlock()

	return frs.rs.ForAll(cb, reset)
}

// Parse the pubkeys in the breached outputs.
func initBreachedOutputs() error {
	for i := range breachedOutputs {
		bo := &breachedOutputs[i]

		// Parse the sign descriptor's pubkey.
		pubkey, err := btcec.ParsePubKey(breachKeys[i])
		if err != nil {
			return fmt.Errorf("unable to parse pubkey: %v",
				breachKeys[i])
		}
		bo.signDesc.KeyDesc.PubKey = pubkey
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
		breachHeight:    retInfo.breachHeight,
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
	mu       sync.Mutex
	state    map[wire.OutPoint]*retributionInfo
	finalTxs map[wire.OutPoint]*wire.MsgTx
}

func newMockRetributionStore() *mockRetributionStore {
	return &mockRetributionStore{
		mu:       sync.Mutex{},
		state:    make(map[wire.OutPoint]*retributionInfo),
		finalTxs: make(map[wire.OutPoint]*wire.MsgTx),
	}
}

func (rs *mockRetributionStore) Add(retInfo *retributionInfo) error {
	rs.mu.Lock()
	rs.state[retInfo.chanPoint] = copyRetInfo(retInfo)
	rs.mu.Unlock()

	return nil
}

func (rs *mockRetributionStore) IsBreached(chanPoint *wire.OutPoint) (bool, error) {
	rs.mu.Lock()
	_, ok := rs.state[*chanPoint]
	rs.mu.Unlock()

	return ok, nil
}

func (rs *mockRetributionStore) Finalize(chanPoint *wire.OutPoint,
	finalTx *wire.MsgTx) error {

	rs.mu.Lock()
	rs.finalTxs[*chanPoint] = finalTx
	rs.mu.Unlock()

	return nil
}

func (rs *mockRetributionStore) GetFinalizedTxn(
	chanPoint *wire.OutPoint) (*wire.MsgTx, error) {

	rs.mu.Lock()
	finalTx := rs.finalTxs[*chanPoint]
	rs.mu.Unlock()

	return finalTx, nil
}

func (rs *mockRetributionStore) Remove(key *wire.OutPoint) error {
	rs.mu.Lock()
	delete(rs.state, *key)
	delete(rs.finalTxs, *key)
	rs.mu.Unlock()

	return nil
}

func (rs *mockRetributionStore) ForAll(cb func(*retributionInfo) error,
	reset func()) error {

	rs.mu.Lock()
	defer rs.mu.Unlock()

	reset()
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
					func() RetributionStorer { return mrs },
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
	// Finally, instantiate retribution store and execute RetributionStore
	// test suite.
	for _, test := range retributionStoreTestSuite {
		t.Run(
			"channeldbDBRetributionStore."+test.name,
			func(tt *testing.T) {
				db := channeldb.OpenForTesting(t, t.TempDir())

				restartDb := func() RetributionStorer {
					// Close and reopen channeldb
					if err := db.Close(); err != nil {
						t.Fatalf("unable to close "+
							"channeldb during "+
							"restart: %v",
							err)
					}
					db = channeldb.OpenForTesting(
						t, db.Path(),
					)

					return NewRetributionStore(db)
				}

				frs := newFailingRetributionStore(restartDb)
				test.test(frs, tt)
			},
		)
	}
}

// countRetributions uses a retribution store's ForAll to count the number of
// elements emitted from the store.
func countRetributions(t *testing.T, rs RetributionStorer) int {
	count := 0
	err := rs.ForAll(func(_ *retributionInfo) error {
		count++
		return nil
	}, func() {
		count = 0
	})
	require.NoError(t, err, "unable to list retributions in db")
	return count
}

// testRetributionStoreAddRemove executes a generic test suite for any concrete
// implementation of the RetributionStore interface. This test adds all
// retributions to the store, confirms that they are all present, and then
// removes each one individually.  Between each addition or removal, the number
// of elements in the store is checked to ensure that it only changes by one.
func testRetributionStoreAddRemove(frs FailingRetributionStore, t *testing.T) {
	// Make sure that a new retribution store is actually empty.
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
	// Make sure that a new retribution store is still empty after failing
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
		// Snapshot number of entries before and after the addition.
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
		// Snapshot number of entries before and after the removal.
		nbefore := countRetributions(t, frs)
		err := frs.Remove(&retInfo.chanPoint)
		switch {
		case nbefore == 0 && err == nil:

		case nbefore > 0 && err != nil:
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
	// Construct a set of all channel points presented by the store. Entries
	// are only be added to the set if their corresponding retribution
	// information matches the test vector.
	var foundSet map[wire.OutPoint]struct{}

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
			return fmt.Errorf("unknown retribution retrieved "+
				"from db: %v", ret)
		}

		return nil
	}, func() {
		foundSet = make(map[wire.OutPoint]struct{})
	}); err != nil {
		t.Fatalf("failed to iterate over persistent retributions: %v",
			err)
	}

	// Check that retribution store emits nrets entries
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

func initBreachedState(t *testing.T) (*BreachArbitrator,
	*lnwallet.LightningChannel, *lnwallet.LightningChannel,
	*lnwallet.LocalForceCloseSummary, chan *ContractBreachEvent) {

	// Create a pair of channels using a notifier that allows us to signal
	// a spend of the funding transaction. Alice's channel will be the on
	// observing a breach.
	alice, bob, err := createInitChannels(t)
	require.NoError(t, err, "unable to create test channels")

	// Instantiate a breach arbiter to handle the breach of alice's channel.
	contractBreaches := make(chan *ContractBreachEvent)

	brar, err := createTestArbiter(
		t, contractBreaches, alice.State().Db.GetParentDB(),
	)
	require.NoError(t, err, "unable to initialize test breach arbiter")

	// Send one HTLC to Bob and perform a state transition to lock it in.
	htlcAmount := lnwire.NewMSatFromSatoshis(20000)
	htlc, _ := createHTLC(0, htlcAmount)
	if _, err := alice.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bob.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}
	if err := lnwallet.ForceStateTransition(alice, bob); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// Generate the force close summary at this point in time, this will
	// serve as the old state bob will broadcast.
	bobClose, err := bob.ForceClose()
	require.NoError(t, err, "unable to force close bob's channel")

	// Now send another HTLC and perform a state transition, this ensures
	// Alice is ahead of the state Bob will broadcast.
	htlc2, _ := createHTLC(1, htlcAmount)
	if _, err := alice.AddHTLC(htlc2, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bob.ReceiveHTLC(htlc2); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}
	if err := lnwallet.ForceStateTransition(alice, bob); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	return brar, alice, bob, bobClose, contractBreaches
}

// TestBreachHandoffSuccess tests that a channel's close observer properly
// delivers retribution information to the breach arbiter in response to a
// breach close. This test verifies correctness in the event that the handoff
// experiences no interruptions.
func TestBreachHandoffSuccess(t *testing.T) {
	brar, alice, _, bobClose, contractBreaches := initBreachedState(t)

	chanPoint := alice.ChannelPoint()

	// Signal a spend of the funding transaction and wait for the close
	// observer to exit.
	processACK := make(chan error)
	breach := &ContractBreachEvent{
		ChanPoint: chanPoint,
		ProcessACK: func(brarErr error) {
			processACK <- brarErr
		},
		BreachRetribution: &lnwallet.BreachRetribution{
			BreachTxHash: bobClose.CloseTx.TxHash(),
			LocalOutputSignDesc: &input.SignDescriptor{
				Output: &wire.TxOut{
					PkScript: breachKeys[0],
				},
			},
		},
	}
	contractBreaches <- breach

	// We'll also wait to consume the ACK back from the breach arbiter.
	select {
	case err := <-processACK:
		if err != nil {
			t.Fatalf("handoff failed: %v", err)
		}
	case <-time.After(time.Second * 15):
		t.Fatalf("breach arbiter didn't send ack back")
	}

	// After exiting, the breach arbiter should have persisted the
	// retribution information and the channel should be shown as pending
	// force closed.
	assertArbiterBreach(t, brar, &chanPoint)

	// Send another breach event. Since the handoff for this channel was
	// already ACKed, the breach arbiter should immediately ACK and ignore
	// this event.
	breach = &ContractBreachEvent{
		ChanPoint: chanPoint,
		ProcessACK: func(brarErr error) {
			processACK <- brarErr
		},
		BreachRetribution: &lnwallet.BreachRetribution{
			BreachTxHash: bobClose.CloseTx.TxHash(),
			LocalOutputSignDesc: &input.SignDescriptor{
				Output: &wire.TxOut{
					PkScript: breachKeys[0],
				},
			},
		},
	}

	contractBreaches <- breach

	// We'll also wait to consume the ACK back from the breach arbiter.
	select {
	case err := <-processACK:
		if err != nil {
			t.Fatalf("handoff failed: %v", err)
		}
	case <-time.After(time.Second * 15):
		t.Fatalf("breach arbiter didn't send ack back")
	}

	// State should not have changed.
	assertArbiterBreach(t, brar, &chanPoint)
}

// TestBreachHandoffFail tests that a channel's close observer properly
// delivers retribution information to the breach arbiter in response to a
// breach close. This test verifies correctness in the event that the breach
// arbiter fails to write the information to disk, and that a subsequent attempt
// at the handoff succeeds.
func TestBreachHandoffFail(t *testing.T) {
	brar, alice, _, bobClose, contractBreaches := initBreachedState(t)

	// Before alerting Alice of the breach, instruct our failing retribution
	// store to fail the next database operation, which we expect to write
	// the information handed off by the channel's close observer.
	fstore := brar.cfg.Store.(*failingRetributionStore)
	fstore.FailNextAdd(nil)

	// Signal the notifier to dispatch spend notifications of the funding
	// transaction using the transaction from bob's closing summary.
	chanPoint := alice.ChannelPoint()
	processACK := make(chan error)
	breach := &ContractBreachEvent{
		ChanPoint: chanPoint,
		ProcessACK: func(brarErr error) {
			processACK <- brarErr
		},
		BreachRetribution: &lnwallet.BreachRetribution{
			BreachTxHash: bobClose.CloseTx.TxHash(),
			LocalOutputSignDesc: &input.SignDescriptor{
				Output: &wire.TxOut{
					PkScript: breachKeys[0],
				},
			},
		},
	}
	contractBreaches <- breach

	// We'll also wait to consume the ACK back from the breach arbiter.
	select {
	case err := <-processACK:
		if err == nil {
			t.Fatalf("breach write should have failed")
		}
	case <-time.After(time.Second * 15):
		t.Fatalf("breach arbiter didn't send ack back")
	}

	// Since the handoff failed, the breach arbiter should not show the
	// channel as breached, and the channel should also not have been marked
	// pending closed.
	assertNoArbiterBreach(t, brar, &chanPoint)
	assertNotPendingClosed(t, alice)

	brar, err := createTestArbiter(
		t, contractBreaches, alice.State().Db.GetParentDB(),
	)
	require.NoError(t, err, "unable to initialize test breach arbiter")

	// Signal a spend of the funding transaction and wait for the close
	// observer to exit. This time we are allowing the handoff to succeed.
	breach = &ContractBreachEvent{
		ChanPoint: chanPoint,
		ProcessACK: func(brarErr error) {
			processACK <- brarErr
		},
		BreachRetribution: &lnwallet.BreachRetribution{
			BreachTxHash: bobClose.CloseTx.TxHash(),
			LocalOutputSignDesc: &input.SignDescriptor{
				Output: &wire.TxOut{
					PkScript: breachKeys[0],
				},
			},
		},
	}

	contractBreaches <- breach

	select {
	case err := <-processACK:
		if err != nil {
			t.Fatalf("handoff failed: %v", err)
		}
	case <-time.After(time.Second * 15):
		t.Fatalf("breach arbiter didn't send ack back")
	}

	// Check that the breach was properly recorded in the breach arbiter,
	// and that the close observer marked the channel as pending closed
	// before exiting.
	assertArbiterBreach(t, brar, &chanPoint)
}

// TestBreachCreateJusticeTx tests that we create three different variants of
// the justice tx.
func TestBreachCreateJusticeTx(t *testing.T) {
	brar, _, _, _, _ := initBreachedState(t)

	// In this test we just want to check that the correct inputs are added
	// to the justice tx, not that we create a valid spend, so we just set
	// some params making the script generation succeed.
	aliceKeyPriv, _ := btcec.PrivKeyFromBytes(
		channels.AlicesPrivKey,
	)
	alicePubKey := aliceKeyPriv.PubKey()

	signDesc := &breachedOutputs[0].signDesc
	signDesc.KeyDesc.PubKey = alicePubKey
	signDesc.DoubleTweak = aliceKeyPriv

	// We'll test all the different types of outputs we'll sweep with the
	// justice tx.
	outputTypes := []input.StandardWitnessType{
		input.CommitmentNoDelay,
		input.CommitSpendNoDelayTweakless,
		input.CommitmentToRemoteConfirmed,
		input.CommitmentRevoke,
		input.HtlcAcceptedRevoke,
		input.HtlcOfferedRevoke,
		input.HtlcSecondLevelRevoke,
	}

	rBlob := fn.Some([]byte{0x01})

	breachedOutputs := make([]breachedOutput, len(outputTypes))
	for i, wt := range outputTypes {
		// Create a fake breached output for each type, ensuring they
		// have different outpoints for our logic to accept them.
		//
		// NOTE: although they are fake, we need to make sure the
		// outputs are not empty values, otherwise they will be equal
		// to `EmptyOutPoint` and `MultiPrevOutFetcher` will return an
		// error.
		op := breachedOutputs[0].outpoint
		op.Index = uint32(1000 + i)
		breachedOutputs[i] = makeBreachedOutput(
			&op,
			wt,
			// Second level scripts doesn't matter in this test.
			nil,
			signDesc,
			1,
			rBlob,
		)
	}

	// Create the justice transactions.
	justiceTxs, err := brar.createJusticeTx(breachedOutputs)
	require.NoError(t, err)
	require.NotNil(t, justiceTxs)

	// The spendAll tx should be spending all the outputs. This is the
	// "regular" justice transaction type.
	require.Len(t, justiceTxs.spendAll.justiceTx.TxIn, len(breachedOutputs))

	// The spendCommitOuts tx should be spending the 4 types of commit outs
	// (note that in practice there will be at most two commit outputs per
	// commit, but we test all 4 types here).
	require.Len(t, justiceTxs.spendCommitOuts.justiceTx.TxIn, 4)

	// Check that the spendHTLCs tx is spending the two revoked commitment
	// level HTLC output types.
	require.Len(t, justiceTxs.spendHTLCs.justiceTx.TxIn, 2)

	// Finally, check that the spendSecondLevelHTLCs txs are spending the
	// second level type.
	require.Len(t, justiceTxs.spendSecondLevelHTLCs, 1)
}

type publAssertion func(*testing.T, map[wire.OutPoint]struct{},
	chan *wire.MsgTx, chainhash.Hash) *wire.MsgTx

type breachTest struct {
	name string

	// spend2ndLevel requests that second level htlcs be spent *again*, as
	// if by a remote party or watchtower. The outpoint of the second level
	// htlc is in effect "re-added" to the set of inputs.
	spend2ndLevel bool

	// sweepHtlc tests that the HTLC output is swept using the revocation
	// path in a separate tx.
	sweepHtlc bool

	// sendFinalConf informs the test to send a confirmation for the justice
	// transaction before asserting the arbiter is cleaned up.
	sendFinalConf bool

	// whenNonZeroInputs is called after spending an input but there are
	// further inputs to spend in the test.
	whenNonZeroInputs publAssertion

	// whenZeroInputs is called after spending an input but there are no
	// further inputs to spend in the test.
	whenZeroInputs publAssertion
}

type spendTxs struct {
	commitSpendTx    *wire.MsgTx
	htlc2ndLevlTx    *wire.MsgTx
	htlc2ndLevlSpend *wire.MsgTx
	htlcSweep        *wire.MsgTx
}

func getSpendTransactions(signer input.Signer, chanPoint *wire.OutPoint,
	retribution *lnwallet.BreachRetribution) (*spendTxs, error) {

	localOutpoint := retribution.LocalOutpoint
	remoteOutpoint := retribution.RemoteOutpoint
	htlcOutpoint := retribution.HtlcRetributions[0].OutPoint

	// commitSpendTx is used to spend commitment outputs.
	commitSpendTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: localOutpoint,
			},
			{
				PreviousOutPoint: remoteOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{Value: 500000000},
		},
	}

	// htlc2ndLevlTx is used to transition an htlc output on the commitment
	// transaction to a second level htlc.
	htlc2ndLevlTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{Value: 20000},
		},
	}

	secondLvlOp := wire.OutPoint{
		Hash:  htlc2ndLevlTx.TxHash(),
		Index: 0,
	}

	// htlcSpendTx is used to spend from a second level htlc.
	htlcSpendTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: secondLvlOp,
			},
		},

		TxOut: []*wire.TxOut{
			{Value: 10000},
		},
	}

	// htlcSweep is used to spend the HTLC output directly using the
	// revocation key.
	htlcSweep := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{Value: 21000},
		},
	}

	// In order for  the BreachArbitrator to detect that it is being spent
	// using the revocation key, it will inspect the witness. Therefore
	// sign and add the witness to the HTLC sweep.
	retInfo := newRetributionInfo(chanPoint, retribution)

	hashCache := input.NewTxSigHashesV0Only(htlcSweep)
	for i := range retInfo.breachedOutputs {
		inp := &retInfo.breachedOutputs[i]

		// Find the HTLC output. so we can add the witness.
		switch inp.witnessType {
		case input.HtlcAcceptedRevoke:
			fallthrough
		case input.HtlcOfferedRevoke:
			cannedFetcher := txscript.NewCannedPrevOutputFetcher(
				nil, 0,
			)
			inputScript, err := inp.CraftInputScript(
				signer, htlcSweep, hashCache, cannedFetcher, 0,
			)
			if err != nil {
				return nil, err
			}

			htlcSweep.TxIn[0].Witness = inputScript.Witness
		}
	}

	return &spendTxs{
		commitSpendTx:    commitSpendTx,
		htlc2ndLevlTx:    htlc2ndLevlTx,
		htlc2ndLevlSpend: htlcSpendTx,
		htlcSweep:        htlcSweep,
	}, nil
}

var breachTests = []breachTest{
	{
		name:          "all spends",
		spend2ndLevel: true,
		whenNonZeroInputs: func(t *testing.T,
			inputs map[wire.OutPoint]struct{},
			publTx chan *wire.MsgTx, _ chainhash.Hash) *wire.MsgTx {

			var tx *wire.MsgTx
			select {
			case tx = <-publTx:
			case <-time.After(5 * time.Second):
				t.Fatalf("tx was not published")
			}

			// The justice transaction should have the same number
			// of inputs as we are tracking in the test.
			if len(tx.TxIn) != len(inputs) {
				t.Fatalf("expected justice txn to have %d "+
					"inputs, found %d", len(inputs),
					len(tx.TxIn))
			}

			// Ensure that each input exists on the justice
			// transaction.
			for in := range inputs {
				findInputIndex(t, in, tx)
			}

			return tx
		},
		whenZeroInputs: func(t *testing.T,
			inputs map[wire.OutPoint]struct{},
			publTx chan *wire.MsgTx, _ chainhash.Hash) *wire.MsgTx {

			// Sanity check to ensure the brar doesn't try to
			// broadcast another sweep, since all outputs have been
			// spent externally.
			select {
			case <-publTx:
				t.Fatalf("tx published unexpectedly")
			case <-time.After(50 * time.Millisecond):
			}

			return nil
		},
	},
	{
		name:          "commit spends, second level sweep",
		spend2ndLevel: false,
		sendFinalConf: true,
		whenNonZeroInputs: func(t *testing.T,
			inputs map[wire.OutPoint]struct{},
			publTx chan *wire.MsgTx, _ chainhash.Hash) *wire.MsgTx {

			var tx *wire.MsgTx
			select {
			case tx = <-publTx:
			case <-time.After(5 * time.Second):
				t.Fatalf("tx was not published")
			}

			// The justice transaction should have the same number
			// of inputs as we are tracking in the test.
			if len(tx.TxIn) != len(inputs) {
				t.Fatalf("expected justice txn to have %d "+
					"inputs, found %d", len(inputs),
					len(tx.TxIn))
			}

			// Ensure that each input exists on the justice
			// transaction.
			for in := range inputs {
				findInputIndex(t, in, tx)
			}

			return tx
		},
		whenZeroInputs: func(t *testing.T,
			inputs map[wire.OutPoint]struct{},
			publTx chan *wire.MsgTx,
			htlc2ndLevlTxHash chainhash.Hash) *wire.MsgTx {

			// Now a transaction attempting to spend from the second
			// level tx should be published instead. Let this
			// publish succeed by setting the publishing error to
			// nil.
			var tx *wire.MsgTx
			select {
			case tx = <-publTx:
			case <-time.After(5 * time.Second):
				t.Fatalf("tx was not published")
			}

			// The commitment outputs should be gone, and there
			// should only be a single htlc spend.
			if len(tx.TxIn) != 1 {
				t.Fatalf("expect 1 htlc output, found %d "+
					"outputs", len(tx.TxIn))
			}

			// The remaining TxIn  previously attempting to spend
			// the HTLC outpoint should now be spending from the
			// second level tx.
			//
			// NOTE: Commitment outputs and htlc sweeps are spent
			// with a different transactions (and thus txids),
			// ensuring we aren't mistaking this for a different
			// output type.
			onlyInput := tx.TxIn[0].PreviousOutPoint.Hash
			if onlyInput != htlc2ndLevlTxHash {
				t.Fatalf("tx not attempting to spend second "+
					"level tx, %v", tx.TxIn[0])
			}

			return tx
		},
	},
	{ // nolint: dupl
		// Test that if the HTLC output is swept via the revoke path
		// (by us) in a separate tx, it will be handled correctly.
		name:      "sweep htlc",
		sweepHtlc: true,
		whenNonZeroInputs: func(t *testing.T,
			inputs map[wire.OutPoint]struct{},
			publTx chan *wire.MsgTx, _ chainhash.Hash) *wire.MsgTx {

			var tx *wire.MsgTx
			select {
			case tx = <-publTx:
			case <-time.After(5 * time.Second):
				t.Fatalf("tx was not published")
			}

			// The justice transaction should have the same number
			// of inputs as we are tracking in the test.
			if len(tx.TxIn) != len(inputs) {
				t.Fatalf("expected justice txn to have %d "+
					"inputs, found %d", len(inputs),
					len(tx.TxIn))
			}

			// Ensure that each input exists on the justice
			// transaction.
			for in := range inputs {
				findInputIndex(t, in, tx)
			}

			return tx
		},
		whenZeroInputs: func(t *testing.T,
			inputs map[wire.OutPoint]struct{},
			publTx chan *wire.MsgTx, _ chainhash.Hash) *wire.MsgTx {

			// Sanity check to ensure the brar doesn't try to
			// broadcast another sweep, since all outputs have been
			// spent externally.
			select {
			case <-publTx:
				t.Fatalf("tx published unexpectedly")
			case <-time.After(50 * time.Millisecond):
			}

			return nil
		},
	},
}

// TestBreachSpends checks the behavior of the breach arbiter in response to
// spend events on a channels outputs by asserting that it properly removes or
// modifies the inputs from the justice txn.
func TestBreachSpends(t *testing.T) {
	for _, test := range breachTests {
		tc := test
		t.Run(tc.name, func(t *testing.T) {
			testBreachSpends(t, tc)
		})
	}
}

func testBreachSpends(t *testing.T, test breachTest) {
	brar, alice, _, bobClose, contractBreaches := initBreachedState(t)

	var (
		height       = bobClose.ChanSnapshot.CommitHeight
		forceCloseTx = bobClose.CloseTx
		chanPoint    = alice.ChannelPoint()
		publTx       = make(chan *wire.MsgTx)
		publErr      error
		publMtx      sync.Mutex
	)

	// Make PublishTransaction always return ErrDoubleSpend to begin with.
	publErr = lnwallet.ErrDoubleSpend
	brar.cfg.PublishTransaction = func(tx *wire.MsgTx, _ string) error {
		publMtx.Lock()
		err := publErr
		publMtx.Unlock()

		select {
		case publTx <- tx:
		case <-brar.quit:
			return fmt.Errorf("brar quit")
		}

		return err
	}

	// Notify the breach arbiter about the breach.
	retribution, err := lnwallet.NewBreachRetribution(
		alice.State(), height, 1, forceCloseTx,
		fn.Some[lnwallet.AuxLeafStore](&lnwallet.MockAuxLeafStore{}),
		fn.Some[lnwallet.AuxContractResolver](
			&lnwallet.MockAuxContractResolver{},
		),
	)
	require.NoError(t, err, "unable to create breach retribution")

	processACK := make(chan error)
	breach := &ContractBreachEvent{
		ChanPoint: chanPoint,
		ProcessACK: func(brarErr error) {
			processACK <- brarErr
		},
		BreachRetribution: retribution,
	}
	select {
	case contractBreaches <- breach:
	case <-time.After(15 * time.Second):
		t.Fatalf("breach not delivered")
	}

	// We'll also wait to consume the ACK back from the breach arbiter.
	select {
	case err := <-processACK:
		if err != nil {
			t.Fatalf("handoff failed: %v", err)
		}
	case <-time.After(time.Second * 15):
		t.Fatalf("breach arbiter didn't send ack back")
	}

	state := alice.State()
	err = state.CloseChannel(&channeldb.ChannelCloseSummary{
		ChanPoint:               state.FundingOutpoint,
		ChainHash:               state.ChainHash,
		RemotePub:               state.IdentityPub,
		CloseType:               channeldb.BreachClose,
		Capacity:                state.Capacity,
		IsPending:               true,
		ShortChanID:             state.ShortChanID(),
		RemoteCurrentRevocation: state.RemoteCurrentRevocation,
		RemoteNextRevocation:    state.RemoteNextRevocation,
		LocalChanConfig:         state.LocalChanCfg,
	})
	require.NoError(t, err, "unable to close channel")

	// After exiting, the breach arbiter should have persisted the
	// retribution information and the channel should be shown as pending
	// force closed.
	assertArbiterBreach(t, brar, &chanPoint)

	// Assert that the database sees the channel as pending close, otherwise
	// the breach arbiter won't be able to fully close it.
	assertPendingClosed(t, alice)

	// Notify that the breaching transaction is confirmed, to trigger the
	// retribution logic.
	notifier := brar.cfg.Notifier.(*mock.SpendNotifier)

	select {
	case notifier.ConfChan <- &chainntnfs.TxConfirmation{}:
	case <-time.After(15 * time.Second):
		t.Fatalf("conf not delivered")
	}

	// The breach arbiter should attempt to sweep all outputs on the
	// breached commitment. We'll pretend that the HTLC output has been
	// spent by the channel counter party's second level tx already.
	var tx *wire.MsgTx
	select {
	case tx = <-publTx:
	case <-time.After(5 * time.Second):
		t.Fatalf("tx was not published")
	}

	// All outputs should initially spend from the force closed txn.
	forceTxID := forceCloseTx.TxHash()
	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint.Hash != forceTxID {
			t.Fatalf("og justice tx not spending commitment")
		}
	}

	localOutpoint := retribution.LocalOutpoint
	remoteOutpoint := retribution.RemoteOutpoint
	htlcOutpoint := retribution.HtlcRetributions[0].OutPoint

	spendTxs, err := getSpendTransactions(
		brar.cfg.Signer, &chanPoint, retribution,
	)
	require.NoError(t, err)

	// Construct a map from outpoint on the force close to the transaction
	// we want it to be spent by. As the test progresses, this map will be
	// updated to contain only the set of commitment or second level
	// outpoints that remain to be spent.
	spentBy := map[wire.OutPoint]*wire.MsgTx{
		htlcOutpoint:   spendTxs.htlc2ndLevlTx,
		localOutpoint:  spendTxs.commitSpendTx,
		remoteOutpoint: spendTxs.commitSpendTx,
	}

	// We also keep a map of those remaining outputs we expect the
	// BreachArbitrator to try and sweep.
	inputsToSweep := map[wire.OutPoint]struct{}{
		htlcOutpoint:   {},
		localOutpoint:  {},
		remoteOutpoint: {},
	}

	htlc2ndLevlTx := spendTxs.htlc2ndLevlTx
	htlcSpendTx := spendTxs.htlc2ndLevlSpend

	// If the test is checking sweep of the HTLC directly without the
	// second level, insert the sweep tx instead.
	if test.sweepHtlc {
		spentBy[htlcOutpoint] = spendTxs.htlcSweep
	}

	// Until no more inputs to spend remain, deliver the spend events and
	// process the assertions prescribed by the test case.
	var justiceTx *wire.MsgTx
	for len(spentBy) > 0 {
		var (
			op      wire.OutPoint
			spendTx *wire.MsgTx
		)

		// Pick an outpoint at random from the set of inputs.
		for op, spendTx = range spentBy {
			delete(spentBy, op)
			break
		}

		// Deliver the spend notification for the chosen transaction.
		notifier.Spend(&op, 2, spendTx)

		// Since the remote just swept this input, we expect our next
		// justice transaction to not include them.
		delete(inputsToSweep, op)

		// If this is the second-level spend, we must add the new
		// outpoint to our expected sweeps.
		spendTxID := spendTx.TxHash()
		if spendTxID == htlc2ndLevlTx.TxHash() {
			// Create the second level outpoint that will
			// be spent, the index is always zero for these
			// 1-in-1-out txns.
			spendOp := wire.OutPoint{Hash: spendTxID}
			inputsToSweep[spendOp] = struct{}{}

			// When the second layer transfer is detected, add back
			// the outpoint of the second layer tx so that we can
			// spend it again. Only do so if the test requests this
			// behavior.
			if test.spend2ndLevel {
				spentBy[spendOp] = htlcSpendTx
			}
		}

		if len(spentBy) > 0 {
			justiceTx = test.whenNonZeroInputs(t, inputsToSweep, publTx, htlc2ndLevlTx.TxHash())
		} else {
			// Reset the publishing error so that any publication,
			// made by the breach arbiter, if any, will succeed.
			publMtx.Lock()
			publErr = nil
			publMtx.Unlock()
			justiceTx = test.whenZeroInputs(t, inputsToSweep, publTx, htlc2ndLevlTx.TxHash())
		}
	}

	// Deliver confirmation of sweep if the test expects it. Since we are
	// looking for the final justice tx to confirme, we deliver a spend of
	// all its inputs.
	if test.sendFinalConf {
		for _, txin := range justiceTx.TxIn {
			op := txin.PreviousOutPoint
			notifier.Spend(&op, 3, justiceTx)
		}
	}

	// Assert that the channel is fully resolved.
	assertBrarCleanup(t, brar, &chanPoint, alice.State().Db)
}

// TestBreachDelayedJusticeConfirmation tests that the breach arbiter will
// "split" the justice tx in case the first justice tx doesn't confirm within
// a reasonable time.
func TestBreachDelayedJusticeConfirmation(t *testing.T) {
	brar, alice, _, bobClose, contractBreaches := initBreachedState(t)

	var (
		height       = bobClose.ChanSnapshot.CommitHeight
		blockHeight  = int32(10)
		forceCloseTx = bobClose.CloseTx
		chanPoint    = alice.ChannelPoint()
		publTx       = make(chan *wire.MsgTx)
	)

	// Make PublishTransaction always return succeed.
	brar.cfg.PublishTransaction = func(tx *wire.MsgTx, _ string) error {
		select {
		case publTx <- tx:
		case <-brar.quit:
			return fmt.Errorf("brar quit")
		}
		return nil
	}

	// Notify the breach arbiter about the breach.
	retribution, err := lnwallet.NewBreachRetribution(
		alice.State(), height, uint32(blockHeight), forceCloseTx,
		fn.Some[lnwallet.AuxLeafStore](&lnwallet.MockAuxLeafStore{}),
		fn.Some[lnwallet.AuxContractResolver](
			&lnwallet.MockAuxContractResolver{},
		),
	)
	require.NoError(t, err, "unable to create breach retribution")

	processACK := make(chan error, 1)
	breach := &ContractBreachEvent{
		ChanPoint: chanPoint,
		ProcessACK: func(brarErr error) {
			processACK <- brarErr
		},
		BreachRetribution: retribution,
	}

	select {
	case contractBreaches <- breach:
	case <-time.After(defaultTimeout):
		t.Fatalf("breach not delivered")
	}

	// We'll also wait to consume the ACK back from the breach arbiter.
	select {
	case err := <-processACK:
		if err != nil {
			t.Fatalf("handoff failed: %v", err)
		}
	case <-time.After(defaultTimeout):
		t.Fatalf("breach arbiter didn't send ack back")
	}

	state := alice.State()
	err = state.CloseChannel(&channeldb.ChannelCloseSummary{
		ChanPoint:               state.FundingOutpoint,
		ChainHash:               state.ChainHash,
		RemotePub:               state.IdentityPub,
		CloseType:               channeldb.BreachClose,
		Capacity:                state.Capacity,
		IsPending:               true,
		ShortChanID:             state.ShortChanID(),
		RemoteCurrentRevocation: state.RemoteCurrentRevocation,
		RemoteNextRevocation:    state.RemoteNextRevocation,
		LocalChanConfig:         state.LocalChanCfg,
	})
	require.NoError(t, err, "unable to close channel")

	// After exiting, the breach arbiter should have persisted the
	// retribution information and the channel should be shown as pending
	// force closed.
	assertArbiterBreach(t, brar, &chanPoint)

	// Assert that the database sees the channel as pending close, otherwise
	// the breach arbiter won't be able to fully close it.
	assertPendingClosed(t, alice)

	// Notify that the breaching transaction is confirmed, to trigger the
	// retribution logic.
	notifier := brar.cfg.Notifier.(*mock.SpendNotifier)

	select {
	case notifier.ConfChan <- &chainntnfs.TxConfirmation{}:
	case <-time.After(defaultTimeout):
		t.Fatalf("conf not delivered")
	}

	// The breach arbiter should attempt to sweep all outputs on the
	// breached commitment.
	var justiceTx *wire.MsgTx
	select {
	case justiceTx = <-publTx:
	case <-time.After(defaultTimeout):
		t.Fatalf("tx was not published")
	}

	require.Len(t, justiceTx.TxIn, 3)

	// All outputs should initially spend from the force closed txn.
	forceTxID := forceCloseTx.TxHash()
	for _, txIn := range justiceTx.TxIn {
		if txIn.PreviousOutPoint.Hash != forceTxID {
			t.Fatalf("og justice tx not spending commitment")
		}
	}

	// Now we'll pretend some blocks pass without the justice tx
	// confirming.
	const pollInterval = 500 * time.Millisecond
	for i := int32(0); i <= 3; i++ {
		notifier.EpochChan <- &chainntnfs.BlockEpoch{
			Height: blockHeight + i,
		}

		// On every epoch, check that no new tx is published.
		select {
		case <-publTx:
			t.Fatalf("tx was published")
		case <-time.After(pollInterval):
		}
	}

	// Now mine another block without the justice tx confirming. This
	// should lead to the BreachArbitrator publishing the split justice tx
	// variants.
	notifier.EpochChan <- &chainntnfs.BlockEpoch{
		Height: blockHeight + 4,
	}

	var (
		splits   []*wire.MsgTx
		spending = make(map[wire.OutPoint]struct{})
		maxIndex = uint32(len(forceCloseTx.TxOut)) - 1
	)
	for i := 0; i < 2; i++ {
		var tx *wire.MsgTx
		select {
		case tx = <-publTx:
			splits = append(splits, tx)

		case <-time.After(defaultTimeout):
			t.Fatalf("tx not published")
		}

		// Check that every input is from the breached tx and that
		// there are no duplicates.
		for _, in := range tx.TxIn {
			op := in.PreviousOutPoint
			_, ok := spending[op]
			if ok {
				t.Fatal("already spent")
			}

			if op.Hash != forceTxID || op.Index > maxIndex {
				t.Fatalf("not spending breach")
			}

			spending[op] = struct{}{}
		}
	}

	// All the inputs from the original justice transaction should have
	// been spent by the 2 splits.
	require.Len(t, spending, len(justiceTx.TxIn))
	require.Len(t, splits, 2)

	// Notify that the first split confirm, making the breach arbiter
	// publish another TX with the remaining inputs.
	for _, in := range splits[0].TxIn {
		op := &in.PreviousOutPoint
		notifier.Spend(op, blockHeight+5, splits[0])
	}

	select {
	// The published tx should spend the same inputs as our second split.
	case tx := <-publTx:
		require.Len(t, tx.TxIn, len(splits[1].TxIn))
		for i := range tx.TxIn {
			require.Equal(
				t, tx.TxIn[i].PreviousOutPoint,
				splits[1].TxIn[i].PreviousOutPoint,
			)
		}

	case <-time.After(defaultTimeout):
		t.Fatalf("tx not published")
	}

	// Finally notify that the second split confirms, making the breach
	// arbiter clean up since all inputs have been swept.
	for _, in := range splits[1].TxIn {
		op := &in.PreviousOutPoint
		notifier.Spend(op, blockHeight+6, splits[1])
	}

	// Assert that the channel is fully resolved.
	assertBrarCleanup(t, brar, &chanPoint, alice.State().Db)
}

// findInputIndex returns the index of the input that spends from the given
// outpoint. This method fails if the outpoint is not found.
func findInputIndex(t *testing.T, op wire.OutPoint, tx *wire.MsgTx) int {
	t.Helper()

	inputIdx := -1
	for i, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == op {
			inputIdx = i
		}
	}
	if inputIdx == -1 {
		t.Fatalf("input %v in not found", op)
	}

	return inputIdx
}

// assertArbiterBreach checks that the breach arbiter has persisted the breach
// information for a particular channel.
func assertArbiterBreach(t *testing.T, brar *BreachArbitrator,
	chanPoint *wire.OutPoint) {

	t.Helper()

	isBreached, err := brar.IsBreached(chanPoint)
	if err != nil {
		t.Fatalf("unable to determine if channel is "+
			"breached: %v", err)
	}

	if !isBreached {
		t.Fatalf("channel %v was never marked breached",
			chanPoint)
	}

}

// assertNoArbiterBreach checks that the breach arbiter has not persisted the
// breach information for a particular channel.
func assertNoArbiterBreach(t *testing.T, brar *BreachArbitrator,
	chanPoint *wire.OutPoint) {

	t.Helper()

	isBreached, err := brar.IsBreached(chanPoint)
	if err != nil {
		t.Fatalf("unable to determine if channel is "+
			"breached: %v", err)
	}

	if isBreached {
		t.Fatalf("channel %v was marked breached",
			chanPoint)
	}
}

// assertBrarCleanup blocks until the given channel point has been removed the
// retribution store and the channel is fully closed in the database.
func assertBrarCleanup(t *testing.T, brar *BreachArbitrator,
	chanPoint *wire.OutPoint, db *channeldb.ChannelStateDB) {

	t.Helper()

	err := wait.NoError(func() error {
		isBreached, err := brar.IsBreached(chanPoint)
		if err != nil {
			return err
		}

		if isBreached {
			return fmt.Errorf("channel %v still breached",
				chanPoint)
		}

		closedChans, err := db.FetchClosedChannels(false)
		if err != nil {
			return err
		}

		for _, channel := range closedChans {
			switch {
			// Wrong channel.
			case channel.ChanPoint != *chanPoint:
				continue

			// Right channel, fully closed!
			case !channel.IsPending:
				return nil
			}

			// Still pending.
			return fmt.Errorf("channel %v still pending "+
				"close", chanPoint)
		}

		return fmt.Errorf("channel %v not closed", chanPoint)

	}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

// assertPendingClosed checks that the channel has been marked pending closed in
// the channel database.
func assertPendingClosed(t *testing.T, c *lnwallet.LightningChannel) {
	t.Helper()

	closedChans, err := c.State().Db.FetchClosedChannels(true)
	require.NoError(t, err, "unable to load pending closed channels")

	for _, chanSummary := range closedChans {
		if chanSummary.ChanPoint == c.ChannelPoint() {
			return
		}
	}

	t.Fatalf("channel %v was not marked pending closed", c.ChannelPoint())
}

// assertNotPendingClosed checks that the channel has not been marked pending
// closed in the channel database.
func assertNotPendingClosed(t *testing.T, c *lnwallet.LightningChannel) {
	t.Helper()

	closedChans, err := c.State().Db.FetchClosedChannels(true)
	require.NoError(t, err, "unable to load pending closed channels")

	for _, chanSummary := range closedChans {
		if chanSummary.ChanPoint == c.ChannelPoint() {
			t.Fatalf("channel %v was marked pending closed",
				c.ChannelPoint())
		}
	}
}

// createTestArbiter instantiates a breach arbiter with a failing retribution
// store, so that controlled failures can be tested.
func createTestArbiter(t *testing.T, contractBreaches chan *ContractBreachEvent,
	db *channeldb.DB) (*BreachArbitrator, error) {

	// Create a failing retribution store, that wraps a normal one.
	store := newFailingRetributionStore(func() RetributionStorer {
		return NewRetributionStore(db)
	})

	aliceKeyPriv, _ := btcec.PrivKeyFromBytes(channels.AlicesPrivKey)
	signer := input.NewMockSigner([]*btcec.PrivateKey{aliceKeyPriv}, nil)

	// Assemble our test arbiter.
	notifier := mock.MakeMockSpendNotifier()
	ba := NewBreachArbitrator(&BreachConfig{
		CloseLink: func(_ *wire.OutPoint, _ ChannelCloseType) {},
		DB:        db.ChannelStateDB(),
		Estimator: chainfee.NewStaticEstimator(12500, 0),
		GenSweepScript: func() fn.Result[lnwallet.AddrWithKey] {
			return fn.Ok(lnwallet.AddrWithKey{})
		},
		ContractBreaches: contractBreaches,
		Signer:           signer,
		Notifier:         notifier,
		PublishTransaction: func(_ *wire.MsgTx, _ string) error {
			return nil
		},
		Store: store,
	})

	if err := ba.Start(); err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		require.NoError(t, ba.Stop())
	})

	return ba, nil
}

// createInitChannels creates two initialized test channels funded with 10 BTC,
// with 5 BTC allocated to each side. Within the channel, Alice is the
// initiator.
func createInitChannels(t *testing.T) (
	*lnwallet.LightningChannel, *lnwallet.LightningChannel, error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(
		channels.AlicesPrivKey,
	)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(
		channels.BobsPrivKey,
	)

	channelCapacity, err := btcutil.NewAmount(10)
	if err != nil {
		return nil, nil, err
	}

	channelBal := channelCapacity / 2
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(1300)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	prevOut := &wire.OutPoint{
		Hash:  channels.TestHdSeed,
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	aliceCfg := channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      0,
			MinHTLC:          0,
			MaxAcceptedHtlcs: uint16(rand.Int31()),
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: aliceDustLimit,
			CsvDelay:  uint16(csvTimeoutAlice),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
	}
	bobCfg := channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      0,
			MinHTLC:          0,
			MaxAcceptedHtlcs: uint16(rand.Int31()),
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: bobDustLimit,
			CsvDelay:  uint16(csvTimeoutBob),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
	}

	bobRoot, err := chainhash.NewHash(bobKeyPriv.Serialize())
	if err != nil {
		return nil, nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := chainhash.NewHash(aliceKeyPriv.Serialize())
	if err != nil {
		return nil, nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(
		channelBal, channelBal, &aliceCfg, &bobCfg, aliceCommitPoint,
		bobCommitPoint, *fundingTxIn, channeldb.SingleFunderTweaklessBit,
		false, 0,
	)
	if err != nil {
		return nil, nil, err
	}

	dbAlice := channeldb.OpenForTesting(t, t.TempDir())
	dbBob := channeldb.OpenForTesting(t, t.TempDir())

	estimator := chainfee.NewStaticEstimator(12500, 0)
	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		return nil, nil, err
	}

	commitFee := feePerKw.FeeForWeight(input.CommitWeight)

	// TODO(roasbeef): need to factor in commit fee?
	aliceCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal - commitFee),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitFee:     commitFee,
		CommitTx:      aliceCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal - commitFee),
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitFee:     commitFee,
		CommitTx:      bobCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}

	var chanIDBytes [8]byte
	if _, err := io.ReadFull(crand.Reader, chanIDBytes[:]); err != nil {
		return nil, nil, err
	}

	shortChanID := lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(chanIDBytes[:]),
	)

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeyPub,
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             true,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		Db:                      dbAlice.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              channels.TestFundingTx,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeyPub,
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             false,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         bobCommit,
		RemoteCommitment:        bobCommit,
		Db:                      dbBob.ChannelStateDB(),
		Packager:                channeldb.NewChannelPackager(shortChanID),
	}

	aliceSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{aliceKeyPriv}, nil,
	)
	bobSigner := input.NewMockSigner([]*btcec.PrivateKey{bobKeyPriv}, nil)

	signerMock := lnwallet.NewDefaultAuxSignerMock(t)
	alicePool := lnwallet.NewSigPool(1, aliceSigner)
	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, aliceChannelState, alicePool,
		lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
		lnwallet.WithAuxSigner(signerMock),
	)
	if err != nil {
		return nil, nil, err
	}
	alicePool.Start()
	t.Cleanup(func() {
		require.NoError(t, alicePool.Stop())
	})

	bobPool := lnwallet.NewSigPool(1, bobSigner)
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, bobChannelState, bobPool,
		lnwallet.WithLeafStore(&lnwallet.MockAuxLeafStore{}),
		lnwallet.WithAuxSigner(signerMock),
	)
	if err != nil {
		return nil, nil, err
	}
	bobPool.Start()
	t.Cleanup(func() {
		require.NoError(t, bobPool.Stop())
	})

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	if err := channelAlice.State().SyncPending(addr, 101); err != nil {
		return nil, nil, err
	}

	addr = &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	if err := channelBob.State().SyncPending(addr, 101); err != nil {
		return nil, nil, err
	}

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	err = initRevocationWindows(channelAlice, channelBob)
	if err != nil {
		return nil, nil, err
	}

	return channelAlice, channelBob, nil
}

// initRevocationWindows simulates a new channel being opened within the p2p
// network by populating the initial revocation windows of the passed
// commitment state machines.
//
// TODO(conner) remove code duplication
func initRevocationWindows(chanA, chanB *lnwallet.LightningChannel) error {
	aliceNextRevoke, err := chanA.NextRevocationKey()
	if err != nil {
		return err
	}
	if err := chanB.InitNextRevocation(aliceNextRevoke); err != nil {
		return err
	}

	bobNextRevoke, err := chanB.NextRevocationKey()
	if err != nil {
		return err
	}
	if err := chanA.InitNextRevocation(bobNextRevoke); err != nil {
		return err
	}

	return nil
}

// createHTLC is a utility function for generating an HTLC with a given
// preimage and a given amount.
// TODO(conner) remove code duplication
func createHTLC(data int, amount lnwire.MilliSatoshi) (*lnwire.UpdateAddHTLC, [32]byte) {
	preimage := bytes.Repeat([]byte{byte(data)}, 32)
	paymentHash := sha256.Sum256(preimage)

	var returnPreimage [32]byte
	copy(returnPreimage[:], preimage)

	return &lnwire.UpdateAddHTLC{
		ID:          uint64(data),
		PaymentHash: paymentHash,
		Amount:      amount,
		Expiry:      uint32(5),
	}, returnPreimage
}
