package channeldb

import (
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
)

// CommitChainEpoch refers to a single period of time where a particular set
// of commitment parameters are in effect.
type CommitChainEpoch struct {
	// LastHeight is the last commit height that marks the end of this
	// epoch.
	LastHeight uint64

	// normalizedParams are the commitment parameters that affect the
	// rendering of the commitment transaction. They are referred to as
	// "normalized" because they indicate the parameters that actually
	// apply to the party in question, irrespective of which party specified
	// the parameter.
	normalizedParams CommitmentParams
}

// encode encodes a CommitChainEpoch to a writer.
func (c CommitChainEpoch) encode(w io.Writer) error {
	return WriteElements(
		w, c.LastHeight, c.normalizedParams.DustLimit,
		c.normalizedParams.CsvDelay,
	)
}

// decodeCommitChainEpoch decodes a CommitChainEpoch from a reader.
func decodeCommitChainEpoch(r io.Reader) (CommitChainEpoch, error) {
	var lastHeight uint64
	var dustLimit btcutil.Amount
	var csvDelay uint16

	err := ReadElements(r, &lastHeight, &dustLimit, &csvDelay)
	if err != nil {
		return CommitChainEpoch{}, err
	}

	return CommitChainEpoch{
		LastHeight: lastHeight,
		normalizedParams: CommitmentParams{
			DustLimit: dustLimit,
			CsvDelay:  csvDelay,
		},
	}, nil
}

// CommitChainEpochHistory is a data structure designed to maintain the
// CommitmentParams history of both commitment chains.
type CommitChainEpochHistory struct {
	// normalizedCurrent is the current commitment parameters that are
	// in effect. They are separate from the history because we do not
	// yet have the final heights that close these epochs out.
	normalizedCurrent lntypes.Dual[CommitmentParams]

	// historical is a pair of lists of CommitChainEpochs that are sorted
	// by LastHeight.
	historical lntypes.Dual[[]CommitChainEpoch]
}

// Size returns the size of the CommitChainEpochHistory in bytes.
func (c *CommitChainEpochHistory) Size() uint64 {
	commitParamSize := uint64(2 * 8) // DustLimit + CsvDelay
	epochSize := uint64(8) + commitParamSize

	currentSize := 2 * commitParamSize
	localHistorySize := 2 + uint64(len(c.historical.Local))*epochSize
	remoteHistorySize := 2 + uint64(len(c.historical.Remote))*epochSize

	return currentSize + localHistorySize + remoteHistorySize
}

// encode encodes a CommitChainEpochHistory to a writer.
func (c *CommitChainEpochHistory) encode(w io.Writer) error {
	// Write the normalized current params, always writing local before
	// remote and dust limit before csv delay.
	err := WriteElements(
		w, c.normalizedCurrent.Local.DustLimit,
		c.normalizedCurrent.Local.CsvDelay,
		c.normalizedCurrent.Remote.DustLimit,
		c.normalizedCurrent.Remote.CsvDelay,
	)
	if err != nil {
		return err
	}

	// Write the length so we can handle deserialization.
	err = WriteElement(w, uint16(len(c.historical.Local)))
	if err != nil {
		return err
	}

	// Write the local epochs.
	for _, epoch := range c.historical.Local {
		err = epoch.encode(w)
		if err != nil {
			return err
		}
	}

	// Write the length so we can handle deserialization.
	err = WriteElement(w, uint16(len(c.historical.Remote)))
	if err != nil {
		return err
	}

	// Write the remote epochs.
	for _, epoch := range c.historical.Remote {
		err = epoch.encode(w)
		if err != nil {
			return err
		}
	}

	return nil
}

// ECommitChainEpochHistory defines a tlv encoder for CommitChainEpochHistory.
func ECommitChainEpochHistory(w io.Writer, val interface{},
	buf *[8]byte) error {

	if hist, ok := val.(*CommitChainEpochHistory); ok {
		return hist.encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "*CommitChainEpochHistory")
}

// decodeCommitChainEpochHistory decodes a CommitChainEpochHistory from a
// reader.
func decodeCommitChainEpochHistory(r io.Reader) (CommitChainEpochHistory,
	error) {

	var normalizedCurrent lntypes.Dual[CommitmentParams]
	err := ReadElements(r, &normalizedCurrent.Local.DustLimit,
		&normalizedCurrent.Local.CsvDelay,
		&normalizedCurrent.Remote.DustLimit,
		&normalizedCurrent.Remote.CsvDelay,
	)
	if err != nil {
		return CommitChainEpochHistory{}, err
	}

	historical := lntypes.Dual[[]CommitChainEpoch]{}

	var localEpochsLen uint16
	err = ReadElement(r, &localEpochsLen)
	if err != nil {
		return CommitChainEpochHistory{}, err
	}

	if localEpochsLen > 0 {
		historical.Local = make([]CommitChainEpoch, localEpochsLen)
		for i := range historical.Local {
			historical.Local[i], err = decodeCommitChainEpoch(r)
			if err != nil {
				return CommitChainEpochHistory{}, err
			}
		}
	}

	var remoteEpochsLen uint16
	err = ReadElement(r, &remoteEpochsLen)
	if err != nil {
		return CommitChainEpochHistory{}, err
	}

	if remoteEpochsLen > 0 {
		historical.Remote = make([]CommitChainEpoch, remoteEpochsLen)
		for i := range historical.Remote {
			historical.Remote[i], err = decodeCommitChainEpoch(r)
			if err != nil {
				return CommitChainEpochHistory{}, err
			}
		}
	}

	return CommitChainEpochHistory{
		normalizedCurrent: normalizedCurrent,
		historical:        historical,
	}, nil
}

// DCommitChainEpochHistory defines a tlv decoder for CommitChainEpochHistory.
func DCommitChainEpochHistory(r io.Reader, val interface{},
	buf *[8]byte, l uint64) error {

	if hist, ok := val.(*CommitChainEpochHistory); ok {
		decoded, err := decodeCommitChainEpochHistory(r)
		if err != nil {
			return err
		}

		*hist = decoded

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "*CommitChainEpochHistory", l, l)
}

// BeginChainEpochHistory initializes a new CommitChainEpochHistory with the
// original CommitmentParams specified in each party's ChannelConfig.
//
// NOTE: This function is only intended to be used during the funding workflow.
func BeginChainEpochHistory(
	origCfgParams lntypes.Dual[CommitmentParams]) CommitChainEpochHistory {

	return CommitChainEpochHistory{
		normalizedCurrent: origCfgParams,
		historical:        lntypes.Dual[[]CommitChainEpoch]{},
	}
}

// Record returns a TLV record that can be used to encode/decode a
// CommitChainEpochHistory to/from a TLV stream.
//
// NOTE: This is a part of the RecordProducer interface.
func (c *CommitChainEpochHistory) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, c, c.Size, ECommitChainEpochHistory,
		DCommitChainEpochHistory,
	)
}

// Push allows a ChannelParty to change the CommitmentParams for the channel and
// mark the last commit heights for each party that the old CommitmentParams
// applied to. To use this function correctly you must call it with the
// understanding that the party making changes to its ChannelConfig will pass
// in the CommitmentParams from that config change unaltered. Finally the
// current commitment heights of both commit chains are passed in to mark the
// last height for each chain that the current channel epoch applies to.
func (c *CommitChainEpochHistory) Push(whoSpecified lntypes.ChannelParty,
	params CommitmentParams, currentHeights lntypes.Dual[uint64]) {

	// Here we define a function that marks a set of normalized commitment
	// parameters with the current commit height when the epoch concluded
	// to create the CommitChainEpoch structure.
	closeEpoch := func(normalizedCurrent CommitmentParams,
		currentHeight uint64) CommitChainEpoch {

		return CommitChainEpoch{
			LastHeight:       currentHeight,
			normalizedParams: normalizedCurrent,
		}
	}

	// Using the function we just defined we now apply it to both the local
	// and remote components of the current epoch which will define the last
	// height that this set of normalized commitment parameters held for.
	closed := lntypes.ZipWithDual(
		c.normalizedCurrent, currentHeights, closeEpoch,
	)

	// Since go is unprincipled, we can't treat append as an actual function
	// so we make a wrapper for our use case.
	push := func(as []CommitChainEpoch,
		a CommitChainEpoch) []CommitChainEpoch {

		return append(as, a)
	}

	// We now take the closed epoch we just created and add it to the end of
	// our history of channel epochs.
	c.historical = lntypes.ZipWithDual(
		c.historical, closed, push,
	)

	// Now we begin the task of assembling the new normalized current
	// commitment parameters for both parties.
	newCurrent := lntypes.Dual[CommitmentParams]{}

	// The party issuing the commitment parameter change is referred to as
	// "main" here. It could be either the local or remote party but the
	// point is that the new Csv for the main party will always be the same
	// as the Csv from the last epoch, since a change to the Csv at the
	// config level is an imposition on the other party's commitment
	// transaction. However, the dust limit will be the dust limit set in
	// the argument.
	mainCsv := closed.GetForParty(whoSpecified).normalizedParams.CsvDelay
	mainParams := CommitmentParams{
		DustLimit: params.DustLimit,
		CsvDelay:  mainCsv,
	}
	newCurrent.SetForParty(whoSpecified, mainParams)

	// The other party is referred to as counter here and the key here is
	// that for the counterparty, their dust limit will remain the same as
	// it was before, while their Csv will update since it is imposed by the
	// main party.
	counterParty := whoSpecified.CounterParty()
	counterDustLimit := closed.GetForParty(counterParty).
		normalizedParams.DustLimit

	counterParams := CommitmentParams{
		DustLimit: counterDustLimit,
		CsvDelay:  params.CsvDelay,
	}
	newCurrent.SetForParty(counterParty, counterParams)

	// Now that we have set the values appropriately for the newCurrent
	// we set the normalizedCurrent values to be the newly computed current
	// commitment values.
	c.normalizedCurrent = newCurrent
}

// NormalizedParamsAt queries the CommitChainEpochHistory for the normalized
// commitment parameters for the ChannelParty's commitment transaction at a
// given height. The parameters are referred to as "normalized" because they
// indicate the parameters that apply to that party's commitment transaction
// irrespective of which party is responsible for setting those parameters.
func (c *CommitChainEpochHistory) NormalizedParamsAt(
	whoseCommit lntypes.ChannelParty,
	height uint64) CommitmentParams {

	// Try to find the epoch that applies to the height.
	histEpoch := search(c.historical.GetForParty(whoseCommit), height)

	// Extract just the portion of the epoch that specifies the parameters.
	histParams := fn.MapOption(func(e CommitChainEpoch) CommitmentParams {
		return e.normalizedParams
	})(histEpoch)

	// If we didn't find it, then we use the current parameters.
	curr := c.normalizedCurrent.GetForParty(whoseCommit)

	return histParams.UnwrapOr(curr)
}

// search is responsible for finding the epoch that encloses the specified
// height. This means that the LastHeight of that epoch must be greater than
// or equal to the query height AND the previous epoch's LastHeight must be
// less than the query height.
func search(epochs []CommitChainEpoch, h uint64) fn.Option[CommitChainEpoch] {
	// We implement a simple binary search here.
	half := len(epochs) / 2

	switch {
	// We have a couple of edge cases here. If we somehow end up with an
	// empty epoch history we are querying we return None.
	case len(epochs) == 0:
		return fn.None[CommitChainEpoch]()

	// If we have a single epoch in the history then that epoch is the
	// correct one iff its LastHeight is greater than or equal to the
	// query height.
	case len(epochs) == 1:
		if h <= epochs[0].LastHeight {
			return fn.Some(epochs[0])
		} else {
			return fn.None[CommitChainEpoch]()
		}

	// Otherwise we begin our dividing of the slice. If our height falls
	// between the LastHeight of the last epoch in the former half of the
	// history and the LastHeight of the first epoch in the latter half of
	// the history, then the first epoch of the latter half is the correct
	// epoch.
	case epochs[half-1].LastHeight < h &&
		h <= epochs[half].LastHeight:

		return fn.Some(epochs[half])

	// Now that we've excluded the between case, our query height is in
	// either half. If it's less than the LastHeight of the last epoch in
	// the former half, then we will compute the search again on that half.
	case h <= epochs[half-1].LastHeight:
		return search(epochs[:half], h)

	// Otherwise it's in the latter half and so we will compute the search
	// on that half.
	case h > epochs[half].LastHeight:
		return search(epochs[half:], h)

	// We should have excausted all cases, so this indicates something
	// severely wrong with the algorithm and we choose to hard-eject.
	default:
		panic("non-exhaustive cases in commit epochs search")
	}
}
