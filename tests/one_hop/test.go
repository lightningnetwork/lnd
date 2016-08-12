package main
import (
	"os"
	"io/ioutil"
	"log"
	"os/exec"
	"time"
	"bytes"
	"errors"
	"path/filepath"
	"fmt"
	"encoding/json"
	"strings"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcd/chaincfg"
	"strconv"
)

type LndNodeDesc struct {
	WorkDir string
	Host string
	PeerPort int
	RpcPort int
	LightningId string
	IdentityAddress string
}

var LndNodesDefault []LndNodeDesc = []LndNodeDesc{
	{
		WorkDir: "lnd-node1",
		Host: "127.0.0.1",
		PeerPort: 10011,
		RpcPort: 10009,
	},
	{
		WorkDir: "lnd-node2",
		Host: "127.0.0.1",
		PeerPort: 11011,
		RpcPort: 11009,
	},
	{
		WorkDir: "lnd-node3",
		Host: "127.0.0.1",
		PeerPort: 12011,
		RpcPort: 12009,
	},
}

func (node *LndNodeDesc) ConnectionAddress() string{
	return fmt.Sprintf("%v@%v:%v", node.IdentityAddress, node.Host, node.PeerPort)
}

func (node *LndNodeDesc) RpcAddress() string{
	return fmt.Sprintf("%v:%v", node.Host, node.RpcPort)
}

var TimeoutError = errors.New("Timeout error")

func ExecWithTimeout(name string, timeout time.Duration, dir string,  args...string)(stdOut, stdErr string, err error){
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	buffStdOut, buffStdErr := new(bytes.Buffer), new(bytes.Buffer)
	cmd.Stdout, cmd.Stderr = buffStdOut, buffStdErr
	err = cmd.Start()
	if err != nil{
		return
	}
	done := make(chan error, 1)
	go func(){
		done <- cmd.Wait()
	}()
	select{
	case err=<-done:
	case <-time.After(timeout):
		err = TimeoutError
		// TODO: What to do if call to kill fails?
		cmd.Process.Kill()
	}
	stdOut = buffStdOut.String()
	stdErr = buffStdErr.String()
	return
}

type SimNet struct {
	// Dir where initial files lives. Do not include trailing slash
	SeedDir string
	// Temporary dir where simulation is done
	WorkDir string
	// Dir with BTCD.
	btcdDir string
	btcdCmd *exec.Cmd
	btcdClient *chain.RPCClient
	lndDirs []string
	lndCmds []*exec.Cmd
	lndNodesDesc []LndNodeDesc
	btcwalletDir string
	btcwalletCmd *exec.Cmd
	btcctlDir string
}

func NewSimNet(lndNodesDesc []LndNodeDesc) *SimNet{
	if lndNodesDesc == nil{
		lndNodesDesc = LndNodesDefault
	}
	return &SimNet{
		SeedDir: "./simnet2",
		lndDirs: make([]string, len(lndNodesDesc)),
		lndCmds: make([]*exec.Cmd, len(lndNodesDesc)),
		lndNodesDesc: lndNodesDesc,
	}
}

//	 Create simnet directory and copy simulation files
func (sim *SimNet) InitTemp(){
	var err error
	sim.WorkDir, err = ioutil.TempDir("", "simnet")
	if err == nil{
		log.Print("Temp dir created ", sim.WorkDir)
	} else {
		log.Fatalf("Can't create temp dir: %v", err)
	}
	// Go does not include code for copying directories. So usage of external program is the simplest way
	_, _, err = ExecWithTimeout(
		"cp",
		1*time.Second, "",
		"-a",
		sim.SeedDir + "/.",
		sim.WorkDir,
	)
	if err != nil{
		log.Fatalf("Can't copy simnet dir %v", err)
	}
	sim.btcdDir = filepath.Join(sim.WorkDir, "btcd")
	sim.btcwalletDir = filepath.Join(sim.WorkDir, "btcwallet")
	sim.btcctlDir = filepath.Join(sim.WorkDir, "btcctl")
	for i:=0; i<len(sim.lndNodesDesc); i++{
		sim.lndDirs[i] = filepath.Join(sim.WorkDir, sim.lndNodesDesc[i].WorkDir)
	}
}

func (sim *SimNet) RemoveTemp(){
	// Remove simnet directory
	err := os.RemoveAll(sim.WorkDir)
	if err != nil {
		log.Fatalf("Can't remove temp dir %v: %v", sim.WorkDir, err)
	} else {
		log.Print("Temp dir deleted", sim.WorkDir)
	}
}

func (sim *SimNet) NewBTCDClient() *chain.RPCClient{
	certs, err := ioutil.ReadFile(filepath.Join(sim.btcdDir, "rpc.cert"))
	if err != nil {
		log.Fatalf("Error reading certificates:", err)
	}
	connConfig := &btcrpcclient.ConnConfig{
		Host: "127.0.0.1:18556",
		Endpoint: "ws",
		User: "myuser",
		Pass: "SomeDecentp4ssw0rd",
		Certificates: certs,
	}
	client, err := chain.NewRPCClient(&chaincfg.SimNetParams, connConfig.Host, connConfig.User, connConfig.Pass, connConfig.Certificates, false, 1)
	if err != nil {
		log.Fatalf("Error connecting to btcd:", err)
		return nil
	}
	err = client.Start()
	if err != nil {
		log.Fatalf("Error starting RPC client:", err)
		return nil
	}
	return client
}

func (sim *SimNet) StartBTCD(){
	cmd := exec.Command("bash", filepath.Join(sim.btcdDir, "start-btcd.sh"))
	cmd.Dir = sim.btcdDir
	sim.btcdCmd = cmd
	err := cmd.Start()
	if err != nil {
		log.Fatalf("Can't start BTCD")
	} else {
		log.Printf("BTCD started in %v", sim.btcdDir)
	}
	time.Sleep(1*time.Second)

	sim.btcdClient = sim.NewBTCDClient()
	log.Print("Connected to BTCD")
}

// NOTE: for some unknown reason it does not update blockstamps
func (sim *SimNet) GenerateBlocks(n uint32){
	hashes, err := sim.btcdClient.Generate(n)
	if err != nil {
		log.Fatalf("Can't mine blocks:", err)
	}
	log.Print("Mined blocks:", len(hashes))
}

func (sim *SimNet) StopBTCD(){
	sim.btcdClient.Stop()
	err := sim.btcdCmd.Process.Kill()
	if err != nil {
		log.Fatalf("Can't stop BTCD: %v", err)
	} else {
		log.Printf("BTCD stoped in %v", sim.btcdDir)
	}
}

// Start bitcoin wallet as a separate process
// It would be better to include it directly. However there is bug in btcwallet
// blockchain height is not updated when blocks are generated from the same program as wallet
func (sim *SimNet) StartWallet(){
	cmd := exec.Command("bash", filepath.Join(sim.btcwalletDir, "start-btcwallet.sh"))
	cmd.Dir = sim.btcwalletDir
	sim.btcwalletCmd = cmd
	err := cmd.Start()
	if err != nil {
		log.Fatalf("Can't start btcwallet")
	} else {
		log.Printf("btcwallet started in %v", sim.btcwalletDir)
	}
	time.Sleep(1*time.Second)
}

func (sim *SimNet) ShowBalance(){
	stdOut, stdErr,  err := ExecWithTimeout(
		"bash",
		1*time.Second, sim.btcctlDir,
		"btcctl.sh",
		"getbalance",
	)
	if err != nil {
		log.Fatalf("Can't get wallet balance: %v. Output: %v", err, stdErr)
	}
	balance := stdOut
	log.Println("Wallet balance:", balance)
}

func (sim *SimNet) ShowNewAddress(){
	stdOut, stdErr,  err := ExecWithTimeout(
		"bash",
		1*time.Second, sim.btcctlDir,
		"btcctl.sh",
		"getnewaddress",
	)
	if err != nil {
		log.Fatalf("Can't get wallet new address: %v. Output: %v", err, stdErr)
	}
	addr := stdOut
	log.Println("Wallet address:", addr)
}

func (sim *SimNet)UnlockWallet(){
	stdOut, stdErr, err := ExecWithTimeout(
		filepath.Join(sim.btcctlDir, "btcctl.sh"),
		2*time.Second, sim.btcctlDir,
		"walletpassphrase", "lol", "1000",
	)
	if err != nil {
		log.Fatalf("Can't unlock wallet: %v. Output %v %v", err, stdOut, stdErr)
	}
	log.Println("Wallet unlocked")
}

func (sim *SimNet) StopWallet() {
	err := sim.btcwalletCmd.Process.Kill()
	if err != nil {
		log.Println("Can't kill btcwallet", err)
	}
}

// Waits until wallet synchronizes with blockchain
//func (sim *SimNet) WaitWalletSynchronise(){
//	N := 100
//	t := 1 * time.Second
//	for i:=1;i<=N;i++{
//		if sim.wallet.ChainSynced(){
//			break
//		} else {
//			log.Printf("Wallet is not synced, Waiting for %v. Attempt %v/%v", t, i, N)
//			time.Sleep(t)
//		}
//	}
//	if !sim.wallet.ChainSynced(){
//		log.Fatalf("Chain is not synced after % attempts", N)
//	}
//}

// Starts LND node. i should be from 0 to len(lndNodesDesc)-1
func (sim *SimNet) StartLND(i int){
	if (i<0) || (i>=len(sim.lndNodesDesc)){
		log.Fatalf("Incorrect node i %v, should be from 0 to %v including", i, len(sim.lndNodesDesc)-1)
	}
	cmd := exec.Command("bash", filepath.Join(sim.lndDirs[i], "start-lnd.sh"))
	cmd.Dir = sim.lndDirs[i]
	sim.lndCmds[i] = cmd
	sim.lndCmds[i].Stdout = new(bytes.Buffer)
	sim.lndCmds[i].Stderr = new(bytes.Buffer)
	err := cmd.Start()
	if err != nil{
		log.Fatalf("Can't start lnd in %v: %v", sim.lndDirs[i], err)
	} else {
		log.Printf("LND started in %v", sim.lndCmds[i].Dir)
	}
}

// Stop LND node. i should be from 0 to len(sim.lndNodesDesc)-1
func (sim *SimNet) StopLND(i int){
	if (i<0) || (i>len(sim.lndNodesDesc)-1){
		log.Fatalf("Incorrect node i %v, should be from 0 to %v including", i, len(sim.lndNodesDesc)-1)
	}
	err := sim.lndCmds[i].Process.Kill()
	if err != nil {
		log.Fatalf("Can't stop LND: %v", err)
	} else {
		log.Printf("LND stopped in %v", sim.lndDirs[i])
	}
}

// Start all lnd nodes and get their identity addresses
func (sim *SimNet) StartAllLnd(){
	for i:=0; i<len(sim.lndNodesDesc); i++ {
		sim.StartLND(i)
	}
	log.Print("Start waiting until all LNDs fully start")
	time.Sleep(10*time.Second)
	log.Print("End waiting until all LND start")
	for i:=0; i<len(sim.lndNodesDesc); i++ {
		stdOut, _, err := ExecWithTimeout("lncli", 5*time.Second, "", "--rpcserver", sim.lndNodesDesc[i].RpcAddress(), "getinfo")
		if err != nil{
			log.Fatalf("Can't get info for %v which is working in %v", sim.lndNodesDesc[i].RpcAddress(), sim.lndDirs[i])
		}
		var info struct {
			LightningId string `json:"lightning_id"`
			IdentityAddress string `json:"identity_address"`
		}
		err = json.Unmarshal([]byte(stdOut), &info)
		if err != nil {
			log.Fatalf("Can't unmarshall command response %v", err)
		} else {
			log.Printf("Lnd working in %v has LightningId=%v and IdentityAddress=%v", sim.lndDirs[i], info.LightningId, info.IdentityAddress)
			sim.lndNodesDesc[i].IdentityAddress = info.IdentityAddress
			sim.lndNodesDesc[i].LightningId = info.LightningId
		}
	}
}


func (sim *SimNet)StopAllLnd(){
	for i:=0; i<len(sim.lndNodesDesc); i++{
		sim.StopLND(i)
	}
}

// Tries to connect first LND node to second. And do some operations.
func (sim *SimNet) TestConnect(){
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[0].RpcAddress(),
		"connect", sim.lndNodesDesc[1].ConnectionAddress(),
	)
	if err != nil {
		log.Fatalf("Can't connect node to other node: %v. Error output %v", err, stdErr)
	}
	var connectionInfo struct{
		PeerId int `json:"peer_id"`
	}
	err = json.Unmarshal([]byte(stdOut), &connectionInfo)
	if err != nil {
		log.Fatalf("Can't parse output of 'lncli connect': %v", err)
	}
	log.Print("Node 1 connect to node 2. peer_id:", connectionInfo.PeerId)

	// Check if first node knows about the second
	stdOut, stdErr, err = ExecWithTimeout("lncli", 1*time.Second, "", "--rpcserver", sim.lndNodesDesc[0].RpcAddress(), "listpeers")
	if err != nil {
		log.Fatalf("Can't listpeers for node 0: %v. Error output %v", err, stdErr)
	} else {
		var data struct {
			Peers [] *struct {
				LightningId string `json:"lightning_id"`
				PeerId int `json:"peer_id"`
				Address string `json:"address"`

			} `json:"peers"`
		}
		err := json.Unmarshal([]byte(stdOut), &data)
		if err != nil {
			log.Fatalf("Can't parse output of listpeers command")
		}
		if len(data.Peers) != 1{
			log.Fatalf("Incorrect reported by listpeers number of peers")
		}
		if data.Peers[0].LightningId != sim.lndNodesDesc[1].LightningId{
			log.Fatalf("Incorrect reported by listpeers lightning_id of peer. Get %v, want %v", data.Peers[0].LightningId, sim.lndNodesDesc[1].LightningId)
		}
		if data.Peers[0].PeerId != 1 {
			log.Fatalf("Incorrect reported by listpeers peer_id of peer. Get %v, want %v", data.Peers[0].PeerId, 1)
		}
		expectedAddr := fmt.Sprintf("%v:%v", sim.lndNodesDesc[1].Host, sim.lndNodesDesc[1].PeerPort)
		if data.Peers[0].Address != expectedAddr {
			log.Fatalf("Incorrect reported by listpeers lightning_id of peer. Get %v, want %v", data.Peers[0].Address, expectedAddr)
		}
	}

	// Check balance of the first node
	stdOut, stdErr, err = ExecWithTimeout("lncli", 1*time.Second, "", "--rpcserver", sim.lndNodesDesc[0].RpcAddress(), "walletbalance")
	if err != nil {
		log.Fatalf("Error calling walletbalance: %v", err)
	}
	if stdOut != "{}" {
		log.Fatalf("walletbalance for empty wallet should return {}, got", stdOut)
	}

	// Check balance of the second node
	stdOut, stdErr, err = ExecWithTimeout("lncli", 1*time.Second, "", "--rpcserver", sim.lndNodesDesc[1].RpcAddress(), "walletbalance")
	if err != nil {
		log.Fatalf("Error calling walletbalance: %v", err)
	}
	if stdOut != "{}" {
		log.Fatalf("walletbalance for empty wallet should return {}, got", stdOut)
	}
}

func (sim *SimNet) SendInitialMoneyFirstNode(){
	stdOut, _, err := ExecWithTimeout("lncli", 1*time.Second, "", "--rpcserver", sim.lndNodesDesc[0].RpcAddress(), "newaddress", "p2wkh")
	if err != nil {
		log.Fatalf("Error calling newaddress: %v", err)
	}
	var data struct {
		Address string `json:"address"`
	}
	err = json.Unmarshal([]byte(stdOut), &data)
	if err != nil {
		log.Fatalf("Can't parse output of newaddress:", err)
	}
	log.Println("New address for first(0) node generated", data.Address)

	stdOut, stdErr, err := ExecWithTimeout(
		filepath.Join(sim.btcctlDir, "btcctl.sh"),
		1 * time.Second, sim.btcctlDir,
		"sendtoaddress", data.Address, "10",
	)
	if err != nil {
		log.Fatalf("Can't send money to the first node %v .Output: %v %v", err, stdOut, stdErr)
	}
	log.Print("Sending bitcoins to first wallet. TxID:", stdOut)
}

//Kills all btcd, lnd, btcwallet processes to ensure clear start
func KillAll(){
	stdOut, stdErr, err := ExecWithTimeout(
		"pkill",
		1*time.Second, "",
		"-9", "btcd|lnd|btcwallet",
	)
	if err != nil{
		log.Printf("Can't launch pkill: %v. Output: %v %v", err, stdOut, stdErr)
	}
}

func (sim *SimNet) ShowBalanceFirstNode(){
	// Check balance of the first node
	stdOut, stdErr, err := ExecWithTimeout("lncli", 1*time.Second, "", "--rpcserver", sim.lndNodesDesc[0].RpcAddress(), "walletbalance")
	if err != nil {
		log.Fatalf("Error calling walletbalance: %v Output: %v %v", err, stdOut, stdErr)
	}
	log.Printf("Balance of first node is %v", stdOut)
}

func (sim *SimNet) GetBalanceForNode(n int) float64 {
	if ! (0 <= n && n < len(sim.lndNodesDesc)){
		log.Fatalf("Incorrect node number")
	}
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[n].RpcAddress(),
		"walletbalance",
	)
	if err != nil {
		log.Fatalf("Error calling walletbalance: %v Output: %v %v", err, stdOut, stdErr)
	}
	var data struct {
		Balance float64 `json:"balance"`
	}
	err = json.Unmarshal([]byte(stdOut), &data)
	if err != nil {
		log.Fatalf("Can't decode output of walletbalance: %v", err)
	}
	return data.Balance
}

func (sim *SimNet)OpenChannelFromFirstToSecond(){
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[0].RpcAddress(),
		"openchannel", "--peer_id=1", "--local_amt=100000000", "--remote_amt=0", "--num_confs=1",
	)
	if err != nil {
		log.Fatalf("Can't open channel %v. Output: %v %v", err, stdOut, stdErr)
	}
	log.Println("OPenchannel", stdOut, stdErr, err)
}

func (sim *SimNet)SendMoneyBetweenNodes(from, to, amount uint){
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		10*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[from].RpcAddress(),
		"sendpayment", "--dest="+sim.lndNodesDesc[to].LightningId,  fmt.Sprintf("--amt=%v", amount),
	)
	if err!=nil{
		log.Fatalf("Can't send payment from %v to %v: %v. Output %v %v", from, to, err, stdOut, stdErr)
	}
	log.Println("Sendpayment", stdOut, stdErr, err)
}

func (sim *SimNet)ShowListPeers(n int){
	if ! (0 <= n && n < len(sim.lndNodesDesc)){
		log.Fatalf("Incorrect node number")
	}
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[n].RpcAddress(),
		"listpeers",
	)
	if err != nil {
		log.Fatalf("Error calling listpeers: %v Output: %v %v", err, stdOut, stdErr)
	}
	log.Printf("listpeers %v: %v", n, stdOut)
}

func (sim *SimNet)ShowPendingChannels(n int){
	if ! (0 <= n && n < len(sim.lndNodesDesc)){
		log.Fatalf("Incorrect node number")
	}
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[n].RpcAddress(),
		"pendingchannels",
	)
	if err != nil {
		log.Fatalf("Error calling pendingchannels: %v Output: %v %v", err, stdOut, stdErr)
	}
	log.Printf("pendingchannels %v: %v", n, stdOut)
}


func (sim *SimNet)GetLightningBalance(n int) int{
	if ! (0 <= n && n < len(sim.lndNodesDesc)){
		log.Fatalf("Incorrect node number")
	}
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[n].RpcAddress(),
		"listpeers",
	)
	if err != nil {
		log.Fatalf("Error calling listpeers: %v Output: %v %v", err, stdOut, stdErr)
	}
	var data struct {
		Peers []struct {
				Channels []struct{
					LocalBalance int `json:"local_balance"`
					RemoteBalance int `json:"remote_balance"`
				} `json:"channels"`
			  } `json:"peers"`
	}
	log.Println(stdOut)
	err = json.Unmarshal([]byte(stdOut), &data)
	if err != nil {
		log.Fatalln("Error parsing data", err)
	}
	total := 0
	for _, peer := range data.Peers{
		for _, channel := range peer.Channels{
			total += channel.LocalBalance
		}
	}
	return total
}

func (sim *SimNet) GetFundingOutput(node, peer, channel int) (string, int){
	if ! (0 <= node && node < len(sim.lndNodesDesc)){
		log.Fatalf("Incorrect node number")
	}
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[node].RpcAddress(),
		"listpeers",
	)
	if err != nil {
		log.Fatalf("Error calling listpeers: %v Output: %v %v", err, stdOut, stdErr)
	}
	var data struct {
		Peers []struct {
				Channels []struct{
					LocalBalance int `json:"local_balance"`
					RemoteBalance int `json:"remote_balance"`
					ChannelPoint string `json:"channel_point"`
				} `json:"channels"`
			  } `json:"peers"`
	}
	err = json.Unmarshal([]byte(stdOut), &data)
	if err != nil {
		log.Fatalln("Error parsing data", err)
	}
	outPoint := data.Peers[peer].Channels[channel].ChannelPoint
	split := strings.Split(outPoint, ":")
	if len(split) != 2 {
		log.Fatalf("Incorrect format of output point, got %v, want tx:n", outPoint)
	}
	n, err := strconv.Atoi(split[1])
	if err != nil {
		log.Fatalf("Can't convert to int %v", err)
	}
	return split[0], n
}

func (sim *SimNet) CloseChannelFromFirstToSecond(){
	txFund, nFund := sim.GetFundingOutput(0, 0, 0)
	log.Println("Funding transaction", txFund, nFund)
	stdOut, stdErr, err := ExecWithTimeout(
		"lncli",
		1*time.Second, "",
		"--rpcserver", sim.lndNodesDesc[0].RpcAddress(),
		"closechannel", txFund, strconv.Itoa(nFund), "1000", "0",
	)
	if err != nil {
		log.Fatalf("Error calling closechannel: %v Output: %v %v", err, stdOut, stdErr)
	}
	log.Println("closechannel:", stdOut)
}

func main(){
	KillAll()
	sim := NewSimNet(nil)
	// TODO: how to proceed in others if some fails
	sim.InitTemp()
	// Some coins are already generated for wallet
	sim.StartBTCD()
	sim.StartWallet()
	time.Sleep(1*time.Second)
	sim.ShowNewAddress()
	sim.UnlockWallet()
//	sim.WaitWalletSynchronise()
//	time.Sleep(5*time.Second)
	sim.ShowBalance()

	sim.StartAllLnd()
	sim.TestConnect()
	sim.SendInitialMoneyFirstNode()
	sim.GenerateBlocks(10)
	time.Sleep(1*time.Second)
	sim.GenerateBlocks(10)
	sim.ShowBalance()
	for i:=0; i< 3; i++{
		log.Printf("Balance of node %v is %v", i, sim.GetBalanceForNode(i))
	}
	sim.GenerateBlocks(10)
	time.Sleep(1*time.Second)
	sim.OpenChannelFromFirstToSecond()
	time.Sleep(1*time.Second)
	sim.GenerateBlocks(1)
	bestHash, _, err := sim.btcdClient.GetBestBlock()
	if err != nil {
		log.Fatalf("Can't get best block hash: %v", err)
	}
	bestBlock, err := sim.btcdClient.GetBlock(bestHash)
	if err != nil{
		log.Fatalf("Can't get best block: %v", err)
	}
	if len(bestBlock.Transactions()) != 2{
		log.Fatalf("After opening channel blockchain should have funding transaction."+
		" So block should have 2 transactions. Got %v", len(bestBlock.Transactions()))
	}
	time.Sleep(1*time.Second)
	sim.SendMoneyBetweenNodes(0, 1, 1000)

	// I guess there should be direct call for Lightning balances
	// For now version using listpeers
	expectedBalances := []int{100000000-1000, 1000, 0}
	for i:=0; i< 3; i++{
		balance := sim.GetLightningBalance(i)
		if expectedBalances[i] != balance{
			log.Printf("Lightning balance of node %v is %v, want %v", i, balance, expectedBalances[i])
		}
	}

//	sim.SendMoneyBetweenNodes(1, 0, 100)
//	// I guess there should be direct call for Lightning balances
//	// For now version using listpeers
//	expectedBalances = []int{100000000-1000+100, 1000-100, 0}
//	for i:=0; i< 3; i++{
//		balance := sim.GetLightningBalance(i)
//		if expectedBalances[i] != balance{
//			log.Printf("Lightning balance of node %v is %v, want %v", i, balance, expectedBalances[i])
//		}
//	}

	sim.CloseChannelFromFirstToSecond()
	time.Sleep(1*time.Second)
	sim.ShowListPeers(0)
	sim.GenerateBlocks(1)
	bestHash, _, err = sim.btcdClient.GetBestBlock()
	if err != nil {
		log.Fatalf("Can't get best block hash: %v", err)
	}
	bestBlock, err = sim.btcdClient.GetBlock(bestHash)
	if err != nil{
		log.Fatalf("Can't get best block: %v", err)
	}
	if len(bestBlock.Transactions()) != 2{
		log.Fatalf("After closing channel blockchain should have funding transaction."+
		" So block should have 2 transactions. Got %v", len(bestBlock.Transactions()))
	}


	sim.StopWallet()
	sim.StopBTCD()
	sim.RemoveTemp()
	log.Print("SUCCESS")
}
