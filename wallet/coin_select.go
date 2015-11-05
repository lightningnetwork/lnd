package wallet

import (
	"encoding/hex"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
)

// lnCoin...
// to adhere to the coinset.Coin interface
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

// newLnCoin...
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
		hash:     txid,
		value:    btcutil.Amount(output.Amount),
		index:    output.Vout,
		pkScript: pkScript,
		numConfs: output.Confirmations,
		// TODO(roasbeef) outpout.Amount should be a int64 :/
		valueAge: output.Confirmations * int64(output.Amount),
	}, nil
}

// outputsToCoins...
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
