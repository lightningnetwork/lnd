package chanfunding

import (
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// DefaultReservationTimeout is the default time we wait until we remove
	// an unfinished (zombiestate) open channel flow from memory.
	DefaultReservationTimeout = 10 * time.Minute

	// DefaultLockDuration is the default duration used to lock outputs.
	DefaultLockDuration = 10 * time.Minute
)

var (
	// LndInternalLockID is the binary representation of the SHA256 hash of
	// the string "lnd-internal-lock-id" and is used for UTXO lock leases to
	// identify that we ourselves are locking an UTXO, for example when
	// giving out a funded PSBT. The ID corresponds to the hex value of
	// ede19a92ed321a4705f8a1cccc1d4f6182545d4bb4fae08bd5937831b7e38f98.
	LndInternalLockID = wtxmgr.LockID{
		0xed, 0xe1, 0x9a, 0x92, 0xed, 0x32, 0x1a, 0x47,
		0x05, 0xf8, 0xa1, 0xcc, 0xcc, 0x1d, 0x4f, 0x61,
		0x82, 0x54, 0x5d, 0x4b, 0xb4, 0xfa, 0xe0, 0x8b,
		0xd5, 0x93, 0x78, 0x31, 0xb7, 0xe3, 0x8f, 0x98,
	}
)

// FullIntent is an intent that is fully backed by the internal wallet. This
// intent differs from the ShimIntent, in that the funding transaction will be
// constructed internally, and will consist of only inputs we wholly control.
// This Intent implements a basic state machine that must be executed in order
// before CompileFundingTx can be called.
//
// Steps to final channel provisioning:
//  1. Call BindKeys to notify the intent which keys to use when constructing
//     the multi-sig output.
//  2. Call CompileFundingTx afterwards to obtain the funding transaction.
//
// If either of these steps fail, then the Cancel method MUST be called.
type FullIntent struct {
	ShimIntent

	// InputCoins are the set of coins selected as inputs to this funding
	// transaction.
	InputCoins []wallet.Coin

	// ChangeOutputs are the set of outputs that the Assembler will use as
	// change from the main funding transaction.
	ChangeOutputs []*wire.TxOut

	// coinLeaser is the Assembler's instance of the OutputLeaser
	// interface.
	coinLeaser OutputLeaser

	// coinSource is the Assembler's instance of the CoinSource interface.
	coinSource CoinSource

	// signer is the Assembler's instance of the Singer interface.
	signer input.Signer
}

// BindKeys is a method unique to the FullIntent variant. This allows the
// caller to decide precisely which keys are used in the final funding
// transaction. This is kept out of the main Assembler as these may may not
// necessarily be under full control of the wallet. Only after this method has
// been executed will CompileFundingTx succeed.
func (f *FullIntent) BindKeys(localKey *keychain.KeyDescriptor,
	remoteKey *btcec.PublicKey) {

	f.localKey = localKey
	f.remoteKey = remoteKey
}

// CompileFundingTx is to be called after BindKeys on the sub-intent has been
// called. This method will construct the final funding transaction, and fully
// sign all inputs that are known by the backing CoinSource. After this method
// returns, the Intent is assumed to be complete, as the output can be created
// at any point.
func (f *FullIntent) CompileFundingTx(extraInputs []*wire.TxIn,
	extraOutputs []*wire.TxOut) (*wire.MsgTx, error) {

	// Create a blank, fresh transaction. Soon to be a complete funding
	// transaction which will allow opening a lightning channel.
	fundingTx := wire.NewMsgTx(2)

	// Add all multi-party inputs and outputs to the transaction.
	for _, coin := range f.InputCoins {
		fundingTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: coin.OutPoint,
		})
	}
	for _, theirInput := range extraInputs {
		fundingTx.AddTxIn(theirInput)
	}
	for _, ourChangeOutput := range f.ChangeOutputs {
		fundingTx.AddTxOut(ourChangeOutput)
	}
	for _, theirChangeOutput := range extraOutputs {
		fundingTx.AddTxOut(theirChangeOutput)
	}

	_, fundingOutput, err := f.FundingOutput()
	if err != nil {
		return nil, err
	}

	// Sort the transaction. Since both side agree to a canonical ordering,
	// by sorting we no longer need to send the entire transaction. Only
	// signatures will be exchanged.
	fundingTx.AddTxOut(fundingOutput)
	txsort.InPlaceSort(fundingTx)

	// Now that the funding tx has been fully assembled, we'll locate the
	// index of the funding output so we can create our final channel
	// point.
	_, multiSigIndex := input.FindScriptOutputIndex(
		fundingTx, fundingOutput.PkScript,
	)

	// Next, sign all inputs that are ours, collecting the signatures in
	// order of the inputs.
	prevOutFetcher := NewSegWitV0DualFundingPrevOutputFetcher(
		f.coinSource, extraInputs,
	)
	sigHashes := txscript.NewTxSigHashes(fundingTx, prevOutFetcher)
	for i, txIn := range fundingTx.TxIn {
		signDesc := input.SignDescriptor{
			SigHashes:         sigHashes,
			PrevOutputFetcher: prevOutFetcher,
		}
		// We can only sign this input if it's ours, so we'll ask the
		// coin source if it can map this outpoint into a coin we own.
		// If not, then we'll continue as it isn't our input.
		info, err := f.coinSource.CoinFromOutPoint(
			txIn.PreviousOutPoint,
		)
		if err != nil {
			continue
		}

		// Now that we know the input is ours, we'll populate the
		// signDesc with the per input unique information.
		signDesc.Output = &wire.TxOut{
			Value:    info.Value,
			PkScript: info.PkScript,
		}
		signDesc.InputIndex = i

		// We support spending a p2tr input ourselves. But not as part
		// of their inputs.
		signDesc.HashType = txscript.SigHashAll
		if txscript.IsPayToTaproot(info.PkScript) {
			signDesc.HashType = txscript.SigHashDefault
		}

		// Finally, we'll sign the input as is, and populate the input
		// with the witness and sigScript (if needed).
		inputScript, err := f.signer.ComputeInputScript(
			fundingTx, &signDesc,
		)
		if err != nil {
			return nil, err
		}

		txIn.SignatureScript = inputScript.SigScript
		txIn.Witness = inputScript.Witness
	}

	// Finally, we'll populate the chanPoint now that we've fully
	// constructed the funding transaction.
	f.chanPoint = &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: multiSigIndex,
	}

	return fundingTx, nil
}

// Inputs returns all inputs to the final funding transaction that we
// know about. Since this funding transaction is created all from our wallet,
// it will be all inputs.
func (f *FullIntent) Inputs() []wire.OutPoint {
	var ins []wire.OutPoint
	for _, coin := range f.InputCoins {
		ins = append(ins, coin.OutPoint)
	}

	return ins
}

// Outputs returns all outputs of the final funding transaction that we
// know about. This will be the funding output and the change outputs going
// back to our wallet.
func (f *FullIntent) Outputs() []*wire.TxOut {
	outs := f.ShimIntent.Outputs()
	outs = append(outs, f.ChangeOutputs...)

	return outs
}

// Cancel allows the caller to cancel a funding Intent at any time.  This will
// return any resources such as coins back to the eligible pool to be used in
// order channel fundings.
//
// NOTE: Part of the chanfunding.Intent interface.
func (f *FullIntent) Cancel() {
	for _, coin := range f.InputCoins {
		err := f.coinLeaser.ReleaseOutput(
			LndInternalLockID, coin.OutPoint,
		)
		if err != nil {
			log.Warnf("Failed to release UTXO %s (%v))",
				coin.OutPoint, err)
		}
	}

	f.ShimIntent.Cancel()
}

// A compile-time check to ensure FullIntent meets the Intent interface.
var _ Intent = (*FullIntent)(nil)

// WalletConfig is the main config of the WalletAssembler.
type WalletConfig struct {
	// CoinSource is what the WalletAssembler uses to list/locate coins.
	CoinSource CoinSource

	// CoinSelectionStrategy is the strategy that is used for selecting
	// coins when funding a transaction.
	CoinSelectionStrategy wallet.CoinSelectionStrategy

	// CoinSelectionLocker allows the WalletAssembler to gain exclusive
	// access to the current set of coins returned by the CoinSource.
	CoinSelectLocker CoinSelectionLocker

	// CoinLeaser is what the WalletAssembler uses to lease coins that may
	// be used as inputs for a new funding transaction.
	CoinLeaser OutputLeaser

	// Signer allows the WalletAssembler to sign inputs on any potential
	// funding transactions.
	Signer input.Signer

	// DustLimit is the current dust limit. We'll use this to ensure that
	// we don't make dust outputs on the funding transaction.
	DustLimit btcutil.Amount
}

// WalletAssembler is an instance of the Assembler interface that is backed by
// a full wallet. This variant of the Assembler interface will produce the
// entirety of the funding transaction within the wallet. This implements the
// typical funding flow that is initiated either on the p2p level or using the
// CLi.
type WalletAssembler struct {
	cfg WalletConfig
}

// NewWalletAssembler creates a new instance of the WalletAssembler from a
// fully populated wallet config.
func NewWalletAssembler(cfg WalletConfig) *WalletAssembler {
	return &WalletAssembler{
		cfg: cfg,
	}
}

// ProvisionChannel is the main entry point to begin a funding workflow given a
// fully populated request. The internal WalletAssembler will perform coin
// selection in a goroutine safe manner, returning an Intent that will allow
// the caller to finalize the funding process.
//
// NOTE: To cancel the funding flow the Cancel() method on the returned Intent,
// MUST be called.
//
// NOTE: This is a part of the chanfunding.Assembler interface.
func (w *WalletAssembler) ProvisionChannel(r *Request) (Intent, error) {
	var intent Intent

	// We hold the coin select mutex while querying for outputs, and
	// performing coin selection in order to avoid inadvertent double
	// spends across funding transactions.
	err := w.cfg.CoinSelectLocker.WithCoinSelectLock(func() error {
		log.Infof("Performing funding tx coin selection using %v "+
			"sat/kw as fee rate", int64(r.FeeRate))

		var (
			// allCoins refers to the entirety of coins in our
			// wallet that are available for funding a channel.
			allCoins []wallet.Coin

			// manuallySelectedCoins refers to the client-side
			// selected coins that should be considered available
			// for funding a channel.
			manuallySelectedCoins []wallet.Coin
			err                   error
		)

		// Convert manually selected outpoints to coins.
		manuallySelectedCoins, err = outpointsToCoins(
			r.Outpoints, w.cfg.CoinSource.CoinFromOutPoint,
		)
		if err != nil {
			return err
		}

		// Find all unlocked unspent witness outputs that satisfy the
		// minimum number of confirmations required. Coin selection in
		// this function currently ignores the configured coin selection
		// strategy.
		allCoins, err = w.cfg.CoinSource.ListCoins(
			r.MinConfs, math.MaxInt32,
		)
		if err != nil {
			return err
		}

		// Ensure that all manually selected coins remain unspent.
		unspent := make(map[wire.OutPoint]struct{})
		for _, coin := range allCoins {
			unspent[coin.OutPoint] = struct{}{}
		}
		for _, coin := range manuallySelectedCoins {
			if _, ok := unspent[coin.OutPoint]; !ok {
				return fmt.Errorf("outpoint already spent or "+
					"locked by another subsystem: %v",
					coin.OutPoint)
			}
		}

		// The coin selection algorithm requires to know what
		// inputs/outputs are already present in the funding
		// transaction and what a change output would look like. Since
		// a channel funding is always either a P2WSH or P2TR output,
		// we can use just P2WSH here (both of these output types have
		// the same length). And we currently don't support specifying a
		// change output type, so we always use P2TR.
		var fundingOutputWeight input.TxWeightEstimator
		fundingOutputWeight.AddP2WSHOutput()
		changeType := P2TRChangeAddress

		var (
			coins                []wallet.Coin
			selectedCoins        []wallet.Coin
			localContributionAmt btcutil.Amount
			changeAmt            btcutil.Amount
		)

		// If outputs were specified manually then we'll take the
		// corresponding coins as basis for coin selection. Otherwise,
		// all available coins from our wallet are used.
		coins = allCoins
		if len(manuallySelectedCoins) > 0 {
			coins = manuallySelectedCoins
		}

		// Perform coin selection over our available, unlocked unspent
		// outputs in order to find enough coins to meet the funding
		// amount requirements.
		switch {
		// If there's no funding amount at all (receiving an inbound
		// single funder request), then we don't need to perform any
		// coin selection at all.
		case r.LocalAmt == 0 && r.FundUpToMaxAmt == 0:
			break

		// The local funding amount cannot be used in combination with
		// the funding up to some maximum amount. If that is the case
		// we return an error.
		case r.LocalAmt != 0 && r.FundUpToMaxAmt != 0:
			return fmt.Errorf("cannot use a local funding amount " +
				"and fundmax parameters")

		// We cannot use the subtract fees flag while using the funding
		// up to some maximum amount. If that is the case we return an
		// error.
		case r.SubtractFees && r.FundUpToMaxAmt != 0:
			return fmt.Errorf("cannot subtract fees from local " +
				"amount while using fundmax parameters")

		// In case this request uses funding up to some maximum amount,
		// we will call the specialized coin selection function for
		// that.
		case r.FundUpToMaxAmt != 0 && r.MinFundAmt != 0:
			// We need to ensure that manually selected coins, which
			// are spent entirely on the channel funding, leave
			// enough funds in the wallet to cover for a reserve.
			reserve := r.WalletReserve
			if len(manuallySelectedCoins) > 0 {
				sumCoins := func(
					coins []wallet.Coin) btcutil.Amount {

					var sum btcutil.Amount
					for _, coin := range coins {
						sum += btcutil.Amount(
							coin.Value,
						)
					}

					return sum
				}

				sumManual := sumCoins(manuallySelectedCoins)
				sumAll := sumCoins(allCoins)

				// If sufficient reserve funds are available we
				// don't have to provide for it during coin
				// selection. The manually selected coins can be
				// spent entirely on the channel funding. If
				// the excess of coins cover the reserve
				// partially then we have to provide for the
				// rest during coin selection.
				excess := sumAll - sumManual
				if excess >= reserve {
					reserve = 0
				} else {
					reserve -= excess
				}
			}

			selectedCoins, localContributionAmt, changeAmt,
				err = CoinSelectUpToAmount(
				r.FeeRate, r.MinFundAmt, r.FundUpToMaxAmt,
				reserve, w.cfg.DustLimit, coins,
				w.cfg.CoinSelectionStrategy,
				fundingOutputWeight, changeType,
				DefaultMaxFeeRatio,
			)
			if err != nil {
				return err
			}

			// Now where the actual channel capacity is determined
			// we can check for local contribution constraints.
			//
			// Ensure that the remote channel reserve does not
			// exceed 20% of the channel capacity.
			if r.RemoteChanReserve >= localContributionAmt/5 {
				return fmt.Errorf("remote channel reserve " +
					"must be less than the %%20 of the " +
					"channel capacity")
			}
			// Ensure that the initial remote balance does not
			// exceed our local contribution as that would leave a
			// negative balance on our side.
			if r.PushAmt >= localContributionAmt {
				return fmt.Errorf("amount pushed to remote " +
					"peer for initial state must be " +
					"below the local funding amount")
			}

		// In case this request want the fees subtracted from the local
		// amount, we'll call the specialized method for that. This
		// ensures that we won't deduct more that the specified balance
		// from our wallet.
		case r.SubtractFees:
			dustLimit := w.cfg.DustLimit
			selectedCoins, localContributionAmt, changeAmt,
				err = CoinSelectSubtractFees(
				r.FeeRate, r.LocalAmt, dustLimit, coins,
				w.cfg.CoinSelectionStrategy,
				fundingOutputWeight, changeType,
				DefaultMaxFeeRatio,
			)
			if err != nil {
				return err
			}

		// Otherwise do a normal coin selection where we target a given
		// funding amount.
		default:
			dustLimit := w.cfg.DustLimit
			localContributionAmt = r.LocalAmt
			selectedCoins, changeAmt, err = CoinSelect(
				r.FeeRate, r.LocalAmt, dustLimit, coins,
				w.cfg.CoinSelectionStrategy,
				fundingOutputWeight, changeType,
				DefaultMaxFeeRatio,
			)
			if err != nil {
				return err
			}
		}

		// Sanity check: The addition of the outputs should not lead to the
		// creation of dust.
		if changeAmt != 0 && changeAmt < w.cfg.DustLimit {
			return fmt.Errorf("change amount(%v) after coin "+
				"select is below dust limit(%v)", changeAmt,
				w.cfg.DustLimit)
		}

		// Record any change output(s) generated as a result of the
		// coin selection.
		var changeOutput *wire.TxOut
		if changeAmt != 0 {
			changeAddr, err := r.ChangeAddr()
			if err != nil {
				return err
			}
			changeScript, err := txscript.PayToAddrScript(changeAddr)
			if err != nil {
				return err
			}

			changeOutput = &wire.TxOut{
				Value:    int64(changeAmt),
				PkScript: changeScript,
			}
		}

		// Lock the selected coins. These coins are now "reserved",
		// this prevents concurrent funding requests from referring to
		// and this double-spending the same set of coins.
		for _, coin := range selectedCoins {
			outpoint := coin.OutPoint

			_, err = w.cfg.CoinLeaser.LeaseOutput(
				LndInternalLockID, outpoint,
				DefaultReservationTimeout,
			)
			if err != nil {
				return err
			}
		}

		newIntent := &FullIntent{
			ShimIntent: ShimIntent{
				localFundingAmt:  localContributionAmt,
				remoteFundingAmt: r.RemoteAmt,
				musig2:           r.Musig2,
				tapscriptRoot:    r.TapscriptRoot,
			},
			InputCoins: selectedCoins,
			coinLeaser: w.cfg.CoinLeaser,
			coinSource: w.cfg.CoinSource,
			signer:     w.cfg.Signer,
		}

		if changeOutput != nil {
			newIntent.ChangeOutputs = []*wire.TxOut{changeOutput}
		}

		intent = newIntent

		return nil
	})
	if err != nil {
		return nil, err
	}

	return intent, nil
}

// outpointsToCoins maps outpoints to coins in our wallet iff these coins are
// existent and returns an error otherwise.
func outpointsToCoins(outpoints []wire.OutPoint,
	coinFromOutPoint func(wire.OutPoint) (*wallet.Coin, error)) (
	[]wallet.Coin, error) {

	var selectedCoins []wallet.Coin
	for _, outpoint := range outpoints {
		coin, err := coinFromOutPoint(
			outpoint,
		)
		if err != nil {
			return nil, err
		}
		selectedCoins = append(
			selectedCoins, *coin,
		)
	}

	return selectedCoins, nil
}

// FundingTxAvailable is an empty method that an assembler can implement to
// signal to callers that its able to provide the funding transaction for the
// channel via the intent it returns.
//
// NOTE: This method is a part of the FundingTxAssembler interface.
func (w *WalletAssembler) FundingTxAvailable() {}

// A compile-time assertion to ensure the WalletAssembler meets the
// FundingTxAssembler interface.
var _ FundingTxAssembler = (*WalletAssembler)(nil)

// SegWitV0DualFundingPrevOutputFetcher is a txscript.PrevOutputFetcher that
// knows about local and remote funding inputs.
//
// TODO(guggero): Support dual funding with p2tr inputs, currently only segwit
// v0 inputs are supported.
type SegWitV0DualFundingPrevOutputFetcher struct {
	local  CoinSource
	remote *txscript.MultiPrevOutFetcher
}

var _ txscript.PrevOutputFetcher = (*SegWitV0DualFundingPrevOutputFetcher)(nil)

// NewSegWitV0DualFundingPrevOutputFetcher creates a new
// txscript.PrevOutputFetcher from the given local and remote inputs.
//
// NOTE: Since the actual pkScript and amounts aren't passed in, this will just
// make sure that nothing will panic when creating a SegWit v0 sighash. But this
// code will NOT WORK for transactions that spend any _remote_ Taproot inputs!
// So basically dual-funding won't work with Taproot inputs unless the UTXO info
// is exchanged between the peers.
func NewSegWitV0DualFundingPrevOutputFetcher(localSource CoinSource,
	remoteInputs []*wire.TxIn) txscript.PrevOutputFetcher {

	remote := txscript.NewMultiPrevOutFetcher(nil)
	for _, inp := range remoteInputs {
		// We add an empty output to prevent the sighash calculation
		// from panicking. But this will always detect the inputs as
		// SegWig v0!
		remote.AddPrevOut(inp.PreviousOutPoint, &wire.TxOut{})
	}
	return &SegWitV0DualFundingPrevOutputFetcher{
		local:  localSource,
		remote: remote,
	}
}

// FetchPrevOutput attempts to fetch the previous output referenced by the
// passed outpoint.
//
// NOTE: This is a part of the txscript.PrevOutputFetcher interface.
func (d *SegWitV0DualFundingPrevOutputFetcher) FetchPrevOutput(
	op wire.OutPoint) *wire.TxOut {

	// Try the local source first. This will return nil if our internal
	// wallet doesn't know the outpoint.
	coin, err := d.local.CoinFromOutPoint(op)
	if err == nil && coin != nil {
		return &coin.TxOut
	}

	// Fall back to the remote
	return d.remote.FetchPrevOutput(op)
}
