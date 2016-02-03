package uspv

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcutil/txsort"
)

func (t *TxStore) SignThis(tx *wire.MsgTx) error {
	fmt.Printf("-= SignThis =-\n")

	// sort tx before signing.
	txsort.InPlaceSort(tx)

	sigs := make([][]byte, len(tx.TxIn))
	// first iterate over each input
	for j, in := range tx.TxIn {
		for k := uint32(0); k < uint32(len(t.Adrs)); k++ {
			child, err := t.rootPrivKey.Child(k + hdkeychain.HardenedKeyStart)
			if err != nil {
				return err
			}
			myadr, err := child.Address(t.Param)
			if err != nil {
				return err
			}
			adrScript, err := txscript.PayToAddrScript(myadr)
			if err != nil {
				return err
			}
			if bytes.Equal(adrScript, in.SignatureScript) {
				fmt.Printf("Hit; key %d matches input %d. Signing.\n", k, j)
				priv, err := child.ECPrivKey()
				if err != nil {
					return err
				}
				sigs[j], err = txscript.SignatureScript(
					tx, j, in.SignatureScript, txscript.SigHashAll, priv, true)
				if err != nil {
					return err
				}
				break
			}
		}
	}
	for i, s := range sigs {
		if s != nil {
			tx.TxIn[i].SignatureScript = s
		}
	}
	return nil
}
