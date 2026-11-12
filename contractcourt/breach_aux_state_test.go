package contractcourt

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// randPubKey returns a random public key.
func randPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// newAuxRetribution builds the retribution of a breached aux channel the way
// newRetributionInfo does: a revoked commitment output with a resolution
// blob, and one HTLC in each direction, each with a first-level resolution
// blob and the resolution request template a second-level resolution needs.
// Both HTLCs share the HTLC index 0.
func newAuxRetribution(t *testing.T) *retributionInfo {
	t.Helper()

	commitSecret, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	revokeBase := randPubKey(t)

	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&wire.TxIn{})
	for range 3 {
		commitTx.AddTxOut(&wire.TxOut{
			Value: 1000, PkScript: randomPkScript(t),
		})
	}
	commitHash := commitTx.TxHash()

	keyRing := &lnwallet.CommitmentKeyRing{
		CommitPoint:         commitSecret.PubKey(),
		LocalCommitKeyTweak: []byte{0x01, 0x02},
		LocalHtlcKeyTweak:   []byte{0x03, 0x04},
		LocalHtlcKey:        randPubKey(t),
		RemoteHtlcKey:       randPubKey(t),
		ToLocalKey:          randPubKey(t),
		ToRemoteKey:         randPubKey(t),
		RevocationKey:       randPubKey(t),
	}
	htlcBasePoint := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyHtlcBase,
			Index:  7,
		},
		PubKey: randPubKey(t),
	}

	ret := &retributionInfo{
		commitHash:   commitHash,
		chanPoint:    wire.OutPoint{Hash: chainhash.Hash{0xaa}},
		chainHash:    chainhash.Hash{0xbb},
		breachHeight: 321,
	}

	// The revoked commitment output.
	ret.breachedOutputs = append(ret.breachedOutputs, breachedOutput{
		amt:         1000,
		outpoint:    wire.OutPoint{Hash: commitHash, Index: 0},
		witnessType: input.TaprootCommitmentRevoke,
		signDesc: input.SignDescriptor{
			KeyDesc:     keychain.KeyDescriptor{PubKey: revokeBase},
			DoubleTweak: commitSecret,
			Output:      commitTx.TxOut[0],
			HashType:    txscript.SigHashDefault,
		},
		resolutionBlob: fn.Some(tlv.Blob{0xc0}),
	})

	htlcOutput := func(index uint32, incoming bool,
		auxSig []byte) breachedOutput {

		witnessType := input.TaprootHtlcOfferedRevoke
		if incoming {
			witnessType = input.TaprootHtlcAcceptedRevoke
		}

		signDesc := input.SignDescriptor{
			KeyDesc:     keychain.KeyDescriptor{PubKey: revokeBase},
			DoubleTweak: commitSecret,
			Output:      commitTx.TxOut[index],
			HashType:    txscript.SigHashDefault,
			SignMethod:  input.TaprootKeySpendSignMethod,
			TapTweak:    bytes.Repeat([]byte{byte(index)}, 32),
		}

		req := &lnwallet.ResolutionReq{
			ChanPoint:           ret.chanPoint,
			ChanType:            channeldb.SimpleTaprootFeatureBit,
			ShortChanID:         lnwire.NewShortChanIDFromInt(99),
			Initiator:           true,
			CommitBlob:          fn.Some(tlv.Blob{0x0c, 0x0b}),
			FundingBlob:         fn.Some(tlv.Blob{0x0f}),
			HtlcID:              fn.Some(uint64(0)),
			HtlcIncoming:        fn.Some(incoming),
			HtlcAmt:             1000,
			Type:                witnessType,
			CloseType:           lnwallet.Breach,
			CommitTx:            commitTx,
			CommitTxBlockHeight: ret.breachHeight,
			CommitFee:           500,
			SignDesc:            signDesc,
			KeyRing:             keyRing,
			CsvDelay:            144,
			CltvDelay:           fn.Some(700 + index),
			PayHash:             fn.Some([32]byte{byte(index)}),
		}
		if auxSig != nil {
			//nolint:ll
			req.AuxSigDesc = fn.Some(lnwallet.AuxSigDesc{
				AuxSig: auxSig,
				SignDetails: input.SignDetails{
					SignDesc: input.SignDescriptor{
						KeyDesc:     htlcBasePoint,
						SingleTweak: keyRing.LocalHtlcKeyTweak,
						SignMethod:  input.TaprootScriptSpendSignMethod,
					},
				},
			})
		}

		return breachedOutput{
			amt: 1000,
			outpoint: wire.OutPoint{
				Hash: commitHash, Index: index,
			},
			witnessType:              witnessType,
			signDesc:                 signDesc,
			secondLevelWitnessScript: []byte{0x51},
			secondLevelTapTweak:      [32]byte{0xee, byte(index)},
			resolveReq:               req,
			resolutionBlob: fn.Some(
				tlv.Blob{0xf1, byte(index)},
			),
		}
	}

	// One HTLC we received, whose revocation carried an aux sig, and one
	// we offered, whose revocation did not.
	ret.breachedOutputs = append(
		ret.breachedOutputs,
		htlcOutput(1, true, []byte{0xa5, 0x16}),
		htlcOutput(2, false, nil),
	)

	return ret
}

// requireSameResolveReq asserts that a resolution request restored from the
// store carries everything the original one did.
func requireSameResolveReq(t *testing.T, want, got *lnwallet.ResolutionReq) {
	t.Helper()

	require.NotNil(t, got)

	// The key ring and the transaction are compared by value, everything
	// else directly.
	requireSameKey := func(a, b *btcec.PublicKey) {
		require.True(t, a.IsEqual(b))
	}
	requireSameKey(want.KeyRing.CommitPoint, got.KeyRing.CommitPoint)
	requireSameKey(want.KeyRing.LocalHtlcKey, got.KeyRing.LocalHtlcKey)
	requireSameKey(want.KeyRing.RemoteHtlcKey, got.KeyRing.RemoteHtlcKey)
	requireSameKey(want.KeyRing.ToLocalKey, got.KeyRing.ToLocalKey)
	requireSameKey(want.KeyRing.ToRemoteKey, got.KeyRing.ToRemoteKey)
	requireSameKey(want.KeyRing.RevocationKey, got.KeyRing.RevocationKey)
	require.Equal(
		t, want.KeyRing.LocalCommitKeyTweak,
		got.KeyRing.LocalCommitKeyTweak,
	)
	require.Equal(
		t, want.KeyRing.LocalHtlcKeyTweak,
		got.KeyRing.LocalHtlcKeyTweak,
	)

	require.Equal(t, want.CommitTx.TxHash(), got.CommitTx.TxHash())

	requireSameSignDesc := func(a, b input.SignDescriptor) {
		requireSameKey(a.KeyDesc.PubKey, b.KeyDesc.PubKey)
		require.Equal(t, a.KeyDesc.KeyLocator, b.KeyDesc.KeyLocator)
		require.Equal(t, a.SingleTweak, b.SingleTweak)
		require.Equal(t, a.DoubleTweak, b.DoubleTweak)
		require.Equal(t, a.Output, b.Output)
		require.Equal(t, a.HashType, b.HashType)
		require.Equal(t, a.SignMethod, b.SignMethod)
		require.Equal(t, a.TapTweak, b.TapTweak)
	}
	requireSameSignDesc(want.SignDesc, got.SignDesc)

	require.Equal(t, want.AuxSigDesc.IsSome(), got.AuxSigDesc.IsSome())
	want.AuxSigDesc.WhenSome(func(wantDesc lnwallet.AuxSigDesc) {
		gotDesc := got.AuxSigDesc.UnwrapOr(lnwallet.AuxSigDesc{})

		require.Equal(t, wantDesc.AuxSig, gotDesc.AuxSig)
		requireSameSignDesc(
			wantDesc.SignDetails.SignDesc,
			gotDesc.SignDetails.SignDesc,
		)
	})

	wantRest, gotRest := *want, *got
	for _, req := range []*lnwallet.ResolutionReq{&wantRest, &gotRest} {
		req.KeyRing = nil
		req.CommitTx = nil
		req.SignDesc = input.SignDescriptor{}
		req.AuxSigDesc = fn.None[lnwallet.AuxSigDesc]()
	}
	require.Equal(t, wantRest, gotRest)
}

// loadRetribution returns the single retribution held by the store.
func loadRetribution(t *testing.T, rs *RetributionStore) *retributionInfo {
	t.Helper()

	var rets []*retributionInfo
	err := rs.ForAll(func(ret *retributionInfo) error {
		rets = append(rets, ret)

		return nil
	}, func() {
		rets = nil
	})
	require.NoError(t, err)
	require.Len(t, rets, 1)

	return rets[0]
}

// hasAuxRetributionState reports whether the store holds any aux state for
// the given channel.
func hasAuxRetributionState(t *testing.T, db kvdb.Backend,
	chanPoint wire.OutPoint) bool {

	t.Helper()

	var chanKey bytes.Buffer
	require.NoError(t, graphdb.WriteOutpoint(&chanKey, &chanPoint))

	var found bool
	err := kvdb.View(db, func(tx kvdb.RTx) error {
		auxBucket := tx.ReadBucket(auxRetributionBucket)
		if auxBucket == nil {
			return nil
		}
		found = auxBucket.NestedReadBucket(chanKey.Bytes()) != nil

		return nil
	}, func() {
		found = false
	})
	require.NoError(t, err)

	return found
}

// TestRetributionStoreAuxState asserts that the retribution of a breached
// aux channel survives a restart with everything the breach arbiter needs to
// finish the job. Without the resolution request templates, an HTLC that is
// taken to the second level after the restart would be handled like the
// HTLC of a regular channel: its second-level output is never resolved, so
// the HTLC's aux value is not swept. Without the first-level blobs, the same
// goes for an HTLC that is swept straight from the commitment.
func TestRetributionStoreAuxState(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	rs := NewRetributionStore(db)

	original := newAuxRetribution(t)
	require.True(t, original.hasAuxData())
	require.NoError(t, rs.Add(original))

	// Restart: the retribution is loaded from disk.
	loaded := loadRetribution(t, rs)
	require.True(t, loaded.hasAuxData())
	require.Len(t, loaded.breachedOutputs, 3)
	require.Empty(t, loaded.justiceTxs)

	for i := range original.breachedOutputs {
		want := &original.breachedOutputs[i]
		got := &loaded.breachedOutputs[i]

		require.Equal(t, want.outpoint, got.outpoint)
		require.Equal(t, want.witnessType, got.witnessType)

		// Every output keeps its blob, HTLCs included.
		require.Equal(t, want.resolutionBlob, got.resolutionBlob)

		// The commitment output never had a template.
		if want.resolveReq == nil {
			require.Nil(t, got.resolveReq)

			continue
		}

		requireSameResolveReq(t, want.resolveReq, got.resolveReq)
	}

	// The two HTLCs share an index and are told apart by direction.
	require.Equal(
		t, fn.Some(true),
		loaded.breachedOutputs[1].resolveReq.HtlcIncoming,
	)
	require.Equal(
		t, fn.Some(false),
		loaded.breachedOutputs[2].resolveReq.HtlcIncoming,
	)

	// Removing the retribution removes its aux state with it.
	require.True(t, hasAuxRetributionState(t, db, original.chanPoint))
	require.NoError(t, rs.Remove(&original.chanPoint))
	require.False(t, hasAuxRetributionState(t, db, original.chanPoint))
	require.Zero(t, countRetributions(t, rs))
}

// TestRetributionStoreJusticeTxs asserts that the justice transactions
// recorded for a breach are handed back when the retribution is loaded after
// a restart, with what it takes to report the one that confirms to the aux
// sweeper. They are the only way to recognize a justice transaction that was
// broadcast before the restart: the ones built afterwards pay to a new
// address with a new fee and have different txids.
func TestRetributionStoreJusticeTxs(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	rs := NewRetributionStore(db)

	ret := newAuxRetribution(t)
	require.NoError(t, rs.Add(ret))

	internalKey := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{Family: 9, Index: 4},
		PubKey:     randPubKey(t),
	}
	auxKey := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{Family: 212, Index: 1},
		PubKey:     randPubKey(t),
	}

	spendAll := &justiceTxCtx{
		fee:            1234,
		hasSecondLevel: true,
		sweepAddr: lnwallet.AddrWithKey{
			DeliveryAddress: lnwire.DeliveryAddress{0x51, 0x20},
			InternalKey:     fn.Some(internalKey),
		},
		extraTxOut: fn.Some(sweep.SweepOutput{
			TxOut: wire.TxOut{
				Value:    400,
				PkScript: []byte{0x51, 0x21},
			},
			IsExtra:     true,
			InternalKey: fn.Some(auxKey),
		}),
	}
	spendCommit := &justiceTxCtx{
		fee: 99,
		sweepAddr: lnwallet.AddrWithKey{
			DeliveryAddress: lnwire.DeliveryAddress{0x00, 0x14},
		},
	}

	first := map[chainhash.Hash]*justiceTxCtx{
		{0x01}: spendAll,
	}
	second := map[chainhash.Hash]*justiceTxCtx{
		{0x02}: spendCommit,
	}

	// The variants are recorded as they are created, over several
	// rebuilds. Earlier ones are kept.
	require.NoError(t, rs.RecordJusticeTxs(&ret.chanPoint, nil))
	require.NoError(t, rs.RecordJusticeTxs(&ret.chanPoint, first))
	require.NoError(t, rs.RecordJusticeTxs(&ret.chanPoint, second))

	loaded := loadRetribution(t, rs)
	require.Len(t, loaded.justiceTxs, 2)

	requireSameJusticeTx := func(want, got *justiceTxCtx) {
		t.Helper()

		require.NotNil(t, got)
		require.Equal(t, want.fee, got.fee)
		require.Equal(t, want.hasSecondLevel, got.hasSecondLevel)
		require.Equal(
			t, want.sweepAddr.DeliveryAddress,
			got.sweepAddr.DeliveryAddress,
		)

		requireSameKeyDesc := func(
			a, b fn.Option[keychain.KeyDescriptor]) {

			require.Equal(t, a.IsSome(), b.IsSome())
			a.WhenSome(func(wantKey keychain.KeyDescriptor) {
				gotKey := b.UnwrapOr(keychain.KeyDescriptor{})

				require.Equal(
					t, wantKey.KeyLocator,
					gotKey.KeyLocator,
				)
				require.True(
					t,
					wantKey.PubKey.IsEqual(gotKey.PubKey),
				)
			})
		}
		requireSameKeyDesc(
			want.sweepAddr.InternalKey, got.sweepAddr.InternalKey,
		)

		require.Equal(
			t, want.extraTxOut.IsSome(), got.extraTxOut.IsSome(),
		)
		want.extraTxOut.WhenSome(func(wantOut sweep.SweepOutput) {
			gotOut := got.extraTxOut.UnwrapOr(sweep.SweepOutput{})

			require.Equal(t, wantOut.TxOut, gotOut.TxOut)
			require.Equal(t, wantOut.IsExtra, gotOut.IsExtra)
			requireSameKeyDesc(
				wantOut.InternalKey, gotOut.InternalKey,
			)
		})
	}
	requireSameJusticeTx(spendAll, loaded.justiceTxs[chainhash.Hash{1}])
	requireSameJusticeTx(spendCommit, loaded.justiceTxs[chainhash.Hash{2}])

	// The loaded history seeds the tracker of the retribution loop.
	tracker := newJusticeTxTracker(loaded.justiceTxs)
	require.Len(t, tracker.history, 2)

	// Cleaning the breach up removes the recorded transactions too.
	require.NoError(t, rs.Remove(&ret.chanPoint))
	require.False(t, hasAuxRetributionState(t, db, ret.chanPoint))
}

// TestRetributionStoreRegularChannelNoAuxState asserts that nothing at all is
// written to the aux retribution bucket for the breach of a regular channel,
// and that such a retribution is loaded without any aux data.
func TestRetributionStoreRegularChannelNoAuxState(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	rs := NewRetributionStore(db)

	// The same retribution as for the aux channel, minus the aux data.
	ret := newAuxRetribution(t)
	for i := range ret.breachedOutputs {
		ret.breachedOutputs[i].resolveReq = nil
		ret.breachedOutputs[i].resolutionBlob = fn.None[tlv.Blob]()
	}
	require.False(t, ret.hasAuxData())
	require.NoError(t, rs.Add(ret))

	require.False(t, hasAuxRetributionState(t, db, ret.chanPoint))

	loaded := loadRetribution(t, rs)
	require.False(t, loaded.hasAuxData())
	require.Nil(t, loaded.justiceTxs)

	require.NoError(t, rs.Remove(&ret.chanPoint))
}

// TestAuxBreachContextDecodeCorrupt asserts that truncated aux retribution
// records fail to decode instead of yielding a partial state.
func TestAuxBreachContextDecodeCorrupt(t *testing.T) {
	t.Parallel()

	ctx := auxBreachContextFromRetInfo(newAuxRetribution(t))
	require.NotNil(t, ctx)

	var b bytes.Buffer
	require.NoError(t, ctx.encode(&b))
	encoded := b.Bytes()

	var decoded auxBreachContext
	require.NoError(t, decoded.decode(bytes.NewReader(encoded)))
	require.Len(t, decoded.htlcs, 2)

	for _, cut := range []int{0, 1, len(encoded) / 2, len(encoded) - 1} {
		var truncated auxBreachContext
		err := truncated.decode(bytes.NewReader(encoded[:cut]))
		require.Error(t, err, "cut at %d", cut)
	}

	// An unknown version is refused.
	unknown := bytes.Clone(encoded)
	unknown[0] = auxBreachStateVersion + 1
	var versioned auxBreachContext
	require.ErrorContains(
		t, versioned.decode(bytes.NewReader(unknown)),
		"unknown aux breach context version",
	)
}
