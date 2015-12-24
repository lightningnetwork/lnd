package lnwallet

import (
	"encoding/binary"
	"io"
	"time"

	"li.lan/labs/plasma/shachain"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
)
// OpenChannelState...
// TODO(roasbeef): trim...
type OpenChannelState struct {
	// Hash? or Their current pubKey?
	// TODO(roasbeef): switch to Tadge's LNId
	TheirLNID [wire.HashSize]byte

	// The ID of a channel is the txid of the funding transaction.
	ChanID [wire.HashSize]byte

	MinFeePerKb btcutil.Amount
	// Our reserve. Assume symmetric reserve amounts. Only needed if the
	// funding type is CLTV.
	//ReserveAmount btcutil.Amount

	// Keys for both sides to be used for the commitment transactions.
	OurCommitKey   *btcec.PrivateKey
	TheirCommitKey *btcec.PublicKey

	// Tracking total channel capacity, and the amount of funds allocated
	// to each side.
	Capacity     btcutil.Amount
	OurBalance   btcutil.Amount
	TheirBalance btcutil.Amount

	// Commitment transactions for both sides (they're asymmetric). Also
	// their signature which lets us spend our version of the commitment
	// transaction.
	TheirCommitTx  *wire.MsgTx
	OurCommitTx    *wire.MsgTx
	TheirCommitSig []byte

	// The final funding transaction. Kept wallet-related records.
	FundingTx *wire.MsgTx

	// TODO(roasbeef): instead store a btcutil.Address here? Otherwise key
	// is stored unencrypted! Use manager.Encrypt() when storing.
	MultiSigKey *btcec.PrivateKey
	// TODO(roasbeef): encrypt also, or store in waddrmanager?
	FundingRedeemScript []byte

	// Current revocation for their commitment transaction. However, since
	// this is the hash, and not the pre-image, we can't yet verify that
	// it's actually in the chain.
	TheirCurrentRevocation [wire.HashSize]byte
	TheirShaChain          *shachain.HyperShaChain
	OurShaChain            *shachain.HyperShaChain

	// Final delivery address
	OurDeliveryAddress   btcutil.Address
	TheirDeliveryAddress btcutil.Address

	// In blocks
	CsvDelay uint32

	// TODO(roasbeef): track fees, other stats?
	NumUpdates            uint64
	TotalSatoshisSent     uint64
	TotalSatoshisReceived uint64
	CreationTime          time.Time
}
