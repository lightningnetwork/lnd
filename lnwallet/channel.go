package wallet

import (
	"sync"

	"li.lan/labs/plasma/chainntfs"
	"li.lan/labs/plasma/revocation"

	"github.com/btcsuite/btcwallet/walletdb"
)


// P2SHify...
func P2SHify(scriptBytes []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_HASH160)
	bldr.AddData(btcutil.Hash160(scriptBytes))
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}


type nodeId [32]byte

type LightningChannel struct {
	wallet        *LightningWallet
	channelEvents *chainntnfs.ChainNotifier

	// TODO(roasbeef): Stores all previous R values + timeouts for each
	// commitment update, plus some other meta-data...Or just use OP_RETURN
	// to help out?
	channelNamespace walletdb.Namespace

	Id [32]byte

	capacity        btcutil.Amount
	ourTotalFunds   btcutil.Amount
	theirTotalFunds btcutil.Amount

	// TODO(roasbeef): another shachain for R values for HTLC's?
	// solve above?
	ourShaChain   *revocation.HyperShaChain
	theirShaChain *revocation.HyperShaChain

	ourFundingKey   *btcec.PrivateKey
	theirFundingKey *btcec.PublicKey

	ourCommitKey   *btcec.PublicKey
	theirCommitKey *btcec.PublicKey

	fundingTx    *wire.MsgTx
	commitmentTx *wire.MsgTx

	sync.RWMutex

	// TODO(roasbeef): create and embed 'Service' interface w/ below?
	started  int32
	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

// newLightningChannel...
// TODO(roasbeef): bring back and embedd CompleteReservation struct? kinda large
// atm.....
func newLightningChannel(wallet *LightningWallet, events *chainntnfs.ChainNotifier,
	dbNamespace walletdb.Namespace, theirNodeID nodeId, ourFunds,
	theirFunds btcutil.Amount, ourChain, theirChain *revocation.HyperShaChain,
	ourFundingKey *btcec.PrivateKey, theirFundingKey *btcec.PublicKey,
	commitKey, theirCommitKey *btcec.PublicKey, fundingTx,
	commitTx *wire.MsgTx) (*LightningChannel, error) {

	return &LightningChannel{
		wallet:           wallet,
		channelEvents:    events,
		channelNamespace: dbNamespace,
		Id:               theirNodeID,
		capacity:         ourFunds + theirFunds,
		ourTotalFunds:    ourFunds,
		theirTotalFunds:  theirFunds,
		ourShaChain:      ourChain,
		theirShaChain:    theirChain,
		ourFundingKey:    ourFundingKey,
		theirFundingKey:  theirFundingKey,
		ourCommitKey:     commitKey,
		theirCommitKey:   theirCommitKey,
		fundingTx:        fundingTx,
		commitmentTx:     commitTx,
	}, nil
}

// AddHTLC...
func (lc *LightningChannel) AddHTLC() {
}

// SettleHTLC...
func (lc *LightningChannel) SettleHTLC() {
}

// OurBalance...
func (lc *LightningChannel) OurBalance() btcutil.Amount {
	return 0
}

// TheirBalance...
func (lc *LightningChannel) TheirBalance() btcutil.Amount {
	return 0
}

// CurrentCommitTx...
func (lc *LightningChannel) CurrentCommitTx() *btcutil.Tx {
	return nil
}

// SignTheirCommitTx...
func (lc *LightningChannel) SignTheirCommitTx(commitTx *btcutil.Tx) error {
	return nil
}

// AddTheirSig...
func (lc *LightningChannel) AddTheirSig(sig []byte) error {
	return nil
}

// VerifyCommitmentUpdate...
func (lc *LightningChannel) VerifyCommitmentUpdate() error {
	return nil
}
