package lnwallet

import (
	"encoding/hex"

	"github.com/roasbeef/btcd/btcjson"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/coinset"
)

// lnCoin represents a single unspet output. Its purpose is to convert a regular
// output to a struct adhering to the coinset.Coin interface
type lnCoin struct {
	hash     *wire.ShaHash
	index    uint32
	value    btcutil.Amount
	pkScript []byte
	numConfs int64
	valueAge int64
}

func (l *lnCoin) Hash() *wire.ShaHash   { return l.hash }
func (l *lnCoin) Index() uint32         { return l.index }
func (l *lnCoin) Value() btcutil.Amount { return l.value }
func (l *lnCoin) PkScript() []byte      { return l.pkScript }
func (l *lnCoin) NumConfs() int64       { return l.numConfs }
func (l *lnCoin) ValueAge() int64       { return l.valueAge }

// Ensure lnCoin adheres to the coinset.Coin interface.
var _ coinset.Coin = (*lnCoin)(nil)

// newLnCoin creates a new "coin" from the passed output. Coins are required
// in order to perform coin selection upon.
func newLnCoin(output *btcjson.ListUnspentResult) (coinset.Coin, error) {
	txid, err := wire.NewShaHashFromStr(output.TxID)
	if err != nil {
		return nil, err
	}

	pkScript, err := hex.DecodeString(output.ScriptPubKey)
	if err != nil {
		return nil, err
	}

	return &lnCoin{
		hash: txid,
		// btcjson.ListUnspentResult shows the amount in BTC,
		// translate into Satoshi so coin selection can work properly.
		value:    btcutil.Amount(output.Amount * 1e8),
		index:    output.Vout,
		pkScript: pkScript,
		numConfs: output.Confirmations,
		// TODO(roasbeef): output.Amount should be a int64, damn json-RPC :/
		valueAge: output.Confirmations * int64(output.Amount),
	}, nil
}

// outputsToCoins converts a slice of transaction outputs to a coin-selectable
// slice of "Coins"s.
func outputsToCoins(outputs []*btcjson.ListUnspentResult) ([]coinset.Coin, error) {
	coins := make([]coinset.Coin, len(outputs))
	for i, output := range outputs {
		coin, err := newLnCoin(output)
		if err != nil {
			return nil, err
		}

		coins[i] = coin
	}

	return coins, nil
}
