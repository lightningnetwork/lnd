package lntest

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

	// logOutput is a flag that can be set to append the output from the
	// seed nodes to log files.
	logOutput = flag.Bool("logoutput", false,
		"log output from node n to file outputn.log")

	// trickleDelay is the amount of time in milliseconds between each
	// release of announcements by AuthenticatedGossiper to the network.
	trickleDelay = 50
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

type nodeConfig struct {
	RPCConfig *rpcclient.ConnConfig
	NetParams *chaincfg.Params
	BaseDir   string
	ExtraArgs []string

	DataDir      string
	LogDir       string
	TLSCertPath  string
	TLSKeyPath   string
	AdminMacPath string
	ReadMacPath  string
	P2PPort      int
	RPCPort      int
}

func (cfg nodeConfig) P2PAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.P2PPort))
}

func (cfg nodeConfig) RPCAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.RPCPort))
}

func (cfg nodeConfig) DBPath() string {
	return filepath.Join(cfg.DataDir, cfg.NetParams.Name, "bitcoin/channel.db")
}

// genArgs generates a slice of command line arguments from the lightning node
// config struct.
func (cfg nodeConfig) genArgs() []string {
	var args []string

	switch cfg.NetParams {
	case &chaincfg.TestNet3Params:
		args = append(args, "--bitcoin.testnet")
	case &chaincfg.SimNetParams:
		args = append(args, "--bitcoin.simnet")
	case &chaincfg.RegressionNetParams:
		args = append(args, "--bitcoin.regtest")
	}

	encodedCert := hex.EncodeToString(cfg.RPCConfig.Certificates)
	args = append(args, "--bitcoin.active")
	args = append(args, "--nobootstrap")
	args = append(args, "--noencryptwallet")
	args = append(args, "--debuglevel=debug")
	args = append(args, "--defaultchanconfs=1")
	args = append(args, fmt.Sprintf("--bitcoin.rpchost=%v", cfg.RPCConfig.Host))
	args = append(args, fmt.Sprintf("--bitcoin.rpcuser=%v", cfg.RPCConfig.User))
	args = append(args, fmt.Sprintf("--bitcoin.rpcpass=%v", cfg.RPCConfig.Pass))
	args = append(args, fmt.Sprintf("--bitcoin.rawrpccert=%v", encodedCert))
	args = append(args, fmt.Sprintf("--rpcport=%v", cfg.RPCPort))
	args = append(args, fmt.Sprintf("--peerport=%v", cfg.P2PPort))
	args = append(args, fmt.Sprintf("--logdir=%v", cfg.LogDir))
	args = append(args, fmt.Sprintf("--datadir=%v", cfg.DataDir))
	args = append(args, fmt.Sprintf("--tlscertpath=%v", cfg.TLSCertPath))
	args = append(args, fmt.Sprintf("--tlskeypath=%v", cfg.TLSKeyPath))
	args = append(args, fmt.Sprintf("--configfile=%v", cfg.DataDir))
	args = append(args, fmt.Sprintf("--adminmacaroonpath=%v", cfg.AdminMacPath))
	args = append(args, fmt.Sprintf("--readonlymacaroonpath=%v", cfg.ReadMacPath))
	args = append(args, fmt.Sprintf("--externalip=%s", cfg.P2PAddr()))
	args = append(args, fmt.Sprintf("--trickledelay=%v", trickleDelay))

	if cfg.ExtraArgs != nil {
		args = append(args, cfg.ExtraArgs...)
	}

	return args
}

// lightningNode represents an instance of lnd running within our test network
// harness. Each lightningNode instance also fully embedds an RPC client in
// order to pragmatically drive the node.
type lightningNode struct {
	cfg *nodeConfig

	nodeID int

	// PubKey is the serialized compressed identity public key of the node.
	// This field will only be populated once the node itself has been
	// started via the start() method.
	PubKey    [33]byte
	PubKeyStr string

	cmd     *exec.Cmd
	pidFile string

	// processExit is a channel that's closed once it's detected that the
	// process this instance of lightningNode is bound to has exited.
	processExit chan struct{}

	chanWatchRequests chan *chanWatchRequest

	quit chan struct{}
	wg   sync.WaitGroup

	lnrpc.LightningClient
}

// newLightningNode creates a new test lightning node instance from the passed
// rpc config and slice of extra arguments.
func newLightningNode(cfg nodeConfig) (*lightningNode, error) {
	if cfg.BaseDir == "" {
		var err error
		cfg.BaseDir, err = ioutil.TempDir("", "lndtest-node")
		if err != nil {
			return nil, err
		}
	}
	cfg.DataDir = filepath.Join(cfg.BaseDir, "data")
	cfg.LogDir = filepath.Join(cfg.BaseDir, "log")
	cfg.TLSCertPath = filepath.Join(cfg.DataDir, "tls.cert")
	cfg.TLSKeyPath = filepath.Join(cfg.DataDir, "tls.key")
	cfg.AdminMacPath = filepath.Join(cfg.DataDir, "admin.macaroon")
	cfg.ReadMacPath = filepath.Join(cfg.DataDir, "readonly.macaroon")

	cfg.P2PPort, cfg.RPCPort = generateListeningPorts()

	nodeNum := numActiveNodes
	numActiveNodes++

	return &lightningNode{
		cfg:               &cfg,
		nodeID:            nodeNum,
		chanWatchRequests: make(chan *chanWatchRequest),
		processExit:       make(chan struct{}),
		quit:              make(chan struct{}),
	}, nil
}

// Start launches a new process running lnd. Additionally, the PID of the
// launched process is saved in order to possibly kill the process forcibly
// later.
func (l *lightningNode) Start(lndError chan<- error) error {
	args := l.cfg.genArgs()
	l.cmd = exec.Command("lnd", args...)

	// Redirect stderr output to buffer
	var errb bytes.Buffer
	l.cmd.Stderr = &errb

	// If the logoutput flag is passed, redirect output from the nodes to
	// log files.
	if *logOutput {
		logFile := fmt.Sprintf("output%d.log", l.nodeID)

		// Create file if not exists, otherwise append.
		file, err := os.OpenFile(logFile,
			os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
		if err != nil {
			return err
		}

		// Pass node's stderr to both errb and the file.
		w := io.MultiWriter(&errb, file)
		l.cmd.Stderr = w

		// Pass the node's stdout only to the file.
		l.cmd.Stdout = file
	}

	if err := l.cmd.Start(); err != nil {
		return err
	}

	// Launch a new goroutine which that bubbles up any potential fatal
	// process errors to the goroutine running the tests.
	go func() {
		err := l.cmd.Wait()
		if err != nil {
			lndError <- errors.Errorf("%v\n%v\n", err, errb.String())
		}

		// Signal any onlookers that this process has exited.
		close(l.processExit)
	}()

	// Write process ID to a file.
	if err := l.writePidFile(); err != nil {
		l.cmd.Process.Kill()
		return err
	}

	// Since Stop uses the LightningClient to stop the node, if we fail to get a
	// connected client, we have to kill the process.
	conn, err := l.connectRPC()
	if err != nil {
		l.cmd.Process.Kill()
		return err
	}
	l.LightningClient = lnrpc.NewLightningClient(conn)

	// Obtain the lnid of this node for quick identification purposes.
	ctxb := context.Background()
	info, err := l.GetInfo(ctxb, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	l.PubKeyStr = info.IdentityPubkey

	pubkey, err := hex.DecodeString(info.IdentityPubkey)
	if err != nil {
		return err
	}
	copy(l.PubKey[:], pubkey)

	// Launch the watcher that'll hook into graph related topology change
	// from the PoV of this node.
	l.wg.Add(1)
	go l.lightningNetworkWatcher()

	return nil
}

// writePidFile writes the process ID of the running lnd process to a .pid file.
func (l *lightningNode) writePidFile() error {
	filePath := filepath.Join(l.cfg.BaseDir, fmt.Sprintf("%v.pid", l.nodeID))

	pid, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer pid.Close()

	_, err = fmt.Fprintf(pid, "%v\n", l.cmd.Process.Pid)
	if err != nil {
		return err
	}

	l.pidFile = filePath
	return nil
}

// connectRPC uses the TLS certificate and admin macaroon files written by the
// lnd node to create a gRPC client connection.
func (l *lightningNode) connectRPC() (*grpc.ClientConn, error) {
	// Wait until TLS certificate and admin macaroon are created before
	// using them, up to 20 sec.
	tlsTimeout := time.After(30 * time.Second)
	for !fileExists(l.cfg.TLSCertPath) || !fileExists(l.cfg.AdminMacPath) {
		select {
		case <-tlsTimeout:
			return nil, fmt.Errorf("timeout waiting for TLS cert file " +
				"and admin macaroon file to be created after " +
				"20 seconds")
		case <-time.After(100 * time.Millisecond):
		}
	}

	tlsCreds, err := credentials.NewClientTLSFromFile(l.cfg.TLSCertPath, "")
	if err != nil {
		return nil, err
	}
	macBytes, err := ioutil.ReadFile(l.cfg.AdminMacPath)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(macaroons.NewMacaroonCredential(mac)),
		grpc.WithBlock(),
		grpc.WithTimeout(time.Second * 20),
	}
	return grpc.Dial(l.rpcAddr, opts...)
}

// cleanup cleans up all the temporary files created by the node's process.
func (l *lightningNode) cleanup() error {
	return os.RemoveAll(l.cfg.BaseDir)
}

// Stop attempts to stop the active lnd process.
func (l *lightningNode) Stop() error {
	// Do nothing if the process never started successfully.
	if l.LightningClient == nil {
		return nil
	}

	// Do nothing if the process already finished.
	select {
	case <-l.quit:
		return nil
	case <-l.processExit:
		return nil
	default:
	}

	// Don't watch for error because sometimes the RPC connection gets
	// closed before a response is returned.
	req := lnrpc.StopRequest{}
	ctx := context.Background()
	l.LightningClient.StopDaemon(ctx, &req)

	close(l.quit)
	l.wg.Wait()
	return nil
}

// Restart attempts to restart a lightning node by shutting it down cleanly,
// then restarting the process. This function is fully blocking. Upon restart,
// the RPC connection to the node will be re-attempted, continuing iff the
// connection attempt is successful. Additionally, if a callback is passed, the
// closure will be executed after the node has been shutdown, but before the
// process has been started up again.
func (l *lightningNode) Restart(errChan chan error, callback func() error) error {
	if err := l.Stop(); err != nil {
		return err
	}

	<-l.processExit

	l.LightningClient = nil
	l.processExit = make(chan struct{})
	l.quit = make(chan struct{})
	l.wg = sync.WaitGroup{}

	if callback != nil {
		if err := callback(); err != nil {
			return err
		}
	}

	return l.Start(errChan)
}

// Shutdown stops the active lnd process and clean up any temporary directories
// created along the way.
func (l *lightningNode) Shutdown() error {
	if err := l.Stop(); err != nil {
		return err
	}
	if err := l.cleanup(); err != nil {
		return err
	}
	return nil
}

// closeChanWatchRequest is a request to the lightningNetworkWatcher to be
// notified once it's detected within the test Lightning Network, that a
// channel has either been added or closed.
type chanWatchRequest struct {
	chanPoint wire.OutPoint

	chanOpen bool

	eventChan chan struct{}
}

// lightningNetworkWatcher is a goroutine which is able to dispatch
// notifications once it has been observed that a target channel has been
// closed or opened within the network. In order to dispatch these
// notifications, the GraphTopologySubscription client exposed as part of the
// gRPC interface is used.
func (l *lightningNode) lightningNetworkWatcher() {
	defer l.wg.Done()

	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()

		ctxb := context.Background()
		req := &lnrpc.GraphTopologySubscription{}
		topologyClient, err := l.SubscribeChannelGraph(ctxb, req)
		if err != nil {
			// We panic here in case of an error as failure to
			// create the topology client will cause all subsequent
			// tests to fail.
			panic(fmt.Errorf("unable to create topology "+
				"client: %v", err))
		}

		for {
			update, err := topologyClient.Recv()
			if err == io.EOF {
				return
			} else if err != nil {
				return
			}

			select {
			case graphUpdates <- update:
			case <-l.quit:
				return
			}
		}
	}()

	// For each outpoint, we'll track an integer which denotes the number
	// of edges seen for that channel within the network. When this number
	// reaches 2, then it means that both edge advertisements has
	// propagated through the network.
	openChans := make(map[wire.OutPoint]int)
	openClients := make(map[wire.OutPoint][]chan struct{})

	closedChans := make(map[wire.OutPoint]struct{})
	closeClients := make(map[wire.OutPoint][]chan struct{})

	for {
		select {

		// A new graph update has just been received, so we'll examine
		// the current set of registered clients to see if we can
		// dispatch any requests.
		case graphUpdate := <-graphUpdates:
			// For each new channel, we'll increment the number of
			// edges seen by one.
			for _, newChan := range graphUpdate.ChannelUpdates {
				txid, _ := chainhash.NewHash(newChan.ChanPoint.FundingTxid)
				op := wire.OutPoint{
					Hash:  *txid,
					Index: newChan.ChanPoint.OutputIndex,
				}
				openChans[op]++

				// For this new channel, if the number of edges
				// seen is less than two, then the channel
				// hasn't been fully announced yet.
				if numEdges := openChans[op]; numEdges < 2 {
					continue
				}

				// Otherwise, we'll notify all the registered
				// clients and remove the dispatched clients.
				for _, eventChan := range openClients[op] {
					close(eventChan)
				}
				delete(openClients, op)
			}

			// For each channel closed, we'll mark that we've
			// detected a channel closure while lnd was pruning the
			// channel graph.
			for _, closedChan := range graphUpdate.ClosedChans {
				txid, _ := chainhash.NewHash(closedChan.ChanPoint.FundingTxid)
				op := wire.OutPoint{
					Hash:  *txid,
					Index: closedChan.ChanPoint.OutputIndex,
				}
				closedChans[op] = struct{}{}

				// As the channel has been closed, we'll notify
				// all register clients.
				for _, eventChan := range closeClients[op] {
					close(eventChan)
				}
				delete(closeClients, op)
			}

		// A new watch request, has just arrived. We'll either be able
		// to dispatch immediately, or need to add the client for
		// processing later.
		case watchRequest := <-l.chanWatchRequests:
			targetChan := watchRequest.chanPoint

			// TODO(roasbeef): add update type also, checks for
			// multiple of 2
			if watchRequest.chanOpen {
				// If this is a open request, then it can be
				// dispatched if the number of edges seen for
				// the channel is at least two.
				if numEdges := openChans[targetChan]; numEdges >= 2 {
					close(watchRequest.eventChan)
					continue
				}

				// Otherwise, we'll add this to the list of
				// watch open clients for this out point.
				openClients[targetChan] = append(openClients[targetChan],
					watchRequest.eventChan)
				continue
			}

			// If this is a close request, then it can be
			// immediately dispatched if we've already seen a
			// channel closure for this channel.
			if _, ok := closedChans[targetChan]; ok {
				close(watchRequest.eventChan)
				continue
			}

			// Otherwise, we'll add this to the list of close watch
			// clients for this out point.
			closeClients[targetChan] = append(closeClients[targetChan],
				watchRequest.eventChan)

		case <-l.quit:
			return
		}
	}
}

// WaitForNetworkChannelOpen will block until a channel with the target
// outpoint is seen as being fully advertised within the network. A channel is
// considered "fully advertised" once both of its directional edges has been
// advertised within the test Lightning Network.
func (l *lightningNode) WaitForNetworkChannelOpen(ctx context.Context,
	op *lnrpc.ChannelPoint) error {

	eventChan := make(chan struct{})

	txid, err := chainhash.NewHash(op.FundingTxid)
	if err != nil {
		return err
	}

	l.chanWatchRequests <- &chanWatchRequest{
		chanPoint: wire.OutPoint{
			Hash:  *txid,
			Index: op.OutputIndex,
		},
		eventChan: eventChan,
		chanOpen:  true,
	}

	select {
	case <-eventChan:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("channel not opened before timeout")
	}
}

// WaitForNetworkChannelClose will block until a channel with the target
// outpoint is seen as closed within the network. A channel is considered
// closed once a transaction spending the funding outpoint is seen within a
// confirmed block.
func (l *lightningNode) WaitForNetworkChannelClose(ctx context.Context,
	op *lnrpc.ChannelPoint) error {

	eventChan := make(chan struct{})

	txid, err := chainhash.NewHash(op.FundingTxid)
	if err != nil {
		return err
	}

	l.chanWatchRequests <- &chanWatchRequest{
		chanPoint: wire.OutPoint{
			Hash:  *txid,
			Index: op.OutputIndex,
		},
		eventChan: eventChan,
		chanOpen:  false,
	}

	select {
	case <-eventChan:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("channel not closed before timeout")
	}
}

// WaitForBlockchainSync will block until the target nodes has fully
// synchronized with the blockchain. If the passed context object has a set
// timeout, then the goroutine will continually poll until the timeout has
// elapsed. In the case that the chain isn't synced before the timeout is up,
// then this function will return an error.
func (l *lightningNode) WaitForBlockchainSync(ctx context.Context) error {
	errChan := make(chan error, 1)
	retryDelay := time.Millisecond * 100

	go func() {
		for {
			select {
			case <-ctx.Done():
			case <-l.quit:
				return
			default:
			}

			getInfoReq := &lnrpc.GetInfoRequest{}
			getInfoResp, err := l.GetInfo(ctx, getInfoReq)
			if err != nil {
				errChan <- err
				return
			}
			if getInfoResp.SyncedToChain {
				errChan <- nil
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
		}
	}()

	select {
	case <-l.quit:
		return nil
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return fmt.Errorf("Timeout while waiting for blockchain sync")
	}
}
