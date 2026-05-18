package chanstate

import (
	"bytes"
	"io"

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
