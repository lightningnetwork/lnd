package chanstate

import (
	"math"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OutputIndexEmpty is used when the output index doesn't exist.
	OutputIndexEmpty = math.MaxUint16
)

type (
	// BigSizeAmount is a type alias for a TLV record of a btcutil.Amount.
	BigSizeAmount = tlv.BigSizeT[btcutil.Amount]

	// BigSizeMilliSatoshi is a type alias for a TLV record of a
	// lnwire.MilliSatoshi.
	BigSizeMilliSatoshi = tlv.BigSizeT[lnwire.MilliSatoshi]
)

// SparsePayHash is a type alias for a 32 byte array, which when serialized is
// able to save some space by not including an empty payment hash on disk.
type SparsePayHash [32]byte

// NewSparsePayHash creates a new SparsePayHash from a 32 byte array.
func NewSparsePayHash(rHash [32]byte) SparsePayHash {
	return SparsePayHash(rHash)
}

// HTLCEntry specifies the minimal info needed to be stored on disk for ALL the
// historical HTLCs, which is useful for constructing RevocationLog when a
// breach is detected.
// The actual size of each HTLCEntry varies based on its RHash and Amt(sat),
// summarized as follows,
//
//	| RHash | Amt<=252 | Amt<=65,535 | Amt<=4,294,967,295 | otherwise |
//	|:-----:|:--------:|:-----------:|:------------------:|:---------:|
//	| true  |    19    |      21     |         23         |     26    |
//	| false |    51    |      53     |         55         |     58    |
//
// So the size varies from 19 bytes to 58 bytes, where most likely to be 23 or
// 55 bytes.
//
// NOTE: all the fields saved to disk use the primitive go types so they can be
// made into tlv records without further conversion.
type HTLCEntry struct {
	// RHash is the payment hash of the HTLC.
	RHash tlv.RecordT[tlv.TlvType0, SparsePayHash]

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout tlv.RecordT[tlv.TlvType1, uint32]

	// OutputIndex is the output index for this particular HTLC output
	// within the commitment transaction.
	//
	// NOTE: we use uint16 instead of int32 here to save us 2 bytes, which
	// gives us a max number of HTLCs of 65K.
	OutputIndex tlv.RecordT[tlv.TlvType2, uint16]

	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	Incoming tlv.RecordT[tlv.TlvType3, bool]

	// Amt is the amount of satoshis this HTLC escrows.
	Amt tlv.RecordT[tlv.TlvType4, tlv.BigSizeT[btcutil.Amount]]

	// CustomBlob is an optional blob that can be used to store information
	// specific to revocation handling for a custom channel type.
	CustomBlob tlv.OptionalRecordT[tlv.TlvType5, tlv.Blob]

	// HtlcIndex is the index of the HTLC in the channel.
	HtlcIndex tlv.OptionalRecordT[tlv.TlvType6, tlv.BigSizeT[uint64]]
}

// NewHTLCEntryFromHTLC creates a new HTLCEntry from an HTLC.
func NewHTLCEntryFromHTLC(htlc HTLC) (*HTLCEntry, error) {
	h := &HTLCEntry{
		RHash: tlv.NewRecordT[tlv.TlvType0](
			NewSparsePayHash(htlc.RHash),
		),
		RefundTimeout: tlv.NewPrimitiveRecord[tlv.TlvType1](
			htlc.RefundTimeout,
		),
		OutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType2](
			uint16(htlc.OutputIndex),
		),
		Incoming: tlv.NewPrimitiveRecord[tlv.TlvType3](htlc.Incoming),
		Amt: tlv.NewRecordT[tlv.TlvType4](
			tlv.NewBigSizeT(htlc.Amt.ToSatoshis()),
		),
		HtlcIndex: tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType6](
			tlv.NewBigSizeT(htlc.HtlcIndex),
		)),
	}

	if len(htlc.CustomRecords) != 0 {
		blob, err := htlc.CustomRecords.Serialize()
		if err != nil {
			return nil, err
		}

		h.CustomBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5, tlv.Blob](blob),
		)
	}

	return h, nil
}

// RevocationLog stores the info needed to construct a breach retribution. Its
// fields can be viewed as a subset of a ChannelCommitment's. In the database,
// all historical versions of the RevocationLog are saved using the
// CommitHeight as the key.
type RevocationLog struct {
	// OurOutputIndex specifies our output index in this commitment. In a
	// remote commitment transaction, this is the to remote output index.
	OurOutputIndex tlv.RecordT[tlv.TlvType0, uint16]

	// TheirOutputIndex specifies their output index in this commitment. In
	// a remote commitment transaction, this is the to local output index.
	TheirOutputIndex tlv.RecordT[tlv.TlvType1, uint16]

	// CommitTxHash is the hash of the latest version of the commitment
	// state, broadcast able by us.
	CommitTxHash tlv.RecordT[tlv.TlvType2, [32]byte]

	// HTLCEntries is the set of HTLCEntry's that are pending at this
	// particular commitment height.
	HTLCEntries []*HTLCEntry

	// OurBalance is the current available balance within the channel
	// directly spendable by us. In other words, it is the value of the
	// to_remote output on the remote parties' commitment transaction.
	//
	// NOTE: this is an option so that it is clear if the value is zero or
	// nil. Since migration 30 of the channeldb initially did not include
	// this field, it could be the case that the field is not present for
	// all revocation logs.
	OurBalance tlv.OptionalRecordT[tlv.TlvType3, BigSizeMilliSatoshi]

	// TheirBalance is the current available balance within the channel
	// directly spendable by the remote node. In other words, it is the
	// value of the to_local output on the remote parties' commitment.
	//
	// NOTE: this is an option so that it is clear if the value is zero or
	// nil. Since migration 30 of the channeldb initially did not include
	// this field, it could be the case that the field is not present for
	// all revocation logs.
	TheirBalance tlv.OptionalRecordT[tlv.TlvType4, BigSizeMilliSatoshi]

	// CustomBlob is an optional blob that can be used to store information
	// specific to a custom channel type. This information is only created
	// at channel funding time, and after wards is to be considered
	// immutable.
	CustomBlob tlv.OptionalRecordT[tlv.TlvType5, tlv.Blob]
}

// NewRevocationLog creates a new RevocationLog from the given parameters.
func NewRevocationLog(ourOutputIndex uint16, theirOutputIndex uint16,
	commitHash [32]byte, ourBalance,
	theirBalance fn.Option[lnwire.MilliSatoshi], htlcs []*HTLCEntry,
	customBlob fn.Option[tlv.Blob]) RevocationLog {

	rl := RevocationLog{
		OurOutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType0](
			ourOutputIndex,
		),
		TheirOutputIndex: tlv.NewPrimitiveRecord[tlv.TlvType1](
			theirOutputIndex,
		),
		CommitTxHash: tlv.NewPrimitiveRecord[tlv.TlvType2](commitHash),
		HTLCEntries:  htlcs,
	}

	ourBalance.WhenSome(func(balance lnwire.MilliSatoshi) {
		rl.OurBalance = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType3](
			tlv.NewBigSizeT(balance),
		))
	})

	theirBalance.WhenSome(func(balance lnwire.MilliSatoshi) {
		rl.TheirBalance = tlv.SomeRecordT(tlv.NewRecordT[tlv.TlvType4](
			tlv.NewBigSizeT(balance),
		))
	})

	customBlob.WhenSome(func(blob tlv.Blob) {
		rl.CustomBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5, tlv.Blob](blob),
		)
	})

	return rl
}
