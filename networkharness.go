package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
)

var (
	// numActiveNodes is the number of active nodes within the test network.
	numActiveNodes = 0

	// defaultNodePort is the initial p2p port which will be used by the
	// first created lightning node to listen on for incoming p2p
	// connections.  Subsequent allocated ports for future lighting nodes
	// instances will be monotonically increasing odd numbers calculated as
	// such: defaultP2pPort + (2 * harness.nodeNum).
	defaultNodePort = 19555

	// defaultClientPort is the initial rpc port which will be used by the
	// first created lightning node to listen on for incoming rpc
	// connections. Subsequent allocated ports for future rpc harness
	// instances will be monotonically increasing even numbers calculated
	// as such: defaultP2pPort + (2 * harness.nodeNum).
	defaultClientPort = 19556

	harnessNetParams = &chaincfg.SimNetParams
)

// generateListeningPorts returns two strings representing ports to listen on
// designated for the current lightning network test. If there haven't been any
// test instances created, the default ports are used. Otherwise, in order to
// support multiple test nodes running at once, the p2p and rpc port are
// incremented after each initialization.
func generateListeningPorts() (int, int) {
	var p2p, rpc int
	if numActiveNodes == 0 {
		p2p = defaultNodePort
		rpc = defaultClientPort
	} else {
		p2p = defaultNodePort + (2 * numActiveNodes)
		rpc = defaultClientPort + (2 * numActiveNodes)
	}

	return p2p, rpc
}

// lightningNode represents an instance of lnd running within our test network
// harness.
type lightningNode struct {
	cfg *config

	rpcAddr string
	p2pAddr string
	rpcCert []byte

	nodeId int

	cmd     *exec.Cmd
	pidFile string

	extraArgs []string
}

// newLightningNode creates a new test lightning node instance from the passed
// rpc config and slice of extra arguments.
func newLightningNode(rpcConfig *btcrpcclient.ConnConfig, lndArgs []string) (*lightningNode, error) {
	var err error

	cfg := &config{
		RPCHost: "127.0.0.1",
		RPCUser: rpcConfig.User,
		RPCPass: rpcConfig.Pass,
	}

	nodeNum := numActiveNodes
	cfg.DataDir, err = ioutil.TempDir("", "lndtest-data")
	if err != nil {
		return nil, err
	}
	cfg.LogDir, err = ioutil.TempDir("", "lndtest-log")
	if err != nil {
		return nil, err
	}

	cfg.PeerPort, cfg.RPCPort = generateListeningPorts()

	numActiveNodes++

	return &lightningNode{
		cfg:       cfg,
		p2pAddr:   net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.PeerPort)),
		rpcAddr:   net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.RPCPort)),
		rpcCert:   rpcConfig.Certificates,
		nodeId:    nodeNum,
		extraArgs: lndArgs,
	}, nil
}

// genArgs generates a slice of command line arguments from the lightningNode's
// current config struct.
func (l *lightningNode) genArgs() []string {
	var args []string

	encodedCert := hex.EncodeToString(l.rpcCert)
	args = append(args, fmt.Sprintf("--btcdhost=%v", l.cfg.RPCHost))
	args = append(args, fmt.Sprintf("--rpcuser=%v", l.cfg.RPCUser))
	args = append(args, fmt.Sprintf("--rpcpass=%v", l.cfg.RPCPass))
	args = append(args, fmt.Sprintf("--rawrpccert=%v", encodedCert))
	args = append(args, fmt.Sprintf("--rpcport=%v", l.cfg.RPCPort))
	args = append(args, fmt.Sprintf("--peerport=%v", l.cfg.PeerPort))
	args = append(args, fmt.Sprintf("--logdir=%v", l.cfg.LogDir))
	args = append(args, fmt.Sprintf("--datadir=%v", l.cfg.DataDir))
	args = append(args, fmt.Sprintf("--simnet"))

	if l.extraArgs != nil {
		args = append(args, l.extraArgs...)
	}

	return args
}

// start launches a new process running lnd. Additionally, the PID of the
// launched process is saved in order to possibly kill the process forcibly
// later.
func (l *lightningNode) start() error {
	args := l.genArgs()

	l.cmd = exec.Command("lnd", args...)
	if err := l.cmd.Start(); err != nil {
		return err
	}

	pid, err := os.Create(filepath.Join(l.cfg.DataDir,
		fmt.Sprintf("%s.pid", l.nodeId)))
	if err != nil {
		return err
	}
	l.pidFile = pid.Name()
	if _, err = fmt.Fprintf(pid, "%s\n", l.cmd.Process.Pid); err != nil {
		return err
	}
	if err := pid.Close(); err != nil {
		return err
	}

	return nil
}

// cleanup cleans up all the temporary files created by the node's process.
func (l *lightningNode) cleanup() error {
	dirs := []string{
		l.cfg.LogDir,
		l.cfg.DataDir,
	}

	var err error
	for _, dir := range dirs {
		if err = os.RemoveAll(dir); err != nil {
			log.Printf("Cannot remove dir %s: %v", dir, err)
		}
	}
	return err
}

// stop attempts to stop the active lnd process.
func (l *lightningNode) stop() error {
	if l.cmd == nil || l.cmd.Process == nil {
		return nil
	}

	defer l.cmd.Wait()

	if runtime.GOOS == "windows" {
		return l.cmd.Process.Signal(os.Kill)
	}
	return l.cmd.Process.Signal(os.Interrupt)
}

// shutdown stops the active lnd process and clean up any temporary directories
// created along the way.
func (l *lightningNode) shutdown() error {
	if err := l.stop(); err != nil {
		return err
	}
	if err := l.cleanup(); err != nil {
		return err
	}
	return nil
}

// networkHarness is an integration testing harness for the lightning network.
// The harness by default is created with two active nodes on the network:
// Alice and Bob.
type networkHarness struct {
	rpcConfig btcrpcclient.ConnConfig
	netParams *chaincfg.Params
	Miner     *rpctest.Harness

	activeNodes map[int]*lightningNode

	aliceNode *lightningNode
	bobNode   *lightningNode

	AliceClient lnrpc.LightningClient
	BobClient   lnrpc.LightningClient
}

// newNetworkHarness creates a new network test harness given an already
// running instance of btcd's rpctest harness. Any extra command line flags
// which should be passed to create lnd instance should be formatted properly
// in the lndArgs slice (--arg=value).
// TODO(roasbeef): add option to use golang's build library to a binary of the
// current repo. This'll save developers from having to manually `go install`
// within the repo each time before changes.
func newNetworkHarness(r *rpctest.Harness, lndArgs []string) (*networkHarness, error) {
	var err error

	nodeConfig := r.RPCConfig()

	testNet := &networkHarness{
		rpcConfig: nodeConfig,
		netParams: r.ActiveNet,
		Miner:     r,

		activeNodes: make(map[int]*lightningNode),
	}

	testNet.aliceNode, err = newLightningNode(&nodeConfig, nil)
	if err != nil {
		return nil, err
	}
	testNet.bobNode, err = newLightningNode(&nodeConfig, nil)
	if err != nil {
		return nil, err
	}

	testNet.activeNodes[testNet.aliceNode.nodeId] = testNet.aliceNode
	testNet.activeNodes[testNet.bobNode.nodeId] = testNet.bobNode

	return testNet, nil
}

// fakeLogger is a fake grpclog.Logger implementation. This is used to stop
// grpc's logger from printing directly to stdout.
type fakeLogger struct{}

func (f *fakeLogger) Fatal(args ...interface{})                 {}
func (f *fakeLogger) Fatalf(format string, args ...interface{}) {}
func (f *fakeLogger) Fatalln(args ...interface{})               {}
func (f *fakeLogger) Print(args ...interface{})                 {}
func (f *fakeLogger) Printf(format string, args ...interface{}) {}
func (f *fakeLogger) Println(args ...interface{})               {}

// SetUp starts the initial seeder nodes within the test harness. The initial
// node's wallets will be funded wallets with ten 1 BTC outputs each. Finally
// rpc clients capable of communicating with the initial seeder nodes are
// created.
func (n *networkHarness) SetUp() error {
	// Swap out grpc's default logger with out fake logger which drops the
	// statements on the floor.
	grpclog.SetLogger(&fakeLogger{})

	// Start the initial seeder nodes within the test network, then connect
	// their respective RPC clients.
	var err error
	if err := n.aliceNode.start(); err != nil {
		return err
	}
	if err := n.bobNode.start(); err != nil {
		return err
	}
	n.AliceClient, err = initRpcClient(n.aliceNode.rpcAddr)
	if err != nil {
		return nil
	}
	n.BobClient, err = initRpcClient(n.bobNode.rpcAddr)
	if err != nil {
		return nil
	}

	// Load up the wallets of the seeder nodes with 10 outputs of 1 BTC
	// each.
	ctxb := context.Background()
	addrReq := &lnrpc.NewAddressRequest{lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH}
	clients := []lnrpc.LightningClient{n.AliceClient, n.BobClient}
	for _, client := range clients {
		for i := 0; i < 10; i++ {
			resp, err := client.NewAddress(ctxb, addrReq)
			if err != nil {
				return err
			}
			addr, err := btcutil.DecodeAddress(resp.Address, n.netParams)
			if err != nil {
				return err
			}
			addrScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return err
			}

			output := &wire.TxOut{
				PkScript: addrScript,
				Value:    btcutil.SatoshiPerBitcoin,
			}
			if _, err := n.Miner.CoinbaseSpend([]*wire.TxOut{output}); err != nil {
				return err
			}
		}
	}

	// We generate several blocks in order to give the outputs created
	// above a good number of confirmations.
	if _, err := n.Miner.Node.Generate(10); err != nil {
		return err
	}

	// Finally, make a connection between both of the nodes.
	bobInfo, err := n.BobClient.GetInfo(ctxb, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}
	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			PubKeyHash: bobInfo.IdentityAddress,
			Host:       n.bobNode.p2pAddr,
		},
	}
	if _, err := n.AliceClient.ConnectPeer(ctxb, req); err != nil {
		return err
	}

	return nil
}

// TearDownAll tears down all active nodes within the test lightning network.
func (n *networkHarness) TearDownAll() error {
	for _, node := range n.activeNodes {
		if err := node.shutdown(); err != nil {
			return err
		}
	}

	return nil
}

// initRpcClient attempts to make an rpc connection, then create a gRPC client
// connected to the specified server address.
func initRpcClient(serverAddr string) (lnrpc.LightningClient, error) {
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(time.Second * 10),
	}
	conn, err := grpc.Dial(serverAddr, opts...)
	if err != nil {
		return nil, err
	}

	return lnrpc.NewLightningClient(conn), nil
}
