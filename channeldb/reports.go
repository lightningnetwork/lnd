package channeldb

import (
	"bytes"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// closeSummaryBucket is a top level bucket which holds additional
	// information about channel closes. It nests channels by chainhash
	// and channel point.
	// [closeSummaryBucket]
	//	[chainHashBucket]
	//		[channelBucket]
	//			[resolversBucket]
	closeSummaryBucket = []byte("close-summaries")

	// resolversBucket holds the outcome of a channel's resolvers. It is
	// nested under a channel and chainhash bucket in the close summaries
	// bucket.
	resolversBucket = []byte("resolvers-bucket")
)

var (
	// ErrNoChainHashBucket is returned when we have not created a bucket
	// for the current chain hash.
	ErrNoChainHashBucket = errors.New("no chain hash bucket")

	// ErrNoChannelSummaries is returned when a channel is not found in the
	// chain hash bucket.
	ErrNoChannelSummaries = errors.New("channel bucket not found")

	amountType    tlv.Type = 1
	resolverType  tlv.Type = 2
	outcomeType   tlv.Type = 3
	spendTxIDType tlv.Type = 4
)

// ResolverType indicates the type of resolver that was resolved on chain.
type ResolverType uint8

const (
	// ResolverTypeAnchor represents a resolver for an anchor output.
	ResolverTypeAnchor ResolverType = 0

	// ResolverTypeIncomingHtlc represents resolution of an incoming htlc.
	ResolverTypeIncomingHtlc ResolverType = 1

	// ResolverTypeOutgoingHtlc represents resolution of an outgoing htlc.
	ResolverTypeOutgoingHtlc ResolverType = 2

	// ResolverTypeCommit represents resolution of our time locked commit
	// when we force close.
	ResolverTypeCommit ResolverType = 3
)

// ResolverOutcome indicates the outcome for the resolver that that the contract
// court reached. This state is not necessarily final, since htlcs on our own
// commitment are resolved across two resolvers.
type ResolverOutcome uint8

const (
	// ResolverOutcomeClaimed indicates that funds were claimed on chain.
	ResolverOutcomeClaimed ResolverOutcome = 0

	// ResolverOutcomeUnclaimed indicates that we did not claim our funds on
	// chain. This may be the case for anchors that we did not sweep, or
	// outputs that were not economical to sweep.
	ResolverOutcomeUnclaimed ResolverOutcome = 1

	// ResolverOutcomeAbandoned indicates that we did not attempt to claim
	// an output on chain. This is the case for htlcs that we could not
	// decode to claim, or invoice which we fail when an attempt is made
	// to settle them on chain.
	ResolverOutcomeAbandoned ResolverOutcome = 2

	// ResolverOutcomeTimeout indicates that a contract was timed out on
	// chain.
	ResolverOutcomeTimeout ResolverOutcome = 3

	// ResolverOutcomeFirstStage indicates that a htlc had to be claimed
	// over two stages, with this outcome representing the confirmation
	// of our success/timeout tx.
	ResolverOutcomeFirstStage ResolverOutcome = 4
)

// ResolverReport provides an account of the outcome of a resolver. This differs
// from a ContractReport because it does not necessarily fully resolve the
// contract; each step of two stage htlc resolution is included.
type ResolverReport struct {
	// OutPoint is the on chain outpoint that was spent as a result of this
	// resolution. When an output is directly resolved (eg, commitment
	// sweeps and single stage htlcs on the remote party's output) this
	// is an output on the commitment tx that was broadcast. When we resolve
	// across two stages (eg, htlcs on our own force close commit), the
	// first stage outpoint is the output on our commitment and the second
	// stage output is the spend from our htlc success/timeout tx.
	OutPoint wire.OutPoint

	// Amount is the value of the output referenced above.
	Amount btcutil.Amount

	// ResolverType indicates the type of resolution that occurred.
	ResolverType

	// ResolverOutcome indicates the outcome of the resolver.
	ResolverOutcome

	// SpendTxID is the transaction ID of the spending transaction that
	// claimed the outpoint. This may be a sweep transaction, or a first
	// stage success/timeout transaction.
	SpendTxID *chainhash.Hash
}

// PutResolverReport creates and commits a transaction that is used to write a
// resolver report to disk.
func (d *DB) PutResolverReport(tx kvdb.RwTx, chainHash chainhash.Hash,
	channelOutpoint *wire.OutPoint, report *ResolverReport) error {

	putReportFunc := func(tx kvdb.RwTx) error {
		return putReport(tx, chainHash, channelOutpoint, report)
	}

	// If the transaction is nil, we'll create a new one.
	if tx == nil {
		return kvdb.Update(d, putReportFunc, func() {})
	}

	// Otherwise, we can write the report to disk using the existing
	// transaction.
	return putReportFunc(tx)
}

// putReport puts a report in the bucket provided, with its outpoint as its key.
func putReport(tx kvdb.RwTx, chainHash chainhash.Hash,
	channelOutpoint *wire.OutPoint, report *ResolverReport) error {

	channelBucket, err := fetchReportWriteBucket(
		tx, chainHash, channelOutpoint,
	)
	if err != nil {
		return err
	}

	// If the resolvers bucket does not exist yet, create it.
	resolvers, err := channelBucket.CreateBucketIfNotExists(
		resolversBucket,
	)
	if err != nil {
		return err
	}

	var valueBuf bytes.Buffer
	if err := serializeReport(&valueBuf, report); err != nil {
		return err
	}

	// Finally write our outpoint to be used as the key for this record.
	var keyBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&keyBuf, &report.OutPoint); err != nil {
		return err
	}

	return resolvers.Put(keyBuf.Bytes(), valueBuf.Bytes())
}

// serializeReport serialized a report using a TLV stream to allow for optional
// fields.
func serializeReport(w io.Writer, report *ResolverReport) error {
	amt := uint64(report.Amount)
	resolver := uint8(report.ResolverType)
	outcome := uint8(report.ResolverOutcome)

	// Create a set of TLV records for the values we know to be present.
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(amountType, &amt),
		tlv.MakePrimitiveRecord(resolverType, &resolver),
		tlv.MakePrimitiveRecord(outcomeType, &outcome),
	}

	// If our spend txid is non-nil, we add a tlv entry for it.
	if report.SpendTxID != nil {
		var spendBuf bytes.Buffer
		err := WriteElement(&spendBuf, *report.SpendTxID)
		if err != nil {
			return err
		}
		spendBytes := spendBuf.Bytes()

		records = append(records, tlv.MakePrimitiveRecord(
			spendTxIDType, &spendBytes,
		))
	}

	// Create our stream and encode it.
	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// FetchChannelReports fetches the set of reports for a channel.
func (d DB) FetchChannelReports(chainHash chainhash.Hash,
	outPoint *wire.OutPoint) ([]*ResolverReport, error) {

	var reports []*ResolverReport

	if err := kvdb.View(d.Backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchReportReadBucket(
			tx, chainHash, outPoint,
		)
		if err != nil {
			return err
		}

		// If there are no resolvers for this channel, we simply
		// return nil, because nothing has been persisted yet.
		resolvers := chanBucket.NestedReadBucket(resolversBucket)
		if resolvers == nil {
			return nil
		}

		// Run through each resolution and add it to our set of
		// resolutions.
		return resolvers.ForEach(func(k, v []byte) error {
			// Deserialize the contents of our field.
			r := bytes.NewReader(v)
			report, err := deserializeReport(r)
			if err != nil {
				return err
			}

			// Once we have read our values out, set the outpoint
			// on the report using the key.
			r = bytes.NewReader(k)
			if err := ReadElement(r, &report.OutPoint); err != nil {
				return err
			}

			reports = append(reports, report)

			return nil
		})
	}, func() {
		reports = nil
	}); err != nil {
		return nil, err
	}

	return reports, nil
}

// deserializeReport gets a resolver report from a tlv stream. The outpoint on
// the resolver will not be set because we key reports by their outpoint, and
// this function reads only the values saved in the stream.
func deserializeReport(r io.Reader) (*ResolverReport, error) {
	var (
		resolver, outcome uint8
		amt               uint64
		spentTx           []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(amountType, &amt),
		tlv.MakePrimitiveRecord(resolverType, &resolver),
		tlv.MakePrimitiveRecord(outcomeType, &outcome),
		tlv.MakePrimitiveRecord(spendTxIDType, &spentTx),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(r); err != nil {
		return nil, err
	}

	report := &ResolverReport{
		Amount:          btcutil.Amount(amt),
		ResolverOutcome: ResolverOutcome(outcome),
		ResolverType:    ResolverType(resolver),
	}

	// If our spend tx is set, we set it on our report.
	if len(spentTx) != 0 {
		spendTx, err := chainhash.NewHash(spentTx)
		if err != nil {
			return nil, err
		}
		report.SpendTxID = spendTx
	}

	return report, nil
}

// fetchReportWriteBucket returns a write channel bucket within the reports
// top level bucket. If the channel's bucket does not yet exist, it will be
// created.
func fetchReportWriteBucket(tx kvdb.RwTx, chainHash chainhash.Hash,
	outPoint *wire.OutPoint) (kvdb.RwBucket, error) {

	// Get the channel close summary bucket.
	closedBucket := tx.ReadWriteBucket(closeSummaryBucket)

	// Create the chain hash bucket if it does not exist.
	chainHashBkt, err := closedBucket.CreateBucketIfNotExists(chainHash[:])
	if err != nil {
		return nil, err
	}

	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}

	return chainHashBkt.CreateBucketIfNotExists(chanPointBuf.Bytes())
}

// fetchReportReadBucket returns a read channel bucket within the reports
// top level bucket. If any bucket along the way does not exist, it will error.
func fetchReportReadBucket(tx kvdb.RTx, chainHash chainhash.Hash,
	outPoint *wire.OutPoint) (kvdb.RBucket, error) {

	// First fetch the top level channel close summary bucket.
	closeBucket := tx.ReadBucket(closeSummaryBucket)

	// Next we get the chain hash bucket for our current chain.
	chainHashBucket := closeBucket.NestedReadBucket(chainHash[:])
	if chainHashBucket == nil {
		return nil, ErrNoChainHashBucket
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for the channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}

	chanBucket := chainHashBucket.NestedReadBucket(chanPointBuf.Bytes())
	if chanBucket == nil {
		return nil, ErrNoChannelSummaries
	}

	return chanBucket, nil
}
