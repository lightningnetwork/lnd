//go:build walletrpc
// +build walletrpc

package commands

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/urfave/cli"
)

var (
	// psbtCommand is a wallet subcommand that is responsible for PSBT
	// operations.
	psbtCommand = cli.Command{
		Name: "psbt",
		Usage: "Interact with partially signed bitcoin transactions " +
			"(PSBTs).",
		Subcommands: []cli.Command{
			fundPsbtCommand,
			fundTemplatePsbtCommand,
			finalizePsbtCommand,
		},
	}

	// accountsCommand is a wallet subcommand that is responsible for
	// account management operations.
	accountsCommand = cli.Command{
		Name:  "accounts",
		Usage: "Interact with wallet accounts.",
		Subcommands: []cli.Command{
			listAccountsCommand,
			importAccountCommand,
			importPubKeyCommand,
		},
	}

	// addressesCommand is a wallet subcommand that is responsible for
	// address management operations.
	addressesCommand = cli.Command{
		Name:  "addresses",
		Usage: "Interact with wallet addresses.",
		Subcommands: []cli.Command{
			listAddressesCommand,
			signMessageWithAddrCommand,
			verifyMessageWithAddrCommand,
		},
	}

	p2TrChangeType = walletrpc.ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR
)

// walletCommands will return the set of commands to enable for walletrpc
// builds.
func walletCommands() []cli.Command {
	return []cli.Command{
		{
			Name:        "wallet",
			Category:    "Wallet",
			Usage:       "Interact with the wallet.",
			Description: "",
			Subcommands: []cli.Command{
				estimateFeeRateCommand,
				pendingSweepsCommand,
				bumpFeeCommand,
				bumpCloseFeeCommand,
				bumpForceCloseFeeCommand,
				listSweepsCommand,
				labelTxCommand,
				publishTxCommand,
				getTxCommand,
				removeTxCommand,
				releaseOutputCommand,
				leaseOutputCommand,
				listLeasesCommand,
				psbtCommand,
				accountsCommand,
				requiredReserveCommand,
				addressesCommand,
			},
		},
	}
}

func parseAddrType(addrTypeStr string) (walletrpc.AddressType, error) {
	switch addrTypeStr {
	case "":
		return walletrpc.AddressType_UNKNOWN, nil
	case "p2wkh":
		return walletrpc.AddressType_WITNESS_PUBKEY_HASH, nil
	case "np2wkh":
		return walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH, nil
	case "np2wkh-p2wkh":
		return walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH, nil
	case "p2tr":
		return walletrpc.AddressType_TAPROOT_PUBKEY, nil
	default:
		return 0, errors.New("invalid address type, supported address " +
			"types are: p2wkh, p2tr, np2wkh, and np2wkh-p2wkh")
	}
}

func getWalletClient(ctx *cli.Context) (walletrpc.WalletKitClient, func()) {
	conn := getClientConn(ctx, false)
	cleanUp := func() {
		conn.Close()
	}
	return walletrpc.NewWalletKitClient(conn), cleanUp
}

var estimateFeeRateCommand = cli.Command{
	Name: "estimatefeerate",
	Usage: "Estimates the on-chain fee rate to achieve a confirmation " +
		"target.",
	ArgsUsage: "conf_target",
	Description: `
	Returns the fee rate estimate for on-chain transactions in sat/kw and
	sat/vb to achieve a given confirmation target. The source of the fee
	rate depends on the configuration and is either the on-chain backend or
	alternatively an external URL.
	`,
	Action: actionDecorator(estimateFeeRate),
}

func estimateFeeRate(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	confTarget, err := strconv.ParseInt(ctx.Args().First(), 10, 64)
	if err != nil {
		return cli.ShowCommandHelp(ctx, "estimatefeerate")
	}

	if confTarget <= 0 || confTarget > math.MaxInt32 {
		return errors.New("conf_target out of range")
	}

	resp, err := client.EstimateFee(ctxc, &walletrpc.EstimateFeeRequest{
		ConfTarget: int32(confTarget),
	})
	if err != nil {
		return err
	}

	rateKW := chainfee.SatPerKWeight(resp.SatPerKw)
	rateVB := rateKW.FeePerVByte()
	relayFeeKW := chainfee.SatPerKWeight(resp.MinRelayFeeSatPerKw)
	relayFeeVB := relayFeeKW.FeePerVByte()

	printJSON(struct {
		SatPerKw            int64 `json:"sat_per_kw"`
		SatPerVByte         int64 `json:"sat_per_vbyte"`
		MinRelayFeeSatPerKw int64 `json:"min_relay_fee_sat_per_kw"`
		//nolint:ll
		MinRelayFeeSatPerVByte int64 `json:"min_relay_fee_sat_per_vbyte"`
	}{
		SatPerKw:               int64(rateKW),
		SatPerVByte:            int64(rateVB),
		MinRelayFeeSatPerKw:    int64(relayFeeKW),
		MinRelayFeeSatPerVByte: int64(relayFeeVB),
	})

	return nil
}

var pendingSweepsCommand = cli.Command{
	Name:      "pendingsweeps",
	Usage:     "List all outputs that are pending to be swept within lnd.",
	ArgsUsage: "",
	Description: `
	List all on-chain outputs that lnd is currently attempting to sweep
	within its central batching engine. Outputs with similar fee rates are
	batched together in order to sweep them within a single transaction.
	`,
	Flags:  []cli.Flag{},
	Action: actionDecorator(pendingSweeps),
}

func pendingSweeps(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.PendingSweepsRequest{}
	resp, err := client.PendingSweeps(ctxc, req)
	if err != nil {
		return err
	}

	// Sort them in ascending fee rate order for display purposes.
	sort.Slice(resp.PendingSweeps, func(i, j int) bool {
		return resp.PendingSweeps[i].SatPerVbyte <
			resp.PendingSweeps[j].SatPerVbyte
	})

	var pendingSweepsResp = struct {
		PendingSweeps []*PendingSweep `json:"pending_sweeps"`
	}{
		PendingSweeps: make([]*PendingSweep, 0, len(resp.PendingSweeps)),
	}

	for _, protoPendingSweep := range resp.PendingSweeps {
		pendingSweep := NewPendingSweepFromProto(protoPendingSweep)
		pendingSweepsResp.PendingSweeps = append(
			pendingSweepsResp.PendingSweeps, pendingSweep,
		)
	}

	printJSON(pendingSweepsResp)

	return nil
}

var bumpFeeCommand = cli.Command{
	Name:      "bumpfee",
	Usage:     "Bumps the fee of an arbitrary input/transaction.",
	ArgsUsage: "outpoint",
	Description: `
	BumpFee is an endpoint that allows users to interact with lnd's sweeper
	directly. It takes an outpoint from an unconfirmed transaction and
	sends it to the sweeper for potential fee bumping. Depending on whether
	the outpoint has been registered in the sweeper (an existing input,
	e.g., an anchor output) or not (a new input, e.g., an unconfirmed
	wallet utxo), this will either be an RBF or CPFP attempt.

	When receiving an input, lnd’s sweeper needs to understand its time
	sensitivity to make economical fee bumps - internally a fee function is
	created using the deadline and budget to guide the process. When the
	deadline is approaching, the fee function will increase the fee rate
	and perform an RBF.

	When a force close happens, all the outputs from the force closing
	transaction will be registered in the sweeper. The sweeper will then
	handle the creation, publish, and fee bumping of the sweeping
	transactions. Everytime a new block comes in, unless the sweeping
	transaction is confirmed, an RBF is attempted. To interfere with this
	automatic process, users can use BumpFee to specify customized fee
	rate, budget, deadline, and whether the sweep should happen
	immediately. It's recommended to call listsweeps to understand the
	shape of the existing sweeping transaction first - depending on the
	number of inputs in this transaction, the RBF requirements can be quite
	different.

	This RPC also serves useful when wanting to perform a
	Child-Pays-For-Parent (CPFP), where the child transaction pays for its
	parent's fee. This can be done by specifying an outpoint within the low
	fee transaction that is under the control of the wallet.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: `
	The conf target is the starting fee rate of the fee function expressed
	in number of blocks. So instead of using sat_per_vbyte the conf target
	can be specified and LND will query its fee estimator for the current
	fee rate for the given target.`,
		},
		cli.Uint64Flag{
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.BoolFlag{
			Name:   "force",
			Usage:  "Deprecated, use immediate instead.",
			Hidden: true,
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: `
	The starting fee rate, expressed in sat/vbyte, that will be used to
	spend the input with initially. This value will be used by the
	sweeper's fee function as its starting fee rate. When not set, the
	sweeper will use the estimated fee rate using the target_conf as the
	starting fee rate.`,
		},
		cli.BoolFlag{
			Name: "immediate",
			Usage: `
	Whether this input will be swept immediately. When set to true, the
	sweeper will sweep this input without waiting for the next batch.`,
		},
		cli.Uint64Flag{
			Name: "budget",
			Usage: `
	The max amount in sats that can be used as the fees. Setting this value
	greater than the input's value may result in CPFP - one or more wallet
	utxos will be used to pay the fees specified by the budget. If not set,
	for new inputs, by default 50% of the input's value will be treated as
	the budget for fee bumping; for existing inputs, their current budgets
	will be retained.`,
		},
		cli.Uint64Flag{
			Name: "deadline_delta",
			Usage: `
	The deadline delta in number of blocks that this input should be spent
	within to bump the transaction. When specified also a budget value is
	required. When the deadline is reached, ALL the budget will be spent as
	fee.`,
		},
	},
	Action: actionDecorator(bumpFee),
}

func bumpFee(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "bumpfee")
	}

	// Validate and parse the relevant arguments/flags.
	protoOutPoint, err := NewProtoOutPoint(ctx.Args().Get(0))
	if err != nil {
		return err
	}

	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	// Parse immediate flag (force flag was deprecated).
	immediate := false
	switch {
	case ctx.IsSet("immediate") && ctx.IsSet("force"):
		return fmt.Errorf("cannot set immediate and force flag at " +
			"the same time")

	case ctx.Bool("immediate"):
		immediate = true

	case ctx.Bool("force"):
		immediate = true
	}

	resp, err := client.BumpFee(ctxc, &walletrpc.BumpFeeRequest{
		Outpoint:      protoOutPoint,
		TargetConf:    uint32(ctx.Uint64("conf_target")),
		Immediate:     immediate,
		Budget:        ctx.Uint64("budget"),
		SatPerVbyte:   ctx.Uint64("sat_per_vbyte"),
		DeadlineDelta: uint32(ctx.Uint64("deadline_delta")),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var bumpCloseFeeCommand = cli.Command{
	Name:      "bumpclosefee",
	Usage:     "Bumps the fee of a channel force closing transaction.",
	ArgsUsage: "channel_point",
	Hidden:    true,
	Description: `
	This command works only for unilateral closes of anchor channels. It
	allows the fee of a channel force closing transaction to be increased by
	using the child-pays-for-parent mechanism. It will instruct the sweeper
	to sweep the anchor outputs of the closing transaction at the requested
	fee rate or confirmation target. The specified fee rate will be the
	effective fee rate taking the parent fee into account.
	NOTE: This cmd is DEPRECATED please use bumpforceclosefee instead.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: `
	The conf target is the starting fee rate of the fee function expressed
	in number of blocks. So instead of using sat_per_vbyte the conf target
	can be specified and LND will query its fee estimator for the current
	fee rate for the given target.`,
		},
		cli.Uint64Flag{
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.BoolFlag{
			Name:   "force",
			Usage:  "Deprecated, use immediate instead.",
			Hidden: true,
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: `
	The starting fee rate, expressed in sat/vbyte, that will be used to
	spend the input with initially. This value will be used by the
	sweeper's fee function as its starting fee rate. When not set, the
	sweeper will use the estimated fee rate using the target_conf as the
	starting fee rate.`,
		},
		cli.BoolFlag{
			Name: "immediate",
			Usage: `
	Whether this input will be swept immediately. When set to true, the
	sweeper will sweep this input without waiting for the next batch.`,
		},
		cli.Uint64Flag{
			Name: "budget",
			Usage: `
	The max amount in sats that can be used as the fees. Setting this value
	greater than the input's value may result in CPFP - one or more wallet
	utxos will be used to pay the fees specified by the budget. If not set,
	for new inputs, by default 50% of the input's value will be treated as
	the budget for fee bumping; for existing inputs, their current budgets
	will be retained.`,
		},
	},
	Action: actionDecorator(bumpForceCloseFee),
}

var bumpForceCloseFeeCommand = cli.Command{
	Name:      "bumpforceclosefee",
	Usage:     "Bumps the fee of a channel force closing transaction.",
	ArgsUsage: "channel_point",
	Description: `
	This command works only for unilateral closes of anchor channels. It
	allows the fee of a channel force closing transaction to be increased by
	using the child-pays-for-parent mechanism. It will instruct the sweeper
	to sweep the anchor outputs of the closing transaction at the requested
	confirmation target and limit the fees to the specified budget.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: `
	The conf target is the starting fee rate of the fee function expressed
	in number of blocks. So instead of using sat_per_vbyte the conf target
	can be specified and LND will query its fee estimator for the current
	fee rate for the given target.`,
		},
		cli.Uint64Flag{
			Name: "deadline_delta",
			Usage: `
	The deadline delta in number of blocks that the anchor output should
	be spent within to bump the closing transaction. When the deadline is
	reached, ALL the budget will be spent as fees.`,
		},
		cli.Uint64Flag{
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: `
	The starting fee rate, expressed in sat/vbyte. This value will be used
	by the sweeper's fee function as its starting fee rate. When not set,
	the sweeper will use the estimated fee rate using the target_conf as the
	starting fee rate.`,
		},
		cli.BoolFlag{
			Name:   "force",
			Usage:  "Deprecated, use immediate instead.",
			Hidden: true,
		},
		cli.BoolFlag{
			Name: "immediate",
			Usage: `
	Whether this cpfp transaction will be triggered immediately. When set to
	true, the sweeper will consider all currently pending registered sweeps
	and trigger new batch transactions including the sweeping of the anchor 
	output related to the selected force close transaction.`,
		},
		cli.Uint64Flag{
			Name: "budget",
			Usage: `
	The max amount in sats that can be used as the fees. For already
	registered anchor outputs if not set explicitly the old value will be
	used. For channel force closes which have no HTLCs in their commitment
	transaction this value has to be set to an appropriate amount to pay for
	the cpfp transaction of the force closed channel otherwise the fee 
	bumping will fail.`,
		},
	},
	Action: actionDecorator(bumpForceCloseFee),
}

func bumpForceCloseFee(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "bumpclosefee")
	}

	// Validate the channel point.
	channelPoint := ctx.Args().Get(0)
	rpcChannelPoint, err := parseChanPoint(channelPoint)
	if err != nil {
		return err
	}

	// `sat_per_byte` was deprecated we only use sats/vbyte now.
	if ctx.IsSet("sat_per_byte") {
		return fmt.Errorf("deprecated, use sat_per_vbyte instead")
	}

	// Retrieve pending sweeps.
	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	// Parse immediate flag (force flag was deprecated).
	if ctx.IsSet("immediate") && ctx.IsSet("force") {
		return fmt.Errorf("cannot set immediate and force flag at " +
			"the same time")
	}
	immediate := ctx.Bool("immediate") || ctx.Bool("force")

	resp, err := walletClient.BumpForceCloseFee(
		ctxc, &walletrpc.BumpForceCloseFeeRequest{
			ChanPoint:       rpcChannelPoint,
			Budget:          ctx.Uint64("budget"),
			Immediate:       immediate,
			StartingFeerate: ctx.Uint64("sat_per_vbyte"),
			TargetConf:      uint32(ctx.Uint64("conf_target")),
			DeadlineDelta:   uint32(ctx.Uint64("deadline_delta")),
		})
	if err != nil {
		return err
	}

	fmt.Printf("BumpForceCloseFee result: %s\n", resp.Status)

	return nil
}

var listSweepsCommand = cli.Command{
	Name:  "listsweeps",
	Usage: "Lists all sweeps that have been published by our node.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose",
			Usage: "lookup full transaction",
		},
		cli.IntFlag{
			Name: "startheight",
			Usage: "The start height to use when fetching " +
				"sweeps. If not specified (0), the result " +
				"will start from the earliest sweep. If set " +
				"to -1 the result will only include " +
				"unconfirmed sweeps (at the time of the call).",
		},
	},
	Description: `
	Get a list of the hex-encoded transaction ids of every sweep that our
	node has published. Note that these sweeps may not be confirmed on chain
	yet, as we store them on transaction broadcast, not confirmation.

	If the verbose flag is set, the full set of transactions will be 
	returned, otherwise only the sweep transaction ids will be returned. 
	`,
	Action: actionDecorator(listSweeps),
}

func listSweeps(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	resp, err := client.ListSweeps(
		ctxc, &walletrpc.ListSweepsRequest{
			Verbose:     ctx.IsSet("verbose"),
			StartHeight: int32(ctx.Int("startheight")),
		},
	)
	if err != nil {
		return err
	}

	printJSON(resp)

	return nil
}

var labelTxCommand = cli.Command{
	Name:      "labeltx",
	Usage:     "Adds a label to a transaction.",
	ArgsUsage: "txid label",
	Description: `
	Add a label to a transaction. If the transaction already has a label, 
	this call will fail unless the overwrite option is set. The label is 
	limited to 500 characters. Note that multi word labels must be contained
	in quotation marks ("").
	`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "overwrite",
			Usage: "set to overwrite existing labels",
		},
	},
	Action: actionDecorator(labelTransaction),
}

func labelTransaction(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 2 {
		return cli.ShowCommandHelp(ctx, "labeltx")
	}

	// Get the transaction id and check that it is a valid hash.
	txid := ctx.Args().Get(0)
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return err
	}

	label := ctx.Args().Get(1)

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	_, err = walletClient.LabelTransaction(
		ctxc, &walletrpc.LabelTransactionRequest{
			Txid:      hash[:],
			Label:     label,
			Overwrite: ctx.Bool("overwrite"),
		},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Transaction: %v labelled with: %v\n", txid, label)

	return nil
}

var publishTxCommand = cli.Command{
	Name:      "publishtx",
	Usage:     "Attempts to publish the passed transaction to the network.",
	ArgsUsage: "tx_hex",
	Description: `
	Publish a hex-encoded raw transaction to the on-chain network. The 
	wallet will continually attempt to re-broadcast the transaction on start up, until it 
	enters the chain. The label parameter is optional and limited to 500 characters. Note 
	that multi word labels must be contained in quotation marks ("").
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "label",
			Usage: "(optional) transaction label",
		},
	},
	Action: actionDecorator(publishTransaction),
}

func publishTransaction(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 || ctx.NumFlags() > 1 {
		return cli.ShowCommandHelp(ctx, "publishtx")
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	tx, err := hex.DecodeString(ctx.Args().First())
	if err != nil {
		return err
	}

	// Deserialize the transaction to get the transaction hash.
	msgTx := &wire.MsgTx{}
	txReader := bytes.NewReader(tx)
	if err := msgTx.Deserialize(txReader); err != nil {
		return err
	}

	req := &walletrpc.Transaction{
		TxHex: tx,
	}

	_, err = walletClient.PublishTransaction(ctxc, req)
	if err != nil {
		return err
	}

	printJSON(&struct {
		TXID string `json:"txid"`
	}{
		TXID: msgTx.TxHash().String(),
	})

	return nil
}

var getTxCommand = cli.Command{
	Name:      "gettx",
	Usage:     "Returns details of a transaction.",
	ArgsUsage: "txid",
	Description: `
	Query the transaction using the given transaction id and return its 
	details. An error is returned if the transaction is not found.
	`,
	Action: actionDecorator(getTransaction),
}

func getTransaction(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "gettx")
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.GetTransactionRequest{
		Txid: ctx.Args().First(),
	}

	res, err := walletClient.GetTransaction(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(res)

	return nil
}

var removeTxCommand = cli.Command{
	Name: "removetx",
	Usage: "Attempts to remove the unconfirmed transaction with the " +
		"specified txid and all its children from the underlying " +
		"internal wallet.",
	ArgsUsage: "txid",
	Description: `
	Removes the transaction with the specified txid from the underlying 
	wallet which must still be unconfirmmed (in mempool). This command is 
	useful when a transaction is RBFed by another transaction. The wallet 
	will only resolve this conflict when the other transaction is mined 
	(which can take time). If a transaction was removed erronously a simple
	rebroadcast of the former transaction with the "publishtx" cmd will
	register the relevant outputs of the raw tx again with the wallet
	(if there are no errors broadcasting this transaction due to an RBF
	replacement sitting in the mempool). As soon as a removed transaction
	is confirmed funds will be registered with the wallet again.`,
	Flags:  []cli.Flag{},
	Action: actionDecorator(removeTransaction),
}

func removeTransaction(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "removetx")
	}

	// Fetch the only cmd argument which must be a valid txid.
	txid := ctx.Args().First()
	txHash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return err
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.GetTransactionRequest{
		Txid: txHash.String(),
	}

	resp, err := walletClient.RemoveTransaction(ctxc, req)
	if err != nil {
		return err
	}

	printJSON(&struct {
		Status string `json:"status"`
		TxID   string `json:"txid"`
	}{
		Status: resp.GetStatus(),
		TxID:   txHash.String(),
	})

	return nil
}

// utxoLease contains JSON annotations for a lease on an unspent output.
type utxoLease struct {
	ID         string   `json:"id"`
	OutPoint   OutPoint `json:"outpoint"`
	Expiration uint64   `json:"expiration"`
	PkScript   []byte   `json:"pk_script"`
	Value      uint64   `json:"value"`
}

// fundPsbtResponse is a struct that contains JSON annotations for nice result
// serialization.
type fundPsbtResponse struct {
	Psbt              string       `json:"psbt"`
	ChangeOutputIndex int32        `json:"change_output_index"`
	Locks             []*utxoLease `json:"locks"`
}

var fundTemplatePsbtCommand = cli.Command{
	Name: "fundtemplate",
	Usage: "Fund a Partially Signed Bitcoin Transaction (PSBT) from a " +
		"template.",
	ArgsUsage: "[--template_psbt=T | [--outputs=O [--inputs=I]]] " +
		"[--conf_target=C | --sat_per_vbyte=S] " +
		"[--change_type=A] [--change_output_index=I]",
	Description: `
	The fund command creates a fully populated PSBT that contains enough
	inputs to fund the outputs specified in either the template.

	The main difference to the 'fund' command is that the template PSBT
	is allowed to already contain both inputs and outputs and coin selection
	and fee estimation is still performed.

	If '--inputs' and '--outputs' are provided instead of a template, then
	those are used to create a new PSBT template.

	The 'outputs' flag decodes addresses and the amount to send respectively
	in the following JSON format:

	    --outputs='["ExampleAddr:NumCoinsInSatoshis", "SecondAddr:Sats"]'

	The 'outputs' format is different from the 'fund' command as the order
	is important for being able to specify the change output index, so an
	array is used rather than a map.

	The optional 'inputs' flag decodes a JSON list of UTXO outpoints as
	returned by the listunspent command for example:

	    --inputs='["<txid1>:<output-index1>","<txid2>:<output-index2>",...]

	Any inputs specified that belong to this lnd node MUST be locked/leased
	(by using 'lncli wallet leaseoutput') manually to make sure they aren't
	selected again by the coin selection algorithm.

	After verifying and possibly adding new inputs, all input UTXOs added by
	the command are locked with an internal app ID. Inputs already present
	in the template are NOT locked, as they must already be locked when
	invoking the command.

	The '--change_output_index' flag can be used to specify the index of the
	output in the PSBT that should be used as the change output. If '-1' is
	specified, the wallet will automatically add a change output if one is
	required!

	The optional '--change-type' flag permits to choose the address type
	for the change for default accounts and single imported public keys.
	The custom address type can only be p2tr at the moment (p2wkh will be
	used by default). No custom address type should be provided for custom
	accounts as we will always generate the change address using the coin
	selection key scope.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "template_psbt",
			Usage: "the outputs to fund and optional inputs to " +
				"spend provided in the base64 PSBT format",
		},
		cli.StringFlag{
			Name: "outputs",
			Usage: "a JSON compatible map of destination " +
				"addresses to amounts to send, must not " +
				"include a change address as that will be " +
				"added automatically by the wallet",
		},
		cli.StringFlag{
			Name: "inputs",
			Usage: "an optional JSON compatible list of UTXO " +
				"outpoints to use as the PSBT's inputs",
		},
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks that the transaction " +
				"should be confirmed on-chain within",
			Value: 6,
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "a manual fee expressed in sat/vbyte that " +
				"should be used when creating the transaction",
		},
		cli.StringFlag{
			Name: "account",
			Usage: "(optional) the name of the account to use to " +
				"create/fund the PSBT",
		},
		cli.StringFlag{
			Name: "change_type",
			Usage: "(optional) the type of the change address to " +
				"use to create/fund the PSBT. If no address " +
				"type is provided, p2wpkh will be used for " +
				"default accounts and single imported public " +
				"keys. No custom address type should be " +
				"provided for custom accounts as we will " +
				"always use the coin selection key scope to " +
				"generate the change address",
		},
		cli.Uint64Flag{
			Name: "min_confs",
			Usage: "(optional) the minimum number of " +
				"confirmations each input used for the PSBT " +
				"transaction must satisfy",
			Value: defaultUtxoMinConf,
		},
		cli.IntFlag{
			Name: "change_output_index",
			Usage: "(optional) define an existing output in the " +
				"PSBT template that should be used as the " +
				"change output. The value of -1 means a " +
				"change output will be added automatically " +
				"if required",
			Value: -1,
		},
		coinSelectionStrategyFlag,
	},
	Action: actionDecorator(fundTemplatePsbt),
}

// fundTemplatePsbt implements the fundtemplate sub command.
//
//nolint:funlen
func fundTemplatePsbt(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if there aren't any flags
	// specified.
	if ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "fund")
	}

	chainParams, err := networkParams(ctx)
	if err != nil {
		return err
	}

	coinSelect := &walletrpc.PsbtCoinSelect{}

	// Parse template flags.
	switch {
	// The PSBT flag is mutually exclusive with the outputs/inputs flags.
	case ctx.IsSet("template_psbt") &&
		(ctx.IsSet("inputs") || ctx.IsSet("outputs")):

		return fmt.Errorf("cannot set template_psbt and inputs/" +
			"outputs flags at the same time")

	// Use a pre-existing PSBT as the transaction template.
	case len(ctx.String("template_psbt")) > 0:
		psbtBase64 := ctx.String("template_psbt")
		psbtBytes, err := base64.StdEncoding.DecodeString(psbtBase64)
		if err != nil {
			return err
		}

		coinSelect.Psbt = psbtBytes

	// The user manually specified outputs and/or inputs in JSON
	// format.
	case len(ctx.String("outputs")) > 0 || len(ctx.String("inputs")) > 0:
		var (
			inputs  []*wire.OutPoint
			outputs []*wire.TxOut
		)

		if len(ctx.String("outputs")) > 0 {
			var outputStrings []string

			// Parse the address to amount map as JSON now. At least
			// one entry must be present.
			jsonMap := []byte(ctx.String("outputs"))
			err := json.Unmarshal(jsonMap, &outputStrings)
			if err != nil {
				return fmt.Errorf("error parsing outputs "+
					"JSON: %w", err)
			}

			// Parse the addresses and amounts into a slice of
			// transaction outputs.
			for idx, addrAndAmount := range outputStrings {
				parts := strings.Split(addrAndAmount, ":")
				if len(parts) != 2 {
					return fmt.Errorf("invalid output "+
						"format at index %d", idx)
				}

				addrStr, amountStr := parts[0], parts[1]
				amount, err := strconv.ParseInt(
					amountStr, 10, 64,
				)
				if err != nil {
					return fmt.Errorf("error parsing "+
						"amount at index %d: %w", idx,
						err)
				}

				addr, err := btcutil.DecodeAddress(
					addrStr, chainParams,
				)
				if err != nil {
					return fmt.Errorf("error parsing "+
						"address at index %d: %w", idx,
						err)
				}

				pkScript, err := txscript.PayToAddrScript(addr)
				if err != nil {
					return fmt.Errorf("error creating pk "+
						"script for address at index "+
						"%d: %w", idx, err)
				}

				outputs = append(outputs, &wire.TxOut{
					PkScript: pkScript,
					Value:    amount,
				})
			}
		}

		// Inputs are optional.
		if len(ctx.String("inputs")) > 0 {
			var inputStrings []string

			jsonList := []byte(ctx.String("inputs"))
			err := json.Unmarshal(jsonList, &inputStrings)
			if err != nil {
				return fmt.Errorf("error parsing inputs JSON: "+
					"%w", err)
			}

			for idx, input := range inputStrings {
				op, err := wire.NewOutPointFromString(input)
				if err != nil {
					return fmt.Errorf("error parsing "+
						"UTXO outpoint %d: %w", idx,
						err)
				}
				inputs = append(inputs, op)
			}
		}

		packet, err := psbt.New(
			inputs, outputs, 2, 0, make([]uint32, len(inputs)),
		)
		if err != nil {
			return fmt.Errorf("error creating template PSBT: %w",
				err)
		}

		var buf bytes.Buffer
		err = packet.Serialize(&buf)
		if err != nil {
			return fmt.Errorf("error serializing template PSBT: %w",
				err)
		}

		coinSelect.Psbt = buf.Bytes()

	default:
		return fmt.Errorf("must specify either template_psbt or " +
			"inputs/outputs flag")
	}

	coinSelectionStrategy, err := parseCoinSelectionStrategy(ctx)
	if err != nil {
		return err
	}

	minConfs := int32(ctx.Uint64("min_confs"))
	req := &walletrpc.FundPsbtRequest{
		Account:          ctx.String("account"),
		MinConfs:         minConfs,
		SpendUnconfirmed: minConfs == 0,
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: coinSelect,
		},
		CoinSelectionStrategy: coinSelectionStrategy,
	}

	// Parse fee flags.
	switch {
	case ctx.IsSet("conf_target") && ctx.IsSet("sat_per_vbyte"):
		return fmt.Errorf("cannot set conf_target and sat_per_vbyte " +
			"at the same time")

	case ctx.Uint64("sat_per_vbyte") > 0:
		req.Fees = &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: ctx.Uint64("sat_per_vbyte"),
		}

	// Check conf_target last because it has a default value.
	case ctx.Uint64("conf_target") > 0:
		req.Fees = &walletrpc.FundPsbtRequest_TargetConf{
			TargetConf: uint32(ctx.Uint64("conf_target")),
		}
	}

	type existingIndex = walletrpc.PsbtCoinSelect_ExistingOutputIndex

	// Parse change type flag.
	changeOutputIndex := ctx.Int("change_output_index")
	switch {
	case changeOutputIndex == -1:
		coinSelect.ChangeOutput = &walletrpc.PsbtCoinSelect_Add{
			Add: true,
		}
	case changeOutputIndex >= 0:
		coinSelect.ChangeOutput = &existingIndex{
			ExistingOutputIndex: int32(changeOutputIndex),
		}

	default:
		return fmt.Errorf("invalid change_output_index: %d",
			changeOutputIndex)
	}

	if ctx.IsSet("change_type") {
		switch addressType := ctx.String("change_type"); addressType {
		case "p2tr":
			req.ChangeType = p2TrChangeType

		default:
			return fmt.Errorf("invalid type for the change type: "+
				"%s. At the moment, the only address type "+
				"supported is p2tr (default to p2wkh)",
				addressType)
		}
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	response, err := walletClient.FundPsbt(ctxc, req)
	if err != nil {
		return err
	}

	jsonLocks := marshallLocks(response.LockedUtxos)

	printJSON(&fundPsbtResponse{
		Psbt: base64.StdEncoding.EncodeToString(
			response.FundedPsbt,
		),
		ChangeOutputIndex: response.ChangeOutputIndex,
		Locks:             jsonLocks,
	})

	return nil
}

var fundPsbtCommand = cli.Command{
	Name:  "fund",
	Usage: "Fund a Partially Signed Bitcoin Transaction (PSBT).",
	ArgsUsage: "[--template_psbt=T | [--outputs=O [--inputs=I]]] " +
		"[--conf_target=C | --sat_per_vbyte=S | --sat_per_kw=K] " +
		"[--change_type=A]",
	Description: `
	The fund command creates a fully populated PSBT that contains enough
	inputs to fund the outputs specified in either the PSBT or the
	--outputs flag.

	If there are no inputs specified in the template (or --inputs flag),
	coin selection is performed automatically. If inputs are specified, the
	wallet assumes that full coin selection happened externally and it will
	not add any additional inputs to the PSBT. If the specified inputs
	aren't enough to fund the outputs with the given fee rate, an error is
	returned.

	After either selecting or verifying the inputs, all input UTXOs are
	locked with an internal app ID.

	The 'outputs' flag decodes addresses and the amount to send respectively
	in the following JSON format:

	    --outputs='{"ExampleAddr": NumCoinsInSatoshis, "SecondAddr": Sats}'

	The optional 'inputs' flag decodes a JSON list of UTXO outpoints as
	returned by the listunspent command for example:

	    --inputs='["<txid1>:<output-index1>","<txid2>:<output-index2>",...]

	The optional '--change-type' flag permits to choose the address type
	for the change for default accounts and single imported public keys.
	The custom address type can only be p2tr at the moment (p2wkh will be
	used by default). No custom address type should be provided for custom
	accounts as we will always generate the change address using the coin
	selection key scope.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "template_psbt",
			Usage: "the outputs to fund and optional inputs to " +
				"spend provided in the base64 PSBT format",
		},
		cli.StringFlag{
			Name: "outputs",
			Usage: "a JSON compatible map of destination " +
				"addresses to amounts to send, must not " +
				"include a change address as that will be " +
				"added automatically by the wallet",
		},
		cli.StringFlag{
			Name: "inputs",
			Usage: "an optional JSON compatible list of UTXO " +
				"outpoints to use as the PSBT's inputs",
		},
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks that the transaction " +
				"should be confirmed on-chain within",
			Value: 6,
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "a manual fee expressed in sat/vbyte that " +
				"should be used when creating the transaction",
		},
		cli.Uint64Flag{
			Name: "sat_per_kw",
			Usage: "a manual fee expressed in sat/kw that " +
				"should be used when creating the transaction",
		},
		cli.StringFlag{
			Name: "account",
			Usage: "(optional) the name of the account to use to " +
				"create/fund the PSBT",
		},
		cli.StringFlag{
			Name: "change_type",
			Usage: "(optional) the type of the change address to " +
				"use to create/fund the PSBT. If no address " +
				"type is provided, p2wpkh will be used for " +
				"default accounts and single imported public " +
				"keys. No custom address type should be " +
				"provided for custom accounts as we will " +
				"always use the coin selection key scope to " +
				"generate the change address",
		},
		cli.Uint64Flag{
			Name: "min_confs",
			Usage: "(optional) the minimum number of " +
				"confirmations each input used for the PSBT " +
				"transaction must satisfy",
			Value: defaultUtxoMinConf,
		},
		coinSelectionStrategyFlag,
		cli.Float64Flag{
			Name: "max_fee_ratio",
			Usage: "the maximum fee to total output amount ratio " +
				"that this psbt should adhere to",
			Value: chanfunding.DefaultMaxFeeRatio,
		},
	},
	Action: actionDecorator(fundPsbt),
}

func fundPsbt(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if there aren't any flags
	// specified.
	if ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "fund")
	}

	coinSelectionStrategy, err := parseCoinSelectionStrategy(ctx)
	if err != nil {
		return err
	}

	minConfs := int32(ctx.Uint64("min_confs"))
	req := &walletrpc.FundPsbtRequest{
		Account:               ctx.String("account"),
		MinConfs:              minConfs,
		SpendUnconfirmed:      minConfs == 0,
		CoinSelectionStrategy: coinSelectionStrategy,
	}

	// Parse template flags.
	switch {
	// The PSBT flag is mutually exclusive with the outputs/inputs flags.
	case ctx.IsSet("template_psbt") &&
		(ctx.IsSet("inputs") || ctx.IsSet("outputs")):

		return fmt.Errorf("cannot set template_psbt and inputs/" +
			"outputs flags at the same time")

	// Use a pre-existing PSBT as the transaction template.
	case len(ctx.String("template_psbt")) > 0:
		psbtBase64 := ctx.String("template_psbt")
		psbtBytes, err := base64.StdEncoding.DecodeString(psbtBase64)
		if err != nil {
			return err
		}

		req.Template = &walletrpc.FundPsbtRequest_Psbt{
			Psbt: psbtBytes,
		}

	// The user manually specified outputs and/or inputs in JSON
	// format.
	case len(ctx.String("outputs")) > 0 || len(ctx.String("inputs")) > 0:
		var (
			tpl          = &walletrpc.TxTemplate{}
			amountToAddr map[string]uint64
		)

		if len(ctx.String("outputs")) > 0 {
			// Parse the address to amount map as JSON now. At least
			// one entry must be present.
			jsonMap := []byte(ctx.String("outputs"))
			err := json.Unmarshal(jsonMap, &amountToAddr)
			if err != nil {
				return fmt.Errorf("error parsing outputs "+
					"JSON: %w", err)
			}
			tpl.Outputs = amountToAddr
		}

		// Inputs are optional.
		if len(ctx.String("inputs")) > 0 {
			var inputs []string

			jsonList := []byte(ctx.String("inputs"))
			err := json.Unmarshal(jsonList, &inputs)
			if err != nil {
				return fmt.Errorf("error parsing inputs JSON: "+
					"%v", err)
			}

			for idx, input := range inputs {
				op, err := NewProtoOutPoint(input)
				if err != nil {
					return fmt.Errorf("error parsing "+
						"UTXO outpoint %d: %v", idx,
						err)
				}
				tpl.Inputs = append(tpl.Inputs, op)
			}
		}

		req.Template = &walletrpc.FundPsbtRequest_Raw{
			Raw: tpl,
		}

	default:
		return fmt.Errorf("must specify either template_psbt or " +
			"inputs/outputs flag")
	}

	// Parse fee flags.
	switch {
	case ctx.IsSet("conf_target") && ctx.IsSet("sat_per_vbyte") ||
		ctx.IsSet("conf_target") && ctx.IsSet("sat_per_kw") ||
		ctx.IsSet("sat_per_vbyte") && ctx.IsSet("sat_per_kw"):

		return fmt.Errorf("only one of conf_target, sat_per_vbyte, " +
			"or sat_per_kw can be set at the same time")

	case ctx.Uint64("sat_per_vbyte") > 0:
		req.Fees = &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: ctx.Uint64("sat_per_vbyte"),
		}

	case ctx.Uint64("sat_per_kw") > 0:
		req.Fees = &walletrpc.FundPsbtRequest_SatPerKw{
			SatPerKw: ctx.Uint64("sat_per_kw"),
		}

	// Check conf_target last because it has a default value.
	case ctx.Uint64("conf_target") > 0:
		req.Fees = &walletrpc.FundPsbtRequest_TargetConf{
			TargetConf: uint32(ctx.Uint64("conf_target")),
		}
	}

	if ctx.IsSet("change_type") {
		switch addressType := ctx.String("change_type"); addressType {
		case "p2tr":
			req.ChangeType = p2TrChangeType

		default:
			return fmt.Errorf("invalid type for the "+
				"change type: %s. At the moment, the "+
				"only address type supported is p2tr "+
				"(default to p2wkh)",
				addressType)
		}
	}

	req.MaxFeeRatio = ctx.Float64("max_fee_ratio")

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	response, err := walletClient.FundPsbt(ctxc, req)
	if err != nil {
		return err
	}

	jsonLocks := marshallLocks(response.LockedUtxos)

	printJSON(&fundPsbtResponse{
		Psbt: base64.StdEncoding.EncodeToString(
			response.FundedPsbt,
		),
		ChangeOutputIndex: response.ChangeOutputIndex,
		Locks:             jsonLocks,
	})

	return nil
}

// marshallLocks converts the rpc lease information to a more json-friendly
// format.
func marshallLocks(lockedUtxos []*walletrpc.UtxoLease) []*utxoLease {
	jsonLocks := make([]*utxoLease, len(lockedUtxos))
	for idx, lock := range lockedUtxos {
		jsonLocks[idx] = &utxoLease{
			ID:         hex.EncodeToString(lock.Id),
			OutPoint:   NewOutPointFromProto(lock.Outpoint),
			Expiration: lock.Expiration,
			PkScript:   lock.PkScript,
			Value:      lock.Value,
		}
	}

	return jsonLocks
}

// finalizePsbtResponse is a struct that contains JSON annotations for nice
// result serialization.
type finalizePsbtResponse struct {
	Psbt    string `json:"psbt"`
	FinalTx string `json:"final_tx"`
}

var finalizePsbtCommand = cli.Command{
	Name:      "finalize",
	Usage:     "Finalize a Partially Signed Bitcoin Transaction (PSBT).",
	ArgsUsage: "funded_psbt",
	Description: `
	The finalize command expects a partial transaction with all inputs
	and outputs fully declared and tries to sign all inputs that belong to
	the wallet. Lnd must be the last signer of the transaction. That means,
	if there are any unsigned non-witness inputs or inputs without UTXO
	information attached or inputs without witness data that do not belong
	to lnd's wallet, this method will fail. If no error is returned, the
	PSBT is ready to be extracted and the final TX within to be broadcast.

	This method does NOT publish the transaction after it's been finalized
	successfully.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "funded_psbt",
			Usage: "the base64 encoded PSBT to finalize",
		},
		cli.StringFlag{
			Name: "account",
			Usage: "(optional) the name of the account to " +
				"finalize the PSBT with",
		},
	},
	Action: actionDecorator(finalizePsbt),
}

func finalizePsbt(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() > 1 || ctx.NumFlags() > 2 {
		return cli.ShowCommandHelp(ctx, "finalize")
	}

	var (
		args       = ctx.Args()
		psbtBase64 string
	)
	switch {
	case ctx.IsSet("funded_psbt"):
		psbtBase64 = ctx.String("funded_psbt")
	case args.Present():
		psbtBase64 = args.First()
	default:
		return fmt.Errorf("funded_psbt argument missing")
	}

	psbtBytes, err := base64.StdEncoding.DecodeString(psbtBase64)
	if err != nil {
		return err
	}
	req := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: psbtBytes,
		Account:    ctx.String("account"),
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	response, err := walletClient.FinalizePsbt(ctxc, req)
	if err != nil {
		return err
	}

	printJSON(&finalizePsbtResponse{
		Psbt:    base64.StdEncoding.EncodeToString(response.SignedPsbt),
		FinalTx: hex.EncodeToString(response.RawFinalTx),
	})

	return nil
}

var leaseOutputCommand = cli.Command{
	Name:  "leaseoutput",
	Usage: "Lease an output.",
	Description: `
	The leaseoutput command locks an output, making it unavailable
	for coin selection.

	An app lock ID and expiration duration must be specified when locking
	the output.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "outpoint",
			Usage: "the output to lock",
		},
		cli.StringFlag{
			Name:  "lockid",
			Usage: "the hex-encoded app lock ID",
		},
		cli.Uint64Flag{
			Name:  "expiry",
			Usage: "expiration duration in seconds",
		},
	},
	Action: actionDecorator(leaseOutput),
}

func leaseOutput(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 0 || ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "leaseoutput")
	}

	outpointStr := ctx.String("outpoint")
	outpoint, err := NewProtoOutPoint(outpointStr)
	if err != nil {
		return fmt.Errorf("error parsing outpoint: %w", err)
	}

	lockIDStr := ctx.String("lockid")
	if lockIDStr == "" {
		return errors.New("lockid not specified")
	}
	lockID, err := hex.DecodeString(lockIDStr)
	if err != nil {
		return fmt.Errorf("error parsing lockid: %w", err)
	}

	expiry := ctx.Uint64("expiry")
	if expiry == 0 {
		return errors.New("expiry not specified or invalid")
	}

	req := &walletrpc.LeaseOutputRequest{
		Outpoint:          outpoint,
		Id:                lockID,
		ExpirationSeconds: expiry,
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	response, err := walletClient.LeaseOutput(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(response)

	return nil
}

var releaseOutputCommand = cli.Command{
	Name:      "releaseoutput",
	Usage:     "Release an output previously locked by lnd.",
	ArgsUsage: "outpoint",
	Description: `
	The releaseoutput command unlocks an output, allowing it to be available
	for coin selection if it remains unspent.

	If no lock ID is specified, the internal lnd app lock ID is used when
	releasing the output. With the internal ID, only UTXOs locked by the
	fundpsbt command can be released.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "outpoint",
			Usage: "the output to unlock",
		},
		cli.StringFlag{
			Name:  "lockid",
			Usage: "the hex-encoded app lock ID",
		},
	},
	Action: actionDecorator(releaseOutput),
}

func releaseOutput(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 1 && ctx.NumFlags() != 1 {
		return cli.ShowCommandHelp(ctx, "releaseoutput")
	}

	var (
		args        = ctx.Args()
		outpointStr string
	)
	switch {
	case ctx.IsSet("outpoint"):
		outpointStr = ctx.String("outpoint")
	case args.Present():
		outpointStr = args.First()
	default:
		return fmt.Errorf("outpoint argument missing")
	}

	outpoint, err := NewProtoOutPoint(outpointStr)
	if err != nil {
		return fmt.Errorf("error parsing outpoint: %w", err)
	}

	lockID := chanfunding.LndInternalLockID[:]
	lockIDStr := ctx.String("lockid")
	if lockIDStr != "" {
		var err error
		lockID, err = hex.DecodeString(lockIDStr)
		if err != nil {
			return fmt.Errorf("error parsing lockid: %w", err)
		}
	}

	req := &walletrpc.ReleaseOutputRequest{
		Outpoint: outpoint,
		Id:       lockID,
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	response, err := walletClient.ReleaseOutput(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(response)

	return nil
}

var listLeasesCommand = cli.Command{
	Name:   "listleases",
	Usage:  "Return a list of currently held leases.",
	Action: actionDecorator(listLeases),
}

func listLeases(ctx *cli.Context) error {
	ctxc := getContext()

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.ListLeasesRequest{}
	response, err := walletClient.ListLeases(ctxc, req)
	if err != nil {
		return err
	}

	printJSON(marshallLocks(response.LockedUtxos))
	return nil
}

var listAccountsCommand = cli.Command{
	Name:  "list",
	Usage: "Retrieve information of existing on-chain wallet accounts.",
	Description: `
	Retrieves all accounts belonging to the wallet by default. A name and
	key scope filter can be provided to filter through all of the wallet
	accounts and return only those matching.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "name",
			Usage: "(optional) only accounts matching this name " +
				"are returned",
		},
		cli.StringFlag{
			Name: "address_type",
			Usage: "(optional) only accounts matching this " +
				"address type are returned",
		},
	},
	Action: actionDecorator(listAccounts),
}

func listAccounts(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() > 0 || ctx.NumFlags() > 2 {
		return cli.ShowCommandHelp(ctx, "list")
	}

	addrType, err := parseAddrType(ctx.String("address_type"))
	if err != nil {
		return err
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.ListAccountsRequest{
		Name:        ctx.String("name"),
		AddressType: addrType,
	}
	resp, err := walletClient.ListAccounts(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var requiredReserveCommand = cli.Command{
	Name:  "requiredreserve",
	Usage: "Returns the wallet reserve.",
	Description: `
	Returns the minimum amount of satoshis that should be kept in the
	wallet in order to fee bump anchor channels if necessary. The value
	scales with the number of public anchor channels but is	capped at
	a maximum.

	Use the flag --additional_channels to get the reserve value based
	on the additional channels you would like to open.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "additional_channels",
			Usage: "(optional) specify the additional public channels " +
				"that you would like to open",
		},
	},
	Action: actionDecorator(requiredReserve),
}

func requiredReserve(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() > 0 || ctx.NumFlags() > 1 {
		return cli.ShowCommandHelp(ctx, "requiredreserve")
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.RequiredReserveRequest{
		AdditionalPublicChannels: uint32(ctx.Uint64("additional_channels")),
	}
	resp, err := walletClient.RequiredReserve(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listAddressesCommand = cli.Command{
	Name:  "list",
	Usage: "Retrieve information of existing on-chain wallet addresses.",
	Description: `
	Retrieves information of existing on-chain wallet addresses along with
	their type, internal/external and balance.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "account_name",
			Usage: "(optional) only addresses matching this " +
				"account are returned",
		},
		cli.BoolFlag{
			Name: "show_custom_accounts",
			Usage: "(optional) set this to true to show lnd's " +
				"custom accounts",
		},
	},
	Action: actionDecorator(listAddresses),
}

func listAddresses(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() > 0 || ctx.NumFlags() > 2 {
		return cli.ShowCommandHelp(ctx, "list")
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.ListAddressesRequest{
		AccountName:        ctx.String("account_name"),
		ShowCustomAccounts: ctx.Bool("show_custom_accounts"),
	}
	resp, err := walletClient.ListAddresses(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var signMessageWithAddrCommand = cli.Command{
	Name: "signmessage",
	Usage: "Sign a message with the private key of the provided " +
		"address.",
	ArgsUsage: "address msg",
	Description: `
	Sign a message with the private key of the specified address, and
	return the signature. Signing is solely done in the ECDSA compact
	signature format. This is also done when signing with a P2TR address
	meaning that the private key of the P2TR address (internal key) is used
	to sign the provided message with the ECDSA format. Only addresses are
	accepted which are owned by the internal lnd wallet.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "address",
			Usage: "specify the address which private key " +
				"will be used to sign the message",
		},
		cli.StringFlag{
			Name:  "msg",
			Usage: "the message to sign for",
		},
	},
	Action: actionDecorator(signMessageWithAddr),
}

func signMessageWithAddr(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() > 2 || ctx.NumFlags() > 2 {
		return cli.ShowCommandHelp(ctx, "signmessagewithaddr")
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	var (
		args = ctx.Args()
		addr string
		msg  []byte
	)

	switch {
	case ctx.IsSet("address"):
		addr = ctx.String("address")

	case ctx.Args().Present():
		addr = args.First()
		args = args.Tail()

	default:
		return fmt.Errorf("address argument missing")
	}

	switch {
	case ctx.IsSet("msg"):
		msg = []byte(ctx.String("msg"))

	case ctx.Args().Present():
		msg = []byte(args.First())
		args = args.Tail()

	default:
		return fmt.Errorf("msg argument missing")
	}

	resp, err := walletClient.SignMessageWithAddr(
		ctxc,
		&walletrpc.SignMessageWithAddrRequest{
			Msg:  msg,
			Addr: addr,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var verifyMessageWithAddrCommand = cli.Command{
	Name: "verifymessage",
	Usage: "Verify a message signed with the private key of the " +
		"provided address.",
	ArgsUsage: "address sig msg",
	Description: `
	Verify a message signed with the signature of the public key
	of the provided address. The signature must be in compact ECDSA format
	The verification is independent whether the address belongs to the
	wallet or not. This is achieved by only accepting ECDSA compacted
	signatures. When verifying a signature with a taproot address, the
	signature still has to be in the ECDSA compact format and no tapscript
	has to be included in the P2TR address.
	Supports address types P2PKH, P2WKH, NP2WKH, P2TR.

	Besides whether the signature is valid or not, the recoverd public key
	of the compact ECDSA signature is returned.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "address",
			Usage: "specify the address which corresponding" +
				"public key will be used",
		},
		cli.StringFlag{
			Name: "sig",
			Usage: "the base64 encoded compact signature " +
				"of the message",
		},
		cli.StringFlag{
			Name:  "msg",
			Usage: "the message to sign",
		},
	},
	Action: actionDecorator(verifyMessageWithAddr),
}

func verifyMessageWithAddr(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() > 3 || ctx.NumFlags() > 3 {
		return cli.ShowCommandHelp(ctx, "signmessagewithaddr")
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	var (
		args = ctx.Args()
		addr string
		sig  string
		msg  []byte
	)

	switch {
	case ctx.IsSet("address"):
		addr = ctx.String("address")

	case args.Present():
		addr = args.First()
		args = args.Tail()

	default:
		return fmt.Errorf("address argument missing")
	}

	switch {
	case ctx.IsSet("sig"):
		sig = ctx.String("sig")

	case ctx.Args().Present():
		sig = args.First()
		args = args.Tail()

	default:
		return fmt.Errorf("sig argument missing")
	}

	switch {
	case ctx.IsSet("msg"):
		msg = []byte(ctx.String("msg"))

	case ctx.Args().Present():
		msg = []byte(args.First())
		args = args.Tail()

	default:
		return fmt.Errorf("msg argument missing")
	}

	resp, err := walletClient.VerifyMessageWithAddr(
		ctxc,
		&walletrpc.VerifyMessageWithAddrRequest{
			Msg:       msg,
			Signature: sig,
			Addr:      addr,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var importAccountCommand = cli.Command{
	Name: "import",
	Usage: "Import an on-chain account into the wallet through its " +
		"extended public key.",
	ArgsUsage: "extended_public_key name",
	Description: `
	Imports an account backed by an account extended public key. The master
	key fingerprint denotes the fingerprint of the root key corresponding to
	the account public key (also known as the key with derivation path m/).
	This may be required by some hardware wallets for proper identification
	and signing.

	The address type can usually be inferred from the key's version, but may
	be required for certain keys to map them into the proper scope.

	If an account with the same name already exists (even with a different
	key scope), an error will be returned.

	For BIP-0044 keys, an address type must be specified as we intend to not
	support importing BIP-0044 keys into the wallet using the legacy
	pay-to-pubkey-hash (P2PKH) scheme. A nested witness address type will
	force the standard BIP-0049 derivation scheme, while a witness address
	type will force the standard BIP-0084 derivation scheme.

	For BIP-0049 keys, an address type must also be specified to make a
	distinction between the standard BIP-0049 address schema (nested witness
	pubkeys everywhere) and our own BIP-0049Plus address schema (nested
	pubkeys externally, witness pubkeys internally).

	NOTE: Events (deposits/spends) for keys derived from an account will
	only be detected by lnd if they happen after the import. Rescans to
	detect past events will be supported later on.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "address_type",
			Usage: "(optional) specify the type of addresses the " +
				"imported account should generate",
		},
		cli.StringFlag{
			Name: "master_key_fingerprint",
			Usage: "(optional) the fingerprint of the root key " +
				"(derivation path m/) corresponding to the " +
				"account public key",
		},
		cli.BoolFlag{
			Name:  "dry_run",
			Usage: "(optional) perform a dry run",
		},
	},
	Action: actionDecorator(importAccount),
}

func importAccount(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 2 || ctx.NumFlags() > 3 {
		return cli.ShowCommandHelp(ctx, "import")
	}

	addrType, err := parseAddrType(ctx.String("address_type"))
	if err != nil {
		return err
	}

	var mkfpBytes []byte
	if ctx.IsSet("master_key_fingerprint") {
		mkfpBytes, err = hex.DecodeString(
			ctx.String("master_key_fingerprint"),
		)
		if err != nil {
			return fmt.Errorf("invalid master key fingerprint: %w",
				err)
		}
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	dryRun := ctx.Bool("dry_run")
	req := &walletrpc.ImportAccountRequest{
		Name:                 ctx.Args().Get(1),
		ExtendedPublicKey:    ctx.Args().Get(0),
		MasterKeyFingerprint: mkfpBytes,
		AddressType:          addrType,
		DryRun:               dryRun,
	}
	resp, err := walletClient.ImportAccount(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var importPubKeyCommand = cli.Command{
	Name:      "import-pubkey",
	Usage:     "Import a public key as watch-only into the wallet.",
	ArgsUsage: "public_key address_type",
	Description: `
	Imports a public key represented in hex as watch-only into the wallet.
	The address type must be one of the following: np2wkh, p2wkh.

	NOTE: Events (deposits/spends) for a key will only be detected by lnd if
	they happen after the import. Rescans to detect past events will be
	supported later on.
	`,
	Action: actionDecorator(importPubKey),
}

func importPubKey(ctx *cli.Context) error {
	ctxc := getContext()

	// Display the command's help message if we do not have the expected
	// number of arguments/flags.
	if ctx.NArg() != 2 || ctx.NumFlags() > 0 {
		return cli.ShowCommandHelp(ctx, "import-pubkey")
	}

	pubKeyBytes, err := hex.DecodeString(ctx.Args().Get(0))
	if err != nil {
		return err
	}
	addrType, err := parseAddrType(ctx.Args().Get(1))
	if err != nil {
		return err
	}

	walletClient, cleanUp := getWalletClient(ctx)
	defer cleanUp()

	req := &walletrpc.ImportPublicKeyRequest{
		PublicKey:   pubKeyBytes,
		AddressType: addrType,
	}
	resp, err := walletClient.ImportPublicKey(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
