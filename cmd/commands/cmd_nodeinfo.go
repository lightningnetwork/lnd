package commands

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

var nodeInfoCommand = cli.Command{
	Name:     "nodeinfo",
	Category: "Info",
	Usage:    "Display a human-readable summary of the local node.",
	Description: `
nodeinfo prints a combined view of the running lnd node: identity,
on-chain wallet balance, and Lightning channel balances — all in one
place without having to call getinfo, walletbalance, and channelbalance
separately.

Use --json to get machine-readable output instead of formatted text.`,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw JSON instead of formatted text",
		},
	},
	Action: actionDecorator(nodeInfo),
}

// nodeInfoClient is the subset of lnrpc.LightningClient that nodeinfo needs.
// Using a narrow interface keeps the command testable without mocking all 167
// methods of the full LightningClient.
type nodeInfoClient interface {
	GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest, opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error)
	WalletBalance(ctx context.Context, in *lnrpc.WalletBalanceRequest, opts ...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error)
	ChannelBalance(ctx context.Context, in *lnrpc.ChannelBalanceRequest, opts ...grpc.CallOption) (*lnrpc.ChannelBalanceResponse, error)
}

func nodeInfo(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()
	return nodeInfoFromClient(ctxc, ctx.Bool("json"), client)
}

// nodeInfoFromClient is the testable core: it fires the three RPCs, then
// either prints formatted text or JSON depending on jsonOutput.
func nodeInfoFromClient(ctxc context.Context, jsonOutput bool, client nodeInfoClient) error {
	type infoRes struct {
		v   *lnrpc.GetInfoResponse
		err error
	}
	type walletRes struct {
		v   *lnrpc.WalletBalanceResponse
		err error
	}
	type chanRes struct {
		v   *lnrpc.ChannelBalanceResponse
		err error
	}

	infoCh := make(chan infoRes, 1)
	walletCh := make(chan walletRes, 1)
	chanCh := make(chan chanRes, 1)

	go func() {
		v, err := client.GetInfo(ctxc, &lnrpc.GetInfoRequest{})
		infoCh <- infoRes{v, err}
	}()
	go func() {
		v, err := client.WalletBalance(ctxc, &lnrpc.WalletBalanceRequest{})
		walletCh <- walletRes{v, err}
	}()
	go func() {
		v, err := client.ChannelBalance(ctxc, &lnrpc.ChannelBalanceRequest{})
		chanCh <- chanRes{v, err}
	}()

	ir := <-infoCh
	if ir.err != nil {
		return ir.err
	}
	wr := <-walletCh
	if wr.err != nil {
		return wr.err
	}
	cr := <-chanCh
	if cr.err != nil {
		return cr.err
	}

	if jsonOutput {
		printJSON(struct {
			Info    *lnrpc.GetInfoResponse        `json:"info"`
			Wallet  *lnrpc.WalletBalanceResponse  `json:"wallet"`
			Channel *lnrpc.ChannelBalanceResponse `json:"channel"`
		}{ir.v, wr.v, cr.v})
		return nil
	}

	return printNodeInfo(ir.v, wr.v, cr.v)
}

func printNodeInfo(
	info *lnrpc.GetInfoResponse,
	wallet *lnrpc.WalletBalanceResponse,
	channel *lnrpc.ChannelBalanceResponse,
) error {

	// ── Identity ──────────────────────────────────────────────────────────────
	alias := info.Alias
	if alias == "" {
		alias = "(no alias)"
	}
	network := ""
	if len(info.Chains) > 0 {
		network = fmt.Sprintf("%s/%s", info.Chains[0].Chain, info.Chains[0].Network)
	}
	syncStatus := "synced"
	if !info.SyncedToChain {
		syncStatus = "NOT synced"
	}

	fmt.Printf("%-12s %s\n", "Node", info.IdentityPubkey)
	fmt.Printf("%-12s %s  %s\n", "Alias", alias, info.Color)
	fmt.Printf("%-12s %s   block %-7d  %s\n", "Network", network, info.BlockHeight, syncStatus)
	for _, uri := range info.Uris {
		fmt.Printf("%-12s %s\n", "URI", uri)
	}

	// ── On-chain balance ──────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("On-chain")
	fmt.Printf("  %-14s %s\n", "Confirmed", commaSats(wallet.ConfirmedBalance))
	if wallet.UnconfirmedBalance != 0 {
		fmt.Printf("  %-14s %s\n", "Unconfirmed", commaSats(wallet.UnconfirmedBalance))
	}

	// ── Lightning channels ────────────────────────────────────────────────────
	localSat := int64(channel.GetLocalBalance().GetSat())
	remoteSat := int64(channel.GetRemoteBalance().GetSat())
	pendingLocalSat := int64(channel.GetPendingOpenLocalBalance().GetSat())
	pendingRemoteSat := int64(channel.GetPendingOpenRemoteBalance().GetSat())
	totalSat := localSat + remoteSat

	fmt.Println()
	fmt.Println("Channels")
	fmt.Printf("  %-14s active %-4d  inactive %-4d  pending %d\n",
		"Count",
		info.NumActiveChannels,
		info.NumInactiveChannels,
		info.NumPendingChannels,
	)

	if totalSat > 0 {
		pct := int(localSat * 100 / totalSat)
		fmt.Printf("  %-14s %s\n", "Capacity", commaSats(totalSat))
		fmt.Printf("  %-14s %s  (%d%% local)\n", "Balance", commaSats(localSat), pct)
		fmt.Printf("  %-14s %s\n", "Remote", commaSats(remoteSat))
	}
	if pendingLocalSat > 0 || pendingRemoteSat > 0 {
		fmt.Printf("  %-14s local %s  remote %s\n",
			"Pending open",
			commaSats(pendingLocalSat),
			commaSats(pendingRemoteSat),
		)
	}

	// ── Peers ─────────────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Printf("%-12s %d\n", "Peers", info.NumPeers)

	return nil
}

// commaSats formats a satoshi amount with thousands separators,
// e.g. "1,234,567 sats".
func commaSats(sats int64) string {
	neg := sats < 0
	if neg {
		sats = -sats
	}

	s := fmt.Sprintf("%d", sats)
	out := make([]byte, 0, len(s)+len(s)/3+1)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}

	result := string(out) + " sats"
	if neg {
		result = "-" + result
	}
	return result
}
