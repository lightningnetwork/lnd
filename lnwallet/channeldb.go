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

var (
	// Namespace bucket keys.
	lightningNamespaceKey = []byte("ln-wallet")
	waddrmgrNamespaceKey  = []byte("waddrmgr")
	wtxmgrNamespaceKey    = []byte("wtxmgr")

	openChannelBucket   = []byte("o-chans")
	closedChannelBucket = []byte("c-chans")
	fundingTxKey        = []byte("funding")

	endian = binary.BigEndian
)

// ChannelDB...
type ChannelDB struct {
	// TODO(roasbeef): caching, etc?
	wallet *LightningWallet

	namespace walletdb.Namespace
}

func NewChannelDB(wallet *LightningWallet, n walletdb.Namespace) *ChannelDB {
	return &ChannelDB{wallet, n}
}

// OpenChannelState...
// TODO(roasbeef): store only the essentials? optimize space...
// TODO(roasbeef): switch to "column store"
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
	OurCommitTx    *wire.MsgTx // TODO(roasbeef): store hash instead?
	TheirCommitSig []byte      // TODO(roasbeef): fixed length?, same w/ redeem

	// The final funding transaction. Kept wallet-related records.
	FundingTx *wire.MsgTx

	MultiSigKey         *btcec.PrivateKey
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

// Encode...
// TODO(roasbeef): checksum
func (o *OpenChannelState) Encode(b io.Writer, addrManager *waddrmgr.Manager) error {
	if _, err := b.Write(o.TheirLNID[:]); err != nil {
		return err
	}
	if _, err := b.Write(o.ChanID[:]); err != nil {
		return err
	}

	if err := binary.Write(b, endian, uint64(o.MinFeePerKb)); err != nil {
		return err
	}

	encryptedPriv, err := addrManager.Encrypt(waddrmgr.CKTPrivate,
		o.OurCommitKey.Serialize())
	if err != nil {
		return err
	}
	if _, err := b.Write(encryptedPriv); err != nil {
		return err
	}
	if _, err := b.Write(o.TheirCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	if err := binary.Write(b, endian, uint64(o.Capacity)); err != nil {
		return err
	}
	if err := binary.Write(b, endian, uint64(o.OurBalance)); err != nil {
		return err
	}
	if err := binary.Write(b, endian, uint64(o.TheirBalance)); err != nil {
		return err
	}

	if err := o.TheirCommitTx.Serialize(b); err != nil {
		return err
	}
	if err := o.OurCommitTx.Serialize(b); err != nil {
		return err
	}
	if _, err := b.Write(o.TheirCommitSig[:]); err != nil {
		return err
	}

	if err := o.FundingTx.Serialize(b); err != nil {
		return err
	}

	encryptedPriv, err = addrManager.Encrypt(waddrmgr.CKTPrivate,
		o.MultiSigKey.Serialize())
	if err != nil {
		return err
	}
	if _, err := b.Write(encryptedPriv); err != nil {
		return err
	}
	if _, err := b.Write(o.FundingRedeemScript); err != nil {
		return err
	}

	if _, err := b.Write(o.TheirCurrentRevocation[:]); err != nil {
		return err
	}
	// TODO(roasbeef): serialize shachains

	if _, err := b.Write([]byte(o.OurDeliveryAddress.EncodeAddress())); err != nil {
		return err
	}
	if _, err := b.Write([]byte(o.TheirDeliveryAddress.EncodeAddress())); err != nil {
		return err
	}

	if err := binary.Write(b, endian, o.CsvDelay); err != nil {
		return err
	}
	if err := binary.Write(b, endian, o.NumUpdates); err != nil {
		return err
	}
	if err := binary.Write(b, endian, o.TotalSatoshisSent); err != nil {
		return err
	}
	if err := binary.Write(b, endian, o.TotalSatoshisReceived); err != nil {
		return err
	}

	if err := binary.Write(b, endian, o.CreationTime.Unix()); err != nil {
		return err
	}

	return nil
}

// Decode...
func (o *OpenChannelState) Decode(b io.Reader, addrManager *waddrmgr.Manager) error {
	var scratch [8]byte

	if _, err := b.Read(o.TheirLNID[:]); err != nil {
		return err
	}
	if _, err := b.Read(o.ChanID[:]); err != nil {
		return err
	}

	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.MinFeePerKb = btcutil.Amount(endian.Uint64(scratch[:]))

	// nonce + serPrivKey + mac
	var encryptedPriv [24 + 32 + 16]byte
	if _, err := b.Read(encryptedPriv[:]); err != nil {
		return err
	}
	decryptedPriv, err := addrManager.Decrypt(waddrmgr.CKTPrivate, encryptedPriv[:])
	if err != nil {
		return err
	}
	o.OurCommitKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), decryptedPriv)

	var serPubKey [33]byte
	if _, err := b.Read(serPubKey[:]); err != nil {
		return err
	}
	o.TheirCommitKey, err = btcec.ParsePubKey(serPubKey[:], btcec.S256())
	if err != nil {
		return err
	}

	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.Capacity = btcutil.Amount(endian.Uint64(scratch[:]))
	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.OurBalance = btcutil.Amount(endian.Uint64(scratch[:]))
	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.TheirBalance = btcutil.Amount(endian.Uint64(scratch[:]))

	o.TheirCommitTx = wire.NewMsgTx()
	if err := o.TheirCommitTx.Deserialize(b); err != nil {
		return err
	}
	o.OurCommitTx = wire.NewMsgTx()
	if err := o.OurCommitTx.Deserialize(b); err != nil {
		return err
	}

	var sig [64]byte
	if _, err := b.Read(sig[:]); err != nil {
		return err
	}
	o.TheirCommitSig = sig[:]
	if err != nil {
		return err
	}

	o.FundingTx = wire.NewMsgTx()
	if err := o.FundingTx.Deserialize(b); err != nil {
		return err
	}

	if _, err := b.Read(encryptedPriv[:]); err != nil {
		return err
	}
	decryptedPriv, err = addrManager.Decrypt(waddrmgr.CKTPrivate, encryptedPriv[:])
	if err != nil {
		return err
	}
	o.MultiSigKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), decryptedPriv)

	var redeemScript [71]byte
	if _, err := b.Read(redeemScript[:]); err != nil {
		return err
	}
	o.FundingRedeemScript = redeemScript[:]

	if _, err := b.Read(o.TheirCurrentRevocation[:]); err != nil {
		return err
	}

	var addr [34]byte
	if _, err := b.Read(addr[:]); err != nil {
		return err
	}
	o.OurDeliveryAddress, err = btcutil.DecodeAddress(string(addr[:]), ActiveNetParams)
	if err != nil {
		return err
	}

	if _, err := b.Read(addr[:]); err != nil {
		return err
	}
	o.TheirDeliveryAddress, err = btcutil.DecodeAddress(string(addr[:]), ActiveNetParams)
	if err != nil {
		return err
	}

	if err := binary.Read(b, endian, &o.CsvDelay); err != nil {
		return err
	}
	if err := binary.Read(b, endian, &o.NumUpdates); err != nil {
		return err
	}
	if err := binary.Read(b, endian, &o.TotalSatoshisSent); err != nil {
		return err
	}
	if err := binary.Read(b, endian, &o.TotalSatoshisReceived); err != nil {
		return err
	}

	var unix int64
	if err := binary.Read(b, endian, &unix); err != nil {
		return err
	}
	o.CreationTime = time.Unix(unix, 0)

	return nil
}

func newOpenChannelState(ID [32]byte) *OpenChannelState {
	return &OpenChannelState{TheirLNID: ID}
}
