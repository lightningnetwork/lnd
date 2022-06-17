package chanvalidate

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	aliceKey = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x18, 0xa3, 0xef, 0xb9,
		0x64, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
	bobKey = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x98, 0xa3, 0xef, 0xb9,
		0x69, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePriv, alicePub = btcec.PrivKeyFromBytes(aliceKey[:])
	bobPriv, bobPub     = btcec.PrivKeyFromBytes(bobKey[:])
)

// channelTestCtx holds shared context that will be used in all tests cases
// below.
type channelTestCtx struct {
	fundingTx *wire.MsgTx

	invalidCommitTx, validCommitTx *wire.MsgTx

	chanPoint wire.OutPoint
	cid       lnwire.ShortChannelID

	fundingScript []byte
}

// newChannelTestCtx creates a new channelCtx for use in the validation tests
// below. This creates a fake funding transaction, as well as an invalid and
// valid commitment transaction.
func newChannelTestCtx(chanSize int64) (*channelTestCtx, error) {
	multiSigScript, err := input.GenMultiSigScript(
		alicePub.SerializeCompressed(), bobPub.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}
	pkScript, err := input.WitnessScriptHash(multiSigScript)
	if err != nil {
		return nil, err
	}

	fundingOutput := wire.TxOut{
		Value:    chanSize,
		PkScript: pkScript,
	}

	fundingTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{},
		},
		TxOut: []*wire.TxOut{
			&fundingOutput,
			{
				Value:    9999,
				PkScript: bytes.Repeat([]byte{'a'}, 32),
			},
			{
				Value:    99999,
				PkScript: bytes.Repeat([]byte{'b'}, 32),
			},
		},
	}

	fundingTxHash := fundingTx.TxHash()

	commitTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  fundingTxHash,
					Index: 0,
				},
			},
		},
		TxOut: []*wire.TxOut{
			&fundingOutput,
		},
	}

	sigHashes := input.NewTxSigHashesV0Only(commitTx)
	aliceSigRaw, err := txscript.RawTxInWitnessSignature(
		commitTx, sigHashes, 0, chanSize,
		multiSigScript, txscript.SigHashAll, alicePriv,
	)
	if err != nil {
		return nil, err
	}

	aliceSig, err := ecdsa.ParseDERSignature(aliceSigRaw)
	if err != nil {
		return nil, err
	}

	bobSigRaw, err := txscript.RawTxInWitnessSignature(
		commitTx, sigHashes, 0, chanSize,
		multiSigScript, txscript.SigHashAll, bobPriv,
	)
	if err != nil {
		return nil, err
	}

	bobSig, err := ecdsa.ParseDERSignature(bobSigRaw)
	if err != nil {
		return nil, err
	}

	commitTx.TxIn[0].Witness = input.SpendMultiSig(
		multiSigScript, alicePub.SerializeCompressed(), aliceSig,
		bobPub.SerializeCompressed(), bobSig,
	)

	invalidCommitTx := commitTx.Copy()
	invalidCommitTx.TxIn[0].PreviousOutPoint.Index = 2

	return &channelTestCtx{
		fundingTx:       fundingTx,
		validCommitTx:   commitTx,
		invalidCommitTx: invalidCommitTx,
		chanPoint: wire.OutPoint{
			Hash:  fundingTxHash,
			Index: 0,
		},
		cid: lnwire.ShortChannelID{
			TxPosition: 0,
		},
		fundingScript: pkScript,
	}, nil
}

// TestValidate ensures that the Validate method is able to detect all cases of
// invalid channels, and properly accept invalid channels.
func TestValidate(t *testing.T) {
	t.Parallel()

	chanSize := int64(1000000)
	channelCtx, err := newChannelTestCtx(chanSize)
	require.NoError(t, err, "unable to make channel context")

	testCases := []struct {
		// expectedErr is the error we expect, this should be nil if
		// the channel is valid.
		expectedErr error

		// locator is how the Validate method should find the target
		// outpoint.
		locator ChanLocator

		// chanPoint is the expected final out point.
		chanPoint wire.OutPoint

		// chanScript is the funding pkScript.
		chanScript []byte

		// fundingTx is the funding transaction to use in the test.
		fundingTx *wire.MsgTx

		// commitTx is the commitment transaction to use in the test,
		// this is optional.
		commitTx *wire.MsgTx

		// expectedValue is the value of the funding transaction we
		// should expect. This is only required if commitTx is non-nil.
		expectedValue int64
	}{
		// Short chan ID channel locator, unable to find target
		// outpoint.
		{
			expectedErr: ErrInvalidOutPoint,
			locator: &ShortChanIDChanLocator{
				ID: lnwire.NewShortChanIDFromInt(9),
			},
			fundingTx: &wire.MsgTx{},
		},

		// Chan point based channel locator, unable to find target
		// outpoint.
		{
			expectedErr: ErrInvalidOutPoint,
			locator: &OutPointChanLocator{
				ChanPoint: wire.OutPoint{
					Index: 99,
				},
			},
			fundingTx: &wire.MsgTx{},
		},

		// Invalid pkScript match on mined funding transaction, chan
		// point based locator.
		{
			expectedErr: ErrWrongPkScript,
			locator: &OutPointChanLocator{
				ChanPoint: channelCtx.chanPoint,
			},
			chanScript: bytes.Repeat([]byte("a"), 32),
			fundingTx:  channelCtx.fundingTx,
		},

		// Invalid pkScript match on mined funding transaction, short
		// chan ID based locator.
		{
			expectedErr: ErrWrongPkScript,
			locator: &ShortChanIDChanLocator{
				ID: channelCtx.cid,
			},
			chanScript: bytes.Repeat([]byte("a"), 32),
			fundingTx:  channelCtx.fundingTx,
		},

		// Invalid amount on funding transaction.
		{
			expectedErr: ErrInvalidSize,
			locator: &OutPointChanLocator{
				ChanPoint: channelCtx.chanPoint,
			},
			chanScript:    channelCtx.fundingScript,
			fundingTx:     channelCtx.fundingTx,
			expectedValue: 555,
			commitTx:      channelCtx.validCommitTx,
		},

		// Validation failure on final commitment transaction
		{
			expectedErr: &ErrScriptValidateError{},
			locator: &OutPointChanLocator{
				ChanPoint: channelCtx.chanPoint,
			},
			chanScript:    channelCtx.fundingScript,
			fundingTx:     channelCtx.fundingTx,
			expectedValue: chanSize,
			commitTx:      channelCtx.invalidCommitTx,
		},

		// Fully valid 3rd party verification.
		{
			expectedErr: nil,
			locator: &OutPointChanLocator{
				ChanPoint: channelCtx.chanPoint,
			},
			chanScript: channelCtx.fundingScript,
			fundingTx:  channelCtx.fundingTx,
			chanPoint:  channelCtx.chanPoint,
		},

		// Fully valid self-channel verification.
		{
			expectedErr: nil,
			locator: &OutPointChanLocator{
				ChanPoint: channelCtx.chanPoint,
			},
			chanScript:    channelCtx.fundingScript,
			fundingTx:     channelCtx.fundingTx,
			expectedValue: chanSize,
			commitTx:      channelCtx.validCommitTx,
			chanPoint:     channelCtx.chanPoint,
		},
	}

	for i, testCase := range testCases {
		ctx := &Context{
			Locator:          testCase.locator,
			MultiSigPkScript: testCase.chanScript,
			FundingTx:        testCase.fundingTx,
		}

		if testCase.commitTx != nil {
			ctx.CommitCtx = &CommitmentContext{
				Value: btcutil.Amount(
					testCase.expectedValue,
				),
				FullySignedCommitTx: testCase.commitTx,
			}
		}

		chanPoint, err := Validate(ctx)
		if err != testCase.expectedErr {
			_, ok := testCase.expectedErr.(*ErrScriptValidateError)
			_, scriptErr := err.(*ErrScriptValidateError)
			if ok && scriptErr {
				continue
			}

			t.Fatalf("test #%v: validation failed: expected %v, "+
				"got %v", i, testCase.expectedErr, err)
		}

		if err != nil {
			continue
		}

		if *chanPoint != testCase.chanPoint {
			t.Fatalf("test #%v: wrong outpoint: want %v, got %v",
				i, testCase.chanPoint, chanPoint)
		}
	}
}
