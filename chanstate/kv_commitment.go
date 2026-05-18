package chanstate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
)

// serializeHtlcExtraData encodes a TLV stream of extra data to be stored with a
// HTLC. It uses the update_add_htlc TLV types, because this is where extra
// data is passed with a HTLC. At present blinding points are the only extra
// data that we will store, and the function is a no-op if a nil blinding
// point is provided.
//
// This function MUST be called to persist all HTLC values when they are
// serialized.
func serializeHtlcExtraData(h *HTLC) error {
	var records []tlv.RecordProducer
	h.BlindingPoint.WhenSome(func(b tlv.RecordT[lnwire.BlindingPointTlvType,
		*btcec.PublicKey]) {

		records = append(records, &b)
	})

	records, err := h.CustomRecords.ExtendRecordProducers(records)
	if err != nil {
		return err
	}

	return h.ExtraData.PackRecords(records...)
}

// deserializeHtlcExtraData extracts TLVs from the extra data persisted for the
// HTLC and populates values in the struct accordingly.
//
// This function MUST be called to populate the struct properly when HTLCs
// are deserialized.
func deserializeHtlcExtraData(h *HTLC) error {
	if len(h.ExtraData) == 0 {
		return nil
	}

	blindingPoint := h.BlindingPoint.Zero()
	tlvMap, err := h.ExtraData.ExtractRecords(&blindingPoint)
	if err != nil {
		return err
	}

	if val, ok := tlvMap[h.BlindingPoint.TlvType()]; ok && val == nil {
		h.BlindingPoint = tlv.SomeRecordT(blindingPoint)

		// Remove the entry from the TLV map. Anything left in the map
		// will be included in the custom records field.
		delete(tlvMap, h.BlindingPoint.TlvType())
	}

	// Set the custom records field to the remaining TLV records.
	customRecords, err := lnwire.NewCustomRecords(tlvMap)
	if err != nil {
		return err
	}
	h.CustomRecords = customRecords

	return nil
}

// SerializeHtlcs writes out the passed set of HTLC's into the passed writer
// using the current default on-disk serialization format.
//
// This inline serialization has been extended to allow storage of extra data
// associated with a HTLC in the following way:
//   - The known-length onion blob (1366 bytes) is serialized as var bytes in
//     WriteElements (ie, the length 1366 was written, followed by the 1366
//     onion bytes).
//   - To include extra data, we append any extra data present to this one
//     variable length of data. Since we know that the onion is strictly 1366
//     bytes, any length after that should be considered to be extra data.
//
// NOTE: This API is NOT stable, the on-disk format will likely change in the
// future.
func SerializeHtlcs(b io.Writer, htlcs ...HTLC) error {
	numHtlcs := uint16(len(htlcs))
	if err := WriteElement(b, numHtlcs); err != nil {
		return err
	}

	for _, htlc := range htlcs {
		// Populate TLV stream for any additional fields contained
		// in the TLV.
		if err := serializeHtlcExtraData(&htlc); err != nil {
			return err
		}

		// The onion blob and hltc data are stored as a single var
		// bytes blob.
		onionAndExtraData := make(
			[]byte, lnwire.OnionPacketSize+len(htlc.ExtraData),
		)
		copy(onionAndExtraData, htlc.OnionBlob[:])
		copy(onionAndExtraData[lnwire.OnionPacketSize:], htlc.ExtraData)

		if err := WriteElements(b,
			//nolint:ll
			htlc.Signature, htlc.RHash, htlc.Amt, htlc.RefundTimeout,
			htlc.OutputIndex, htlc.Incoming, onionAndExtraData,
			htlc.HtlcIndex, htlc.LogIndex,
		); err != nil {
			return err
		}
	}

	return nil
}

// DeserializeHtlcs attempts to read out a slice of HTLC's from the passed
// io.Reader. The bytes within the passed reader MUST have been previously
// written to using the SerializeHtlcs function.
//
// This inline deserialization has been extended to allow storage of extra data
// associated with a HTLC in the following way:
//   - The known-length onion blob (1366 bytes) and any additional data present
//     are read out as a single blob of variable byte data.
//   - They are stored like this to take advantage of the variable space
//     available for extension without migration (see SerializeHtlcs).
//   - The first 1366 bytes are interpreted as the onion blob, and any remaining
//     bytes as extra HTLC data.
//   - This extra HTLC data is expected to be serialized as a TLV stream, and
//     its parsing is left to higher layers.
//
// NOTE: This API is NOT stable, the on-disk format will likely change in the
// future.
func DeserializeHtlcs(r io.Reader) ([]HTLC, error) {
	var numHtlcs uint16
	if err := ReadElement(r, &numHtlcs); err != nil {
		return nil, err
	}

	var htlcs []HTLC
	if numHtlcs == 0 {
		return htlcs, nil
	}

	htlcs = make([]HTLC, numHtlcs)
	for i := uint16(0); i < numHtlcs; i++ {
		var onionAndExtraData []byte
		if err := ReadElements(r,
			&htlcs[i].Signature, &htlcs[i].RHash, &htlcs[i].Amt,
			&htlcs[i].RefundTimeout, &htlcs[i].OutputIndex,
			&htlcs[i].Incoming, &onionAndExtraData,
			&htlcs[i].HtlcIndex, &htlcs[i].LogIndex,
		); err != nil {
			return htlcs, err
		}

		// Sanity check that we have at least the onion blob size we
		// expect.
		if len(onionAndExtraData) < lnwire.OnionPacketSize {
			return nil, ErrOnionBlobLength
		}

		// First OnionPacketSize bytes are our fixed length onion
		// packet.
		copy(
			htlcs[i].OnionBlob[:],
			onionAndExtraData[0:lnwire.OnionPacketSize],
		)

		// Any additional bytes belong to extra data. ExtraDataLen
		// will be >= 0, because we know that we always have a fixed
		// length onion packet.
		extraDataLen := len(onionAndExtraData) - lnwire.OnionPacketSize
		if extraDataLen > 0 {
			htlcs[i].ExtraData = make([]byte, extraDataLen)

			copy(
				htlcs[i].ExtraData,
				onionAndExtraData[lnwire.OnionPacketSize:],
			)
		}

		// Finally, deserialize any TLVs contained in that extra data
		// if they are present.
		if err := deserializeHtlcExtraData(&htlcs[i]); err != nil {
			return nil, err
		}
	}

	return htlcs, nil
}

// SerializeChanCommit serializes the channel commitment.
func SerializeChanCommit(w io.Writer, c *ChannelCommitment) error {
	if err := WriteElements(w,
		c.CommitHeight, c.LocalLogIndex, c.LocalHtlcIndex,
		c.RemoteLogIndex, c.RemoteHtlcIndex, c.LocalBalance,
		c.RemoteBalance, c.CommitFee, c.FeePerKw, c.CommitTx,
		c.CommitSig,
	); err != nil {
		return err
	}

	return SerializeHtlcs(w, c.Htlcs...)
}

// DeserializeChanCommit deserializes the channel commitment.
func DeserializeChanCommit(r io.Reader) (ChannelCommitment, error) {
	var c ChannelCommitment

	err := ReadElements(r,
		&c.CommitHeight, &c.LocalLogIndex, &c.LocalHtlcIndex,
		&c.RemoteLogIndex, &c.RemoteHtlcIndex, &c.LocalBalance,
		&c.RemoteBalance, &c.CommitFee, &c.FeePerKw, &c.CommitTx,
		&c.CommitSig,
	)
	if err != nil {
		return c, err
	}

	c.Htlcs, err = DeserializeHtlcs(r)
	if err != nil {
		return c, err
	}

	return c, nil
}

func chanCommitKey(local bool) []byte {
	commitKey := make([]byte, 0, len(chanCommitmentKey)+1)
	commitKey = append(commitKey, chanCommitmentKey...)
	if local {
		return append(commitKey, byte(0x00))
	}

	return append(commitKey, byte(0x01))
}

// PutChanCommitment writes a channel commitment to the channel bucket.
func PutChanCommitment(chanBucket kvdb.RwBucket, c *ChannelCommitment,
	local bool) error {

	var b bytes.Buffer
	if err := SerializeChanCommit(&b, c); err != nil {
		return err
	}

	// Before we write to disk, we'll also write our aux data as well.
	if err := EncodeCommitTlvData(&b, c); err != nil {
		return fmt.Errorf("unable to write aux data: %w", err)
	}

	return chanBucket.Put(chanCommitKey(local), b.Bytes())
}

// PutChanCommitments writes the local and remote commitments to the channel
// bucket.
func PutChanCommitments(chanBucket kvdb.RwBucket,
	channel *OpenChannel) error {

	// If this is a restored channel, then we don't have any commitments to
	// write.
	if channel.HasChanStatusForStore(ChanStatusRestored) {
		return nil
	}

	err := PutChanCommitment(
		chanBucket, &channel.LocalCommitment, true,
	)
	if err != nil {
		return err
	}

	return PutChanCommitment(
		chanBucket, &channel.RemoteCommitment, false,
	)
}

// PutChanRevocationState writes the remote revocation state to the channel
// bucket.
func PutChanRevocationState(chanBucket kvdb.RwBucket,
	channel *OpenChannel) error {

	var b bytes.Buffer
	err := WriteElements(
		&b, channel.RemoteCurrentRevocation, channel.RevocationProducer,
		channel.RevocationStore,
	)
	if err != nil {
		return err
	}

	// If the next revocation is present, which is only the case after the
	// ChannelReady message has been sent, then we'll write it to disk.
	if channel.RemoteNextRevocation != nil {
		err = WriteElements(&b, channel.RemoteNextRevocation)
		if err != nil {
			return err
		}
	}

	return chanBucket.Put(revocationStateKey, b.Bytes())
}

// FetchChanCommitment reads a channel commitment from the channel bucket.
func FetchChanCommitment(chanBucket kvdb.RBucket,
	local bool) (ChannelCommitment, error) {

	commitBytes := chanBucket.Get(chanCommitKey(local))
	if commitBytes == nil {
		return ChannelCommitment{}, ErrNoCommitmentsFound
	}

	r := bytes.NewReader(commitBytes)
	chanCommit, err := DeserializeChanCommit(r)
	if err != nil {
		return ChannelCommitment{}, fmt.Errorf("unable to decode "+
			"chan commit: %w", err)
	}

	// We'll also check to see if we have any aux data stored as the end of
	// the stream.
	if err := DecodeCommitTlvData(r, &chanCommit); err != nil {
		return ChannelCommitment{}, fmt.Errorf("unable to decode "+
			"chan aux data: %w", err)
	}

	return chanCommit, nil
}

// FetchChanCommitments reads the local and remote commitments from the channel
// bucket.
func FetchChanCommitments(chanBucket kvdb.RBucket,
	channel *OpenChannel) error {

	var err error

	// If this is a restored channel, then we don't have any commitments to
	// read.
	if channel.HasChanStatusForStore(ChanStatusRestored) {
		return nil
	}

	channel.LocalCommitment, err = FetchChanCommitment(chanBucket, true)
	if err != nil {
		return err
	}
	channel.RemoteCommitment, err = FetchChanCommitment(chanBucket, false)
	if err != nil {
		return err
	}

	return nil
}

// FetchChanRevocationState reads the remote revocation state from the channel
// bucket.
func FetchChanRevocationState(chanBucket kvdb.RBucket,
	channel *OpenChannel) error {

	revBytes := chanBucket.Get(revocationStateKey)
	if revBytes == nil {
		return ErrNoRevocationsFound
	}
	r := bytes.NewReader(revBytes)

	err := ReadElements(
		r,
		&channel.RemoteCurrentRevocation, &channel.RevocationProducer,
		&channel.RevocationStore,
	)
	if err != nil {
		return err
	}

	// If there aren't any bytes left in the buffer, then we don't yet have
	// the next remote revocation, so we can exit early here.
	if r.Len() == 0 {
		return nil
	}

	// Otherwise we'll read the next revocation for the remote party which
	// is always the last item within the buffer.
	return ReadElements(r, &channel.RemoteNextRevocation)
}

// DeleteOpenChannel deletes the persisted open channel state from the channel
// bucket.
func DeleteOpenChannel(chanBucket kvdb.RwBucket) error {
	if err := chanBucket.Delete(chanInfoKey); err != nil {
		return err
	}

	err := chanBucket.Delete(chanCommitKey(true))
	if err != nil {
		return err
	}
	err = chanBucket.Delete(chanCommitKey(false))
	if err != nil {
		return err
	}

	if err := chanBucket.Delete(revocationStateKey); err != nil {
		return err
	}

	if diff := chanBucket.Get(commitDiffKey); diff != nil {
		return chanBucket.Delete(commitDiffKey)
	}

	return nil
}

// RemoteCommitChainTip returns the "tip" of the current remote commitment
// chain.
func RemoteCommitChainTip(backend kvdb.Backend,
	channel *OpenChannel) (*CommitDiff, error) {

	var cd *CommitDiff
	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return ErrNoPendingCommit
		default:
			return err
		}

		tipBytes := chanBucket.Get(commitDiffKey)
		if tipBytes == nil {
			return ErrNoPendingCommit
		}

		tipReader := bytes.NewReader(tipBytes)
		dcd, err := DeserializeCommitDiff(tipReader)
		if err != nil {
			return err
		}

		cd = dcd

		return nil
	}, func() {
		cd = nil
	})
	if err != nil {
		return nil, err
	}

	return cd, nil
}

// UnsignedAckedUpdates retrieves the persisted unsigned acked remote log
// updates that still need to be signed for.
func UnsignedAckedUpdates(backend kvdb.Backend,
	channel *OpenChannel) ([]LogUpdate, error) {

	var updates []LogUpdate
	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil
		default:
			return err
		}

		updateBytes := chanBucket.Get(unsignedAckedUpdatesKey)
		if updateBytes == nil {
			return nil
		}

		r := bytes.NewReader(updateBytes)
		updates, err = DeserializeLogUpdates(r)

		return err
	}, func() {
		updates = nil
	})
	if err != nil {
		return nil, err
	}

	return updates, nil
}

// RemoteUnsignedLocalUpdates retrieves the persisted, unsigned local log
// updates that the remote still needs to sign for.
func RemoteUnsignedLocalUpdates(backend kvdb.Backend,
	channel *OpenChannel) ([]LogUpdate, error) {

	var updates []LogUpdate
	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil
		default:
			return err
		}

		updateBytes := chanBucket.Get(remoteUnsignedLocalUpdatesKey)
		if updateBytes == nil {
			return nil
		}

		r := bytes.NewReader(updateBytes)
		updates, err = DeserializeLogUpdates(r)

		return err
	}, func() {
		updates = nil
	})
	if err != nil {
		return nil, err
	}

	return updates, nil
}

// InsertNextRevocation inserts the next commitment point into the persisted
// channel state.
func InsertNextRevocation(backend kvdb.Backend, channel *OpenChannel,
	revKey *btcec.PublicKey) error {

	channel.RemoteNextRevocation = revKey

	err := kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chanBucket, err := FetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return PutChanRevocationState(chanBucket, channel)
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}

// UpdateChannelCommitment updates the local commitment state.
func UpdateChannelCommitment(backend kvdb.Backend, channel *OpenChannel,
	newCommitment *ChannelCommitment,
	unsignedAckedUpdates []LogUpdate, storeFinalHtlcResolutions bool) (
	map[uint64]bool, error) {

	var finalHtlcs = make(map[uint64]bool)

	err := kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chanBucket, err := FetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// If the channel is marked as borked, then for safety reasons,
		// we shouldn't attempt any further updates.
		isBorked, err := IsChannelBorked(channel, chanBucket)
		if err != nil {
			return err
		}
		if isBorked {
			return ErrChanBorked
		}

		if err = PutChanInfo(chanBucket, channel); err != nil {
			return fmt.Errorf("unable to store chan info: %w", err)
		}

		// With the proper bucket fetched, we'll now write the latest
		// commitment state to disk for the target party.
		err = PutChanCommitment(
			chanBucket, newCommitment, true,
		)
		if err != nil {
			return fmt.Errorf("unable to store chan "+
				"revocations: %v", err)
		}

		// Persist unsigned but acked remote updates that need to be
		// restored after a restart.
		var b bytes.Buffer
		err = SerializeLogUpdates(&b, unsignedAckedUpdates)
		if err != nil {
			return err
		}

		err = chanBucket.Put(unsignedAckedUpdatesKey, b.Bytes())
		if err != nil {
			return fmt.Errorf("unable to store dangline remote "+
				"updates: %v", err)
		}

		//nolint:ll
		// Since we have just sent the counterparty a revocation, store true
		// under lastWasRevokeKey.
		var b2 bytes.Buffer
		if err := WriteElements(&b2, true); err != nil {
			return err
		}

		err = chanBucket.Put(lastWasRevokeKey, b2.Bytes())
		if err != nil {
			return err
		}

		//nolint:ll
		// Persist the remote unsigned local updates that are not included
		// in our new commitment.
		updateBytes := chanBucket.Get(remoteUnsignedLocalUpdatesKey)
		if updateBytes == nil {
			return nil
		}

		r := bytes.NewReader(updateBytes)
		updates, err := DeserializeLogUpdates(r)
		if err != nil {
			return err
		}

		// Get the bucket where settled htlcs are recorded if the user
		// opted in to storing this information.
		var finalHtlcsBucket kvdb.RwBucket
		if storeFinalHtlcResolutions {
			bucket, err := FetchFinalHtlcsBucketRw(
				tx, channel.ShortChannelID,
			)
			if err != nil {
				return err
			}

			finalHtlcsBucket = bucket
		}

		var unsignedUpdates []LogUpdate
		for _, upd := range updates {
			// Gather updates that are not on our local commitment.
			if upd.LogIndex >= newCommitment.LocalLogIndex {
				unsignedUpdates = append(unsignedUpdates, upd)

				continue
			}

			// The update was locked in. If the update was a
			// resolution, then store it in the database.
			err := ProcessFinalHtlc(
				finalHtlcsBucket, upd, finalHtlcs,
			)
			if err != nil {
				return err
			}
		}

		var b3 bytes.Buffer
		err = SerializeLogUpdates(&b3, unsignedUpdates)
		if err != nil {
			return fmt.Errorf("unable to serialize log updates: %w",
				err)
		}

		err = chanBucket.Put(remoteUnsignedLocalUpdatesKey, b3.Bytes())
		if err != nil {
			return fmt.Errorf("unable to restore chanbucket: %w",
				err)
		}

		return nil
	}, func() {
		finalHtlcs = make(map[uint64]bool)
	})
	if err != nil {
		return nil, err
	}

	return finalHtlcs, nil
}

// AppendRemoteCommitChain appends a new CommitDiff to the remote party's
// commitment chain.
func AppendRemoteCommitChain(backend kvdb.Backend, channel *OpenChannel,
	diff *CommitDiff) error {

	return kvdb.Update(backend, func(tx kvdb.RwTx) error {
		// First, we'll grab the writable bucket where this channel's
		// data resides.
		chanBucket, err := FetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// If the channel is marked as borked, then for safety reasons,
		// we shouldn't attempt any further updates.
		isBorked, err := IsChannelBorked(channel, chanBucket)
		if err != nil {
			return err
		}
		if isBorked {
			return ErrChanBorked
		}

		// Any outgoing settles and fails necessarily have a
		// corresponding adds in this channel's forwarding packages.
		// Mark all of these as being fully processed in our forwarding
		// package, which prevents us from reprocessing them after
		// startup.
		packager := NewChannelPackager(channel.ShortChannelID)

		err = packager.AckAddHtlcs(tx, diff.AddAcks...)
		if err != nil {
			return err
		}

		// Additionally, we ack from any fails or settles that are
		// persisted in another channel's forwarding package. This
		// prevents the same fails and settles from being retransmitted
		// after restarts. The actual fail or settle we need to
		// propagate to the remote party is now in the commit diff.
		err = packager.AckSettleFails(
			tx, diff.SettleFailAcks...,
		)
		if err != nil {
			return err
		}

		//nolint:ll
		// We are sending a commitment signature so lastWasRevokeKey should
		// store false.
		var b bytes.Buffer
		if err := WriteElements(&b, false); err != nil {
			return err
		}
		err = chanBucket.Put(lastWasRevokeKey, b.Bytes())
		if err != nil {
			return err
		}

		// TODO(roasbeef): use seqno to derive key for later LCP

		// With the bucket retrieved, we'll now serialize the commit
		// diff itself, and write it to disk.
		var b2 bytes.Buffer
		if err := SerializeCommitDiff(&b2, diff); err != nil {
			return err
		}

		return chanBucket.Put(commitDiffKey, b2.Bytes())
	}, func() {})
}

// AdvanceCommitChainTail records the new state transition within the
// revocation log and promotes the pending remote commitment to the current
// remote commitment.
func AdvanceCommitChainTail(backend kvdb.Backend, channel *OpenChannel,
	fwdPkg *FwdPkg, updates []LogUpdate, ourOutputIndex,
	theirOutputIndex uint32, noRevLogAmtData bool) error {

	var newRemoteCommit *ChannelCommitment

	err := kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chanBucket, err := FetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// If the channel is marked as borked, then for safety reasons,
		// we shouldn't attempt any further updates.
		isBorked, err := IsChannelBorked(channel, chanBucket)
		if err != nil {
			return err
		}
		if isBorked {
			return ErrChanBorked
		}

		// Persist the latest preimage state to disk as the remote peer
		// has just added to our local preimage store, and given us a
		// new pending revocation key.
		err = PutChanRevocationState(chanBucket, channel)
		if err != nil {
			return err
		}

		// With the current preimage producer/store state updated,
		// append a new log entry recording this the delta of this
		// state transition.
		//
		// TODO(roasbeef): could make the deltas relative, would save
		// space, but then tradeoff for more disk-seeks to recover the
		// full state.
		logKey := revocationLogBucket
		logBucket, err := chanBucket.CreateBucketIfNotExists(logKey)
		if err != nil {
			return err
		}

		// Before we append this revoked state to the revocation log,
		// we'll swap out what's currently the tail of the commit tip,
		// with the current locked-in commitment for the remote party.
		tipBytes := chanBucket.Get(commitDiffKey)
		tipReader := bytes.NewReader(tipBytes)
		newCommit, err := DeserializeCommitDiff(tipReader)
		if err != nil {
			return err
		}
		err = PutChanCommitment(
			chanBucket, &newCommit.Commitment, false,
		)
		if err != nil {
			return err
		}
		if err := chanBucket.Delete(commitDiffKey); err != nil {
			return err
		}

		// With the commitment pointer swapped, we can now add the
		// revoked (prior) state to the revocation log.
		err = PutRevocationLog(
			logBucket, &channel.RemoteCommitment, ourOutputIndex,
			theirOutputIndex, noRevLogAmtData,
		)
		if err != nil {
			return err
		}

		// Lastly, we write the forwarding package to disk so that we
		// can properly recover from failures and reforward HTLCs that
		// have not received a corresponding settle/fail.
		err = NewChannelPackager(channel.ShortChannelID).AddFwdPkg(
			tx, fwdPkg,
		)
		if err != nil {
			return err
		}

		// Persist the unsigned acked updates that are not included
		// in their new commitment.
		updateBytes := chanBucket.Get(unsignedAckedUpdatesKey)
		if updateBytes == nil {
			// This shouldn't normally happen as we always store
			// the number of updates, but could still be
			// encountered by nodes that are upgrading.
			newRemoteCommit = &newCommit.Commitment
			return nil
		}

		r := bytes.NewReader(updateBytes)
		unsignedUpdates, err := DeserializeLogUpdates(r)
		if err != nil {
			return err
		}

		var validUpdates []LogUpdate
		for _, upd := range unsignedUpdates {
			lIdx := upd.LogIndex

			// Filter for updates that are not on the remote
			// commitment.
			if lIdx >= newCommit.Commitment.RemoteLogIndex {
				validUpdates = append(validUpdates, upd)
			}
		}

		var b bytes.Buffer
		err = SerializeLogUpdates(&b, validUpdates)
		if err != nil {
			return fmt.Errorf("unable to serialize log updates: %w",
				err)
		}

		err = chanBucket.Put(unsignedAckedUpdatesKey, b.Bytes())
		if err != nil {
			return fmt.Errorf("unable to store under "+
				"unsignedAckedUpdatesKey: %w", err)
		}

		// Persist the local updates the peer hasn't yet signed so they
		// can be restored after restart.
		var b2 bytes.Buffer
		err = SerializeLogUpdates(&b2, updates)
		if err != nil {
			return err
		}

		err = chanBucket.Put(remoteUnsignedLocalUpdatesKey, b2.Bytes())
		if err != nil {
			return fmt.Errorf("unable to restore remote unsigned "+
				"local updates: %v", err)
		}

		newRemoteCommit = &newCommit.Commitment

		return nil
	}, func() {
		newRemoteCommit = nil
	})
	if err != nil {
		return err
	}

	// With the db transaction complete, we'll swap over the in-memory
	// pointer of the new remote commitment, which was previously the tip
	// of the commit chain.
	channel.RemoteCommitment = *newRemoteCommit

	return nil
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to date.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to date.
func CommitmentHeight(backend kvdb.Backend, channel *OpenChannel) (
	uint64, error) {

	var height uint64
	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		commit, err := FetchChanCommitment(chanBucket, true)
		if err != nil {
			return err
		}

		height = commit.CommitHeight

		return nil
	}, func() {
		height = 0
	})
	if err != nil {
		return 0, err
	}

	return height, nil
}

// LatestCommitments returns the two latest commitments for both the local and
// remote party. These commitments are read from disk to ensure that only the
// latest fully committed state is returned. The first commitment returned is
// the local commitment, and the second returned is the remote commitment.
func LatestCommitments(backend kvdb.Backend, channel *OpenChannel) (
	*ChannelCommitment, *ChannelCommitment, error) {

	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return FetchChanCommitments(chanBucket, channel)
	}, func() {})
	if err != nil {
		return nil, nil, err
	}

	return &channel.LocalCommitment, &channel.RemoteCommitment, nil
}

// RemoteRevocationStore returns the most up to date commitment version of the
// revocation storage tree for the remote party. This method can be used when
// acting on a possible contract breach to ensure, that the caller has the most
// up to date information required to deliver justice.
func RemoteRevocationStore(backend kvdb.Backend,
	channel *OpenChannel) (shachain.Store, error) {

	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return FetchChanRevocationState(chanBucket, channel)
	}, func() {})
	if err != nil {
		return nil, err
	}

	return channel.RevocationStore, nil
}

// commitTlvData stores all the optional data that may be stored as a TLV stream
// at the _end_ of the normal serialized commit on disk.
type commitTlvData struct {
	// customBlob is a custom blob that may store extra data for custom
	// channels.
	customBlob tlv.OptionalRecordT[tlv.TlvType1, tlv.Blob]
}

// encode encodes the aux data into the passed io.Writer.
func (c *commitTlvData) encode(w io.Writer) error {
	var tlvRecords []tlv.Record
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType1, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode attempts to decode the aux data from the passed io.Reader.
func (c *commitTlvData) decode(r io.Reader) error {
	blob := c.customBlob.Zero()

	tlvStream, err := tlv.NewStream(
		blob.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}

	return nil
}

// DecodeCommitTlvData decodes and applies auxiliary TLV data to a commitment.
func DecodeCommitTlvData(r io.Reader, c *ChannelCommitment) error {
	var auxData commitTlvData
	if err := auxData.decode(r); err != nil {
		return err
	}

	amendCommitTlvData(c, auxData)

	return nil
}

// EncodeCommitTlvData extracts and encodes auxiliary TLV data from a
// commitment.
func EncodeCommitTlvData(w io.Writer, c *ChannelCommitment) error {
	auxData := extractCommitTlvData(c)
	return auxData.encode(w)
}

// amendCommitTlvData updates the commitment with the given auxiliary TLV data.
func amendCommitTlvData(c *ChannelCommitment, auxData commitTlvData) {
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		c.CustomBlob = fn.Some(blob)
	})
}

// extractCommitTlvData creates a new commitTlvData from the given commitment.
func extractCommitTlvData(c *ChannelCommitment) commitTlvData {
	var auxData commitTlvData

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType1](blob),
		)
	})

	return auxData
}

// SerializeLogUpdates serializes provided list of updates to a stream.
func SerializeLogUpdates(w io.Writer, logUpdates []LogUpdate) error {
	numUpdates := uint16(len(logUpdates))
	if err := binary.Write(w, byteOrder, numUpdates); err != nil {
		return err
	}

	for _, diff := range logUpdates {
		err := WriteElements(w, diff.LogIndex, diff.UpdateMsg)
		if err != nil {
			return err
		}
	}

	return nil
}

// DeserializeLogUpdates deserializes a list of updates from a stream.
func DeserializeLogUpdates(r io.Reader) ([]LogUpdate, error) {
	var numUpdates uint16
	if err := binary.Read(r, byteOrder, &numUpdates); err != nil {
		return nil, err
	}

	logUpdates := make([]LogUpdate, numUpdates)
	for i := 0; i < int(numUpdates); i++ {
		err := ReadElements(r,
			&logUpdates[i].LogIndex, &logUpdates[i].UpdateMsg,
		)
		if err != nil {
			return nil, err
		}
	}

	return logUpdates, nil
}

// SerializeCommitDiff serializes the commit diff.
func SerializeCommitDiff(w io.Writer, diff *CommitDiff) error {
	if err := SerializeChanCommit(w, &diff.Commitment); err != nil {
		return err
	}

	if err := WriteElements(w, diff.CommitSig); err != nil {
		return err
	}

	if err := SerializeLogUpdates(w, diff.LogUpdates); err != nil {
		return err
	}

	numOpenRefs := uint16(len(diff.OpenedCircuitKeys))
	if err := binary.Write(w, byteOrder, numOpenRefs); err != nil {
		return err
	}

	for _, openRef := range diff.OpenedCircuitKeys {
		err := WriteElements(w, openRef.ChanID, openRef.HtlcID)
		if err != nil {
			return err
		}
	}

	numClosedRefs := uint16(len(diff.ClosedCircuitKeys))
	if err := binary.Write(w, byteOrder, numClosedRefs); err != nil {
		return err
	}

	for _, closedRef := range diff.ClosedCircuitKeys {
		err := WriteElements(w, closedRef.ChanID, closedRef.HtlcID)
		if err != nil {
			return err
		}
	}

	// We'll also encode the commit aux data stream here. We do this here
	// rather than above (at the call to serializeChanCommit), to ensure
	// backwards compat for reads to existing non-custom channels.
	if err := EncodeCommitTlvData(w, &diff.Commitment); err != nil {
		return fmt.Errorf("unable to write aux data: %w", err)
	}

	return nil
}

// DeserializeCommitDiff deserializes the commit diff.
func DeserializeCommitDiff(r io.Reader) (*CommitDiff, error) {
	var (
		d   CommitDiff
		err error
	)

	d.Commitment, err = DeserializeChanCommit(r)
	if err != nil {
		return nil, err
	}

	var msg lnwire.Message
	if err := ReadElements(r, &msg); err != nil {
		return nil, err
	}
	commitSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		return nil, fmt.Errorf("expected lnwire.CommitSig, instead "+
			"read: %T", msg)
	}
	d.CommitSig = commitSig

	d.LogUpdates, err = DeserializeLogUpdates(r)
	if err != nil {
		return nil, err
	}

	var numOpenRefs uint16
	if err := binary.Read(r, byteOrder, &numOpenRefs); err != nil {
		return nil, err
	}

	d.OpenedCircuitKeys = make([]models.CircuitKey, numOpenRefs)
	for i := 0; i < int(numOpenRefs); i++ {
		err := ReadElements(r,
			&d.OpenedCircuitKeys[i].ChanID,
			&d.OpenedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	var numClosedRefs uint16
	if err := binary.Read(r, byteOrder, &numClosedRefs); err != nil {
		return nil, err
	}

	d.ClosedCircuitKeys = make([]models.CircuitKey, numClosedRefs)
	for i := 0; i < int(numClosedRefs); i++ {
		err := ReadElements(r,
			&d.ClosedCircuitKeys[i].ChanID,
			&d.ClosedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	// As a final step, we'll read out any aux commit data that we have at
	// the end of this byte stream. We do this here to ensure backward
	// compatibility, as otherwise we risk erroneously reading into the
	// wrong field.
	if err := DecodeCommitTlvData(r, &d.Commitment); err != nil {
		return nil, fmt.Errorf("unable to decode aux data: %w", err)
	}

	return &d, nil
}
