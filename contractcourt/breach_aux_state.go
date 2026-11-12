package contractcourt

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
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
)

var (
	// auxRetributionBucket stores the aux (custom channel) state of a
	// retribution, keyed by channel point. It holds what the breach
	// arbiter cannot recover from anywhere else after a restart: the
	// request template it needs to resolve an HTLC that was taken to the
	// second level, and the justice transactions it has created so far,
	// whose confirmation the aux sweeper has to be told about. The bucket
	// is only ever written for channels that carry aux resolution data,
	// so retributions of regular channels are stored exactly as before.
	auxRetributionBucket = []byte("aux-retribution")

	// auxBreachContextKey is the key, inside a channel's aux retribution
	// bucket, of the encoded auxBreachContext.
	auxBreachContextKey = []byte("context")

	// auxBreachJusticeBucket is the sub-bucket, inside a channel's aux
	// retribution bucket, that maps the txid of every justice transaction
	// created for the channel to its encoded metadata.
	auxBreachJusticeBucket = []byte("justice-txs")
)

// auxBreachStateVersion is the encoding version of the aux retribution
// records.
const auxBreachStateVersion uint8 = 0

// auxBreachContext is the part of the aux resolution request of a breached
// channel that all of its HTLCs share, together with the per-HTLC remainder.
// Storing the shared part once keeps the record small: it includes the breach
// transaction and the commitment blob, which grow with the number of HTLCs.
type auxBreachContext struct {
	chanType    channeldb.ChannelType
	shortChanID lnwire.ShortChannelID
	initiator   bool
	fundingBlob fn.Option[tlv.Blob]
	commitBlob  fn.Option[tlv.Blob]
	commitTx    *wire.MsgTx
	commitFee   btcutil.Amount
	csvDelay    uint32
	keyRing     lnwallet.CommitmentKeyRing

	// htlcs holds the HTLC specific part of the request, keyed by the
	// HTLC's outpoint on the breached commitment.
	htlcs map[wire.OutPoint]auxBreachHtlc
}

// auxBreachHtlc is the HTLC specific part of an aux resolution request.
type auxBreachHtlc struct {
	htlcID    fn.Option[input.HtlcIndex]
	incoming  fn.Option[bool]
	payHash   fn.Option[[32]byte]
	cltvDelay fn.Option[uint32]

	// auxSig is the remote party's revocation aux sig for the HTLC, empty
	// if the state was revoked without one.
	auxSig []byte

	// auxSigKeyDesc and auxSigTweak identify our key the aux sig is
	// paired with.
	auxSigKeyDesc keychain.KeyDescriptor
	auxSigTweak   []byte
}

// auxBreachContextFromRetInfo extracts the aux resolution context from the
// request templates of a retribution. It returns nil if no breached output
// carries a template, which is the case for every regular channel.
func auxBreachContextFromRetInfo(ret *retributionInfo) *auxBreachContext {
	var ctx *auxBreachContext
	for i := range ret.breachedOutputs {
		bo := &ret.breachedOutputs[i]
		req := bo.resolveReq
		if req == nil || req.KeyRing == nil {
			continue
		}

		if ctx == nil {
			ctx = &auxBreachContext{
				chanType:    req.ChanType,
				shortChanID: req.ShortChanID,
				initiator:   req.Initiator,
				fundingBlob: req.FundingBlob,
				commitBlob:  req.CommitBlob,
				commitTx:    req.CommitTx,
				commitFee:   req.CommitFee,
				csvDelay:    req.CsvDelay,
				keyRing:     *req.KeyRing,
				htlcs: make(
					map[wire.OutPoint]auxBreachHtlc,
				),
			}
		}

		htlc := auxBreachHtlc{
			htlcID:    req.HtlcID,
			incoming:  req.HtlcIncoming,
			payHash:   req.PayHash,
			cltvDelay: req.CltvDelay,
		}
		req.AuxSigDesc.WhenSome(func(desc lnwallet.AuxSigDesc) {
			signDesc := desc.SignDetails.SignDesc

			htlc.auxSig = desc.AuxSig
			htlc.auxSigKeyDesc = signDesc.KeyDesc
			htlc.auxSigTweak = signDesc.SingleTweak
		})

		ctx.htlcs[bo.outpoint] = htlc
	}

	return ctx
}

// apply restores the aux resolution request templates of the breached HTLC
// outputs of a retribution that was just loaded from disk. It must run
// before any output is morphed, while each output still sits at its outpoint
// on the breached commitment.
func (c *auxBreachContext) apply(ret *retributionInfo) {
	for i := range ret.breachedOutputs {
		bo := &ret.breachedOutputs[i]

		htlc, ok := c.htlcs[bo.outpoint]
		if !ok {
			continue
		}

		// The sign descriptor of the request is the one of the
		// breached output. The store doesn't persist the sign method,
		// which for the key spend of a taproot HTLC revocation is
		// always the same.
		signDesc := bo.signDesc
		signDesc.SignMethod = input.TaprootKeySpendSignMethod

		keyRing := c.keyRing
		req := &lnwallet.ResolutionReq{
			ChanPoint:           ret.chanPoint,
			ChanType:            c.chanType,
			ShortChanID:         c.shortChanID,
			Initiator:           c.initiator,
			CommitBlob:          c.commitBlob,
			FundingBlob:         c.fundingBlob,
			HtlcID:              htlc.htlcID,
			HtlcIncoming:        htlc.incoming,
			HtlcAmt:             bo.amt,
			Type:                bo.witnessType,
			CloseType:           lnwallet.Breach,
			CommitTx:            c.commitTx,
			CommitTxBlockHeight: ret.breachHeight,
			CommitFee:           c.commitFee,
			SignDesc:            signDesc,
			KeyRing:             &keyRing,
			CsvDelay:            c.csvDelay,
			CltvDelay:           htlc.cltvDelay,
			PayHash:             htlc.payHash,
		}

		if len(htlc.auxSig) > 0 {
			//nolint:ll
			req.AuxSigDesc = fn.Some(lnwallet.AuxSigDesc{
				AuxSig: htlc.auxSig,
				SignDetails: input.SignDetails{
					SignDesc: input.SignDescriptor{
						KeyDesc:     htlc.auxSigKeyDesc,
						SingleTweak: htlc.auxSigTweak,
						SignMethod:  input.TaprootScriptSpendSignMethod,
					},
				},
			})
		}

		bo.resolveReq = req
	}
}

// writeOptBlob writes an optional blob as a presence flag followed by the
// blob.
func writeOptBlob(w io.Writer, blob fn.Option[tlv.Blob]) error {
	err := channeldb.WriteElement(w, blob.IsSome())
	if err != nil {
		return err
	}

	return fn.MapOptionZ(blob, func(b tlv.Blob) error {
		return wire.WriteVarBytes(w, 0, b)
	})
}

// readOptBlob reads an optional blob written by writeOptBlob.
func readOptBlob(r io.Reader) (fn.Option[tlv.Blob], error) {
	none := fn.None[tlv.Blob]()

	var isSome bool
	if err := channeldb.ReadElement(r, &isSome); err != nil {
		return none, err
	}
	if !isSome {
		return none, nil
	}

	blob, err := wire.ReadVarBytes(r, 0, wire.MaxMessagePayload, "blob")
	if err != nil {
		return none, err
	}

	return fn.Some[tlv.Blob](blob), nil
}

// writeOptKeyDesc writes an optional key descriptor as a presence flag
// followed by the descriptor.
func writeOptKeyDesc(w io.Writer,
	keyDesc fn.Option[keychain.KeyDescriptor]) error {

	err := channeldb.WriteElement(w, keyDesc.IsSome())
	if err != nil {
		return err
	}

	return fn.MapOptionZ(keyDesc, func(k keychain.KeyDescriptor) error {
		return writeKeyDesc(w, k)
	})
}

// readOptKeyDesc reads an optional key descriptor written by
// writeOptKeyDesc.
func readOptKeyDesc(r io.Reader) (fn.Option[keychain.KeyDescriptor], error) {
	none := fn.None[keychain.KeyDescriptor]()

	var isSome bool
	if err := channeldb.ReadElement(r, &isSome); err != nil {
		return none, err
	}
	if !isSome {
		return none, nil
	}

	keyDesc, err := readKeyDesc(r)
	if err != nil {
		return none, err
	}

	return fn.Some(keyDesc), nil
}

// writeKeyDesc writes a key descriptor whose public key may be unset.
func writeKeyDesc(w io.Writer, keyDesc keychain.KeyDescriptor) error {
	err := channeldb.WriteElements(
		w, uint32(keyDesc.Family), keyDesc.Index,
	)
	if err != nil {
		return err
	}

	return writeOptPubKey(w, keyDesc.PubKey)
}

// readKeyDesc reads a key descriptor written by writeKeyDesc.
func readKeyDesc(r io.Reader) (keychain.KeyDescriptor, error) {
	var (
		keyDesc keychain.KeyDescriptor
		family  uint32
	)
	err := channeldb.ReadElements(r, &family, &keyDesc.Index)
	if err != nil {
		return keyDesc, err
	}
	keyDesc.Family = keychain.KeyFamily(family)

	keyDesc.PubKey, err = readOptPubKey(r)

	return keyDesc, err
}

// keyRingKeys returns the public keys of the context's key ring in the order
// they are encoded in.
func (c *auxBreachContext) keyRingKeys() []**btcec.PublicKey {
	return []**btcec.PublicKey{
		&c.keyRing.CommitPoint, &c.keyRing.LocalHtlcKey,
		&c.keyRing.RemoteHtlcKey, &c.keyRing.ToLocalKey,
		&c.keyRing.ToRemoteKey, &c.keyRing.RevocationKey,
	}
}

// writeOptPubKey writes a public key that may be unset as a presence flag
// followed by the key.
func writeOptPubKey(w io.Writer, key *btcec.PublicKey) error {
	err := channeldb.WriteElement(w, key != nil)
	if err != nil || key == nil {
		return err
	}

	return channeldb.WriteElement(w, key)
}

// readOptPubKey reads a public key written by writeOptPubKey.
func readOptPubKey(r io.Reader) (*btcec.PublicKey, error) {
	var hasKey bool
	if err := channeldb.ReadElement(r, &hasKey); err != nil {
		return nil, err
	}
	if !hasKey {
		return nil, nil
	}

	var key *btcec.PublicKey
	err := channeldb.ReadElement(r, &key)

	return key, err
}

// encode serializes the aux breach context.
func (c *auxBreachContext) encode(w io.Writer) error {
	err := channeldb.WriteElements(
		w, auxBreachStateVersion, c.chanType, c.shortChanID,
		c.initiator, c.commitFee, c.csvDelay,
	)
	if err != nil {
		return err
	}

	if err := writeOptBlob(w, c.fundingBlob); err != nil {
		return err
	}
	if err := writeOptBlob(w, c.commitBlob); err != nil {
		return err
	}

	// The breach transaction is unknown when the retribution was built
	// without the spending transaction at hand.
	err = channeldb.WriteElement(w, c.commitTx != nil)
	if err != nil {
		return err
	}
	if c.commitTx != nil {
		err = channeldb.WriteElement(w, c.commitTx)
		if err != nil {
			return err
		}
	}

	for _, key := range c.keyRingKeys() {
		if err := writeOptPubKey(w, *key); err != nil {
			return err
		}
	}
	err = wire.WriteVarBytes(w, 0, c.keyRing.LocalCommitKeyTweak)
	if err != nil {
		return err
	}
	err = wire.WriteVarBytes(w, 0, c.keyRing.LocalHtlcKeyTweak)
	if err != nil {
		return err
	}

	err = wire.WriteVarInt(w, 0, uint64(len(c.htlcs)))
	if err != nil {
		return err
	}
	for op, htlc := range c.htlcs {
		if err := graphdb.WriteOutpoint(w, &op); err != nil {
			return err
		}
		if err := htlc.encode(w); err != nil {
			return err
		}
	}

	return nil
}

// decode deserializes the aux breach context.
func (c *auxBreachContext) decode(r io.Reader) error {
	var version uint8
	err := channeldb.ReadElements(
		r, &version, &c.chanType, &c.shortChanID, &c.initiator,
		&c.commitFee, &c.csvDelay,
	)
	if err != nil {
		return err
	}
	if version != auxBreachStateVersion {
		return fmt.Errorf("unknown aux breach context version %d",
			version)
	}

	if c.fundingBlob, err = readOptBlob(r); err != nil {
		return err
	}
	if c.commitBlob, err = readOptBlob(r); err != nil {
		return err
	}

	var hasCommitTx bool
	if err := channeldb.ReadElement(r, &hasCommitTx); err != nil {
		return err
	}
	if hasCommitTx {
		err = channeldb.ReadElement(r, &c.commitTx)
		if err != nil {
			return err
		}
	}

	for _, key := range c.keyRingKeys() {
		if *key, err = readOptPubKey(r); err != nil {
			return err
		}
	}
	c.keyRing.LocalCommitKeyTweak, err = wire.ReadVarBytes(
		r, 0, 1000, "commit key tweak",
	)
	if err != nil {
		return err
	}
	c.keyRing.LocalHtlcKeyTweak, err = wire.ReadVarBytes(
		r, 0, 1000, "htlc key tweak",
	)
	if err != nil {
		return err
	}

	numHtlcs, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return err
	}

	c.htlcs = make(map[wire.OutPoint]auxBreachHtlc)
	for range numHtlcs {
		var op wire.OutPoint
		if err := graphdb.ReadOutpoint(r, &op); err != nil {
			return err
		}

		var htlc auxBreachHtlc
		if err := htlc.decode(r); err != nil {
			return err
		}

		c.htlcs[op] = htlc
	}

	return nil
}

// encode serializes the HTLC specific part of an aux resolution request.
func (h *auxBreachHtlc) encode(w io.Writer) error {
	err := channeldb.WriteElements(
		w, h.htlcID.IsSome(), h.htlcID.UnwrapOr(0),
		h.incoming.IsSome(), h.incoming.UnwrapOr(false),
		h.payHash.IsSome(), h.payHash.UnwrapOr([32]byte{}),
		h.cltvDelay.IsSome(), h.cltvDelay.UnwrapOr(0),
	)
	if err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, h.auxSig); err != nil {
		return err
	}
	if err := writeKeyDesc(w, h.auxSigKeyDesc); err != nil {
		return err
	}

	return wire.WriteVarBytes(w, 0, h.auxSigTweak)
}

// decode deserializes the HTLC specific part of an aux resolution request.
func (h *auxBreachHtlc) decode(r io.Reader) error {
	var (
		hasID, hasIncoming, hasPayHash, hasCltv bool
		htlcID                                  uint64
		incoming                                bool
		payHash                                 [32]byte
		cltvDelay                               uint32
	)
	err := channeldb.ReadElements(
		r, &hasID, &htlcID, &hasIncoming, &incoming, &hasPayHash,
		&payHash, &hasCltv, &cltvDelay,
	)
	if err != nil {
		return err
	}

	if hasID {
		h.htlcID = fn.Some(htlcID)
	}
	if hasIncoming {
		h.incoming = fn.Some(incoming)
	}
	if hasPayHash {
		h.payHash = fn.Some(payHash)
	}
	if hasCltv {
		h.cltvDelay = fn.Some(cltvDelay)
	}

	h.auxSig, err = wire.ReadVarBytes(
		r, 0, wire.MaxMessagePayload, "aux sig",
	)
	if err != nil {
		return err
	}
	if h.auxSigKeyDesc, err = readKeyDesc(r); err != nil {
		return err
	}
	h.auxSigTweak, err = wire.ReadVarBytes(r, 0, 1000, "aux sig tweak")

	return err
}

// encodeJusticeTxCtx serializes what the breach arbiter needs to tell the
// aux sweeper about a justice transaction once it confirms. The transaction
// itself is not stored: it is taken from the spend that reports it.
func encodeJusticeTxCtx(w io.Writer, jtx *justiceTxCtx) error {
	err := channeldb.WriteElements(
		w, auxBreachStateVersion, jtx.fee, jtx.hasSecondLevel,
	)
	if err != nil {
		return err
	}

	err = wire.WriteVarBytes(w, 0, jtx.sweepAddr.DeliveryAddress)
	if err != nil {
		return err
	}
	if err := writeOptKeyDesc(w, jtx.sweepAddr.InternalKey); err != nil {
		return err
	}

	err = channeldb.WriteElement(w, jtx.extraTxOut.IsSome())
	if err != nil {
		return err
	}

	return fn.MapOptionZ(jtx.extraTxOut, func(o sweep.SweepOutput) error {
		err := channeldb.WriteElements(w, o.Value, o.IsExtra)
		if err != nil {
			return err
		}

		err = wire.WriteVarBytes(w, 0, o.PkScript)
		if err != nil {
			return err
		}

		return writeOptKeyDesc(w, o.InternalKey)
	})
}

// decodeJusticeTxCtx deserializes a justice transaction context written by
// encodeJusticeTxCtx.
func decodeJusticeTxCtx(r io.Reader) (*justiceTxCtx, error) {
	var (
		jtx     justiceTxCtx
		version uint8
	)
	err := channeldb.ReadElements(
		r, &version, &jtx.fee, &jtx.hasSecondLevel,
	)
	if err != nil {
		return nil, err
	}
	if version != auxBreachStateVersion {
		return nil, fmt.Errorf("unknown justice tx context version %d",
			version)
	}

	jtx.sweepAddr.DeliveryAddress, err = wire.ReadVarBytes(
		r, 0, 1000, "sweep address",
	)
	if err != nil {
		return nil, err
	}
	if jtx.sweepAddr.InternalKey, err = readOptKeyDesc(r); err != nil {
		return nil, err
	}

	var hasExtraOut bool
	if err := channeldb.ReadElement(r, &hasExtraOut); err != nil {
		return nil, err
	}
	if !hasExtraOut {
		return &jtx, nil
	}

	var extraOut sweep.SweepOutput
	err = channeldb.ReadElements(r, &extraOut.Value, &extraOut.IsExtra)
	if err != nil {
		return nil, err
	}
	extraOut.PkScript, err = wire.ReadVarBytes(
		r, 0, 1000, "extra output script",
	)
	if err != nil {
		return nil, err
	}
	if extraOut.InternalKey, err = readOptKeyDesc(r); err != nil {
		return nil, err
	}
	jtx.extraTxOut = fn.Some(extraOut)

	return &jtx, nil
}

// putAuxBreachContext stores the aux resolution context of a retribution,
// if it has any.
func putAuxBreachContext(tx kvdb.RwTx, chanKey []byte,
	ret *retributionInfo) error {

	ctx := auxBreachContextFromRetInfo(ret)
	if ctx == nil {
		return nil
	}

	auxBucket, err := tx.CreateTopLevelBucket(auxRetributionBucket)
	if err != nil {
		return err
	}
	chanBucket, err := auxBucket.CreateBucketIfNotExists(chanKey)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := ctx.encode(&b); err != nil {
		return err
	}

	return chanBucket.Put(auxBreachContextKey, b.Bytes())
}

// fetchAuxBreachState loads the aux state stored for a retribution and
// applies it: the request templates of its HTLC outputs are restored, and
// the justice transactions created for it so far are attached.
func fetchAuxBreachState(tx kvdb.RTx, chanKey []byte,
	ret *retributionInfo) error {

	auxBucket := tx.ReadBucket(auxRetributionBucket)
	if auxBucket == nil {
		return nil
	}
	chanBucket := auxBucket.NestedReadBucket(chanKey)
	if chanBucket == nil {
		return nil
	}

	if ctxBytes := chanBucket.Get(auxBreachContextKey); ctxBytes != nil {
		var ctx auxBreachContext
		err := ctx.decode(bytes.NewReader(ctxBytes))
		if err != nil {
			return fmt.Errorf("decode aux breach context: %w", err)
		}

		ctx.apply(ret)
	}

	justiceBucket := chanBucket.NestedReadBucket(auxBreachJusticeBucket)
	if justiceBucket == nil {
		return nil
	}

	ret.justiceTxs = make(map[chainhash.Hash]*justiceTxCtx)

	return justiceBucket.ForEach(func(k, v []byte) error {
		txid, err := chainhash.NewHash(k)
		if err != nil {
			return err
		}

		jtx, err := decodeJusticeTxCtx(bytes.NewReader(v))
		if err != nil {
			return fmt.Errorf("decode justice tx %v: %w", txid, err)
		}
		ret.justiceTxs[*txid] = jtx

		return nil
	})
}

// deleteAuxBreachState removes the aux state stored for a retribution.
func deleteAuxBreachState(tx kvdb.RwTx, chanKey []byte) error {
	auxBucket := tx.ReadWriteBucket(auxRetributionBucket)
	if auxBucket == nil {
		return nil
	}

	err := auxBucket.DeleteNestedBucket(chanKey)
	if err != nil && !errors.Is(err, kvdb.ErrBucketNotFound) {
		return err
	}

	return nil
}

// RecordJusticeTxs persists the metadata of the given justice transactions
// of a breached channel, keyed by txid. Transactions recorded earlier are
// kept.
func (rs *RetributionStore) RecordJusticeTxs(chanPoint *wire.OutPoint,
	justiceTxs map[chainhash.Hash]*justiceTxCtx) error {

	if len(justiceTxs) == 0 {
		return nil
	}

	return kvdb.Update(rs.db, func(tx kvdb.RwTx) error {
		var chanBuf bytes.Buffer
		err := graphdb.WriteOutpoint(&chanBuf, chanPoint)
		if err != nil {
			return err
		}

		auxBucket, err := tx.CreateTopLevelBucket(
			auxRetributionBucket,
		)
		if err != nil {
			return err
		}
		chanBucket, err := auxBucket.CreateBucketIfNotExists(
			chanBuf.Bytes(),
		)
		if err != nil {
			return err
		}
		justiceBucket, err := chanBucket.CreateBucketIfNotExists(
			auxBreachJusticeBucket,
		)
		if err != nil {
			return err
		}

		for txid, jtx := range justiceTxs {
			var b bytes.Buffer
			if err := encodeJusticeTxCtx(&b, jtx); err != nil {
				return err
			}

			err := justiceBucket.Put(txid[:], b.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}
