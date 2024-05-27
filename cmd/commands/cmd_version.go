package commands

import (
	"fmt"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lnrpc/lnclipb"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/urfave/cli"
)

var versionCommand = cli.Command{
	Name:  "version",
	Usage: "Display lncli and lnd version info.",
	Description: `
	Returns version information about both lncli and lnd. If lncli is unable
	to connect to lnd, the command fails but still prints the lncli version.
	`,
	Action: actionDecorator(version),
}

func version(ctx *cli.Context) error {
	ctxc := getContext()
	conn := getClientConn(ctx, false)
	defer conn.Close()

	versions := &lnclipb.VersionResponse{
		Lncli: &verrpc.Version{
			Commit:        build.Commit,
			CommitHash:    build.CommitHash,
			Version:       build.Version(),
			AppMajor:      uint32(build.AppMajor),
			AppMinor:      uint32(build.AppMinor),
			AppPatch:      uint32(build.AppPatch),
			AppPreRelease: build.AppPreRelease,
			BuildTags:     build.Tags(),
			GoVersion:     build.GoVersion,
		},
	}

	client := verrpc.NewVersionerClient(conn)

	lndVersion, err := client.GetVersion(ctxc, &verrpc.VersionRequest{})
	if err != nil {
		printRespJSON(versions)
		return fmt.Errorf("unable fetch version from lnd: %w", err)
	}
	versions.Lnd = lndVersion

	printRespJSON(versions)

	return nil
}
