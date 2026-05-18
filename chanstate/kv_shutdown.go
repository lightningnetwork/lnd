package chanstate

import (
	"bytes"
	"errors"
	"io"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// shutdownInfoKey points to the serialised shutdown info that has been
	// persisted for a channel. The existence of this info means that we
	// have sent the Shutdown message before and so should re-initiate the
	// shutdown on re-establish.
	shutdownInfoKey = []byte("shutdown-info-key")
)

// ShutdownInfoKey returns the key for the serialised shutdown info stored in a
// channel bucket.
func ShutdownInfoKey() []byte {
	return shutdownInfoKey
}

// PutChannelShutdownInfo persists the ShutdownInfo in the target channel
// bucket.
func PutChannelShutdownInfo(chanBucket kvdb.RwBucket,
	info *ShutdownInfo) error {

	var b bytes.Buffer
	err := EncodeShutdownInfo(info, &b)
	if err != nil {
		return err
	}

	return chanBucket.Put(shutdownInfoKey, b.Bytes())
}

// FetchChannelShutdownInfo fetches the persisted ShutdownInfo from the target
// channel bucket.
func FetchChannelShutdownInfo(chanBucket kvdb.RBucket) (
	*ShutdownInfo, error) {

	shutdownInfoBytes := chanBucket.Get(shutdownInfoKey)
	if shutdownInfoBytes == nil {
		return nil, ErrNoShutdownInfo
	}

	return DecodeShutdownInfo(shutdownInfoBytes)
}

// StoreChannelShutdownInfo persists the ShutdownInfo for the target channel.
func StoreChannelShutdownInfo(backend kvdb.Backend, channel *OpenChannel,
	info *ShutdownInfo) error {

	return kvdb.Update(backend, func(tx kvdb.RwTx) error {
		chanBucket, err := FetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return PutChannelShutdownInfo(chanBucket, info)
	}, func() {})
}

// FetchShutdownInfo fetches the persisted ShutdownInfo for the target channel.
func FetchShutdownInfo(backend kvdb.Backend,
	channel *OpenChannel) (fn.Option[ShutdownInfo], error) {

	var shutdownInfo *ShutdownInfo
	err := kvdb.View(backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return ErrNoShutdownInfo
		default:
			return err
		}

		shutdownInfo, err = FetchChannelShutdownInfo(chanBucket)

		return err
	}, func() {
		shutdownInfo = nil
	})
	if err != nil {
		return fn.None[ShutdownInfo](), err
	}

	return fn.Some[ShutdownInfo](*shutdownInfo), nil
}

// EncodeShutdownInfo serialises the ShutdownInfo to the given io.Writer.
func EncodeShutdownInfo(s *ShutdownInfo, w io.Writer) error {
	records := []tlv.Record{
		s.DeliveryScript.Record(),
		s.LocalInitiator.Record(),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// DecodeShutdownInfo constructs a ShutdownInfo struct by decoding the given
// byte slice.
func DecodeShutdownInfo(b []byte) (*ShutdownInfo, error) {
	tlvStream := lnwire.ExtraOpaqueData(b)

	var info ShutdownInfo
	records := []tlv.RecordProducer{
		&info.DeliveryScript,
		&info.LocalInitiator,
	}

	_, err := tlvStream.ExtractRecords(records...)

	return &info, err
}
