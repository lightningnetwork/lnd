//go:build peersrpc
// +build peersrpc

package commands

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/urfave/cli"
)

// peersCommands will return the set of commands to enable for peersrpc
// builds.
func peersCommands() []cli.Command {
	return []cli.Command{
		{
			Name:     "peers",
			Category: "Peers",
			Usage: "Interacts with the other nodes of the " +
				"network",
			Subcommands: []cli.Command{
				updateNodeAnnouncementCommand,
			},
		},
	}
}

func getPeersClient(ctx *cli.Context) (peersrpc.PeersClient, func()) {
	conn := getClientConn(ctx, false)
	cleanUp := func() {
		conn.Close()
	}
	return peersrpc.NewPeersClient(conn), cleanUp
}

var updateNodeAnnouncementCommand = cli.Command{
	Name:     "updatenodeannouncement",
	Category: "Peers",
	Usage:    "update and broadcast a new node announcement",
	Description: `
	Update the node's information and broadcast a new node announcement.

	Add or remove addresses where your node can be reached at, change the
	alias/color of the node or enable/disable supported feature bits without
	restarting the node. A node announcement with the new information will
	be created and broadcast to the network.`,
	ArgsUsage: "[--address_add=] [--address_remove=] [--alias=] " +
		"[--color=] [--feature_bit_add=] [--feature_bit_remove=]",
	Flags: []cli.Flag{
		cli.StringSliceFlag{
			Name: "address_add",
			Usage: "a new address that should be added to the " +
				"set of URIs of this node. Can be set " +
				"multiple times in the same command",
		},
		cli.StringSliceFlag{
			Name: "address_remove",
			Usage: "an address that needs to be removed from the " +
				"set of URIs of this node. Can be set " +
				"multiple times in the same command",
		},
		cli.StringFlag{
			Name:  "alias",
			Usage: "the new alias for this node, e.g. \"bob\"",
		},
		cli.StringFlag{
			Name:  "color",
			Usage: "the new color for this node, e.g. #c42a81",
		},
		cli.Int64SliceFlag{
			Name: "feature_bit_add",
			Usage: "a feature bit index that needs to be enabled. " +
				"Can be set multiple times in the same command",
		},
		cli.Int64SliceFlag{
			Name: "feature_bit_remove",
			Usage: "a feature bit that needs to be disabled. " +
				"Can be set multiple times in the same command",
		},
	},
	Action: actionDecorator(updateNodeAnnouncement),
}

func updateNodeAnnouncement(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getPeersClient(ctx)
	defer cleanUp()

	change := false

	req := &peersrpc.NodeAnnouncementUpdateRequest{}

	if ctx.IsSet("address_add") {
		change = true
		for _, addr := range ctx.StringSlice("address_add") {
			action := &peersrpc.UpdateAddressAction{
				Action:  peersrpc.UpdateAction_ADD,
				Address: addr,
			}
			req.AddressUpdates = append(req.AddressUpdates, action)
		}
	}

	if ctx.IsSet("address_remove") {
		change = true
		for _, addr := range ctx.StringSlice("address_remove") {
			action := &peersrpc.UpdateAddressAction{
				Action:  peersrpc.UpdateAction_REMOVE,
				Address: addr,
			}
			req.AddressUpdates = append(req.AddressUpdates, action)
		}
	}

	if ctx.IsSet("alias") {
		change = true
		req.Alias = ctx.String("alias")
	}

	if ctx.IsSet("color") {
		change = true
		req.Color = ctx.String("color")
	}

	if ctx.IsSet("feature_bit_add") {
		change = true
		for _, feature := range ctx.Int64Slice("feature_bit_add") {
			action := &peersrpc.UpdateFeatureAction{
				Action:     peersrpc.UpdateAction_ADD,
				FeatureBit: lnrpc.FeatureBit(feature),
			}
			req.FeatureUpdates = append(req.FeatureUpdates, action)
		}
	}

	if ctx.IsSet("feature_bit_remove") {
		change = true
		for _, feature := range ctx.Int64Slice("feature_bit_remove") {
			action := &peersrpc.UpdateFeatureAction{
				Action:     peersrpc.UpdateAction_REMOVE,
				FeatureBit: lnrpc.FeatureBit(feature),
			}
			req.FeatureUpdates = append(req.FeatureUpdates, action)
		}
	}

	if !change {
		return fmt.Errorf("no changes for the node information " +
			"detected")
	}

	resp, err := client.UpdateNodeAnnouncement(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
