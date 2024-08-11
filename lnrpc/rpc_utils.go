package lnrpc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// RegisterRPCMiddlewareURI is the full RPC method URI for the
	// middleware registration call. This is declared here rather than where
	// it's mainly used to avoid circular package dependencies.
	RegisterRPCMiddlewareURI = "/lnrpc.Lightning/RegisterRPCMiddleware"
)

var (
	// ProtoJSONMarshalOpts is a struct that holds the default marshal
	// options for marshaling protobuf messages into JSON in a
	// human-readable way. This should only be used in the CLI and in
	// integration tests.
	ProtoJSONMarshalOpts = &protojson.MarshalOptions{
		EmitUnpopulated: true,
		UseProtoNames:   true,
		Indent:          "    ",
		UseHexForBytes:  true,
	}

	// ProtoJSONUnmarshalOpts is a struct that holds the default unmarshal
	// options for un-marshaling lncli JSON into protobuf messages. This
	// should only be used in the CLI and in integration tests.
	ProtoJSONUnmarshalOpts = &protojson.UnmarshalOptions{
		AllowPartial:   false,
		UseHexForBytes: true,
	}

	// RESTJsonMarshalOpts is a struct that holds the default marshal
	// options for marshaling protobuf messages into REST JSON in a
	// human-readable way. This should be used when interacting with the
	// REST proxy only.
	RESTJsonMarshalOpts = &protojson.MarshalOptions{
		EmitUnpopulated: true,
		UseProtoNames:   true,
	}

	// RESTJsonUnmarshalOpts is a struct that holds the default unmarshal
	// options for un-marshaling REST JSON into protobuf messages. This
	// should be used when interacting with the REST proxy only.
	RESTJsonUnmarshalOpts = &protojson.UnmarshalOptions{
		AllowPartial: false,
	}
)

// RPCTransaction returns a rpc transaction.
func RPCTransaction(tx *lnwallet.TransactionDetail) *Transaction {
	var destAddresses []string
	// Re-package destination output information.
	var outputDetails []*OutputDetail
	for _, o := range tx.OutputDetails {
		// Note: DestAddresses is deprecated but we keep
		// populating it with addresses for backwards
		// compatibility.
		for _, a := range o.Addresses {
			destAddresses = append(destAddresses,
				a.EncodeAddress())
		}

		var address string
		if len(o.Addresses) == 1 {
			address = o.Addresses[0].EncodeAddress()
		}

		outputDetails = append(outputDetails, &OutputDetail{
			OutputType:   MarshallOutputType(o.OutputType),
			Address:      address,
			PkScript:     hex.EncodeToString(o.PkScript),
			OutputIndex:  int64(o.OutputIndex),
			Amount:       int64(o.Value),
			IsOurAddress: o.IsOurAddress,
		})
	}

	previousOutpoints := make([]*PreviousOutPoint, len(tx.PreviousOutpoints))
	for idx, previousOutPoint := range tx.PreviousOutpoints {
		previousOutpoints[idx] = &PreviousOutPoint{
			Outpoint:    previousOutPoint.OutPoint,
			IsOurOutput: previousOutPoint.IsOurOutput,
		}
	}

	// We also get unconfirmed transactions, so BlockHash can be nil.
	blockHash := ""
	if tx.BlockHash != nil {
		blockHash = tx.BlockHash.String()
	}

	return &Transaction{
		TxHash:            tx.Hash.String(),
		Amount:            int64(tx.Value),
		NumConfirmations:  tx.NumConfirmations,
		BlockHash:         blockHash,
		BlockHeight:       tx.BlockHeight,
		TimeStamp:         tx.Timestamp,
		TotalFees:         tx.TotalFees,
		DestAddresses:     destAddresses,
		OutputDetails:     outputDetails,
		RawTxHex:          hex.EncodeToString(tx.RawTx),
		Label:             tx.Label,
		PreviousOutpoints: previousOutpoints,
	}
}

// RPCTransactionDetails returns a set of rpc transaction details.
func RPCTransactionDetails(txns []*lnwallet.TransactionDetail, firstIdx,
	lastIdx uint64) *TransactionDetails {

	txDetails := &TransactionDetails{
		Transactions: make([]*Transaction, len(txns)),
		FirstIndex:   firstIdx,
		LastIndex:    lastIdx,
	}

	for i, tx := range txns {
		txDetails.Transactions[i] = RPCTransaction(tx)
	}

	// Sort transactions by number of confirmations rather than height so
	// that unconfirmed transactions (height =0; confirmations =-1) will
	// follow the most recently set of confirmed transactions. If we sort
	// by height, unconfirmed transactions will follow our oldest
	// transactions, because they have lower block heights.
	sort.Slice(txDetails.Transactions, func(i, j int) bool {
		return txDetails.Transactions[i].NumConfirmations <
			txDetails.Transactions[j].NumConfirmations
	})

	return txDetails
}

// ExtractMinConfs extracts the minimum number of confirmations that each
// output used to fund a transaction should satisfy.
func ExtractMinConfs(minConfs int32, spendUnconfirmed bool) (int32, error) {
	switch {
	// Ensure that the MinConfs parameter is non-negative.
	case minConfs < 0:
		return 0, errors.New("minimum number of confirmations must " +
			"be a non-negative number")

	// The transaction should not be funded with unconfirmed outputs
	// unless explicitly specified by SpendUnconfirmed. We do this to
	// provide sane defaults to the OpenChannel RPC, as otherwise, if the
	// MinConfs field isn't explicitly set by the caller, we'll use
	// unconfirmed outputs without the caller being aware.
	case minConfs == 0 && !spendUnconfirmed:
		return 1, nil

	// In the event that the caller set MinConfs > 0 and SpendUnconfirmed to
	// true, we'll return an error to indicate the conflict.
	case minConfs > 0 && spendUnconfirmed:
		return 0, errors.New("SpendUnconfirmed set to true with " +
			"MinConfs > 0")

	// The funding transaction of the new channel to be created can be
	// funded with unconfirmed outputs.
	case spendUnconfirmed:
		return 0, nil

	// If none of the above cases matched, we'll return the value set
	// explicitly by the caller.
	default:
		return minConfs, nil
	}
}

// GetChanPointFundingTxid returns the given channel point's funding txid in
// raw bytes.
func GetChanPointFundingTxid(chanPoint *ChannelPoint) (*chainhash.Hash, error) {
	var txid []byte

	// A channel point's funding txid can be get/set as a byte slice or a
	// string. In the case it is a string, decode it.
	switch chanPoint.GetFundingTxid().(type) {
	case *ChannelPoint_FundingTxidBytes:
		txid = chanPoint.GetFundingTxidBytes()
	case *ChannelPoint_FundingTxidStr:
		s := chanPoint.GetFundingTxidStr()
		h, err := chainhash.NewHashFromStr(s)
		if err != nil {
			return nil, err
		}

		txid = h[:]
	}

	return chainhash.NewHash(txid)
}

// GetChannelOutPoint returns the outpoint of the related channel point.
func GetChannelOutPoint(chanPoint *ChannelPoint) (*OutPoint, error) {
	var txid []byte

	// A channel point's funding txid can be get/set as a byte slice or a
	// string. In the case it is a string, decode it.
	switch chanPoint.GetFundingTxid().(type) {
	case *ChannelPoint_FundingTxidBytes:
		txid = chanPoint.GetFundingTxidBytes()

	case *ChannelPoint_FundingTxidStr:
		s := chanPoint.GetFundingTxidStr()
		h, err := chainhash.NewHashFromStr(s)
		if err != nil {
			return nil, err
		}

		txid = h[:]
	}

	return &OutPoint{
		TxidBytes:   txid,
		OutputIndex: chanPoint.OutputIndex,
	}, nil
}

// CalculateFeeRate uses either satPerByte or satPerVByte, but not both, from a
// request to calculate the fee rate. It provides compatibility for the
// deprecated field, satPerByte. Once the field is safe to be removed, the
// check can then be deleted.
func CalculateFeeRate(satPerByte, satPerVByte uint64, targetConf uint32,
	estimator chainfee.Estimator) (chainfee.SatPerKWeight, error) {

	var feeRate chainfee.SatPerKWeight

	// We only allow using either the deprecated field or the new field.
	if satPerByte != 0 && satPerVByte != 0 {
		return feeRate, fmt.Errorf("either SatPerByte or " +
			"SatPerVByte should be set, but not both")
	}

	// Default to satPerVByte, and overwrite it if satPerByte is set.
	satPerKw := chainfee.SatPerKVByte(satPerVByte * 1000).FeePerKWeight()
	if satPerByte != 0 {
		satPerKw = chainfee.SatPerKVByte(
			satPerByte * 1000,
		).FeePerKWeight()
	}

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for this transaction.
	feePref := sweep.FeeEstimateInfo{
		ConfTarget: targetConf,
		FeeRate:    satPerKw,
	}
	// TODO(yy): need to pass the configured max fee here.
	feeRate, err := feePref.Estimate(estimator, 0)
	if err != nil {
		return feeRate, err
	}

	return feeRate, nil
}
