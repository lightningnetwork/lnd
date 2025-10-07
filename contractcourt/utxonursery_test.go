package contractcourt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

var (
	outPoints = []wire.OutPoint{
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
				0x0d, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
				0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
			},
			Index: 23,
		},
		{
			Hash: [chainhash.HashSize]byte{
				0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
				0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
				0x0d, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
				0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
			},
			Index: 30,
		},
		{
			Hash: [chainhash.HashSize]byte{
				0x0d, 0xe7, 0x95, 0xe4, 0xfc, 0xd2, 0xc6, 0xda,
				0xb7, 0x25, 0xb8, 0x4d, 0x63, 0x59, 0xe6, 0x96,
				0x31, 0x13, 0xa1, 0x17, 0x81, 0xb6, 0x37, 0xd8,
				0x1e, 0x0b, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
			},
			Index: 2,
		},
		{
			Hash: [chainhash.HashSize]byte{
				0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
				0x51, 0xb6, 0x37, 0xd8, 0x1f, 0x0b, 0x4c, 0xf9,
				0x9e, 0xc5, 0x8c, 0xe9, 0xfc, 0xd2, 0xc6, 0xda,
				0x2d, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
			},
			Index: 9,
		},
	}

	keys = [][]byte{
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

	signDescriptors = []input.SignDescriptor{
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

	kidOutputs = []kidOutput{
		{
			breachedOutput: breachedOutput{
				amt:         btcutil.Amount(13e7),
				outpoint:    outPoints[1],
				witnessType: input.CommitmentTimeLock,
				confHeight:  uint32(1000),
			},
			originChanPoint:  outPoints[0],
			blocksToMaturity: uint32(42),
		},

		{
			breachedOutput: breachedOutput{
				amt:         btcutil.Amount(24e7),
				outpoint:    outPoints[2],
				witnessType: input.CommitmentTimeLock,
				confHeight:  uint32(1000),
			},
			originChanPoint:  outPoints[0],
			blocksToMaturity: uint32(42),
		},

		{
			breachedOutput: breachedOutput{
				amt:         btcutil.Amount(2e5),
				outpoint:    outPoints[3],
				witnessType: input.CommitmentTimeLock,
				confHeight:  uint32(500),
			},
			originChanPoint:  outPoints[0],
			blocksToMaturity: uint32(28),
		},

		{
			breachedOutput: breachedOutput{
				amt:         btcutil.Amount(10e6),
				outpoint:    outPoints[4],
				witnessType: input.CommitmentTimeLock,
				confHeight:  uint32(500),
			},
			originChanPoint:  outPoints[0],
			blocksToMaturity: uint32(28),
		},
	}

	babyOutputs = []babyOutput{
		{
			kidOutput: kidOutputs[1],
			expiry:    3829,
			timeoutTx: timeoutTx,
		},
		{
			kidOutput: kidOutputs[2],
			expiry:    4,
			timeoutTx: timeoutTx,
		},
		{
			kidOutput: kidOutputs[3],
			expiry:    4,
			timeoutTx: timeoutTx,
		},
	}

	// Dummy timeout tx used to test serialization, borrowed from btcd
	// msgtx_test
	timeoutTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash: chainhash.Hash{
						0xa5, 0x33, 0x52, 0xd5, 0x13, 0x57, 0x66, 0xf0,
						0x30, 0x76, 0x59, 0x74, 0x18, 0x26, 0x3d, 0xa2,
						0xd9, 0xc9, 0x58, 0x31, 0x59, 0x68, 0xfe, 0xa8,
						0x23, 0x52, 0x94, 0x67, 0x48, 0x1f, 0xf9, 0xcd,
					},
					Index: 19,
				},
				SignatureScript: []byte{},
				Witness: [][]byte{
					{ // 70-byte signature
						0x30, 0x43, 0x02, 0x1f, 0x4d, 0x23, 0x81, 0xdc,
						0x97, 0xf1, 0x82, 0xab, 0xd8, 0x18, 0x5f, 0x51,
						0x75, 0x30, 0x18, 0x52, 0x32, 0x12, 0xf5, 0xdd,
						0xc0, 0x7c, 0xc4, 0xe6, 0x3a, 0x8d, 0xc0, 0x36,
						0x58, 0xda, 0x19, 0x02, 0x20, 0x60, 0x8b, 0x5c,
						0x4d, 0x92, 0xb8, 0x6b, 0x6d, 0xe7, 0xd7, 0x8e,
						0xf2, 0x3a, 0x2f, 0xa7, 0x35, 0xbc, 0xb5, 0x9b,
						0x91, 0x4a, 0x48, 0xb0, 0xe1, 0x87, 0xc5, 0xe7,
						0x56, 0x9a, 0x18, 0x19, 0x70, 0x01,
					},
					{ // 33-byte serialize pub key
						0x03, 0x07, 0xea, 0xd0, 0x84, 0x80, 0x7e, 0xb7,
						0x63, 0x46, 0xdf, 0x69, 0x77, 0x00, 0x0c, 0x89,
						0x39, 0x2f, 0x45, 0xc7, 0x64, 0x25, 0xb2, 0x61,
						0x81, 0xf5, 0x21, 0xd7, 0xf3, 0x70, 0x06, 0x6a,
						0x8f,
					},
				},
				Sequence: 0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 395019,
				PkScript: []byte{ // p2wkh output
					0x00, // Version 0 witness program
					0x14, // OP_DATA_20
					0x9d, 0xda, 0xc6, 0xf3, 0x9d, 0x51, 0xe0, 0x39,
					0x8e, 0x53, 0x2a, 0x22, 0xc4, 0x1b, 0xa1, 0x89,
					0x40, 0x6a, 0x85, 0x23, // 20-byte pub key hash
				},
			},
		},
	}

	testChanPoint      = wire.OutPoint{}
	defaultTestTimeout = 5 * time.Second
)

func init() {
	// Finish initializing our test vectors by parsing the desired public keys and
	// properly populating the sign descriptors of all baby and kid outputs.
	for i := range signDescriptors {
		pk, err := btcec.ParsePubKey(keys[i])
		if err != nil {
			panic(fmt.Sprintf("unable to parse pub key during init: %v", err))
		}
		signDescriptors[i].KeyDesc.PubKey = pk

	}
	for i := range kidOutputs {
		isd := i % len(signDescriptors)
		kidOutputs[i].signDesc = signDescriptors[isd]
	}

	for i := range babyOutputs {
		isd := i % len(signDescriptors)
		babyOutputs[i].kidOutput.signDesc = signDescriptors[isd]
	}

	initIncubateTests()
}

func TestKidOutputSerialization(t *testing.T) {
	t.Parallel()

	for i, kid := range kidOutputs {
		var b bytes.Buffer
		if err := kid.Encode(&b); err != nil {
			t.Fatalf("Encode #%d: unable to serialize "+
				"kid output: %v", i, err)
		}

		var deserializedKid kidOutput
		if err := deserializedKid.Decode(&b); err != nil {
			t.Fatalf("Decode #%d: unable to deserialize "+
				"kid output: %v", i, err)
		}

		if !reflect.DeepEqual(kid, deserializedKid) {
			t.Fatalf("DeepEqual #%d: unexpected kidOutput, "+
				"want %+v, got %+v",
				i, kid, deserializedKid)
		}
	}
}

func TestBabyOutputSerialization(t *testing.T) {
	t.Parallel()

	for i, baby := range babyOutputs {
		var b bytes.Buffer
		if err := baby.Encode(&b); err != nil {
			t.Fatalf("Encode #%d: unable to serialize "+
				"baby output: %v", i, err)
		}

		var deserializedBaby babyOutput
		if err := deserializedBaby.Decode(&b); err != nil {
			t.Fatalf("Decode #%d: unable to deserialize "+
				"baby output: %v", i, err)
		}

		if !reflect.DeepEqual(baby, deserializedBaby) {
			t.Fatalf("DeepEqual #%d: unexpected babyOutput, "+
				"want %+v, got %+v",
				i, baby, deserializedBaby)
		}

	}
}

type nurseryTestContext struct {
	nursery     *UtxoNursery
	notifier    *sweep.MockNotifier
	chainIO     *mock.ChainIO
	publishChan chan wire.MsgTx
	store       *nurseryStoreInterceptor
	restart     func() bool
	receiveTx   func() wire.MsgTx
	sweeper     *mockSweeperFull
	timeoutChan chan chan time.Time
	t           *testing.T
}

func createNurseryTestContext(t *testing.T,
	checkStartStop func(func()) bool) *nurseryTestContext {

	// Create a temporary database and connect nurseryStore to it. The
	// alternative, mocking nurseryStore, is not chosen because there is
	// still considerable logic in the store.

	cdb, err := channeldb.MakeTestDB(t)
	require.NoError(t, err, "unable to open channeldb")

	store, err := NewNurseryStore(&chainhash.Hash{}, cdb)
	if err != nil {
		t.Fatal(err)
	}

	// Wrap the store in an inceptor to be able to wait for events in this
	// test.
	storeIntercepter := newNurseryStoreInterceptor(store)

	notifier := sweep.NewMockNotifier(t)

	publishChan := make(chan wire.MsgTx, 1)
	publishFunc := func(tx *wire.MsgTx, source string) error {
		log.Tracef("Publishing tx %v by %v", tx.TxHash(), source)
		publishChan <- *tx
		return nil
	}

	timeoutChan := make(chan chan time.Time)

	chainIO := &mock.ChainIO{
		BestHeight: 0,
	}

	sweeper := newMockSweeperFull(t)

	nurseryCfg := NurseryConfig{
		Notifier: notifier,
		FetchClosedChannels: func(pendingOnly bool) (
			[]*channeldb.ChannelCloseSummary, error) {
			return []*channeldb.ChannelCloseSummary{}, nil
		},
		FetchClosedChannel: func(chanID *wire.OutPoint) (
			*channeldb.ChannelCloseSummary, error) {
			return &channeldb.ChannelCloseSummary{
				CloseHeight: 0,
			}, nil
		},
		Store:      storeIntercepter,
		ChainIO:    chainIO,
		SweepInput: sweeper.sweepInput,
		PublishTransaction: func(tx *wire.MsgTx, _ string) error {
			return publishFunc(tx, "nursery")
		},
		Budget: DefaultBudgetConfig(),
	}

	nursery := NewUtxoNursery(&nurseryCfg)
	nursery.Start()

	ctx := &nurseryTestContext{
		nursery:     nursery,
		notifier:    notifier,
		chainIO:     chainIO,
		store:       storeIntercepter,
		publishChan: publishChan,
		sweeper:     sweeper,
		timeoutChan: timeoutChan,
		t:           t,
	}

	ctx.receiveTx = func() wire.MsgTx {
		var tx wire.MsgTx
		select {
		case tx = <-ctx.publishChan:
			log.Debugf("Published tx %v", tx.TxHash())
			return tx
		case <-time.After(defaultTestTimeout):
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

			t.Fatalf("tx not published")
		}
		return tx
	}

	ctx.restart = func() bool {
		return checkStartStop(func() {
			log.Tracef("Restart sweeper and nursery")
			// Simulate lnd restart.
			ctx.nursery.Stop()

			// Restart sweeper.
			ctx.sweeper = newMockSweeperFull(t)

			/// Restart nursery.
			nurseryCfg.SweepInput = ctx.sweeper.sweepInput
			ctx.nursery = NewUtxoNursery(&nurseryCfg)
			require.NoError(t, ctx.nursery.Start())
		})
	}

	// Start with testing an immediate restart.
	ctx.restart()

	return ctx
}

func (ctx *nurseryTestContext) notifyEpoch(height int32) {
	ctx.t.Helper()

	ctx.chainIO.BestHeight = height
	ctx.notifier.NotifyEpoch(height)
}

func (ctx *nurseryTestContext) finish() {
	// Add a final restart point in this state
	ctx.restart()

	// We assume that when finish is called, nursery has finished all its
	// goroutines. This implies that the waitgroup is empty.
	signalChan := make(chan struct{})
	go func() {
		ctx.nursery.wg.Wait()
		close(signalChan)
	}()

	// The only goroutine that is still expected to be running is
	// incubator(). Simulate exit of this goroutine.
	ctx.nursery.wg.Done()

	// We now expect the Wait to succeed.
	select {
	case <-signalChan:
	case <-time.After(time.Second):
		ctx.t.Fatalf("lingering goroutines detected after test " +
			"is finished")
	}

	// Restore waitgroup state to what it was before.
	ctx.nursery.wg.Add(1)

	ctx.nursery.Stop()

	// We should have consumed and asserted all published transactions in
	// our unit tests.
	select {
	case <-ctx.publishChan:
		ctx.t.Fatalf("unexpected transactions published")
	default:
	}

	// Assert that the database is empty. All channels removed and height
	// index cleared.
	nurseryChannels, err := ctx.nursery.cfg.Store.ListChannels()
	if err != nil {
		ctx.t.Fatal(err)
	}
	if len(nurseryChannels) > 0 {
		ctx.t.Fatalf("Expected all channels to be removed from store")
	}

	activeHeights, err := ctx.nursery.cfg.Store.HeightsBelowOrEqual(
		math.MaxUint32)
	if err != nil {
		ctx.t.Fatal(err)
	}
	if len(activeHeights) > 0 {
		ctx.t.Fatalf("Expected height index to be empty")
	}
}

func createOutgoingRes(onLocalCommitment bool) *lnwallet.OutgoingHtlcResolution {
	// Set up an outgoing htlc resolution to hand off to nursery.
	closeTx := &wire.MsgTx{}

	htlcOp := wire.OutPoint{
		Hash:  closeTx.TxHash(),
		Index: 0,
	}

	outgoingRes := lnwallet.OutgoingHtlcResolution{
		Expiry: 125,
		SweepSignDesc: input.SignDescriptor{
			Output: &wire.TxOut{
				Value: 10000,
			},
		},
	}

	if onLocalCommitment {
		timeoutTx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{
					PreviousOutPoint: htlcOp,
					Witness:          [][]byte{{}},
				},
			},
			TxOut: []*wire.TxOut{
				{},
			},
		}

		outgoingRes.SignedTimeoutTx = timeoutTx
		outgoingRes.CsvDelay = 2
	} else {
		outgoingRes.ClaimOutpoint = htlcOp
		outgoingRes.CsvDelay = 0
	}

	return &outgoingRes
}

func incubateTestOutput(t *testing.T, nursery *UtxoNursery,
	onLocalCommitment bool) *lnwallet.OutgoingHtlcResolution {

	outgoingRes := createOutgoingRes(onLocalCommitment)

	// Hand off to nursery.
	err := nursery.IncubateOutputs(
		testChanPoint, fn.Some(*outgoingRes),
		fn.None[lnwallet.IncomingHtlcResolution](), 0, fn.None[int32](),
	)
	if err != nil {
		t.Fatal(err)
	}

	// IncubateOutputs is executing synchronously and we expect the output
	// to immediately show up in the report.
	expectedStage := uint32(2)
	if onLocalCommitment {
		expectedStage = 1
	}
	assertNurseryReport(t, nursery, 1, expectedStage, 10000)

	return outgoingRes
}

// TestRejectedCribTransaction makes sure that our nursery does not fail to
// start up in case a Crib transaction (htlc-timeout) is rejected by the
// bitcoin backend for some excepted reasons.
func TestRejectedCribTransaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// The specific error during broadcasting the transaction.
		broadcastErr error

		// expectErr specifies whether the rejection of the transaction
		// fails the nursery engine.
		expectErr bool
	}{
		{
			name: "Crib tx is rejected because of low mempool " +
				"fees",
			broadcastErr: lnwallet.ErrMempoolFee,
		},
		{
			// We map a rejected rbf transaction to ErrDoubleSpend
			// in lnd.
			name: "Crib tx is rejected because of a " +
				"rbf transaction not succeeding",
			broadcastErr: lnwallet.ErrDoubleSpend,
		},
		{
			name: "Crib tx is rejected with an " +
				"unmatched error",
			broadcastErr: fmt.Errorf("Reject Commitment Tx"),
			expectErr:    true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// The checkStartStop function just calls the callback
			// here to make sure the restart routine works
			// correctly.
			ctx := createNurseryTestContext(t,
				func(callback func()) bool {
					callback()
					return true
				})

			outgoingRes := createOutgoingRes(true)

			ctx.nursery.cfg.PublishTransaction =
				func(tx *wire.MsgTx, source string) error {
					log.Tracef("Publishing tx %v "+
						"by %v", tx.TxHash(), source)
					return test.broadcastErr
				}
			ctx.notifyEpoch(125)

			// Hand off to nursery.
			err := ctx.nursery.IncubateOutputs(
				testChanPoint, fn.Some(*outgoingRes),
				fn.None[lnwallet.IncomingHtlcResolution](), 0,
				fn.None[int32](),
			)
			if test.expectErr {
				require.ErrorIs(t, err, test.broadcastErr)
				return
			}
			require.NoError(t, err)

			// Make sure that a restart is not affected by the
			// rejected Crib transaction.
			ctx.restart()

			// Confirm the timeout tx. This should promote the
			// HTLC to KNDR state.
			timeoutTxHash := outgoingRes.SignedTimeoutTx.TxHash()
			err = ctx.notifier.ConfirmTx(&timeoutTxHash, 126)
			require.NoError(t, err)

			// Wait for output to be promoted in store to KNDR.
			select {
			case <-ctx.store.cribToKinderChan:
			case <-time.After(defaultTestTimeout):
				t.Fatalf("output not promoted to KNDR")
			}

			// Notify arrival of block where second level HTLC
			// unlocks.
			ctx.notifyEpoch(128)

			// Check final sweep into wallet.
			testSweepHtlc(t, ctx)

			// Cleanup utxonursery.
			ctx.finish()
		})
	}
}

func assertNurseryReport(t *testing.T, nursery *UtxoNursery,
	expectedNofHtlcs int, expectedStage uint32,
	expectedLimboBalance btcutil.Amount) {
	report, err := nursery.NurseryReport(&testChanPoint)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Htlcs) != expectedNofHtlcs {
		t.Fatalf("expected %v outputs to be reported, but report "+
			"contains %s", expectedNofHtlcs,
			spew.Sdump(report.Htlcs))
	}

	if expectedNofHtlcs != 0 {
		htlcReport := report.Htlcs[0]
		if htlcReport.Stage != expectedStage {
			t.Fatalf("expected htlc be advanced to stage %v, but "+
				"it is reported in stage %v",
				expectedStage, htlcReport.Stage)
		}
	}

	if report.LimboBalance != expectedLimboBalance {
		t.Fatalf("expected limbo balance to be %v, but it is %v instead",
			expectedLimboBalance, report.LimboBalance)
	}
}

func assertNurseryReportUnavailable(t *testing.T, nursery *UtxoNursery) {
	_, err := nursery.NurseryReport(&testChanPoint)
	if err != ErrContractNotFound {
		t.Fatal("expected report to be unavailable")
	}
}

// testRestartLoop runs the specified test multiple times and in every run it
// will attempt to execute a restart action in a different location. This is to
// assert that the unit under test is recovering correctly from restarts.
func testRestartLoop(t *testing.T, test func(*testing.T,
	func(func()) bool)) {

	// Start with running the test without any restarts (index zero)
	restartIdx := 0

	for {
		currentStartStopIdx := 0

		// checkStartStop is called at every point in the test where a
		// restart should be exercised. When this function is called as
		// many times as the current value of currentStartStopIdx, it
		// will execute startStopFunc.
		checkStartStop := func(startStopFunc func()) bool {
			currentStartStopIdx++
			if restartIdx == currentStartStopIdx {
				startStopFunc()

				return true
			}
			log.Debugf("Skipping restart point %v",
				currentStartStopIdx)
			return false
		}

		var subTestName string
		if restartIdx == 0 {
			subTestName = "no_restart"
		} else {
			subTestName = fmt.Sprintf("restart_%v", restartIdx)
		}
		t.Run(subTestName,
			func(t *testing.T) {
				test(t, checkStartStop)
			})

		// Exit the loop when all restart points have been tested.
		if currentStartStopIdx == restartIdx {
			return
		}
		restartIdx++
	}
}

func TestNurseryOutgoingHtlcSuccessOnLocal(t *testing.T) {
	testRestartLoop(t, testNurseryOutgoingHtlcSuccessOnLocal)
}

func testNurseryOutgoingHtlcSuccessOnLocal(t *testing.T,
	checkStartStop func(func()) bool) {

	ctx := createNurseryTestContext(t, checkStartStop)

	outgoingRes := incubateTestOutput(t, ctx.nursery, true)

	ctx.restart()

	// Notify arrival of block where HTLC CLTV expires.
	ctx.notifyEpoch(125)

	// This should trigger nursery to publish the timeout tx.
	ctx.receiveTx()

	if ctx.restart() {
		// Restart should retrigger broadcast of timeout tx.
		ctx.receiveTx()
	}

	// Confirm the timeout tx. This should promote the HTLC to KNDR state.
	timeoutTxHash := outgoingRes.SignedTimeoutTx.TxHash()
	if err := ctx.notifier.ConfirmTx(&timeoutTxHash, 126); err != nil {
		t.Fatal(err)
	}

	// Wait for output to be promoted in store to KNDR.
	select {
	case <-ctx.store.cribToKinderChan:
	case <-time.After(defaultTestTimeout):
		t.Fatalf("output not promoted to KNDR")
	}

	ctx.restart()

	// Notify arrival of block where second level HTLC unlocks.
	ctx.notifyEpoch(128)

	// Check final sweep into wallet.
	testSweepHtlc(t, ctx)

	ctx.finish()
}

func TestNurseryOutgoingHtlcSuccessOnRemote(t *testing.T) {
	testRestartLoop(t, testNurseryOutgoingHtlcSuccessOnRemote)
}

func testNurseryOutgoingHtlcSuccessOnRemote(t *testing.T,
	checkStartStop func(func()) bool) {

	ctx := createNurseryTestContext(t, checkStartStop)

	outgoingRes := incubateTestOutput(t, ctx.nursery, false)

	ctx.restart()

	// Notify confirmation of the commitment tx. Is only listened to when
	// resolving remote commitment tx.
	//
	// TODO(joostjager): This is probably not correct?
	err := ctx.notifier.ConfirmTx(&outgoingRes.ClaimOutpoint.Hash, 124)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for output to be promoted from PSCL to KNDR.
	select {
	case <-ctx.store.preschoolToKinderChan:
	case <-time.After(defaultTestTimeout):
		t.Fatalf("output not promoted to KNDR")
	}

	ctx.restart()

	// Notify arrival of block where HTLC CLTV expires.
	ctx.notifyEpoch(125)

	// Check final sweep into wallet.
	testSweepHtlc(t, ctx)

	ctx.finish()
}

func testSweepHtlc(t *testing.T, ctx *nurseryTestContext) {
	testSweep(t, ctx, func() {
		// Verify stage in nursery report. HTLCs should now both still
		// be in stage two.
		assertNurseryReport(t, ctx.nursery, 1, 2, 10000)
	})
}

func testSweep(t *testing.T, ctx *nurseryTestContext,
	afterPublishAssert func()) {

	// Wait for nursery to publish the sweep tx.
	ctx.sweeper.expectSweep()

	if ctx.restart() {
		// Nursery reoffers its input after a restart.
		ctx.sweeper.expectSweep()
	}

	afterPublishAssert()

	// Confirm the sweep tx.
	ctx.sweeper.sweepAll()

	// Wait for output to be promoted in store to GRAD.
	select {
	case <-ctx.store.graduateKinderChan:
	case <-time.After(defaultTestTimeout):
		pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		t.Fatalf("output not graduated")
	}

	ctx.restart()

	// As there only was one output to graduate, we expect the channel to be
	// closed and no report available anymore.
	assertNurseryReportUnavailable(t, ctx.nursery)
}

type nurseryStoreInterceptor struct {
	ns NurseryStorer

	// TODO(joostjager): put more useful info through these channels.
	cribToKinderChan      chan struct{}
	cribToRemoteSpendChan chan struct{}
	graduateKinderChan    chan struct{}
	preschoolToKinderChan chan struct{}
}

func newNurseryStoreInterceptor(ns NurseryStorer) *nurseryStoreInterceptor {
	return &nurseryStoreInterceptor{
		ns:                    ns,
		cribToKinderChan:      make(chan struct{}),
		cribToRemoteSpendChan: make(chan struct{}),
		graduateKinderChan:    make(chan struct{}),
		preschoolToKinderChan: make(chan struct{}),
	}
}

func (i *nurseryStoreInterceptor) Incubate(kidOutputs []kidOutput,
	babyOutputs []babyOutput) error {

	return i.ns.Incubate(kidOutputs, babyOutputs)
}

func (i *nurseryStoreInterceptor) CribToKinder(babyOutput *babyOutput) error {
	err := i.ns.CribToKinder(babyOutput)

	i.cribToKinderChan <- struct{}{}

	return err
}

func (i *nurseryStoreInterceptor) PreschoolToKinder(kidOutput *kidOutput,
	lastGradHeight uint32) error {

	err := i.ns.PreschoolToKinder(kidOutput, lastGradHeight)

	i.preschoolToKinderChan <- struct{}{}

	return err
}

func (i *nurseryStoreInterceptor) GraduateKinder(height uint32, kid *kidOutput) error {
	err := i.ns.GraduateKinder(height, kid)

	i.graduateKinderChan <- struct{}{}

	return err
}

func (i *nurseryStoreInterceptor) FetchPreschools() ([]kidOutput, error) {
	return i.ns.FetchPreschools()
}

func (i *nurseryStoreInterceptor) FetchClass(height uint32) (
	[]kidOutput, []babyOutput, error) {

	return i.ns.FetchClass(height)
}

func (i *nurseryStoreInterceptor) HeightsBelowOrEqual(height uint32) (
	[]uint32, error) {

	return i.ns.HeightsBelowOrEqual(height)
}

func (i *nurseryStoreInterceptor) ForChanOutputs(chanPoint *wire.OutPoint,
	callback func([]byte, []byte) error, reset func()) error {

	return i.ns.ForChanOutputs(chanPoint, callback, reset)
}

func (i *nurseryStoreInterceptor) ListChannels() ([]wire.OutPoint, error) {
	return i.ns.ListChannels()
}

func (i *nurseryStoreInterceptor) IsMatureChannel(chanPoint *wire.OutPoint) (
	bool, error) {

	return i.ns.IsMatureChannel(chanPoint)
}

func (i *nurseryStoreInterceptor) RemoveChannel(chanPoint *wire.OutPoint) error {
	return i.ns.RemoveChannel(chanPoint)
}

type mockSweeperFull struct {
	lock sync.Mutex

	resultChans map[wire.OutPoint]chan sweep.Result
	t           *testing.T

	sweepChan chan input.Input
}

func newMockSweeperFull(t *testing.T) *mockSweeperFull {
	return &mockSweeperFull{
		resultChans: make(map[wire.OutPoint]chan sweep.Result),
		sweepChan:   make(chan input.Input, 1),
		t:           t,
	}
}

func (s *mockSweeperFull) sweepInput(input input.Input,
	_ sweep.Params) (chan sweep.Result, error) {

	log.Debugf("mockSweeper sweepInput called for %v", input.OutPoint())

	select {
	case s.sweepChan <- input:
	case <-time.After(defaultTestTimeout):
		s.t.Fatal("signal result timeout")
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	c := make(chan sweep.Result, 1)
	s.resultChans[input.OutPoint()] = c

	return c, nil
}

func (s *mockSweeperFull) expectSweep() {
	s.t.Helper()

	select {
	case <-s.sweepChan:
	case <-time.After(defaultTestTimeout):
		s.t.Fatal("signal result timeout")
	}
}

func (s *mockSweeperFull) sweepAll() {
	s.t.Helper()

	s.lock.Lock()
	currentChans := s.resultChans
	s.resultChans = make(map[wire.OutPoint]chan sweep.Result)
	s.lock.Unlock()

	for o, c := range currentChans {
		log.Debugf("mockSweeper signal swept for %v", o)

		select {
		case c <- sweep.Result{}:
		case <-time.After(defaultTestTimeout):
			s.t.Fatal("signal result timeout")
		}
	}
}

// writeOutpointVarBytes writes an outpoint using the variable-length encoding.
func writeOutpointVarBytes(w io.Writer, o *wire.OutPoint) error {
	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	var scratch [4]byte
	byteOrder.PutUint32(scratch[:], o.Index)
	_, err := w.Write(scratch[:])

	return err
}

// encodeKidOutputLegacy encodes a kidOutput using the legacy format.
func encodeKidOutputLegacy(w io.Writer, k *kidOutput) error {
	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(k.Amount()))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	op := k.OutPoint()
	if err := writeOutpointVarBytes(w, &op); err != nil {
		return err
	}
	if err := writeOutpointVarBytes(w, k.OriginChanPoint()); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, k.isHtlc); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.BlocksToMaturity())
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.absoluteMaturity)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.ConfHeight())
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint16(scratch[:2], uint16(k.witnessType))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	if err := input.WriteSignDescriptor(w, k.SignDesc()); err != nil {
		return err
	}

	if k.SignDesc().ControlBlock == nil {
		return nil
	}

	return wire.WriteVarBytes(w, 1000, k.SignDesc().ControlBlock)
}

// TestKidOutputDecode tests that we can decode a kidOutput from both the
// new and legacy formats. It also checks that the decoded output matches the
// original output, except for the deadlineHeight field, which is not encoded
// in the legacy format.
func TestKidOutputDecode(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 1,
	}
	originOp := wire.OutPoint{
		Hash:  chainhash.Hash{2},
		Index: 2,
	}
	pkScript := []byte{
		0x00, 0x14, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12,
		0x13, 0x14,
	}
	signDesc := &input.SignDescriptor{
		Output: &wire.TxOut{
			Value:    12345,
			PkScript: pkScript,
		},
		HashType:      txscript.SigHashAll,
		WitnessScript: []byte{},
	}

	// Since makeKidOutput is not exported, we construct the kid output
	// manually.
	kid := kidOutput{
		breachedOutput: breachedOutput{
			amt:         btcutil.Amount(signDesc.Output.Value),
			outpoint:    op,
			witnessType: input.CommitmentRevoke,
			signDesc:    *signDesc,
			confHeight:  100,
		},
		originChanPoint:  originOp,
		blocksToMaturity: 144,
		isHtlc:           false,
		absoluteMaturity: 0,
	}

	// Encode the kid output in both formats.
	var newBuf bytes.Buffer
	err := kid.Encode(&newBuf)
	require.NoError(t, err)

	var legacyBuf bytes.Buffer
	err = encodeKidOutputLegacy(&legacyBuf, &kid)
	require.NoError(t, err)

	testCases := []struct {
		name string
		data []byte
	}{
		{
			name: "new format",
			data: newBuf.Bytes(),
		},
		{
			name: "legacy format",
			data: legacyBuf.Bytes(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var decodedKid kidOutput
			err := decodedKid.Decode(bytes.NewReader(tc.data))
			require.NoError(t, err)

			// The deadlineHeight field is not encoded, so we need
			// to set it manually for the comparison.
			kid.deadlineHeight = decodedKid.deadlineHeight

			require.Equal(t, kid, decodedKid)
		})
	}
}

// TestPatchZeroHeightHint tests the patchZeroHeightHint function to ensure
// it correctly handles both normal cases and the edge case where classHeight
// is zero due to a historical bug.
func TestPatchZeroHeightHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		classHeight    uint32
		closeHeight    uint32
		confDepth      uint32
		shortChanID    lnwire.ShortChannelID
		fetchError     error
		expectedHeight uint32
		expectError    bool
		errorContains  string
	}{
		{
			name:           "normal case - non-zero class height",
			classHeight:    100,
			closeHeight:    200,
			confDepth:      6,
			shortChanID:    lnwire.ShortChannelID{BlockHeight: 50},
			expectedHeight: 100,
			expectError:    false,
		},
		{
			name: "zero class height - fetch closed " +
				"channel error",
			classHeight:   0,
			closeHeight:   100,
			confDepth:     6,
			shortChanID:   lnwire.ShortChannelID{BlockHeight: 50},
			fetchError:    fmt.Errorf("channel not found"),
			expectError:   true,
			errorContains: "cannot fetch close summary",
		},
		{
			name: "zero class height - both close " +
				"height and short chan ID = 0",
			classHeight:    0,
			closeHeight:    0,
			confDepth:      6,
			shortChanID:    lnwire.ShortChannelID{BlockHeight: 0},
			expectedHeight: 0,
			expectError:    true,
			errorContains: "cannot use fallback height hint: " +
				"close height is 0 and short channel " +
				"ID block height is 0",
		},
		{
			name: "zero class height - fallback height hint " +
				"= conf depth",
			classHeight:    0,
			closeHeight:    6,
			confDepth:      6,
			shortChanID:    lnwire.ShortChannelID{BlockHeight: 50},
			expectedHeight: 0,
			expectError:    true,
			errorContains: "fallback height hint 6 <= " +
				"confirmation depth 6",
		},
		{
			name: "zero class height - fallback height hint " +
				"< conf depth",
			classHeight:    0,
			closeHeight:    3,
			confDepth:      6,
			shortChanID:    lnwire.ShortChannelID{BlockHeight: 50},
			expectedHeight: 0,
			expectError:    true,
			errorContains: "fallback height hint 3 <= " +
				"confirmation depth 6",
		},
		{
			name: "zero class height - close " +
				"height = 0, fallback height hint = conf depth",
			classHeight: 0,
			closeHeight: 0,
			confDepth:   6,
			shortChanID: lnwire.ShortChannelID{BlockHeight: 6},
			expectError: true,
			errorContains: "fallback height hint 6 <= " +
				"confirmation depth 6",
		},
		{
			name: "zero class height - close " +
				"height = 0, fallback height hint < conf depth",
			classHeight:    0,
			closeHeight:    0,
			confDepth:      6,
			shortChanID:    lnwire.ShortChannelID{BlockHeight: 3},
			expectedHeight: 0,
			expectError:    true,
			errorContains: "fallback height hint 3 <= " +
				"confirmation depth 6",
		},
		{
			name: "zero class height, fallback height is " +
				"valid",
			classHeight: 0,
			closeHeight: 100,
			confDepth:   6,
			shortChanID: lnwire.ShortChannelID{BlockHeight: 50},
			// heightHint - confDepth = 100 - 6 = 94.
			expectedHeight: 94,
			expectError:    false,
		},
		{
			name: "zero class height - close " +
				"height = 0, fallback height is valid",
			classHeight: 0,
			closeHeight: 0,
			confDepth:   6,
			shortChanID: lnwire.ShortChannelID{BlockHeight: 50},
			// heightHint - confDepth = 50 - 6 = 44.
			expectedHeight: 44,
			expectError:    false,
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a mock baby output.
			chanPoint := &wire.OutPoint{
				Hash: [chainhash.HashSize]byte{
					0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2,
					0xc6, 0xda, 0x48, 0x59, 0xe6, 0x96,
					0x31, 0x13, 0xa1, 0x17, 0x2d, 0xe7,
					0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
					0x1f, 0xb, 0x4c, 0xf9, 0x9e, 0xc5,
					0x8c, 0xe9,
				},
				Index: 9,
			}

			baby := &babyOutput{
				expiry: tc.classHeight,
				kidOutput: kidOutput{
					breachedOutput: breachedOutput{
						outpoint: *chanPoint,
					},
					originChanPoint: *chanPoint,
				},
			}

			cfg := &NurseryConfig{
				ConfDepth: tc.confDepth,
				FetchClosedChannel: func(
					chanID *wire.OutPoint) (
					*channeldb.ChannelCloseSummary,
					error) {

					if tc.fetchError != nil {
						return nil, tc.fetchError
					}

					return &channeldb.ChannelCloseSummary{
						CloseHeight: tc.closeHeight,
						ShortChanID: tc.shortChanID,
					}, nil
				},
			}

			nursery := &UtxoNursery{
				cfg: cfg,
			}

			resultHeight, err := nursery.patchZeroHeightHint(
				baby, tc.classHeight,
			)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(
						t, err.Error(),
						tc.errorContains,
					)
				}

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedHeight, resultHeight)
		})
	}
}
