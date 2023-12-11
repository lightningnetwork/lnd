package lnwire

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// QueryOptionsRecordType is the TLV number of the query_options TLV
	// record in the query_channel_range message.
	QueryOptionsRecordType tlv.Type = 1

	// QueryOptionTimestampBit is the bit position in the query_option
	// feature bit vector which is used to indicate that timestamps are
	// desired in the reply_channel_range response.
	QueryOptionTimestampBit = 0
)

// QueryOptions is the type used to represent the query_options feature bit
// vector in the query_channel_range message.
type QueryOptions RawFeatureVector

// NewTimestampQueryOption is a helper constructor used to construct a
// QueryOption with the timestamp bit set.
func NewTimestampQueryOption() *QueryOptions {
	opt := QueryOptions(*NewRawFeatureVector(
		QueryOptionTimestampBit,
	))

	return &opt
}

// featureBitLen calculates and returns the size of the resulting feature bit
// vector.
func (c *QueryOptions) featureBitLen() uint64 {
	fv := RawFeatureVector(*c)

	return uint64(fv.SerializeSize())
}

// Record constructs a tlv.Record from the QueryOptions to be used in the
// query_channel_range message.
func (c *QueryOptions) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		QueryOptionsRecordType, c, c.featureBitLen, queryOptionsEncoder,
		queryOptionsDecoder,
	)
}

// queryOptionsEncoder encodes the QueryOptions and writes it to the provided
// writer.
func queryOptionsEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*QueryOptions); ok {
		// Encode the feature bits as a byte slice without its length
		// prepended, as that's already taken care of by the TLV record.
		fv := RawFeatureVector(*v)
		return fv.encode(w, fv.SerializeSize(), 8)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.QueryOptions")
}

// queryOptionsDecoder attempts to read a QueryOptions from the given reader.
func queryOptionsDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*QueryOptions); ok {
		fv := NewRawFeatureVector()
		if err := fv.decode(r, int(l), 8); err != nil {
			return err
		}

		*v = QueryOptions(*fv)

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.QueryOptions")
}
