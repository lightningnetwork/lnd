package main

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// OutPoint displays an outpoint string in the form "<txid>:<output-index>".
type OutPoint string

// NewOutPointFromProto formats the lnrpc.OutPoint into an OutPoint for display.
func NewOutPointFromProto(op *lnrpc.OutPoint) OutPoint {
	return OutPoint(fmt.Sprintf("%s:%d", op.TxidStr, op.OutputIndex))
}

// Utxo displays information about an unspent output, including its address,
// amount, pkscript, and confirmations.
type Utxo struct {
	Type          lnrpc.AddressType `json:"address_type"`
	Address       string            `json:"address"`
	AmountSat     int64             `json:"amount_sat"`
	ScriptPubkey  string            `json:"script_pubkey"`
	OutPoint      OutPoint          `json:"outpoint"`
	Confirmations int64             `json:"confirmations"`
}

// NewUtxoFromProto creates a display Utxo from the Utxo proto. This filters out
// the raw txid bytes from the provided outpoint, which will otherwise be
// printed in base64.
func NewUtxoFromProto(utxo *lnrpc.Utxo) *Utxo {
	return &Utxo{
		Type:          utxo.Type,
		Address:       utxo.Address,
		AmountSat:     utxo.AmountSat,
		ScriptPubkey:  utxo.ScriptPubkey,
		OutPoint:      NewOutPointFromProto(utxo.Outpoint),
		Confirmations: utxo.Confirmations,
	}
}
