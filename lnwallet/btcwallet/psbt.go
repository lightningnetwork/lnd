package btcwallet

import (
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/psbt"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// FundPsbt creates a fully populated PSBT packet that contains enough inputs to
// fund the outputs specified in the passed in packet with the specified fee
// rate. If there is change left, a change output from the internal wallet is
// added and the index of the change output is returned. Otherwise no additional
// output is created and the index -1 is returned.
//
// NOTE: If the packet doesn't contain any inputs, coin selection is performed
// automatically. The account parameter must be non-empty as it determines which
// set of coins are eligible for coin selection. If the packet does contain any
// inputs, it is assumed that full coin selection happened externally and no
// additional inputs are added. If the specified inputs aren't enough to fund
// the outputs with the given fee rate, an error is returned. No lock lease is
// acquired for any of the selected/validated inputs. It is in the caller's
// responsibility to lock the inputs before handing them out.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FundPsbt(packet *psbt.Packet, minConfs int32,
	feeRate chainfee.SatPerKWeight, accountName string) (int32, error) {

	// The fee rate is passed in using units of sat/kw, so we'll convert
	// this to sat/KB as the CreateSimpleTx method requires this unit.
	feeSatPerKB := btcutil.Amount(feeRate.FeePerKVByte())

	var (
		keyScope   *waddrmgr.KeyScope
		accountNum uint32
	)
	switch accountName {
	// If the default/imported account name was specified, we'll provide a
	// nil key scope to FundPsbt, allowing it to select inputs from both key
	// scopes (NP2WKH, P2WKH).
	case lnwallet.DefaultAccountName:
		accountNum = defaultAccount

	case waddrmgr.ImportedAddrAccountName:
		accountNum = importedAccount

	// Otherwise, map the account name to its key scope and internal account
	// number to only select inputs from said account.
	default:
		scope, account, err := b.wallet.LookupAccount(accountName)
		if err != nil {
			return 0, err
		}
		keyScope = &scope
		accountNum = account
	}

	// Let the wallet handle coin selection and/or fee estimation based on
	// the partial TX information in the packet.
	return b.wallet.FundPsbt(
		packet, keyScope, minConfs, accountNum, feeSatPerKB,
		b.cfg.CoinSelectionStrategy,
	)
}

// FinalizePsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all inputs that belong to the specified account.
// Lnd must be the last signer of the transaction. That means, if there are any
// unsigned non-witness inputs or inputs without UTXO information attached or
// inputs without witness data that do not belong to lnd's wallet, this method
// will fail. If no error is returned, the PSBT is ready to be extracted and the
// final TX within to be broadcast.
//
// NOTE: This method does NOT publish the transaction after it's been
// finalized successfully.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FinalizePsbt(packet *psbt.Packet, accountName string) error {
	var (
		keyScope   *waddrmgr.KeyScope
		accountNum uint32
	)
	switch accountName {
	// If the default/imported account name was specified, we'll provide a
	// nil key scope to FundPsbt, allowing it to sign inputs from both key
	// scopes (NP2WKH, P2WKH).
	case lnwallet.DefaultAccountName:
		accountNum = defaultAccount

	case waddrmgr.ImportedAddrAccountName:
		accountNum = importedAccount

	// Otherwise, map the account name to its key scope and internal account
	// number to determine if the inputs belonging to this account should be
	// signed.
	default:
		scope, account, err := b.wallet.LookupAccount(accountName)
		if err != nil {
			return err
		}
		keyScope = &scope
		accountNum = account
	}

	return b.wallet.FinalizePsbt(keyScope, accountNum, packet)
}
