package funding

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
)

var (
	// byteOrder defines the endian-ness we use for encoding to and from
	// buffers.
	byteOrder = binary.BigEndian
)

// WriteOutpoint writes an outpoint to an io.Writer. This is not the same as
// the channeldb variant as this uses WriteVarBytes for the Hash.
func WriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	_, err := w.Write(scratch)
	return err
}

const (
	// MinBtcRemoteDelay is the minimum CSV delay we will require the remote
	// to use for its commitment transaction.
	MinBtcRemoteDelay uint16 = 144

	// MaxBtcRemoteDelay is the maximum CSV delay we will require the remote
	// to use for its commitment transaction.
	MaxBtcRemoteDelay uint16 = 2016

	// MinLtcRemoteDelay is the minimum Litecoin CSV delay we will require the
	// remote to use for its commitment transaction.
	MinLtcRemoteDelay uint16 = 576

	// MaxLtcRemoteDelay is the maximum Litecoin CSV delay we will require the
	// remote to use for its commitment transaction.
	MaxLtcRemoteDelay uint16 = 8064

	// MinChanFundingSize is the smallest channel that we'll allow to be
	// created over the RPC interface.
	MinChanFundingSize = btcutil.Amount(20000)

	// MaxBtcFundingAmount is a soft-limit of the maximum channel size
	// currently accepted on the Bitcoin chain within the Lightning
	// Protocol. This limit is defined in BOLT-0002, and serves as an
	// initial precautionary limit while implementations are battle tested
	// in the real world.
	MaxBtcFundingAmount = btcutil.Amount(1<<24) - 1

	// MaxBtcFundingAmountWumbo is a soft-limit on the maximum size of wumbo
	// channels. This limit is 10 BTC and is the only thing standing between
	// you and limitless channel size (apart from 21 million cap)
	MaxBtcFundingAmountWumbo = btcutil.Amount(1000000000)

	// MaxLtcFundingAmount is a soft-limit of the maximum channel size
	// currently accepted on the Litecoin chain within the Lightning
	// Protocol.
	MaxLtcFundingAmount = MaxBtcFundingAmount * chainreg.BtcToLtcConversionRate
)
