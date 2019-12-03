package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var channelReportsCommand = cli.Command{
	Name:      "channelreports",
	Category:  "Channels",
	Usage:     "Get a report for channels over a period of time",
	ArgsUsage: "[--start_time=] [--end_time=]",
	Description: `
	Generate a set of channel reports over a period of time. These reports
	contain revenue reports for the channel, and uptime of the remote peer,
	if it is available.

	If start time is not provided, reports are produced for the last week 
	of activity. If no end time is provided, reports are produced until the 
	present.

	lncli channelrevenue [--start_time] [--end_time]
	`,
	Flags: []cli.Flag{
		cli.Int64Flag{
			Name: "start_time",
			Usage: "(optional) the time from which to generate reports, " +
				"expressed in seconds since the unix epoch",
		},
		cli.Int64Flag{
			Name: "end_time",
			Usage: "(optional) the time that reports should be generated " +
				"until, expressed in seconds since the unix epoch",
		},
	},
	Action: actionDecorator(channelReports),
}

func channelReports(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	args := ctx.Args()

	var (
		startTime, endTime uint64
	)

	switch {
	case ctx.IsSet("start_time"):
		startTime = ctx.Uint64("start_time")
	case args.Present():
		i, err := strconv.ParseUint(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode start_time: %v", err)
		}
		startTime = i
		args = args.Tail()
	default:
		week := time.Hour * 24 * 7
		startTime = uint64(time.Now().Add(week * -1).Unix())
	}

	switch {
	case ctx.IsSet("end_time"):
		endTime = ctx.Uint64("end_time")
	case args.Present():
		i, err := strconv.ParseUint(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode end_time: %v", err)
		}
		endTime = i
		args = args.Tail()
	default:
		endTime = uint64(time.Now().Unix())
	}

	req := &lnrpc.ChannelReportsRequest{
		StartTime: startTime,
		EndTime:   endTime,
	}

	resp, err := client.ChannelReports(context.Background(), req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
