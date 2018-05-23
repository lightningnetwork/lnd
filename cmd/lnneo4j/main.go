package main

import (
	"fmt"
	"image/color"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	bolt "github.com/coreos/bbolt"
	neobolt "github.com/johnnadratowski/golang-neo4j-bolt-driver"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcutil"
	"github.com/urfave/cli"
)

var (
	//Commit stores the current commit hash of this build. This should be
	//set using -ldflags during compilation.
	Commit string

	defaultLndDir  = btcutil.AppDataDir("lnd", false)
	defaultNetwork = "mainnet"
	defaultNeoUser = "neo4j"
	defaultNeoHost = "localhost"
	defaultNeoPort = "7687"
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[lnneo4j] %v\n", err)
	os.Exit(1)
}

func main() {
	app := cli.NewApp()
	app.Name = "lnneo4j"
	app.Version = fmt.Sprintf("%s commit=%s", "0.1.1", Commit)
	app.Usage = "import a snapshot of your channel databse to neo4js"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "lnddir",
			Value: defaultLndDir,
			Usage: "path to lnd's base directory",
		},
		cli.StringFlag{
			Name:  "network",
			Value: defaultNetwork,
			Usage: "either mainnet, testnet, simnet, regtest, etc.",
		},
		cli.StringFlag{
			Name:  "neouser",
			Value: defaultNeoUser,
			Usage: "neo4j user for authentication",
		},
		cli.StringFlag{
			Name:  "neopass",
			Value: "",
			Usage: "neo4j password for authentication. Required unless neo4j authentication is disabled, e.g. dbms.security.auth_enabled=false is set in neo4j.conf",
		},
		cli.StringFlag{
			Name:  "neohost",
			Value: defaultNeoHost,
			Usage: "neo4j hostname",
		},
		cli.StringFlag{
			Name:  "neoport",
			Value: defaultNeoPort,
			Usage: "neo4j port number",
		},
	}
	app.Commands = []cli.Command{}
	app.Action = func(c *cli.Context) error {

		lndDir := cleanAndExpandPath(c.GlobalString("lnddir"))
		network := c.GlobalString("network")
		neoUser := c.GlobalString("neouser")
		neoPass := c.GlobalString("neopass")
		neoHost := c.GlobalString("neohost")
		neoPort := c.GlobalString("neoport")

		// Create the snapshot network-segmented directory for the channel database.
		snapshotGraphDir := filepath.Join(lndDir, "snapshot", "graph", network)

		// Open the channeldb snapshot
		chanDB, err := channeldb.Open(snapshotGraphDir)
		if err != nil {
			fatal(err)
		}
		defer chanDB.Close()

		// Open the neo4j connection
		driver := neobolt.NewDriver()
		neoURL := neo4jURL(neoHost, neoPort, neoUser, neoPass)
		conn, err := driver.OpenNeo(neoURL)
		if err != nil {
			fatal(err)
		}
		defer conn.Close()

		// clean previous import
		neo4jClean(conn)
		// import new data
		graph := chanDB.ChannelGraph()
		neo4jSync(conn, graph)

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fatal(err)
	}

}

// build the driver connection url
func neo4jURL(neoHost, neoPort, neoUser, neoPass string) string {
	if len(neoPass) > 0 {
		return fmt.Sprintf("bolt://%s:%s@%s:%s", neoUser, neoPass, neoHost, neoPort)
	} else {
		return fmt.Sprintf("bolt://%s:%s", neoHost, neoPort)
	}
}

// Remove all nodes and channels
func neo4jClean(conn neobolt.Conn) {
	// delete all nodes and relationships
	result, err := conn.ExecNeo(`MATCH (n:LNNode) DETACH DELETE n`, nil)
	if err != nil {
		fatal(err)
	}
	numResult, _ := result.RowsAffected()
	if numResult > 0 {
		fmt.Printf("[lnneo4j] %d nodes and channels deleted.\n", numResult)
	}
}

// Add all nodes and open channels from snapshot
func neo4jSync(conn neobolt.Conn, graph *channeldb.ChannelGraph) {
	var err error
	var pubKey []byte
	var pubKey2 []byte
	var nodeAnnStmt, nodeNoAnnStmt, chnlStmt neobolt.Stmt

	var numAnnNodes, numNoAnnNodes uint32
	// Add all the known nodes in the within our view of the network
	if err := graph.ForEachNode(nil, func(tx *bolt.Tx, node *channeldb.LightningNode) error {
		// Increment the total number of nodes with each iteration.

		if node.HaveNodeAnnouncement {
			addresses := make([]string, len(node.Addresses), len(node.Addresses))
			for i, a := range node.Addresses {
				addresses[i] = a.String()
			}
			pubKey = node.PubKeyBytes[:]

			// statement for a node for which we already received an announcment
			nodeAnnStmt, err = conn.PrepareNeo(`CREATE (n:LNNode:Detailed {pubKey: {pubKey},
				alias: {alias},
				lastUpdate: {lastUpdate},
				addresses: {addresses},
				color: {color}})`)
			if err != nil {
				fatal(err)
			}
			_, err = nodeAnnStmt.ExecNeo(map[string]interface{}{
				"pubKey":     string(pubKey),
				"alias":      node.Alias,
				"lastUpdate": node.LastUpdate.Unix(),
				"addresses":  strings.Join(addresses, ", "),
				"color":      rgb2Hex(node.Color),
			})
			if err != nil {
				fatal(err)
			}
			nodeAnnStmt.Close()
			numAnnNodes++
		} else {
			// statement for a node for which we did not received an announcment
			nodeNoAnnStmt, err = conn.PrepareNeo(`CREATE (n:LNNode {pubKey: {pubKey}})`)
			if err != nil {
				fatal(err)
			}

			_, err = nodeNoAnnStmt.ExecNeo(map[string]interface{}{
				"pubKey": string(pubKey),
			})
			if err != nil {
				fatal(err)
			}
			nodeNoAnnStmt.Close()
			numNoAnnNodes++
		}
		return nil

	}); err != nil {
		fatal(err)
	}
	fmt.Printf("[lnneo4j] Added %d detailed nodes and %d nodes known only by public key.\n", numAnnNodes, numNoAnnNodes)

	// Add channels
	var numBiChnls, numUniChnls, numNoChnls uint32
	var disabled bool

	chnlStmt, err = conn.PrepareNeo(`MATCH (n1:LNNode ), (n2:LNNode)
		WHERE n1.pubKey = {pubKey1} AND n2.pubKey = {pubKey2}
		CREATE (n1)-[r:CHANNEL {
			capacity: {capacity},
			channelID: {channelID},
			lastUpdate: {lastUpdate},
			disabled: {disabled},
			timeLockDelta: {timeLockDelta},
			minHTLC: {minHTLC},
			feeBaseMsat: {feeBaseMsat},
			feeRateMilliMsat: {feeRateMilliMsat}
		}]->(n2)`)
	if err != nil {
		fatal(err)
	}
	// Add all the open channels
	// TBD: Add pending and waiting-to-close channels
	if err := graph.ForEachChannel(func(chnl *channeldb.ChannelEdgeInfo, edge1, edge2 *channeldb.ChannelEdgePolicy) error {
		pubKey = chnl.NodeKey1Bytes[:]
		pubKey2 = chnl.NodeKey2Bytes[:]

		// add one direction
		if edge1 != nil {
			disabled = (edge1.Flags & lnwire.ChanUpdateDisabled) != 0
			_, err = chnlStmt.ExecNeo(map[string]interface{}{
				"pubKey1":          string(pubKey),
				"pubKey2":          string(pubKey2),
				"channelID":        chnl.ChannelID,
				"capacity":         int64(chnl.Capacity),
				"lastUpdate":       edge1.LastUpdate.Unix(),
				"disabled":         disabled,
				"timeLockDelta":    uint32(edge1.TimeLockDelta),
				"minHTLC":          int64(edge1.MinHTLC),
				"feeBaseMsat":      int64(edge1.FeeBaseMSat),
				"feeRateMilliMsat": int64(edge1.FeeProportionalMillionths),
			})
			if err != nil {
				fatal(err)
			}
		}

		// add second direction
		if edge2 != nil {
			disabled = (edge2.Flags & lnwire.ChanUpdateDisabled) != 0
			_, err = chnlStmt.ExecNeo(map[string]interface{}{
				"pubKey1":          string(pubKey2),
				"pubKey2":          string(pubKey),
				"channelID":        chnl.ChannelID,
				"capacity":         int64(chnl.Capacity),
				"lastUpdate":       edge2.LastUpdate.Unix(),
				"disabled":         disabled,
				"timeLockDelta":    uint32(edge2.TimeLockDelta),
				"minHTLC":          int64(edge2.MinHTLC),
				"feeBaseMsat":      int64(edge2.FeeBaseMSat),
				"feeRateMilliMsat": int64(edge2.FeeProportionalMillionths),
			})
			if err != nil {
				fatal(err)
			}
		}

		// increment counters
		switch {
		case edge1 != nil && edge2 != nil:
			numBiChnls++
		case edge1 == nil && edge2 == nil:
			numNoChnls++
		default:
			numUniChnls++
		}

		return nil
	}); err != nil {
		fatal(err)
	}
	chnlStmt.Close()

	fmt.Printf("[lnneo4j] Added %d bi-directional channels, %d uni-directional channels and %d channels without edges dropped.\n", numBiChnls, numUniChnls, numNoChnls)
}

// map RGBA to hex
func rgb2Hex(c color.RGBA) uint32 {
	return uint32((uint32(c.A) << 24) | (uint32(c.R) << 16) | (uint32(c.G) << 8) | uint32(c.B))
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		var homeDir string

		user, err := user.Current()
		if err == nil {
			homeDir = user.HomeDir
		} else {
			homeDir = os.Getenv("HOME")
		}

		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}
