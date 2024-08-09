package commands

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// OutPoint displays an outpoint string in the form "<txid>:<output-index>".
type OutPoint string

// NewOutPointFromProto formats the lnrpc.OutPoint into an OutPoint for display.
func NewOutPointFromProto(op *lnrpc.OutPoint) OutPoint {
	var hash chainhash.Hash
	copy(hash[:], op.TxidBytes)
	return OutPoint(fmt.Sprintf("%v:%d", hash, op.OutputIndex))
}

// NewProtoOutPoint parses an OutPoint into its corresponding lnrpc.OutPoint
// type.
func NewProtoOutPoint(op string) (*lnrpc.OutPoint, error) {
	parts := strings.Split(op, ":")
	if len(parts) != 2 {
		return nil, errors.New("outpoint should be of the form txid:index")
	}
	txid := parts[0]
	if hex.DecodedLen(len(txid)) != chainhash.HashSize {
		return nil, fmt.Errorf("invalid hex-encoded txid %v", txid)
	}
	outputIndex, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid output index: %w", err)
	}
	return &lnrpc.OutPoint{
		TxidStr:     txid,
		OutputIndex: uint32(outputIndex),
	}, nil
}

// Utxo displays information about an unspent output, including its address,
// amount, pkscript, and confirmations.
type Utxo struct {
	Type          lnrpc.AddressType `json:"address_type"`
	Address       string            `json:"address"`
	AmountSat     int64             `json:"amount_sat"`
	PkScript      string            `json:"pk_script"`
	OutPoint      OutPoint          `json:"outpoint"`
	Confirmations int64             `json:"confirmations"`
}

// NewUtxoFromProto creates a display Utxo from the Utxo proto. This filters out
// the raw txid bytes from the provided outpoint, which will otherwise be
// printed in base64.
func NewUtxoFromProto(utxo *lnrpc.Utxo) *Utxo {
	return &Utxo{
		Type:          utxo.AddressType,
		Address:       utxo.Address,
		AmountSat:     utxo.AmountSat,
		PkScript:      utxo.PkScript,
		OutPoint:      NewOutPointFromProto(utxo.Outpoint),
		Confirmations: utxo.Confirmations,
	}
}

// FailedUpdate displays information about a failed update, including its
// address, reason and update error.
type FailedUpdate struct {
	OutPoint    OutPoint `json:"outpoint"`
	Reason      string   `json:"reason"`
	UpdateError string   `json:"update_error"`
}

// NewFailedUpdateFromProto creates a display from the FailedUpdate
// proto. This filters out the raw txid bytes from the provided outpoint,
// which will otherwise be printed in base64.
func NewFailedUpdateFromProto(update *lnrpc.FailedUpdate) *FailedUpdate {
	return &FailedUpdate{
		OutPoint:    NewOutPointFromProto(update.Outpoint),
		Reason:      update.Reason.String(),
		UpdateError: update.UpdateError,
	}
}

// UtxosToOutpoints converts a slice of UTXO strings into a slice of OutPoint
// protobuf objects. It returns an error if no UTXOs are specified or if any
// UTXO string cannot be parsed into an OutPoint.
func UtxosToOutpoints(utxos []string) ([]*lnrpc.OutPoint, error) {
	var outpoints []*lnrpc.OutPoint
	if len(utxos) == 0 {
		return nil, fmt.Errorf("no utxos specified")
	}
	for _, utxo := range utxos {
		outpoint, err := NewProtoOutPoint(utxo)
		if err != nil {
			return nil, err
		}
		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}
