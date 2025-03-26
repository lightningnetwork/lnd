package lndtest

import (
	"bufio"
	"context"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcdtest"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

type Config struct {
	Stderr io.Writer
	Stdout io.Writer

	Path string
	Dir  string

	Chain *chaincfg.Params

	Args []string

	RPCAddresses  []string
	P2PAddresses  []string
	RESTAddresses []string
}

type Harness struct {
	*grpc.ClientConn

	cmd *exec.Cmd
	cfg *Config

	rpc  string
	p2p  string
	rest string

	mac  []byte
	cert []byte
}

// Wait for the node to be synced to the chain.
func (h *Harness) Sync(ctx context.Context) {
	ln := lnrpc.NewLightningClient(h.ClientConn)

	for ctx.Err() == nil {
		res, err := ln.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			panic(err)
		}

		if res.SyncedToChain {
			break
		}
	}

	if err := ctx.Err(); err != nil {
		panic(err)
	}
}

func (h *Harness) RPCAddress() string {
	return h.rpc
}

func (h *Harness) RESTAddress() string {
	return h.rest
}

func (h *Harness) P2pAddress() string {
	return h.p2p
}

func (h *Harness) AdminMacPath() string {
	return filepath.Join(h.cfg.Dir, "data", "chain", "bitcoin", nameParams(h.cfg.Chain), "admin.macaroon")
}

func (h *Harness) AdminMac() []byte {
	return h.mac
}

func (h *Harness) TLSCert() []byte {
	return h.cert
}

func (h *Harness) TLSCertPath() string {
	return filepath.Join(h.cfg.Dir, "tls.cert")
}

func (h *Harness) dial(addMac, addTlsCert bool) error {
	// Close any lingering connections.
	if h.ClientConn != nil {
		h.ClientConn.Close()
		h.ClientConn = nil
	}

	var opts []grpc.DialOption

	// Add the macaroon creds.
	if addMac {
		// Read the macaroon file.
		b, err := os.ReadFile(h.AdminMacPath())
		if err != nil {
			return err
		}

		mac := &macaroon.Macaroon{}

		// Decode the macaroon file.
		err = mac.UnmarshalBinary(b)
		if err != nil {
			return err
		}

		// Create a credential from the macaroon.
		cred, err := macaroons.NewMacaroonCredential(mac)
		if err != nil {
			return err
		}

		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	// add the TLS certificate.
	if addTlsCert {
		// Parse the TLS certificate.
		creds, err := credentials.NewClientTLSFromFile(h.TLSCertPath(), "")
		if err != nil {
			return err
		}

		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	// Dial the node.
	var err error
	h.ClientConn, err = grpc.Dial(h.rpc, opts...)
	if err != nil {
		return err
	}

	return nil
}

var DefaultPassword = []byte("password")

func (h *Harness) createWallet() ([]string, error) {
	// Create a new unlocker client.
	unlock := lnrpc.NewWalletUnlockerClient(h)

	// Generate a new wallet seed.
	res, err := unlock.GenSeed(context.Background(), &lnrpc.GenSeedRequest{})
	if err != nil {
		return nil, err
	}

	seed := res.CipherSeedMnemonic

	// Create a new wallet.
	_, err = unlock.InitWallet(context.TODO(), &lnrpc.InitWalletRequest{
		WalletPassword:     DefaultPassword,
		CipherSeedMnemonic: seed,
	})
	if err != nil {
		return nil, err
	}

	start := time.Now()

	// Attempt to dial the node until it is ready or a timeout is reached.
	for time.Since(start) < timeout {
		// Dial the node.
		err := h.dial(true, true)
		if err != nil {
			continue
		}

		err = nil
		break
	}

	if h.ClientConn == nil {
		panic("timeout")
	}

	// Handle any lingering errors.
	if err != nil {
		panic(err)
	}

	start = time.Now()

	//  Wait for the server to be active or a timeout is reached.
	for time.Since(start) < timeout {
		state, err := lnrpc.NewStateClient(h.ClientConn).GetState(context.Background(), &lnrpc.GetStateRequest{})
		if err != nil {
			continue
		}

		if *state.State.Enum() != lnrpc.WalletState_SERVER_ACTIVE {
			continue
		}

		break
	}

	if h.ClientConn == nil {
		panic("timeout")
	}

	// Handle any lingering errors.
	if err != nil {
		panic(err)
	}

	// Read the admin macaroon.
	h.mac, err = os.ReadFile(h.AdminMacPath())
	if err != nil {
		return nil, err
	}

	return seed, nil
}

var timeout = 30 * time.Second

func (h *Harness) Start() {
	name := h.cfg.Path
	if name == "" {
		name = "lnd"
	}

	h.cmd = exec.Command(name, h.cfg.Args...)

	// Create a pipe of stdout.
	pr, pw, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	h.cmd.Stderr = h.cfg.Stderr
	h.cmd.Stdout = pw

	err = h.cmd.Start()
	if err != nil {
		panic(err)
	}

	var r io.Reader = pr

	// Pipe the early output to stdout if configured.
	if h.cfg.Stdout != nil {
		r = io.TeeReader(pr, h.cfg.Stdout)
	}

	// Scan the stdout line by line.
	scan := bufio.NewScanner(r)

	// Scan for the RPC & REST (gRPC proxy) to be listening.
	for scan.Scan() && (h.rpc == "" || h.rest == "") {
		line := scan.Text()

		_, addr, ok := strings.Cut(line, "RPC server listening on ")
		if ok {
			h.rpc = addr
			continue
		}

		_, addr, ok = strings.Cut(line, "gRPC proxy started at ")
		if ok {
			h.rest = addr
			continue
		}
	}

	p2pChan := make(chan string)

	// Scan for the P2P server to be listening, this happens after wallet unlock.
	go func() {
		for scan.Scan() {
			line := scan.Text()

			_, addr, ok := strings.Cut(line, "Server listening on ")
			if ok {
				p2pChan <- addr
				break
			}
		}
	}()

	start := time.Now()

	// Attempt to dial the node until it is ready or a timeout is reached.
	for time.Since(start) < timeout {
		// Dial the node.
		err = h.dial(false, true)
		if err != nil {
			continue
		}

		// Query the state.
		state, err := lnrpc.NewStateClient(h.ClientConn).GetState(context.Background(), &lnrpc.GetStateRequest{})
		if err != nil {
			continue
		}

		// Wait for the RPC to be ready.
		if *state.State.Enum() == lnrpc.WalletState_WAITING_TO_START {
			continue
		}

		// Clear the error.
		err = nil
		break
	}

	if h.ClientConn == nil {
		panic("timeout")
	}

	// Handle any lingering errors.
	if err != nil {
		panic(err)
	}

	// Read the TLC certificate.
	h.cert, err = os.ReadFile(h.TLSCertPath())
	if err != nil {
		panic(err)
	}

	// Create a new wallet.
	_, err = h.createWallet()
	if err != nil {
		panic(err)
	}

	// Wait for the p2p channel.
	h.p2p = <-p2pChan

	// Discard as a fallback.
	stdout := io.Discard

	// Use the configured stdout by default.
	if h.cfg.Stdout != nil {
		stdout = h.cfg.Stdout
	}

	// The pipe needs to continuously be read, otherwise `btcd` will hang.
	go io.Copy(stdout, pr)
}

// Stop the process, wait for it to exit.
func (h *Harness) Stop() {
	err := h.cmd.Process.Signal(os.Interrupt)
	if err != nil {
		panic(err)
	}

	for {
		exit, err := h.cmd.Process.Wait()
		if err != nil {
			panic(err)
		}

		if exit.Exited() {
			break
		}
	}
}

type optFunc = func(*Config)

// Use a `btcdtest` Harness as the chain backend..
func WithBtcd(btcd *btcdtest.Harness) optFunc {
	return func(cfg *Config) {
		rpcCfg, err := btcd.RPCConnConfig()
		if err != nil {
			panic(err)
		}

		cfg.Args = append(cfg.Args,
			"--bitcoin.node", "btcd",
			"--btcd.rpchost", rpcCfg.Host,
			"--btcd.rpcuser", rpcCfg.User,
			"--btcd.rpcpass", rpcCfg.Pass,
			"--btcd.rawrpccert", hex.EncodeToString(rpcCfg.Certificates),
		)
	}
}

// Set the chain params.
func WithChainParams(chain *chaincfg.Params) optFunc {
	return func(cfg *Config) {
		cfg.Chain = chain
	}
}

// Update the root directory.
func WithDir(dir string) func(*Config) {
	return func(cfg *Config) {
		cfg.Dir = dir
	}
}

// Update the output.
func WithOutput(stderr io.Writer, stdout io.Writer) func(*Config) {
	return func(cfg *Config) {
		cfg.Stderr = stderr
		cfg.Stdout = stdout
	}
}

// Set custom binary path.
func WithBinary(path string) func(*Config) {
	return func(cfg *Config) {
		cfg.Path = path
	}
}

// Append arguments.
func WithArgs(args ...string) optFunc {
	return func(cfg *Config) {
		cfg.Args = append(cfg.Args,
			args...,
		)
	}
}

// Append a P2P address.
func WithP2PAddress(addr string) optFunc {
	return func(cfg *Config) {
		cfg.P2PAddresses = append(cfg.P2PAddresses, addr)
	}
}

// Append a REST address.
func WithRESTAddress(addr string) optFunc {
	return func(cfg *Config) {
		cfg.RESTAddresses = append(cfg.RESTAddresses, addr)
	}
}

// Append a RPC address.
func WithRPCAddress(addr string) optFunc {
	return func(cfg *Config) {
		cfg.RPCAddresses = append(cfg.RPCAddresses, addr)
	}
}

// Create a new harness.
func New(opts ...optFunc) *Harness {
	h := NewUnstarted(opts...)
	h.Start()

	return h
}

// Create a new unstarted harness.
func NewUnstarted(opts ...optFunc) *Harness {
	// Create a temporary directory.
	tmp, err := os.MkdirTemp("", "lndtest-*")
	if err != nil {
		panic(err)
	}

	cfg := &Config{
		// Use simnet by default.
		Chain: &chaincfg.SimNetParams,
		Dir:   tmp,

		// Use port zero by default to allocate a random port.
		RPCAddresses:  []string{"127.0.0.1:0"},
		P2PAddresses:  []string{"127.0.0.1:0"},
		RESTAddresses: []string{"127.0.0.1:0"},
	}

	// Apply all the configuration functions..
	for _, opt := range opts {
		opt(cfg)
	}

	// Set the required arguments.
	cfg.Args = append(cfg.Args,
		"--bitcoin."+nameParams(cfg.Chain),
		"--lnddir", cfg.Dir,
	)

	// Set all the P2P addresses.
	for _, addr := range cfg.P2PAddresses {
		cfg.Args = append(cfg.Args, "--listen", addr)
	}

	// Set all the REST addresses.
	for _, addr := range cfg.RESTAddresses {
		cfg.Args = append(cfg.Args, "--restlisten", addr)
	}

	// Set all the RPC addresses.
	for _, addr := range cfg.RPCAddresses {
		cfg.Args = append(cfg.Args, "--rpclisten", addr)
	}

	return &Harness{
		cfg: cfg,
	}
}

// `btcd` refers to testnet3 as "testnet", match behaviour here.
// https://github.com/btcsuite/btcd/blob/cd05d9ad3d0597368adf95c54bdc530700393aed/params.go#L73-L89
func nameParams(chain *chaincfg.Params) string {
	var name string

	switch chain.Name {
	case chaincfg.TestNet3Params.Name:
		name = "testnet"

	default:
		name = chain.Name
	}

	return name
}
